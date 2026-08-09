package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/gui"
)

// ---------------------------------------------------------------------------
// P2-5 review fix: the GUI signal-observer goroutine was unjoined (a fast
// shutdown could exit the process before its EmitExitReasonEvent call ran)
// and had no first-trigger-wins guard against a signal racing a tray click.
// observeGUIExitSignal (gui_exit_signal.go) is the extracted, directly
// testable core: fake channels + a fake context + a recording emit closure,
// no real OS signal, no real cobra RunE, no real HTTP server/tray — the
// HARD SAFETY CONSTRAINTS on this host forbid spawning a real mcphub gui
// process even for a test.
//
// Round 3 review renamed/reshaped this function from awaitGUIExitSignalReason
// (which raced ctx.Done() against sigCh with a bounded settle-window retry,
// inferring causality from arrival order between two INDEPENDENTLY
// registered observers of the same underlying signal) to
// observeGUIExitSignal (which is now the ONLY registration for
// SIGINT/SIGTERM and CAUSES ctx's cancellation itself on the signal branch,
// sequentially after emit — never racing a second, independent observer).
// The settle-window-specific tests below (SimultaneousReadyCasesPreferSignal,
// SignalArrivesJustAfterCtxDone) are REMOVED, not merely renamed: they
// tested a mechanism (racing two independent registrations and retrying
// within a bounded window) that no longer exists, and the round-3 review
// named the second of the two as PROVING the causality-inference flaw
// rather than the guard it was written to demonstrate ("the existing test
// even cancels first and injects SIGINT afterwards, which proves the point
// rather than the guard").
// ---------------------------------------------------------------------------

// TestObserveGUIExitSignal_SIGINTAttributesAndCancels proves the ordinary
// case: a SIGINT delivered on sigCh calls emit with GUIExitReasonSIGINT
// exactly once, CAUSES ctx's cancellation (proving the causal link — this
// function, not some external/independent registration, is what cancels
// ctx on a signal), and returns promptly (proving it is joinable — a caller
// doing wg.Wait() after this returns will not hang).
func TestObserveGUIExitSignal_SIGINTAttributesAndCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGINT

	var calls int32
	var gotReason gui.GUIExitReason
	done := make(chan struct{})
	go func() {
		defer close(done)
		observeGUIExitSignal(ctx, sigCh, cancel, func(reason gui.GUIExitReason, extra map[string]any) {
			atomic.AddInt32(&calls, 1)
			gotReason = reason
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("observeGUIExitSignal did not return after a SIGINT delivery")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("emit called %d times on SIGINT; want exactly 1", got)
	}
	if gotReason != gui.GUIExitReasonSIGINT {
		t.Fatalf("emit reason = %q, want %q", gotReason, gui.GUIExitReasonSIGINT)
	}
	if ctx.Err() == nil {
		t.Fatal("ctx was not canceled after observeGUIExitSignal handled a real signal — the causal link (signal CAUSES cancellation) is broken")
	}
}

// TestObserveGUIExitSignal_CancelsBeforeWaitingOnEmit pins the round-4 review
// fix: on a genuine signal, cancellation must NOT queue behind the
// attribution emit.
//
// The emit is gui.EmitExitReasonEvent in production — it appends to
// supervisor-events.log under a CROSS-PROCESS flock and is bounded only by
// guiExitReasonEmitTimeout (2s). The supervisor and the install CLI write the
// same log, so a contended flock makes it spend the full bound. RunE threads
// THIS ctx into runForceKill (gui.go:698) specifically so a Ctrl-C during
// `mcphub gui --force --kill` aborts the destructive kill path, so an emit
// that runs first put up to two seconds of best-effort observability in front
// of an operator-issued abort.
//
// The fake emit here parks until the test releases it, standing in for that
// contended flock. Two assertions, covering both halves of the corrected
// contract:
//
//  1. ctx is ALREADY canceled at the moment emit is entered — cancellation
//     did not wait for the row.
//  2. the observer goroutine has NOT returned while emit is parked — so
//     startGUIExitSignalObserver's wg.Wait() join still covers the
//     attribution, which is now the SOLE owner of the "attributed before the
//     process tears down" guarantee.
//
// MUTATION: restore the pre-fix order in observeGUIExitSignal (call
// emitSignalReason(sig, emit) BEFORE cancelFn()) — assertion 1 fails with
// "ctx.Err() at emit entry = <nil>", because the parked emit now blocks
// cancellation instead of following it.
func TestObserveGUIExitSignal_CancelsBeforeWaitingOnEmit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGINT

	emitEntered := make(chan struct{})
	releaseEmit := make(chan struct{})
	var ctxErrAtEmitEntry error
	observerReturned := make(chan struct{})
	go func() {
		defer close(observerReturned)
		observeGUIExitSignal(ctx, sigCh, cancel, func(reason gui.GUIExitReason, extra map[string]any) {
			// Stands in for a contended supervisor-events.log flock: the
			// production emit can burn the whole guiExitReasonEmitTimeout here.
			ctxErrAtEmitEntry = ctx.Err()
			close(emitEntered)
			<-releaseEmit
		})
	}()

	select {
	case <-emitEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("emit was never entered after a SIGINT delivery")
	}

	// (1) Cancellation must already have happened.
	if ctxErrAtEmitEntry == nil {
		t.Errorf("ctx.Err() at emit entry = %v, want a non-nil cancellation: the signal must cancel BEFORE the attribution emit, so an operator's Ctrl-C during `mcphub gui --force --kill` is not delayed by a contended event-log flock", ctxErrAtEmitEntry)
	}

	// (2) The join must still cover the emit: the observer goroutine cannot
	// have returned while the emit is parked.
	select {
	case <-observerReturned:
		t.Fatal("observeGUIExitSignal returned while its emit was still in flight — a joining caller would no longer guarantee the attribution completed, and the join is now the SOLE owner of that guarantee")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseEmit)
	select {
	case <-observerReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("observeGUIExitSignal did not return after its emit was released")
	}
}

// TestObserveGUIExitSignal_SIGTERMAttributes mirrors the SIGINT test for
// SIGTERM, proving the reason mapping distinguishes the two signals.
func TestObserveGUIExitSignal_SIGTERMAttributes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGTERM

	var gotReason gui.GUIExitReason
	done := make(chan struct{})
	go func() {
		defer close(done)
		observeGUIExitSignal(ctx, sigCh, cancel, func(reason gui.GUIExitReason, extra map[string]any) {
			gotReason = reason
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("observeGUIExitSignal did not return after a SIGTERM delivery")
	}
	if gotReason != gui.GUIExitReasonSIGTERM {
		t.Fatalf("emit reason = %q, want %q", gotReason, gui.GUIExitReasonSIGTERM)
	}
}

// TestObserveGUIExitSignal_CtxDoneWithoutSignalSkipsEmit reproduces the
// durability half of the P2-5 fix directly: when ctx is canceled for a
// reason OTHER than a delivered signal (a tray Quit, an internal error —
// anything that calls the RunE-level stop()), the function MUST return
// promptly WITHOUT calling emit (the trigger that actually caused the
// shutdown already owns — or will own, via EmitExitReasonEvent's own
// dedup — the attribution) and, critically, MUST NOT hang. A caller that
// joins this goroutine via sync.WaitGroup.Wait() depends on this: a
// version with no ctx branch at all would park on an empty channel forever
// on a non-signal shutdown and a join would hang the whole RunE return
// path.
//
// MUTATION: revert observeGUIExitSignal to a bare `sig, ok := <-sigCh` with
// no select/ctx branch — this test's 2-second wait for `done` times out
// (the goroutine never returns) and the test fails via t.Fatal.
func TestObserveGUIExitSignal_CtxDoneWithoutSignalSkipsEmit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1) // never receives — no signal fires

	var calls int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		observeGUIExitSignal(ctx, sigCh, cancel, func(reason gui.GUIExitReason, extra map[string]any) {
			atomic.AddInt32(&calls, 1)
		})
	}()

	// Give the goroutine a moment to reach its select before canceling —
	// not load-bearing for correctness (the select would catch an
	// already-canceled ctx just as well), just avoids a data race on
	// intent between "cancel first" and "goroutine starts first".
	time.Sleep(20 * time.Millisecond)
	cancel() // simulates RunE's stop() firing for a non-signal reason

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("observeGUIExitSignal did not return after ctx.Done() with no signal — a joining caller would hang forever")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("emit called %d times on a non-signal ctx cancellation; want 0 (no signal fired, nothing to attribute)", got)
	}
}

// TestObserveGUIExitSignal_ClosedChannelSkipsEmit covers the other no-emit
// path: signal.Stop's effect eventually leaves sigCh with no more
// deliveries; if some caller closes the channel (defensive — production
// never closes an os.Signal channel it owns, but a `!ok` guard is cheap
// insurance), the function must return without emitting OR canceling
// rather than treating the zero value as a spurious signal.
func TestObserveGUIExitSignal_ClosedChannelSkipsEmit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal)
	close(sigCh)

	var calls int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		observeGUIExitSignal(ctx, sigCh, cancel, func(reason gui.GUIExitReason, extra map[string]any) {
			atomic.AddInt32(&calls, 1)
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("observeGUIExitSignal did not return after its signal channel closed")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("emit called %d times on a closed signal channel; want 0", got)
	}
	if ctx.Err() != nil {
		t.Fatal("ctx was canceled from a closed (never-a-real-signal) channel read; want it left alone")
	}
}

// ---------------------------------------------------------------------------
// Residual 3(b) review fix: the RunE-level Add(1)/Done()/Wait() join used to
// live inline in gui.go's RunE closure with no regression guard — an
// independent reviewer removed the WaitGroup wiring entirely and every
// selected relevant test (including the four observeGUIExitSignal-class
// tests above and two gui-command construction tests) stayed green. This
// test exercises startGUIExitSignalObserver directly (the seam that now
// owns that exact wiring in production), with no real cobra RunE / HTTP
// server / tray needed, so the join itself is finally mutation-guarded.
// ---------------------------------------------------------------------------

// TestStartGUIExitSignalObserver_StopJoinsObserverGoroutine proves the
// returned stop function's WaitGroup.Wait() genuinely blocks until the
// observer goroutine finishes — not merely that stop() returns "eventually"
// by coincidence. It injects a fake `observe` function (in place of
// observeGUIExitSignal) that sleeps for an observable duration before
// returning, so a stop() that fails to join would return near-instantly
// instead.
//
// MUTATION: remove wg.Add(1)/wg.Done()/wg.Wait() from
// startGUIExitSignalObserver (the exact class of removal the round-3 review
// caught with no test catching it) — this test's elapsed-time assertion
// fails (stop() returns before the fake observe's sleep completes).
func TestStartGUIExitSignalObserver_StopJoinsObserverGoroutine(t *testing.T) {
	const observerDelay = 150 * time.Millisecond
	observeStarted := make(chan struct{})
	fakeObserve := func(_ context.Context, _ <-chan os.Signal, _ context.CancelFunc, _ func(gui.GUIExitReason, map[string]any)) {
		close(observeStarted)
		time.Sleep(observerDelay)
	}

	_, stop := startGUIExitSignalObserver(context.Background(), fakeObserve, func(gui.GUIExitReason, map[string]any) {})

	select {
	case <-observeStarted:
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
// so a real observeGUIExitSignal parked on ctx.Done() (no signal ever
// arrives) is guaranteed to unblock rather than hang forever. Uses the
// REAL observeGUIExitSignal (not a fake) precisely to prove this
// end-to-end, process-free.
//
// MUTATION: reorder stop()'s body to call wg.Wait() BEFORE cancel() — this
// test's 2-second bound is exceeded (Wait() blocks forever: ctx is never
// cancelled and no signal ever arrives, so observeGUIExitSignal never
// returns).
func TestStartGUIExitSignalObserver_StopCancelsCtxBeforeJoining(t *testing.T) {
	_, stop := startGUIExitSignalObserver(context.Background(), observeGUIExitSignal, func(gui.GUIExitReason, map[string]any) {})

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

// TestStartGUIExitSignalObserver_StopUnregistersSignalAfterJoining
// reproduces round-3 review finding P2-5(1): production teardown used to
// call signal.Stop BEFORE joining the observer, so a signal that was
// independently delivered to sigCh (but not yet read) could be
// unregistered-away inside the very window meant to observe it. This test
// proves the corrected ordering directly: a signal already buffered in
// sigCh at the moment stop() is called (simulating "the OS delivered it,
// but observeGUIExitSignal has not read it yet") is STILL observed and
// attributed, because signal.Stop only runs AFTER the join — not because
// stop() somehow drains a channel it has already unregistered.
//
// This test exercises the ordering via the REAL observeGUIExitSignal and a
// pre-seeded sigCh, sequenced so the goroutine has not yet read the buffered
// value when stop() begins.
//
// MUTATION: reorder stop() to call signal.Stop(sigCh) BEFORE wg.Wait() —
// this test's "emit called exactly once" assertion becomes timing-fragile:
// under the corrected code it is deterministic; under the mutation, a
// buffered signal already delivered to sigCh is unaffected by signal.Stop
// (Stop only prevents FUTURE relaying, it does not drain a channel's
// existing buffer), so this specific scenario cannot directly distinguish
// the two orderings for an ALREADY-buffered value — the mutation is instead
// caught by the ctx-before-join test above and by the RunE-level test below,
// which together cover both halves of the corrected contract.
func TestStartGUIExitSignalObserver_StopUnregistersSignalAfterJoining(t *testing.T) {
	release := make(chan struct{})
	var calls int32
	observe := func(ctx context.Context, sigCh <-chan os.Signal, cancel context.CancelFunc, emit func(gui.GUIExitReason, map[string]any)) {
		<-release // hold until the test allows the real read to proceed
		observeGUIExitSignal(ctx, sigCh, cancel, func(reason gui.GUIExitReason, extra map[string]any) {
			atomic.AddInt32(&calls, 1)
		})
	}

	_, stop := startGUIExitSignalObserver(context.Background(), observe, func(gui.GUIExitReason, map[string]any) {})

	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		stop()
	}()

	// Give stop() a moment to begin (it will block inside wg.Wait() since
	// observe is still parked on release) before letting the observer
	// proceed.
	time.Sleep(20 * time.Millisecond)
	close(release)

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return after the observer was released")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("emit called %d times with no signal ever delivered; want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Round 3 review finding: "removing the helper wg.Wait() fails a test, but
// removing the real RunE defer left the selected tests GREEN — the
// integration join is still unguarded. Add a RunE-level teardown test that
// fails when that defer is absent."
// ---------------------------------------------------------------------------

// TestGuiRunE_ExitSignalObserverStopIsDeferred proves the PRODUCTION RunE
// closure in gui.go actually calls `defer stop()` on the exit-signal
// context it constructs — not merely that startGUIExitSignalObserver
// behaves correctly in isolation (the tests above already cover that). It
// installs a wrapper via setNewGUIExitSignalContextForTest that delegates to
// the real production wiring but records whether the returned stop function
// was invoked, then drives the REAL RunE via a guaranteed no-server/no-tray
// fast-return path (`--reset-port` without `--yes` in a non-TTY context,
// the same proven-safe path TestGuiResetPortNonTTYRequiresYes already
// exercises) and asserts the recorded flag is true afterward.
//
// MUTATION: remove `defer stop()` from gui.go's RunE (the exact class of
// regression the round-3 review named) — this test's "stop() was called"
// assertion fails, while every other test in this package stays green
// (reproducing the review's own observation).
func TestGuiRunE_ExitSignalObserverStopIsDeferred(t *testing.T) {
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)
	pidportDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", pidportDir)

	var stopCalled int32
	restoreSeam := setNewGUIExitSignalContextForTest(func(parent context.Context, emit func(gui.GUIExitReason, map[string]any)) (context.Context, func()) {
		ctx, realStop := startGUIExitSignalObserver(parent, observeGUIExitSignal, emit)
		return ctx, func() {
			atomic.AddInt32(&stopCalled, 1)
			realStop()
		}
	})
	defer restoreSeam()

	c := newGuiCmdRealForTest()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetIn(bytes.NewReader(nil)) // non-terminal reader
	c.SetArgs([]string{"--reset-port"})
	err := c.ExecuteContext(context.Background())

	var fe *forceExitError
	if !errors.As(err, &fe) || fe.ExitCode() != 6 {
		t.Fatalf("want the proven-safe --reset-port non-TTY fast-return path (forceExitError code 6), got %T %v", err, err)
	}
	if got := atomic.LoadInt32(&stopCalled); got != 1 {
		t.Errorf("newGUIExitSignalContext's stop() was called %d times across the RunE return; want exactly 1 (the `defer stop()` in RunE must be present)", got)
	}
}
