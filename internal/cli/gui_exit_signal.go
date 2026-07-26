// Package cli — GUI exit-reason signal-observer helper (P2-5 review fix).
//
// Extracted out of gui.go's RunE closure so the signal-vs-shutdown race is
// independently unit-testable with fake channels and a fake context instead
// of a real OS signal delivered to a real cobra RunE running a real HTTP
// server + tray (which this repo's HARD SAFETY CONSTRAINTS forbid spawning
// in tests anyway).
package cli

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"mcp-local-hub/internal/gui"
)

// guiExitSignalSettleWindow bounds residual 3(a)'s causal-arbitration fix
// (see awaitGUIExitSignalReason's doc). Chosen to be enormous relative to
// realistic Go scheduler/os-signal-fanout latency (microseconds in
// practice) while staying small enough that it adds no perceptible delay to
// an ordinary non-signal shutdown (tray Quit, a startup error).
const guiExitSignalSettleWindow = 50 * time.Millisecond

// awaitGUIExitSignalReason blocks until sigCh delivers a SIGINT/SIGTERM, or
// ctx is done for any other reason, then attributes the exit via emit —
// but ONLY on the signal branch. It returns as soon as either branch
// resolves, so a caller that joins (e.g. via sync.WaitGroup.Wait) the
// goroutine running this function is guaranteed the signal-attribution
// attempt has already happened, or was correctly skipped, before proceeding.
//
// P2-5 REVIEW FIX (durability): the pre-fix version was a fire-and-forget
// goroutine that read ONLY sigCh with no ctx branch. A caller that tried to
// JOIN such a goroutine to fix "the observer is unjoined, so a fast
// shutdown can exit before its event is written" would then HANG on every
// non-signal shutdown (tray Quit, a startup error) — the goroutine would sit
// parked on an empty channel forever. The ctx.Done() branch closes that
// gap: RunE's own deferred stop() unconditionally cancels ctx before
// joining (see gui.go), so this function is guaranteed to return promptly
// on every RunE exit path, signal or not.
//
// P2-5 REVIEW FIX (first-trigger-wins): calling emit here even when a
// DIFFERENT trigger (tray Quit, a self-restart) already attributed the
// exit is harmless — gui.EmitExitReasonEvent itself is deduped process-wide
// (exactly one call across the whole process actually writes a row), so
// this function does not need its own separate dedup logic; it only needs
// to avoid calling emit on the ctx.Done()-without-signal path, which would
// misattribute a non-signal shutdown as a signal.
//
// Residual 3(a) REVIEW FIX (causal arbitration): RunE's ctx comes from
// signal.NotifyContext registered on the SAME SIGINT/SIGTERM sigCh receives
// (os/signal fans one incoming signal out to every registered channel —
// NotifyContext's internal channel AND this function's sigCh both receive
// it independently, with NO ordering guarantee between the two
// deliveries). A bare two-case select therefore lets Go's runtime
// pseudo-randomly pick ctx.Done() over a genuinely-arrived signal roughly
// as often as it picks the signal — silently dropping the TRUE causal
// reason for a real Ctrl-C/SIGTERM shutdown.
//
// Fix: if ctx.Done() wins the first select, retry against sigCh once more
// within a BOUNDED settle window (guiExitSignalSettleWindow) before
// concluding the shutdown was not signal-caused. This closes the
// scheduling-order gap between the two independent deliveries of the
// identical underlying signal — whichever channel becomes ready is
// observed, deterministically, within the bound. A genuinely non-signal
// shutdown (tray Quit, a startup error) pays this bounded, imperceptible
// delay once.
//
// An earlier draft of this fix also added a non-blocking priority peek at
// sigCh before the first select, reasoning it would let an already-arrived
// signal return faster than waiting on the settle window. That reasoning
// does not hold: when sigCh already holds a value, the settle-window's own
// select also observes it immediately (a select does not wait out its timer
// case when another case is already ready), so the peek produced no
// measurable behavioral difference in any constructible scenario — verified
// via mutation testing this session, not merely assumed. It was removed as
// unjustified complexity; the settle-window retry alone is both necessary
// and sufficient.
func awaitGUIExitSignalReason(ctx context.Context, sigCh <-chan os.Signal, emit func(gui.GUIExitReason, map[string]any)) {
	select {
	case sig, ok := <-sigCh:
		if ok {
			emitSignalReason(sig, emit)
		}
		return
	case <-ctx.Done():
	}

	// ctx won the race. Give a genuinely-simultaneous signal — whose
	// delivery to THIS channel may simply not have been scheduled yet —
	// one bounded chance to still be observed before concluding this
	// shutdown was not signal-caused.
	settle := time.NewTimer(guiExitSignalSettleWindow)
	defer settle.Stop()
	select {
	case sig, ok := <-sigCh:
		if ok {
			emitSignalReason(sig, emit)
		}
	case <-settle.C:
	}
}

func emitSignalReason(sig os.Signal, emit func(gui.GUIExitReason, map[string]any)) {
	reason := gui.GUIExitReasonSIGTERM
	if sig == syscall.SIGINT {
		reason = gui.GUIExitReasonSIGINT
	}
	emit(reason, nil)
}

// startGUIExitSignalObserver bundles the signal-observer goroutine and its
// join into ONE seam RunE calls (residual 3(b) review fix). Extracted for
// the SAME reason awaitGUIExitSignalReason itself was extracted: this
// wiring — not just that function's internal select logic — needs to be
// independently unit-testable without a real cobra RunE / HTTP server /
// tray, which this repo's HARD SAFETY CONSTRAINTS forbid spawning even in
// tests.
//
// Before this extraction, the identical Add(1)/Done()/Wait() wiring lived
// inline in RunE, untested: a reviewer removed it entirely and every
// existing test still passed, so the join had no regression guard despite
// being genuinely load-bearing in production (see the coverage note below).
// See TestStartGUIExitSignalObserver_StopJoinsObserverGoroutine
// (gui_exit_signal_test.go) for the mutation-sensitive proof this
// extraction makes possible.
//
// Returns a stop function the caller's deferred cleanup calls EXACTLY ONCE:
// it cancels ctx first and unconditionally (idempotent — safe even when the
// caller never touched ctx itself, e.g. an early startup error before any
// signal/shutdown; awaitGUIExitSignalReason could otherwise stay parked with
// neither branch ready and Wait() would hang), unregisters the signal
// channel, then BLOCKS until the observer goroutine has returned. That join
// is what guarantees the attribution attempt (via emit) has completed or was
// correctly skipped before the caller itself returns — see
// awaitGUIExitSignalReason's doc for why a fast shutdown without this join
// could let the process exit before its event is durably written.
//
// Coverage note: this join covers every NORMAL return path out of the
// caller (any return statement — Go guarantees deferred functions run on
// every one), including every error path. It does NOT cover the
// self-restart os.Exit path: gui_self_restart.go's RequestSelfRestartExit
// calls os.Exit, which by contract runs NO deferred functions in this or
// any other goroutine — so a stop() returned by this function is never
// called on that path, and this join provides it no guarantee whatsoever.
// That path instead emits synchronously before its own exit seam,
// independent of any join (see RequestSelfRestartExit's doc and
// TestRequestSelfRestartExit_EmitsSynchronouslyBeforeExit).
func startGUIExitSignalObserver(
	ctx context.Context,
	stopCtx context.CancelFunc,
	await func(context.Context, <-chan os.Signal, func(gui.GUIExitReason, map[string]any)),
	emit func(gui.GUIExitReason, map[string]any),
) (stop func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		await(ctx, sigCh, emit)
	}()
	return func() {
		stopCtx()
		signal.Stop(sigCh)
		wg.Wait()
	}
}
