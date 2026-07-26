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

// ---------------------------------------------------------------------------
// Residual 3(a) review fix: ctx.Done() and sigCh can become ready from the
// exact SAME underlying signal (NotifyContext's own registration and this
// function's sigCh both receive it independently — see
// awaitGUIExitSignalReason's doc), with no guaranteed ordering. A bare
// select{} would non-deterministically drop the signal attribution roughly
// half the time (empirically confirmed via a standalone probe this
// session: ~50/50 across 10000 trials of the same two-channel shape).
//
// The fix is the settle-window retry alone. An earlier draft also added a
// non-blocking priority peek before the first select, reasoning it would
// return faster for an already-arrived signal. That reasoning was tested
// and found NOT to hold: because a select does not wait out its timer case
// when another case is already ready, the settle-window's own retry
// resolves an already-buffered signal just as fast as a dedicated peek
// would — the peek added code with no measurable behavioral difference in
// any constructible scenario, so it was removed. Only the settle-window
// retry remains, and it is both necessary (below) and sufficient (also
// below) for every scenario these tests construct.
// ---------------------------------------------------------------------------

// TestAwaitGUIExitSignalReason_SimultaneousReadyCasesPreferSignal covers the
// "easy" ordering: both channels are ALREADY ready before
// awaitGUIExitSignalReason is ever called. The end-to-end attribution must
// be deterministic (always SIGINT), not depend on Go's runtime select
// tie-break.
//
// MUTATION: remove the settle-window retry (revert to the pre-fix bare
// two-case select) — this test's "always SIGINT" assertion fails on
// roughly half the 200 trials (Go's select tie-break sometimes picks
// ctx.Done() instead, and with no retry the signal is never observed).
func TestAwaitGUIExitSignalReason_SimultaneousReadyCasesPreferSignal(t *testing.T) {
	const trials = 200
	for i := 0; i < trials; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		sigCh := make(chan os.Signal, 1)
		sigCh <- syscall.SIGINT
		cancel() // both channels are ready before awaitGUIExitSignalReason ever runs

		var called bool
		var got gui.GUIExitReason
		awaitGUIExitSignalReason(ctx, sigCh, func(reason gui.GUIExitReason, extra map[string]any) {
			called = true
			got = reason
		})
		if !called || got != gui.GUIExitReasonSIGINT {
			t.Fatalf("trial %d: attribution = (called=%v, reason=%v), want SIGINT every time", i, called, got)
		}
	}
}

// TestAwaitGUIExitSignalReason_SignalArrivesJustAfterCtxDone covers the
// SUBTLER half of the race: ctx.Done() becomes ready BEFORE the identical
// signal's fan-out reaches this function's OWN sigCh registration (both stem
// from ONE OS signal delivered via two independent os/signal registrations
// with no ordering guarantee). Only the bounded settle window can catch
// this ordering — sigCh is genuinely empty at the moment the first select
// resolves.
//
// MUTATION: remove the settle-window retry (return immediately once
// ctx.Done() wins the first blocking select, without re-checking sigCh) —
// this test's "attributes SIGINT" assertion fails because the signal has not
// yet arrived at the moment the function would return.
func TestAwaitGUIExitSignalReason_SignalArrivesJustAfterCtxDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ctx is ALREADY done before the function is even called
	sigCh := make(chan os.Signal, 1)
	go func() {
		time.Sleep(5 * time.Millisecond) // simulate the fan-out lag between the two independent deliveries
		sigCh <- syscall.SIGINT
	}()

	var called bool
	var got gui.GUIExitReason
	done := make(chan struct{})
	go func() {
		defer close(done)
		awaitGUIExitSignalReason(ctx, sigCh, func(reason gui.GUIExitReason, extra map[string]any) {
			called = true
			got = reason
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitGUIExitSignalReason did not return within the settle window")
	}
	if !called || got != gui.GUIExitReasonSIGINT {
		t.Fatalf("attribution = (called=%v, reason=%v), want SIGINT (the settle window must catch a signal that lags ctx.Done())", called, got)
	}
}

// ---------------------------------------------------------------------------
// Residual 3(b) review fix: the RunE-level Add(1)/Done()/Wait() join used to
// live inline in gui.go's RunE closure with no regression guard — an
// independent reviewer removed the WaitGroup wiring entirely and every
// selected relevant test (including the four awaitGUIExitSignalReason tests
// above and two gui-command construction tests) stayed green. This test
// exercises startGUIExitSignalObserver directly (the seam that now owns
// that exact wiring in production), with no real cobra RunE / HTTP server /
// tray needed, so the join itself is finally mutation-guarded.
// ---------------------------------------------------------------------------

// TestStartGUIExitSignalObserver_StopJoinsObserverGoroutine proves the
// returned stop function's WaitGroup.Wait() genuinely blocks until the
// observer goroutine finishes — not merely that stop() returns "eventually"
// by coincidence. It injects a fake `await` function (in place of
// awaitGUIExitSignalReason) that sleeps for an observable duration before
// returning, so a stop() that fails to join would return near-instantly
// instead.
//
// MUTATION: remove wg.Add(1)/wg.Done()/wg.Wait() from
// startGUIExitSignalObserver (the exact class of removal the round-3 review
// caught with no test catching it) — this test's elapsed-time assertion
// fails (stop() returns before the fake await's sleep completes).
func TestStartGUIExitSignalObserver_StopJoinsObserverGoroutine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const observerDelay = 150 * time.Millisecond
	awaitStarted := make(chan struct{})
	fakeAwait := func(_ context.Context, _ <-chan os.Signal, _ func(gui.GUIExitReason, map[string]any)) {
		close(awaitStarted)
		time.Sleep(observerDelay)
	}

	stop := startGUIExitSignalObserver(ctx, cancel, fakeAwait, func(gui.GUIExitReason, map[string]any) {})

	select {
	case <-awaitStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("observer goroutine never started")
	}

	start := time.Now()
	stop()
	elapsed := time.Since(start)
	if elapsed < observerDelay {
		t.Fatalf("stop() returned after %s, want at least %s (the WaitGroup join must block until the observer goroutine finishes)", elapsed, observerDelay)
	}
}

// TestStartGUIExitSignalObserver_StopCancelsCtxBeforeJoining proves the
// documented ordering: stop() cancels ctx BEFORE waiting on the observer,
// so a real awaitGUIExitSignalReason parked on ctx.Done() (no signal ever
// arrived) is guaranteed to unblock rather than hang forever. Uses the
// REAL awaitGUIExitSignalReason (not a fake) precisely to prove this
// end-to-end, process-free.
//
// MUTATION: reorder stop()'s body to call wg.Wait() BEFORE stopCtx() — this
// test's 2-second bound is exceeded (Wait() blocks forever: ctx is never
// cancelled, no signal ever arrives, and the settle window only fires after
// ctx.Done(), which never happens).
func TestStartGUIExitSignalObserver_StopCancelsCtxBeforeJoining(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := startGUIExitSignalObserver(ctx, cancel, awaitGUIExitSignalReason, func(gui.GUIExitReason, map[string]any) {})

	done := make(chan struct{})
	go func() {
		defer close(done)
		stop()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return; ctx must be canceled BEFORE the join so a real observer with no signal still unblocks")
	}
}
