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
	"syscall"

	"mcp-local-hub/internal/gui"
)

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
func awaitGUIExitSignalReason(ctx context.Context, sigCh <-chan os.Signal, emit func(gui.GUIExitReason, map[string]any)) {
	select {
	case sig, ok := <-sigCh:
		if !ok {
			return
		}
		reason := gui.GUIExitReasonSIGTERM
		if sig == syscall.SIGINT {
			reason = gui.GUIExitReasonSIGINT
		}
		emit(reason, nil)
	case <-ctx.Done():
	}
}
