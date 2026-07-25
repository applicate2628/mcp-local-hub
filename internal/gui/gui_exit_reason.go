// internal/gui/gui_exit_reason.go
//
// GUI exit-reason attribution. `mcphub gui`'s RunE converges every shutdown
// trigger — an OS signal (Ctrl-C / service stop), a tray Quit / Quit-and-
// stop-all click, or a restart-v3 self-restart's os.Exit(0) — on the SAME
// `stop()` context-cancel -> normal RunE return -> deferred supervisor-
// manager shutdown (or, for self-restart, deliberately bypasses that defer
// via os.Exit so the fleet survives the handoff). Without per-site
// instrumentation, an operator reading supervisor-events.log cannot tell a
// deliberate shutdown from an unexpected one, or a self-restart from a
// crash. EmitExitReasonEvent is the single owner of that one diagnostic row.
package gui

import (
	"path/filepath"
	"time"

	"mcp-local-hub/internal/api"
)

// guiExitReasonEmitTimeout bounds EmitExitReasonEvent so a caller on an
// os.Exit-bound path (no defers run — see RequestSelfRestartExit in
// gui_self_restart.go) can call it SYNCHRONOUSLY immediately before exiting
// without risking an unbounded hang on a wedged supervisor-events.log flock.
const guiExitReasonEmitTimeout = 2 * time.Second

// GUIExitReason is the machine-filterable discriminator recorded on the
// single gui-exit-reason event every call site below converges on. Kept as
// a named type (not a bare string) so every call site is forced through one
// of the constants here rather than inventing a fresh literal.
type GUIExitReason string

const (
	GUIExitReasonSIGINT             GUIExitReason = "sigint"
	GUIExitReasonSIGTERM            GUIExitReason = "sigterm"
	GUIExitReasonTrayQuit           GUIExitReason = "tray-quit"
	GUIExitReasonTrayQuitAndStopAll GUIExitReason = "tray-quit-and-stop-all"
	GUIExitReasonSelfRestart        GUIExitReason = "self-restart-v3"
)

// EmitExitReasonEvent records, as a bounded best-effort row in
// supervisor-events.log, WHICH trigger began a `mcphub gui` process's exit.
//
// Best-effort: an unresolvable state dir or a wedged event-log flock is
// silently swallowed (mirrors emitLivenessEvent's polarity in
// internal/cli/supervise_ensure_alive.go) — this is an observability aid,
// never a gate, and MUST NOT delay or fail the exit it is describing beyond
// the bounded guiExitReasonEmitTimeout.
func EmitExitReasonEvent(reason GUIExitReason, extra map[string]any) {
	stateDir, err := api.DaemonStateDirReadOnly()
	if err != nil {
		return
	}
	logger, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		return
	}
	defer func() { _ = logger.Close() }()
	body := extra
	if body == nil {
		body = map[string]any{}
	}
	body["reason"] = string(reason)
	_ = logger.EmitWithTimeout(api.SupervisorEvent{
		Severity: api.SupervisorEventSeverityInfo,
		Source:   api.SupervisorEventSourceLifecycle,
		Event:    "gui-exit-reason",
		Body:     body,
	}, guiExitReasonEmitTimeout)
}
