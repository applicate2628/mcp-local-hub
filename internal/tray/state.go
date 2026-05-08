package tray

import (
	"strings"
	"time"

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

// Aggregate maps a slice of daemon rows to one TrayState. See
// AggregateWithIntent for the full rules; this back-compat wrapper
// passes an empty DaemonIntentFile so every row is classified as if
// no operator preference exists. Production callers should prefer
// AggregateWithIntent so the icon respects user-stop / user-disabled /
// uninstalled markers (bug #3 — node MCP servers exit 1 on graceful
// stdin-close, which used to flash a red error icon for a clean stop).
func Aggregate(rows []api.DaemonStatus) TrayState {
	return AggregateWithIntent(rows, api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{}}, time.Time{})
}

// AggregateWithIntent maps a slice of daemon rows to one TrayState
// while consulting the operator-recorded intent file. Rules, in
// priority order:
//
//  1. Per-row failure detection: a row trips the "looks like a
//     failure" predicate when api.IsRealFailure(LastResult) returns
//     true (canonical classifier shared with the watchdog —
//     see internal/api/recovery.go) OR the state string contains
//     "fail" (defensive; deriveState emits "Failed" historically and
//     future labels like "FailedToLaunch" should keep tripping).
//  2. Failure suppression by intent: when a row trips (1), look up
//     the per-task intent (intent.Tasks[row.TaskName]) and ask
//     IsActiveStop(now). If active AND the reason is one of
//     {user-stop within TTL, user-disabled, uninstalled,
//     clock-skew-future-suspect, install/register that decayed to
//     stopped}, the failure is suppressed — the row contributes
//     to the running/total accounting instead of forcing StateError.
//     ChronicFailure is NOT suppressed — that reason exists precisely
//     to surface watchdog-quarantined daemons; the operator must see
//     them.
//  3. Maintenance rows (weekly-refresh) are skipped — they are
//     scheduled jobs, not steady-state daemons.
//  4. Among non-maintenance rows: all Running → StateHealthy.
//     None Running → StateDown. Else → StatePartial.
//  5. Empty input → StateHealthy.
//
// Bug #3 motivation: Node-based MCP servers (e.g.
// @modelcontextprotocol/server-memory) exit with code 1 on graceful
// stdin-close. After `mcphub stop --server memory` Task Scheduler
// records LastResult=1 and pre-fix Aggregate() returned StateError
// — a red badge for a clean user-initiated stop. The intent file
// (PR #134 watchdog feature) already records the user's stop intent;
// AggregateWithIntent re-uses that signal here.
func AggregateWithIntent(rows []api.DaemonStatus, intent api.DaemonIntentFile, now time.Time) TrayState {
	running, total := 0, 0
	for _, r := range rows {
		looksFailed := api.IsRealFailure(r.LastResult) ||
			strings.Contains(strings.ToLower(r.State), "fail")
		if looksFailed && !intentSuppressesFailure(r, intent, now) {
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

// intentSuppressesFailure returns true when the recorded intent for
// this row's TaskName is an "operator wants this stopped" directive
// that should hide an otherwise-failing row from the StateError path.
//
// Logic:
//   - empty TaskName → cannot look up intent → no suppression. Defensive
//     guard for legacy callers that build DaemonStatus rows without
//     TaskName populated.
//   - missing intent entry → no suppression (default behavior).
//   - DaemonIntent.IsActiveStop(now) returns false → no suppression.
//   - active stop with reason=chronic-failure → DO NOT suppress; the
//     watchdog quarantine is precisely the case the operator must see.
//   - any other active-stop reason (user-stop within TTL, user-disabled,
//     uninstalled, clock-skew-future-suspect) → suppress.
//
// `now` of zero value is permitted (legacy Aggregate path); IsActiveStop
// then evaluates against the unix epoch and returns false for every
// realistic intent record (UpdatedAt is always in the future relative
// to year 0001), which is exactly the back-compat semantic Aggregate
// needs: empty intent = no suppression.
func intentSuppressesFailure(row api.DaemonStatus, intent api.DaemonIntentFile, now time.Time) bool {
	if row.TaskName == "" {
		return false
	}
	if intent.Tasks == nil {
		return false
	}
	entry, ok := intent.Tasks[row.TaskName]
	if !ok {
		return false
	}
	active, reason := entry.IsActiveStop(now)
	if !active {
		return false
	}
	// Chronic-failure is the watchdog's fail-closed marker. Hiding it
	// would defeat the entire purpose of the red badge — the operator
	// needs to know the watchdog gave up.
	if reason == api.IntentReasonChronicFailure {
		return false
	}
	return true
}
