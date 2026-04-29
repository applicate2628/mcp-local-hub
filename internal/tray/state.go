package tray

import (
	"strings"

	"mcp-local-hub/internal/api"
)

// TrayState is the per-aggregate label the tray icon renders. Spec
// §6 names four variants: healthy / partial / down / error. The
// distinction matters because the user reads the icon at-a-glance
// without opening the dashboard, so coarsening the four into two
// (e.g. only ok/error) would lose actionable signal.
type TrayState int

const (
	// StateHealthy: every non-maintenance daemon is Running. Default
	// when the daemon set is empty (nothing to be wrong).
	StateHealthy TrayState = iota
	// StatePartial: at least one Running and at least one not-Running.
	// Operator-actionable: one daemon stopped while others are fine.
	StatePartial
	// StateDown: no daemons Running, no recent failures. Either all
	// scheduler tasks are idle (logon-only daemons before next logon)
	// or every daemon was stopped explicitly.
	StateDown
	// StateError: at least one daemon shows a launch failure or
	// LastResult != 0 (Task Scheduler's record of the most recent
	// non-zero exit). Highest-priority state — overrides Partial.
	StateError
)

// String renders the TrayState as the lower-case label used in
// tooltip strings, log messages, and the docs/manifest matrix.
func (s TrayState) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StatePartial:
		return "partial"
	case StateDown:
		return "down"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// Task Scheduler informational LastResult codes — NOT failures.
// 0x41300 (267008) "ready to run" through 0x4130F. Excluded from
// the failure predicate because every never-run / idle-state task
// on the host has one of these set, and treating them as failures
// would (a) paint the tray red the moment any daemon goes idle
// and (b) spam "daemon failed" toasts every poll. See companion
// comment + identical filter in cli/gui_tray_state.go::isFailedRow.
const (
	tsInfoCodeMin = 0x41300
	tsInfoCodeMax = 0x4130F
)

// isRealFailure returns true only for LastResult codes that represent
// an actual daemon failure. Excludes:
//
//   - 0 — clean success.
//   - -1 — internal/scheduler/scheduler.go:53 documented sentinel for
//     "task has never run" (parseTaskQueryOutput's default when
//     schtasks /Query output omits the "Last Result:" line). Without
//     this filter, freshly installed never-run tasks render as red
//     error tray icons (Codex PR #22 r2 P1).
//   - 0x41300-0x4130F — Windows Task Scheduler 2.0 informational
//     codes (e.g. 0x41303 "task has not yet run").
//
// Real user-program exit codes (typically 1-127) and HRESULT-shaped
// codes (high bit set, negative when interpreted as int32 but
// distinct from -1) both pass this check.
func isRealFailure(lastResult int32) bool {
	if lastResult == 0 || lastResult == -1 {
		return false
	}
	if lastResult >= tsInfoCodeMin && lastResult <= tsInfoCodeMax {
		return false
	}
	return true
}

// Aggregate maps a slice of daemon rows to one TrayState. Rules,
// in priority order:
//
//  1. Any row with a REAL-failure LastResult OR a state containing
//     "fail" is StateError. "Real failure" excludes Task Scheduler
//     informational codes (0x41300-0x4130F) which are not failures —
//     see isRealFailure docstring.
//  2. Maintenance rows (weekly-refresh) are skipped — they are
//     scheduled jobs, not steady-state daemons. Including them would
//     confuse the tray (a Scheduled weekly task would always look
//     like a Down daemon).
//  3. Among non-maintenance rows: count Running vs total. All Running
//     → StateHealthy. None Running → StateDown. Else → StatePartial.
//  4. Empty input → StateHealthy. The tray defaults to "everything is
//     fine" before the first Status snapshot arrives.
func Aggregate(rows []api.DaemonStatus) TrayState {
	running, total := 0, 0
	for _, r := range rows {
		// Real failure wins immediately; even a currently-Running
		// daemon that was launched after a failure should keep the
		// red badge until the operator clears the failure.
		if isRealFailure(r.LastResult) {
			return StateError
		}
		// Defensive: deriveState produces "Failed" historically; a
		// future label change ("FailedToLaunch", "LaunchError") would
		// still trip this contains-check.
		if strings.Contains(strings.ToLower(r.State), "fail") {
			return StateError
		}
		if r.IsMaintenance {
			continue
		}
		total++
		if r.State == "Running" {
			running++
		}
	}
	if total == 0 {
		// No non-maintenance daemons at all; nothing to be wrong.
		return StateHealthy
	}
	if running == total {
		return StateHealthy
	}
	if running == 0 {
		return StateDown
	}
	return StatePartial
}
