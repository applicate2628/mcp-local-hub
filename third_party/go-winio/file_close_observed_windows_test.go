//go:build windows

package winio

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

type observedCloseConn interface {
	CloseObserved(context.Context) (*CloseReceipt, error)
}

var (
	_ observedCloseConn = (*win32Pipe)(nil)
	_ observedCloseConn = (*win32MessageBytePipe)(nil)
)

func TestCloseObservedRetainsLegacyWinner(t *testing.T) {
	handle, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &win32File{handle: handle}
	if err := f.Close(); err != nil {
		t.Fatalf("legacy Close: %v", err)
	}
	receipt, err := f.CloseObserved(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if nativeErr, observed := receipt.NativeResult(); !observed || nativeErr != nil {
		t.Fatalf("native result = (%v, %t), want (nil, true)", nativeErr, observed)
	}
	again, err := f.CloseObserved(context.Background())
	if err != nil || again != receipt {
		t.Fatalf("second observation = (%p, %v), want (%p, nil)", again, err, receipt)
	}
}

func TestCloseObservedNativeFailureFromObservedWinner(t *testing.T) {
	var empty CloseReceipt
	if nativeErr, observed := empty.NativeResult(); observed || nativeErr != nil {
		t.Fatalf("zero receipt returned a native result: (%v, %t)", nativeErr, observed)
	}
	// A zero handle is invalid and does not name a resource owned by the test.
	f := &win32File{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	receipt, err := f.CloseObserved(ctx)
	if err != nil {
		t.Fatalf("the native winner must finish its work: %v", err)
	}
	if nativeErr, observed := receipt.NativeResult(); !observed || !errors.Is(nativeErr, windows.ERROR_INVALID_HANDLE) {
		t.Fatalf("native result = (%v, %t), want (ERROR_INVALID_HANDLE, true)", nativeErr, observed)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("legacy Close after failed native close: %v", err)
	}
	again, err := f.CloseObserved(context.Background())
	if err != nil || again != receipt {
		t.Fatalf("second observation = (%p, %v), want (%p, nil)", again, err, receipt)
	}
}

type observedCloseTestContext struct {
	context.Context
	waiting chan struct{}
}

func (c observedCloseTestContext) Done() <-chan struct{} {
	close(c.waiting)
	return c.Context.Done()
}

func TestCloseObservedCancelsOnlyWaitingCaller(t *testing.T) {
	const failure = windows.ERROR_ACCESS_DENIED
	f := &win32File{}
	nativeStarted := make(chan struct{})
	allowNativeReturn := make(chan struct{})
	nativeReturned := make(chan struct{})
	var calls atomic.Int32
	var won *CloseReceipt
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowNativeReturn) }) }

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		defer close(nativeReturned)
		won = f.closeHandle(func(windows.Handle) error {
			calls.Add(1)
			close(nativeStarted)
			<-allowNativeReturn
			return failure
		})
	}()
	t.Cleanup(func() {
		release()
		waitForTestClose(t, nativeReturned, "native owner")
	})
	waitForTestClose(t, nativeStarted, "native attempt")

	legacyReturned := make(chan struct{})
	go func() {
		defer close(legacyReturned)
		if err := f.Close(); err != nil {
			t.Errorf("losing legacy Close: %v", err)
		}
	}()
	t.Cleanup(func() {
		release()
		waitForTestClose(t, legacyReturned, "legacy caller")
	})
	waitForTestClose(t, legacyReturned, "legacy caller while native close is pending")

	waiting := make(chan struct{})
	observerReturned := make(chan struct{})
	var receipt *CloseReceipt
	var waitErr error
	go func() {
		defer close(observerReturned)
		receipt, waitErr = f.CloseObserved(observedCloseTestContext{Context: ctx, waiting: waiting})
	}()
	t.Cleanup(func() {
		cancel()
		release()
		waitForTestClose(t, observerReturned, "observer")
	})
	waitForTestClose(t, waiting, "observer wait")
	cancel()
	waitForTestClose(t, observerReturned, "canceled observer")
	if receipt == nil || receipt != f.closeReceipt || !errors.Is(waitErr, context.Canceled) {
		t.Fatalf("canceled observation = (%p, %v), want (%p, context.Canceled)", receipt, waitErr, f.closeReceipt)
	}
	if nativeErr, observed := receipt.NativeResult(); observed || nativeErr != nil {
		t.Fatalf("pending native close reported a result: (%v, %t)", nativeErr, observed)
	}
	release()
	waitForTestClose(t, nativeReturned, "native owner")
	settled, err := f.CloseObserved(context.Background())
	if err != nil || settled != won || settled != receipt {
		t.Fatalf("settled observation = (%p, %v), want owner %p and canceled receipt %p", settled, err, won, receipt)
	}
	if nativeErr, observed := receipt.NativeResult(); !observed || nativeErr != failure {
		t.Fatalf("native result = (%v, %t), want (%v, true)", nativeErr, observed, failure)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("native calls = %d, want 1", got)
	}
}

func waitForTestClose(t *testing.T, done <-chan struct{}, stage string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", stage)
	}
}
