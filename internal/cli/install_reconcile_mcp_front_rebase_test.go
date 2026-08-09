package cli

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func TestActivateMCPFrontRoute_StagesDescriptorBeforePreflight(t *testing.T) {
	var order []string
	err := activateMCPFrontRoute(context.Background(), 9555, mcpFrontRouteActivationOps{
		stage: func(context.Context, int) error {
			order = append(order, "stage")
			return nil
		},
		waitListener: func(context.Context, int) error {
			order = append(order, "wait-listener")
			return nil
		},
		preflight: func(context.Context, int) error {
			order = append(order, "preflight")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("activate route: %v", err)
	}
	if got := len(order); got != 3 || order[0] != "stage" || order[1] != "wait-listener" || order[2] != "preflight" {
		t.Fatalf("activation order=%v, want [stage wait-listener preflight]", order)
	}

	preflightCalled := false
	sentinel := errors.New("stage failed")
	err = activateMCPFrontRoute(context.Background(), 9555, mcpFrontRouteActivationOps{
		stage: func(context.Context, int) error { return sentinel },
		waitListener: func(context.Context, int) error {
			t.Fatal("listener wait ran after stage failure")
			return nil
		},
		preflight: func(context.Context, int) error {
			preflightCalled = true
			return nil
		},
	})
	if !errors.Is(err, sentinel) || preflightCalled {
		t.Fatalf("stage failure err=%v preflightCalled=%v, want failure before probe", err, preflightCalled)
	}
}

func TestActivateMCPFrontRoute_WaitsForReconciledListener(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve delayed-listener port: %v", err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	if err := reservation.Close(); err != nil {
		t.Fatalf("release delayed-listener reservation: %v", err)
	}

	stageCalls := 0
	preflightCalls := 0
	listenerResult := make(chan net.Listener, 1)
	listenerErr := make(chan error, 1)
	err = activateMCPFrontRoute(ctx, port, mcpFrontRouteActivationOps{
		stage: func(context.Context, int) error {
			stageCalls++
			go func() {
				time.Sleep(100 * time.Millisecond)
				ln, listenErr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
				if listenErr != nil {
					listenerErr <- listenErr
					return
				}
				listenerResult <- ln
			}()
			return nil
		},
		waitListener: waitForMCPFrontRouteListener,
		preflight: func(context.Context, int) error {
			preflightCalls++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("activate route after delayed listener: %v", err)
	}
	if stageCalls != 1 {
		t.Fatalf("stage calls=%d, want exactly 1", stageCalls)
	}
	if preflightCalls != 1 {
		t.Fatalf("preflight calls=%d, want one strict proof after listener wait", preflightCalls)
	}
	select {
	case ln := <-listenerResult:
		if closeErr := ln.Close(); closeErr != nil {
			t.Fatalf("close delayed listener: %v", closeErr)
		}
	case listenErr := <-listenerErr:
		t.Fatalf("bind delayed listener: %v", listenErr)
	case <-time.After(time.Second):
		t.Fatal("delayed listener was not returned for cleanup")
	}
}

func TestWaitForMCPFrontRouteListener_StopsAtCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitForMCPFrontRouteListener(ctx, 9557)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error=%v, want context.Canceled", err)
	}
}

func TestStageMCPFrontRouteDaemon_PersistsExactPortBeforeSupervisorReconcile(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	const port = 9555
	reconcileCalls := 0

	err := stageMCPFrontRouteDaemonWithOps(context.Background(), stateDir, port, mcpFrontRouteStageOps{
		commandPath: func() string { return "mcphub-test" },
		reconcile: func(context.Context, bool) (api.ReconcileResponse, error) {
			reconcileCalls++
			intent, err := api.ReadSupervisorIntent(intentPath)
			if err != nil {
				t.Fatalf("read staged intent: %v", err)
			}
			want := api.BuildBuiltinRouteDaemon("mcphub-test", port)
			if len(intent.Daemons) != 1 || intent.Daemons[0].TaskName != want.TaskName || intent.Daemons[0].Port != port {
				t.Fatalf("reconcile observed intent=%+v, want exact route descriptor at %d", intent.Daemons, port)
			}
			return api.ReconcileResponse{}, nil
		},
	})
	if err != nil {
		t.Fatalf("stage route daemon: %v", err)
	}
	if reconcileCalls != 1 {
		t.Fatalf("reconcile calls=%d, want 1", reconcileCalls)
	}
}
