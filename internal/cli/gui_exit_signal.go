// Package cli — GUI exit-reason signal-observer helper (P2-5 review fix).
//
// Extracted out of gui.go's RunE closure so the signal handling is
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

	"mcp-local-hub/internal/gui"
)

// observeGUIExitSignal blocks until sigCh delivers a SIGINT/SIGTERM, or ctx
// is done for any other reason. On a genuine signal it cancels via cancelFn
// AND attributes the exit via emit — IN THAT ORDER, from this SAME goroutine.
//
// ORDERING (round 4 review fix — cancel BEFORE emit). The two statements are
// wildly asymmetric in cost:
//
//   - cancelFn() is a context cancel: it closes a channel and walks the
//     child list. Bounded by memory access, effectively instantaneous.
//   - emit is gui.EmitExitReasonEvent, which opens supervisor-events.log and
//     appends a row under a CROSS-PROCESS flock, bounded by
//     guiExitReasonEmitTimeout (2s, internal/gui/gui_exit_reason.go:26). The
//     supervisor and the install CLI write that same log, so a contended or
//     wedged flock makes it spend the FULL bound.
//
// The previous order (emit, then cancel) therefore delayed cancellation by up
// to the whole emit bound. That is not academic: RunE threads THIS ctx into
// runForceKill (internal/cli/gui.go:698) precisely so a Ctrl-C during
// `mcphub gui --force --kill` aborts the destructive kill path — gui.Probe
// and the post-kill acquire poll both select on it. Making the operator's
// Ctrl-C wait on a contended event-log flock before the abort is even
// signalled inverts the priority between a best-effort observability row and
// an operator-issued abort of a destructive operation.
//
// WHAT STILL GUARANTEES ATTRIBUTION. The "a signal-caused exit is attributed
// before the process tears down" guarantee does NOT come from this
// function's internal statement order — it comes from the JOIN in
// startGUIExitSignalObserver below (cancel -> wg.Wait -> signal.Stop), which
// RunE arms with `defer stop()`. Go runs deferred functions on EVERY return
// path, so stop() — and therefore wg.Wait() — blocks the RunE return until
// this goroutine has returned, i.e. until emit has completed or been
// correctly skipped. Cancelling first does not weaken that: the emit runs in
// this goroutine either way, and the join waits for this goroutine either
// way. It only moves the emit so it OVERLAPS the teardown it describes
// instead of preceding the teardown's trigger. (`defer stop()` is registered
// at the very top of RunE, so it runs LAST among RunE's defers — the real
// shutdown work happens first and the emit overlaps it rather than
// serializing behind it.)
//
// WHAT IS GIVEN UP, AND WHY IT IS SAFE. A consumer of ctx.Done() can now
// observe cancellation before the row physically lands. Two ways that could
// matter, both checked:
//
//  1. A ctx.Done() consumer terminating the process without unwinding RunE's
//     defers would skip the join. There is no such consumer: the only
//     os.Exit-bound seam in these packages is selfRestartExitFn
//     (internal/gui/gui_self_restart.go:171), reached only from an explicit
//     RequestSelfRestartExit call, never from a ctx cancellation — and that
//     path emits synchronously on its own and is documented as outside this
//     join's coverage regardless (see startGUIExitSignalObserver's
//     "Coverage note").
//  2. A ctx.Done() consumer racing this emit for EmitExitReasonEvent's
//     process-wide first-trigger-wins sync.Once could misattribute the exit.
//     There is no such consumer either: the only other call sites are the
//     tray Quit / QuitAndStopAll handlers (internal/cli/gui.go:1427,1432),
//     which fire from an operator click, not from ctx.Done().
//
// A caller that joins (e.g. via sync.WaitGroup.Wait) the goroutine running
// this function still gets the completed-or-skipped attribution guarantee
// transitively.
//
// P2-5 REVIEW FIX (durability): the pre-fix version was a fire-and-forget
// goroutine that read ONLY sigCh with no ctx branch. A caller that tried to
// JOIN such a goroutine to fix "the observer is unjoined, so a fast
// shutdown can exit before its event is written" would then HANG on every
// non-signal shutdown (tray Quit, a startup error) — the goroutine would sit
// parked on an empty channel forever. The ctx.Done() branch closes that
// gap.
//
// P2-5 REVIEW FIX (first-trigger-wins): calling emit here even when a
// DIFFERENT trigger (tray Quit, a self-restart) already attributed the
// exit is harmless — gui.EmitExitReasonEvent itself is deduped process-wide
// (exactly one call across the whole process actually writes a row), so
// this function does not need its own separate dedup logic; it only needs
// to avoid calling emit on the ctx.Done()-without-signal path, which would
// misattribute a non-signal shutdown as a signal.
//
// Residual 3 REVIEW FIX, round 3 (causal arbitration, replacing the round-2
// settle-window design): the round-2 shape raced TWO INDEPENDENTLY
// registered observers of the identical underlying OS signal —
// signal.NotifyContext's own internal channel (driving ctx.Done()) and this
// package's separate sigCh — with no ordering guarantee between the two
// deliveries, and inferred "was this shutdown signal-caused" from WHICHEVER
// channel happened to become ready first (with a bounded 50ms retry to
// paper over the common case). That is unsound: causality cannot be
// inferred from arrival order between two independent registrations of one
// event, and a stale/lagging signal could be discarded by production
// teardown before the retry window even got to observe it. This function is
// now called from the ONLY registration that exists for SIGINT/SIGTERM
// (sigCh, owned by startGUIExitSignalObserver below) — ctx is a plain
// derived context whose ONLY cancellation causes are (a) THIS function's
// own signal branch, or (b) an external, explicit
// stop()/cancel() call from some OTHER, already-well-defined shutdown
// trigger (RunE's own return, tray Quit — which records its own reason
// before ever calling stop()). There is no longer a race between two
// independent deliveries of the SAME event to reconcile after the fact:
// "ctx is done because of a signal" is CAUSED by this function, in order,
// never inferred from a timing race.
func observeGUIExitSignal(ctx context.Context, sigCh <-chan os.Signal, cancelFn context.CancelFunc, emit func(gui.GUIExitReason, map[string]any)) {
	select {
	case sig, ok := <-sigCh:
		if !ok {
			// Defensive: production never closes an os.Signal channel it
			// owns, but a closed channel reads as a zero value with
			// ok=false immediately (never blocks) — treat it exactly like
			// "no signal observed", not a fabricated signal-caused
			// cancellation.
			return
		}
		// Cancel FIRST: an operator-issued abort must not queue behind a
		// best-effort observability row that can spend the full
		// guiExitReasonEmitTimeout on a contended event-log flock. The
		// attribution is still performed by THIS goroutine immediately
		// afterwards, and startGUIExitSignalObserver's join is what
		// guarantees it completes before the caller proceeds. See this
		// function's ORDERING doc above.
		cancelFn()
		emitSignalReason(sig, emit)
	case <-ctx.Done():
		// Canceled some other way (an explicit stop()/cancel() call on a
		// non-signal shutdown path, or the parent context's own
		// cancellation) — no signal was observed on this, the only
		// registration, so there is nothing to attribute.
	}
}

func emitSignalReason(sig os.Signal, emit func(gui.GUIExitReason, map[string]any)) {
	reason := gui.GUIExitReasonSIGTERM
	if sig == syscall.SIGINT {
		reason = gui.GUIExitReasonSIGINT
	}
	emit(reason, nil)
}

// startGUIExitSignalObserver constructs the SIGINT/SIGTERM-aware context
// RunE uses for the rest of its lifetime and starts the SINGLE goroutine
// that is the ONLY place SIGINT/SIGTERM is observed for this process
// (residual 3(b) review fix; round 3 consolidation folds the causal-
// arbitration fix into this same seam). Before round 3, RunE wired up TWO
// independent signal registrations for the exact same signals —
// signal.NotifyContext's own internal one (driving ctx.Done()) plus this
// file's separate sigCh — and inferred causality by racing them. This
// function removes that race structurally: there is exactly one
// registration, and observe (production: observeGUIExitSignal) is the one
// and only reader of it, in the SAME goroutine that cancels the returned
// ctx because of it.
//
// observe is an injectable parameter (mirroring the pattern this file
// already used) so the wiring itself — not just observeGUIExitSignal's
// internal decision logic — stays independently unit-testable without a
// real cobra RunE / HTTP server / tray, which this repo's HARD SAFETY
// CONSTRAINTS forbid spawning even in tests. See
// TestStartGUIExitSignalObserver_StopJoinsObserverGoroutine
// (gui_exit_signal_test.go) for the mutation-sensitive proof this
// extraction makes possible.
//
// Before this extraction (residual 3(b)'s original review round), the
// identical Add(1)/Done()/Wait() wiring lived inline in RunE, untested: a
// reviewer removed it entirely and every existing test still passed, so the
// join had no regression guard despite being genuinely load-bearing in
// production.
//
// Returns the derived ctx (cancels on SIGINT/SIGTERM exactly as
// signal.NotifyContext's did, so every other ctx.Done()/
// context.WithoutCancel(ctx) call site and every stop()-calling closure —
// tray Quit, QuitAndStopAll — in this package needs no further changes) and
// a stop function the caller's deferred cleanup calls EXACTLY ONCE:
//
//  1. cancel() first and unconditionally (idempotent — safe even when the
//     caller never touched ctx itself, e.g. an early startup error before
//     any signal/shutdown; observeGUIExitSignal could otherwise stay parked
//     with neither branch ready and Wait() would hang).
//  2. wg.Wait() SECOND — blocks until the observer goroutine has returned,
//     which is what guarantees the attribution attempt (via emit) has
//     completed or was correctly skipped before the caller itself
//     continues. Since the round-4 reordering inside observeGUIExitSignal
//     (cancel before emit, so an operator's Ctrl-C is not delayed by a
//     contended event-log flock), this join is the SOLE owner of that
//     guarantee — it is no longer redundant with an intra-goroutine
//     statement order. Removing it does not merely weaken a belt-and-braces
//     property; it drops the durability guarantee outright.
//  3. signal.Stop(sigCh) LAST, only once the observer's own decision is
//     already settled. Round 3 review finding: the round-2 shape called
//     signal.Stop BEFORE joining, so a signal that was independently
//     delivered to sigCh could be unregistered-away inside the exact
//     window meant to observe it. Unregistering only after the join means
//     the channel stays live for as long as observeGUIExitSignal could
//     possibly still be reading from it.
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
	parent context.Context,
	observe func(context.Context, <-chan os.Signal, context.CancelFunc, func(gui.GUIExitReason, map[string]any)),
	emit func(gui.GUIExitReason, map[string]any),
) (ctx context.Context, stop func()) {
	ctx, cancel := context.WithCancel(parent)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		observe(ctx, sigCh, cancel, emit)
	}()
	return ctx, func() {
		cancel()
		wg.Wait()
		signal.Stop(sigCh)
	}
}

// newGUIExitSignalContext is RunE's actual call site (round 3 review fix,
// residual 3(b) follow-up): wires the real observeGUIExitSignal into
// startGUIExitSignalObserver so RunE's own call site stays a single line,
// matching signal.NotifyContext's old two-value return shape exactly
// (ctx, stop) so every other ctx.Done()/context.WithoutCancel(ctx) call
// site and every stop()-calling closure in this package needs no further
// changes.
//
// This is an injectable package-level seam (mirroring guiServingProbeFn /
// guiOwnerLockUnheldProbeFn in supervise_ensure_alive.go) so an RunE-level
// integration test can prove the PRODUCTION `defer stop()` wiring at the
// actual RunE call site is present — not merely that the extracted helper
// behaves correctly in isolation. Round 3 review finding: "removing the
// helper wg.Wait() fails a test, but removing the real RunE defer left the
// selected tests GREEN — the integration join is still unguarded." See
// TestGuiRunE_ExitSignalObserverStopIsDeferred (gui_exit_signal_test.go).
// Production callers MUST NOT reassign this var directly —
// setNewGUIExitSignalContextForTest is the only allowed write path.
var newGUIExitSignalContext = func(parent context.Context, emit func(gui.GUIExitReason, map[string]any)) (context.Context, func()) {
	return startGUIExitSignalObserver(parent, observeGUIExitSignal, emit)
}

// setNewGUIExitSignalContextForTest installs a test wrapper for RunE's
// exit-signal-context seam. Returns an "uninstall" function tests defer to
// restore production wiring. Only gui_exit_signal_test.go invokes this.
func setNewGUIExitSignalContextForTest(fn func(context.Context, func(gui.GUIExitReason, map[string]any)) (context.Context, func())) func() {
	prev := newGUIExitSignalContext
	newGUIExitSignalContext = fn
	return func() { newGUIExitSignalContext = prev }
}
