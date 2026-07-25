package cli

import (
	"context"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mcp-local-hub/internal/gui"
)

// ---------------------------------------------------------------------------
// P2-5 review fix: the GUI signal-observer goroutine was unjoined (a fast
// shutdown could exit the process before its EmitExitReasonEvent call ran)
// and had no first-trigger-wins guard against a signal racing a tray click.
// awaitGUIExitSignalReason (gui_exit_signal.go) is the extracted, directly
// testable core: fake channels + a fake context + a recording emit closure,
// no real OS signal, no real cobra RunE, no real HTTP server/tray — the
// HARD SAFETY CONSTRAINTS on this host forbid spawning a real mcphub gui
// process even for a test.
// ---------------------------------------------------------------------------

// TestAwaitGUIExitSignalReason_SIGINTAttributes proves the ordinary case: a
// SIGINT delivered on sigCh calls emit with GUIExitReasonSIGINT exactly
// once, and the function returns promptly (proving it is joinable — a
// caller doing wg.Wait() after this returns will not hang).
func TestAwaitGUIExitSignalReason_SIGINTAttributes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGINT

	var calls int32
	var gotReason gui.GUIExitReason
	done := make(chan struct{})
	go func() {
		defer close(done)
		awaitGUIExitSignalReason(ctx, sigCh, func(reason gui.GUIExitReason, extra map[string]any) {
			atomic.AddInt32(&calls, 1)
			gotReason = reason
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitGUIExitSignalReason did not return after a SIGINT delivery")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("emit called %d times on SIGINT; want exactly 1", got)
	}
	if gotReason != gui.GUIExitReasonSIGINT {
		t.Fatalf("emit reason = %q, want %q", gotReason, gui.GUIExitReasonSIGINT)
	}
}

// TestAwaitGUIExitSignalReason_SIGTERMAttributes mirrors the SIGINT test for
// SIGTERM, proving the reason mapping distinguishes the two signals.
func TestAwaitGUIExitSignalReason_SIGTERMAttributes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGTERM

	var gotReason gui.GUIExitReason
	done := make(chan struct{})
	go func() {
		defer close(done)
		awaitGUIExitSignalReason(ctx, sigCh, func(reason gui.GUIExitReason, extra map[string]any) {
			gotReason = reason
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitGUIExitSignalReason did not return after a SIGTERM delivery")
	}
	if gotReason != gui.GUIExitReasonSIGTERM {
		t.Fatalf("emit reason = %q, want %q", gotReason, gui.GUIExitReasonSIGTERM)
	}
}

// TestAwaitGUIExitSignalReason_CtxDoneWithoutSignalSkipsEmit reproduces the
// durability half of the P2-5 fix directly: when ctx is canceled for a
// reason OTHER than a delivered signal (a tray Quit, an internal error —
// anything that calls the RunE-level stop()), the function MUST return
// promptly WITHOUT calling emit (the trigger that actually caused the
// shutdown already owns — or will own, via EmitExitReasonEvent's own
// dedup — the attribution) and, critically, MUST NOT hang. A caller that
// joins this goroutine via sync.WaitGroup.Wait() depends on this: the
// pre-fix version had no ctx branch at all, so on a non-signal shutdown the
// goroutine would park on an empty channel forever and a join would hang
// the whole RunE return path.
//
// MUTATION: revert awaitGUIExitSignalReason to the pre-fix shape (bare
// `sig, ok := <-sigCh` with no select/ctx branch) — this test's 2-second
// wait for `done` times out (the goroutine never returns) and the test
// fails via t.Fatal.
func TestAwaitGUIExitSignalReason_CtxDoneWithoutSignalSkipsEmit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1) // never receives — no signal fires

	var calls int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		awaitGUIExitSignalReason(ctx, sigCh, func(reason gui.GUIExitReason, extra map[string]any) {
			atomic.AddInt32(&calls, 1)
		})
	}()

	// Give the goroutine a moment to reach its select before canceling —
	// not load-bearing for correctness (the select would catch a
	// already-canceled ctx just as well), just avoids a data race on
	// intent between "cancel first" and "goroutine starts first".
	time.Sleep(20 * time.Millisecond)
	cancel() // simulates RunE's stop() firing for a non-signal reason

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitGUIExitSignalReason did not return after ctx.Done() with no signal — a joining caller would hang forever")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("emit called %d times on a non-signal ctx cancellation; want 0 (no signal fired, nothing to attribute)", got)
	}
}

// TestAwaitGUIExitSignalReason_ClosedChannelSkipsEmit covers the other
// no-emit path: signal.Stop's effect eventually leaves sigCh with no more
// deliveries; if some caller closes the channel (defensive — production
// never closes an os.Signal channel it owns, but a `for !ok { return }`
// guard is cheap insurance), the function must return without emitting
// rather than treating the zero value as a spurious signal.
func TestAwaitGUIExitSignalReason_ClosedChannelSkipsEmit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal)
	close(sigCh)

	var calls int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		awaitGUIExitSignalReason(ctx, sigCh, func(reason gui.GUIExitReason, extra map[string]any) {
			atomic.AddInt32(&calls, 1)
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitGUIExitSignalReason did not return after its signal channel closed")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("emit called %d times on a closed signal channel; want 0", got)
	}
}
