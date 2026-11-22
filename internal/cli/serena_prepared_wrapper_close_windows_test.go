//go:build windows

package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// This fixture owns only the wrapper receipt. It supplies no native child,
// prepared package, Ready publication, or consumer-intent acceptance evidence.
func preparedWrapperReceiptScope(ctx context.Context) (*preparedWrapperServerScope, *preparedConsumerLifecycleAdapter) {
	adapter := &preparedConsumerLifecycleAdapter{}
	scope := &preparedWrapperServerScope{
		adapter: adapter, server: &http.Server{}, serveEntered: true,
		serveReceived: true, shutdownEntered: true, shutdownReceived: true,
	}
	scope.deadline, _ = ctx.Deadline()
	adapter.wrapper = scope
	return scope, adapter
}

func TestPreparedSerenaWrapperServeFailureReceipt(t *testing.T) {
	childExit := errors.New("controlled upstream exit")
	for _, tc := range []struct {
		name        string
		preReceived bool
		cause       error
	}{
		{name: "serve-before-shutdown", preReceived: true},
		{name: "service-before-serve"},
		{name: "child-exit-before-serve", cause: childExit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Both endpoints belong to this test. The first listener stays open
			// while the real server attempts the same private loopback address.
			held, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			server := &http.Server{Addr: held.Addr().String()}
			defer server.Close()
			serve := make(chan error, 1)
			serveComplete := make(chan struct{})
			go func() {
				defer close(serveComplete)
				defer close(serve)
				serve <- server.ListenAndServe()
			}()
			// Keep the bind result available but unread in the late-receive
			// cases. Shutdown must not win before the real listen attempt.
			select {
			case <-serveComplete:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var first error
			if tc.preReceived {
				select {
				case first = <-serve:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			scope, adapter := preparedWrapperReceiptScope(ctx)
			scope.server, scope.serveDone = server, serve
			scope.shutdownDone = make(chan error, 1)
			stopCalls := 0
			var stopErr error
			scope.shutdownError, stopErr, scope.serveError, scope.shutdownReceived, scope.serveReceived = receivePreparedWrapperShutdown(ctx, server.Shutdown, server.Close, func(stopCtx context.Context) error {
				stopCalls++
				if stopCtx != ctx {
					return errors.New("wrapper renewed the cleanup context")
				}
				return nil
			}, scope.shutdownDone, serve, tc.preReceived, first)
			if stopErr != nil || scope.shutdownError != nil || stopCalls != 1 || !scope.shutdownReceived || !scope.serveReceived {
				t.Fatalf("wrapper workers did not settle: stop=%v shutdown=%v stopCalls=%d shutdownReceived=%v serveReceived=%v", stopErr, scope.shutdownError, stopCalls, scope.shutdownReceived, scope.serveReceived)
			}
			if !errors.Is(scope.serveError, windows.WSAEADDRINUSE) && !errors.Is(scope.serveError, windows.WSAEACCES) {
				t.Fatalf("private occupied address did not produce a bind refusal: %v", scope.serveError)
			}
			proof := scope.receive(ctx)
			receiveErr := proof.validate(ctx, adapter)
			if receiveErr != nil {
				t.Errorf("received Serve failure poisoned resource closure: %v (cause=%v)", receiveErr, scope.serveError)
			}
			cause := tc.cause
			if tc.preReceived {
				cause = first
			}
			result := scope.result(cause, stopErr, receiveErr)
			var exit forceExitFace
			if !errors.As(result, &exit) || exit.ExitCode() != exitBindRefused || !exit.IsMcphubForceExit() {
				t.Errorf("settled bind refusal lost its reallocation exit: %v", result)
			}
			if !errors.Is(result, scope.serveError) || tc.cause != nil && !errors.Is(result, tc.cause) {
				t.Errorf("final result lost the original cause: %v", result)
			}
			if scope.receive(ctx) != proof {
				t.Error("wrapper close observation was replayed")
			}
		})
	}
}

func TestPreparedSerenaWrapperResultPolicy(t *testing.T) {
	serveFailure := errors.New("controlled Serve failure")
	shutdownFailure := errors.New("controlled Shutdown failure")
	stopFailure := errors.New("controlled native stop failure")
	receiveFailure := errors.New("controlled consumer close failure")
	childExit := errors.New("controlled child exit")
	for _, tc := range []struct {
		name                                  string
		serve, shutdown, stop, receive, cause error
		wantBind                              bool
	}{
		{name: "clean", serve: nil},
		{name: "ordinary-serve-failure", serve: serveFailure},
		{name: "child-exit", cause: childExit},
		{name: "address-in-use", serve: windows.WSAEADDRINUSE, wantBind: true},
		{name: "access-denied", serve: windows.WSAEACCES, wantBind: true},
		{name: "shutdown-failure", serve: windows.WSAEADDRINUSE, shutdown: shutdownFailure},
		{name: "stop-failure", serve: windows.WSAEADDRINUSE, stop: stopFailure},
		{name: "receive-failure", serve: windows.WSAEADDRINUSE, receive: receiveFailure},
		{name: "all-causes", serve: windows.WSAEACCES, shutdown: shutdownFailure, stop: stopFailure, receive: receiveFailure, cause: childExit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := &preparedWrapperServerScope{serveError: tc.serve, shutdownError: tc.shutdown}
			result := scope.result(tc.cause, tc.stop, tc.receive)
			var exit forceExitFace
			gotBind := errors.As(result, &exit)
			if gotBind != tc.wantBind || gotBind && (exit.ExitCode() != exitBindRefused || !exit.IsMcphubForceExit()) {
				t.Errorf("bind exit=%v, want %v: %v", gotBind, tc.wantBind, result)
			}
			wantError := false
			for _, cause := range []error{tc.cause, tc.serve, tc.shutdown, tc.stop, tc.receive} {
				if cause != nil {
					wantError = true
					if !errors.Is(result, cause) {
						t.Errorf("lost original cause %v in %v", cause, result)
					}
				}
			}
			if !wantError && result != nil {
				t.Errorf("clean shutdown failed: %v", result)
			}
		})
	}
}

func TestPreparedSerenaWrapperCloseRequiresCompleteReceipt(t *testing.T) {
	for _, name := range []string{
		"serve-not-received", "shutdown-not-entered", "shutdown-not-received",
		"shutdown-failed", "foreign-adapter", "different-deadline", "no-deadline", "canceled",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			scope, adapter := preparedWrapperReceiptScope(ctx)
			scope.serveError = windows.WSAEADDRINUSE
			switch name {
			case "serve-not-received":
				scope.serveReceived = false
			case "shutdown-not-entered":
				scope.shutdownEntered = false
			case "shutdown-not-received":
				scope.shutdownReceived = false
			case "shutdown-failed":
				scope.shutdownError = errors.New("controlled shutdown failure")
			case "foreign-adapter":
				adapter.wrapper = nil
			case "different-deadline":
				scope.deadline = scope.deadline.Add(time.Second)
			case "no-deadline":
				ctx = context.Background()
			case "canceled":
				cancel()
			}
			receiveErr := scope.receive(ctx).validate(ctx, adapter)
			if receiveErr == nil {
				t.Fatal("incomplete or foreign closure was accepted")
			}
			result := scope.result(nil, nil, receiveErr)
			var exit forceExitFace
			if errors.As(result, &exit) {
				t.Fatalf("unsettled consumer was made reallocation-safe: %v", result)
			}
			if !errors.Is(result, windows.WSAEADDRINUSE) || !errors.Is(result, receiveErr) {
				t.Fatalf("refusal lost bind or cleanup cause: %v", result)
			}
		})
	}
}

func TestPreparedSerenaWrapperReceiveRequiresWorkerEnd(t *testing.T) {
	for _, name := range []string{"missing-frame", "duplicate-frame", "frame-without-close"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			serve := make(chan error, 2)
			firstReceived := false
			var first error
			switch name {
			case "missing-frame":
				close(serve)
			case "duplicate-frame":
				serve <- windows.WSAEADDRINUSE
				serve <- windows.WSAEADDRINUSE
				close(serve)
			case "frame-without-close":
				firstReceived, first = true, windows.WSAEADDRINUSE
				defer close(serve)
			}
			scope, adapter := preparedWrapperReceiptScope(ctx)
			scope.shutdownDone = make(chan error, 1)
			var stopErr error
			scope.shutdownError, stopErr, scope.serveError, scope.shutdownReceived, scope.serveReceived = receivePreparedWrapperShutdown(ctx,
				func(context.Context) error { return nil }, func() error { return nil }, func(context.Context) error {
					if name == "frame-without-close" {
						cancel()
					}
					return nil
				}, scope.shutdownDone, serve, firstReceived, first)
			// Even the intentionally canceled case receives this test-owned
			// immediate shutdown worker before the test returns.
			for range scope.shutdownDone {
			}
			if scope.serveReceived || scope.serveError == nil {
				t.Fatalf("Serve completion manufactured: received=%v err=%v", scope.serveReceived, scope.serveError)
			}
			if name == "missing-frame" && !strings.Contains(scope.serveError.Error(), "SERENA_WRAPPER_WORKER_RESULT_MISSING") ||
				name == "duplicate-frame" && !strings.Contains(scope.serveError.Error(), "SERENA_WRAPPER_WORKER_RESULT_DUPLICATE") ||
				name == "frame-without-close" && !errors.Is(scope.serveError, context.Canceled) {
				t.Errorf("worker error lost: %v", scope.serveError)
			}
			receiveErr := scope.receive(ctx).validate(ctx, adapter)
			if receiveErr == nil {
				t.Fatal("incomplete worker released the wrapper receipt")
			}
			var exit forceExitFace
			if result := scope.result(nil, stopErr, receiveErr); errors.As(result, &exit) {
				t.Fatalf("incomplete worker authorized reallocation: %v", result)
			}
		})
	}
}
