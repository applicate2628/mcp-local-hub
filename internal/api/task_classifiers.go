// Package api — shared Task Scheduler task-name + LastResult classifiers.
//
// These pure helpers were migrated here from recovery.go when the v0.6
// redesign (spec §5 Phase D) deleted the watchdog recovery engine. They
// are NOT watchdog-specific — each has live cross-package consumers that
// outlive the watchdog:
//
//   - IsRealFailure: the canonical Windows Task Scheduler LastResult
//     classifier, consumed by the tray icon aggregator
//     (internal/tray/state.go) and the GUI tray-state mirror
//     (internal/cli/gui_tray_state.go).
//   - isMaintenanceTaskName / IsMaintenanceTaskName: the hub-wide
//     maintenance-task classifier (`-watchdog` / `-weekly-refresh`
//     suffix + the exact LivenessTaskName), consumed by the
//     supervisor restart path
//     (restart_supervisor.go), the supervisor status `is_maintenance`
//     flag (internal/cli/supervise_status.go), the partial-uninstall gate
//     (internal/cli/setup.go's shouldRemoveGlobalWatchdog), and the GUI
//     env-override classifiers (internal/gui/daemon_env.go). The
//     `-watchdog` suffix is retained so a legacy / hand-edited row left in
//     supervisor-intent.json is still skipped by the supervisor reconcile.
//   - ServerFromTaskName: the server-segment parser used by the
//     partial-uninstall gate (internal/cli/setup.go).
package api

import "strings"

// tsInfoCodeMin / tsInfoCodeMax bound the Task Scheduler 2.0
// informational LastResult range. Codes in [0x41300, 0x4130F] are NOT
// failures: examples include 0x41300 (ready to run), 0x41301 (currently
// running), 0x41303 (task has not yet run). The tray must suppress these
// so a freshly-installed never-run task does not paint a red badge or
// fire a "daemon failed" toast.
const (
	tsInfoCodeMin = 0x41300
	tsInfoCodeMax = 0x4130F
)

// userExitCodeMin / userExitCodeMax bound the conventional user-program
// exit code range. Per the original watchdog plan §18: 1..0xFFFF treated
// as a real failure (typical exit codes 1, 2, 87, 1063 + 16-bit ceiling).
// Positive values > 0xFFFF and outside the TS info range are treated
// conservatively as non-failures so an unfamiliar code does not blindly
// trigger a "daemon failed" signal.
const (
	userExitCodeMin int32 = 1
	userExitCodeMax int32 = 0xFFFF
)

// IsRealFailure reports whether a Windows Task Scheduler LastResult
// value should be treated as a real failure. Single canonical
// definition consumed by the tray icon aggregator and the CLI
// tray-state mirror.
//
// LastResult semantics (Windows Task Scheduler 2.0):
//
//   - 0x0: clean success → not a failure.
//   - -1: documented sentinel for "task has never run"; emitted by
//     internal/scheduler/scheduler.go when schtasks /Query output omits
//     the "Last Result:" line. NOT a failure.
//   - [0x41300, 0x4130F]: TS informational codes (ready / running /
//     never-run / disabled). NOT failures.
//   - bit 31 set (read as int32: negative): HRESULT / NTSTATUS — real
//     failure. Includes E_FAIL (0x80004005 / -2147467259) and similar.
//   - [1, 0xFFFF]: typical user-program exit codes — real failure.
//   - Other (positive past 0xFFFF and not in TS info range): conservative
//     not-a-failure. Refusing to flag on an unfamiliar code avoids a
//     red-badge flap on an unfamiliar exit value.
//
// Pure: no I/O, no global state, deterministic on the input alone.
func IsRealFailure(lastResult int32) bool {
	if lastResult == 0 || lastResult == -1 {
		return false
	}
	if lastResult >= tsInfoCodeMin && lastResult <= tsInfoCodeMax {
		return false
	}
	if lastResult < 0 {
		return true
	}
	if lastResult >= userExitCodeMin && lastResult <= userExitCodeMax {
		return true
	}
	return false
}

// isMaintenanceTaskName recognizes hub-wide scheduler tasks owned by
// mcp-local-hub itself: the legacy watchdog (`...-watchdog`), the
// supervisor-liveness recovery task (`...-liveness`), and the
// weekly-refresh maintenance jobs (`...-weekly-refresh`). Callers use it
// to keep these operationally-stable scheduled jobs out of per-server /
// per-daemon recovery and gating logic.
//
// The watchdog and weekly-refresh matches are suffix-based; the liveness
// match is EXACT. Naming variants covered:
//   - hub-wide legacy watchdog: "\\mcp-local-hub-watchdog" (suffix)
//   - hub-wide supervisor-liveness task: "\\mcp-local-hub-liveness" (exact)
//   - per-server / hub-wide weekly refresh (suffix):
//     "\\mcp-local-hub-<server>-weekly-refresh",
//     "\\mcp-local-hub-weekly-refresh".
//
// The `-watchdog` suffix is retained after the v0.6 watchdog deletion so
// a legacy or hand-edited `\mcp-local-hub-watchdog` row still classifies
// as maintenance: it must not be respawned as a daemon by the supervisor
// reconcile, and it must not poison the last-server partial-uninstall
// gate (ServerFromTaskName("...-watchdog") would otherwise return a
// non-empty pseudo-server). The `-weekly-refresh` suffix is deliberately
// broad because a weekly-refresh job is BOTH hub-wide
// ("\\mcp-local-hub-weekly-refresh") AND legitimately per-server
// ("\\mcp-local-hub-<server>-weekly-refresh").
//
// The liveness task, by contrast, has exactly ONE canonical name —
// LivenessTaskName ("\\mcp-local-hub-liveness"), the single hub-wide
// supervisor-liveness recovery task (Phase 3a, v0.6 spec §15 P1-b). The
// manifest schema does NOT reserve `liveness` as a daemon name, so a
// legitimate server daemon literally named `liveness` produces the task
// "\\mcp-local-hub-<server>-liveness", which also ends in `-liveness`. A
// suffix match (the pre-r38 form `HasSuffix(name, "-liveness")`) wrongly
// classified that real `<server>/liveness` daemon as hub maintenance,
// making callers (selectSupervisorOwnedTargets, isHubDaemonSchedulerTaskName,
// the GUI env-override gate, supervisorIntentManagedServerSignals) skip or
// reject it. The exact-name check below matches BOTH the canonical
// leading-backslash form and the bare form because callers arrive in both
// shapes: isHubInfrastructureTaskName strips the leading backslash before
// calling, while the supervisor restart path / GUI / status callers pass
// the canonical form verbatim.
func isMaintenanceTaskName(name string) bool {
	return strings.HasSuffix(name, "-watchdog") ||
		name == LivenessTaskName ||
		name == strings.TrimPrefix(LivenessTaskName, "\\") ||
		strings.HasSuffix(name, "-weekly-refresh")
}

// IsMaintenanceTaskName is the exported alias used by cross-package
// callers (e.g. internal/cli/setup.go's "is this the last managed
// server?" gate). Same suffix-match contract as the unexported helper.
func IsMaintenanceTaskName(name string) bool {
	return isMaintenanceTaskName(name)
}

// ServerFromTaskName parses a Task Scheduler name like
// "\\mcp-local-hub-<server>-<daemon>" and returns the server segment.
// Returns "" for unparseable or hub-wide tasks (watchdog/liveness,
// hub-wide weekly-refresh). Mirrors parseTaskName's first return value
// from status_enrich.go but is exported for cross-package consumers
// that need only the server identity (e.g. partial-uninstall gating).
func ServerFromTaskName(taskName string) string {
	srv, _ := parseTaskName(taskName)
	return srv
}
