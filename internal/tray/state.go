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
//     stopped}, the failure is suppressed — the row is excluded
//     from BOTH the error path AND the running/total accounting
//     ("intentionally not running" must be invisible to the
//     healthy-ratio calculation, not just the failure path).
//     ChronicFailure is NOT suppressed — that reason exists precisely
//     to surface watchdog-quarantined daemons; the operator must see
//     them.
//  3. Maintenance rows (weekly-refresh) are skipped — they are
//     scheduled jobs, not steady-state daemons.
//  4. Among non-maintenance rows: all Running → StateHealthy.
//     None Running → StateDown. Else → StatePartial.
//  5. Empty input → StateHealthy.
//
// `now` zero-value handling: when the caller passes time.Time{} AND
// the intent map is non-empty, `now` is promoted to time.Now() so
// IsActiveStop evaluates against real wall-clock instead of year 0001
// (which would otherwise hit IsActiveStop's future-skew fail-closed
// branch and silently re-classify every active-stop reason as
// "clock-skew-future-suspect"). The legacy Aggregate(rows) path
// passes an empty intent map and is unaffected: empty intent → no
// row matches the lookup → no suppression, exactly the legacy
// semantic. Codex deep-sec finding round 1 (LOW).
//
// Bug #3 motivation: Node-based MCP servers (e.g.
// @modelcontextprotocol/server-memory) exit with code 1 on graceful
// stdin-close. After `mcphub stop --server memory` Task Scheduler
// records LastResult=1 and pre-fix Aggregate() returned StateError
// — a red badge for a clean user-initiated stop. The intent file
// (PR #134 watchdog feature) already records the user's stop intent;
// AggregateWithIntent re-uses that signal here.
func AggregateWithIntent(rows []api.DaemonStatus, intent api.DaemonIntentFile, now time.Time) TrayState {
	// Defend against the back-compat shim and any caller that passes
	// time.Time{} with a non-empty intent. IsActiveStop's future-skew
	// fail-closed branch would otherwise mis-attribute every reason as
	// "clock-skew-future-suspect", silently dropping the chronic-failure
	// carve-out. Legacy Aggregate(rows) path keeps its semantics because
	// the intent map is always empty there → lookup misses → no suppression.
	if now.IsZero() && len(intent.Tasks) > 0 {
		now = time.Now()
	}
	running, total := 0, 0
	suppressedCount := 0
	for _, r := range rows {
		looksFailed := api.IsRealFailure(r.LastResult) ||
			strings.Contains(strings.ToLower(r.State), "fail")
		suppressed := looksFailed && intentSuppressesFailure(r, intent, now)
		if looksFailed && !suppressed {
			return StateError
		}
		if r.IsMaintenance {
			continue
		}
		// A row whose failure was suppressed by an active stop intent
		// is "intentionally not running" — exclude it from the
		// running/total denominator so 2 Running + 1 user-stopped
		// classifies as StateHealthy, not StatePartial. Without this
		// exclusion the suppression only covered the StateError path
		// and the icon still flashed yellow on a clean stop. Codex
		// deep-sec finding round 1 (MED). suppressedCount lets the
		// total==0 branch below distinguish "every daemon
		// intentionally stopped" (StateDown) from "no daemons exist"
		// (StateHealthy) — codex bot PR #142 round 4 P2.
		if suppressed {
			suppressedCount++
			continue
		}
		total++
		if r.State == "Running" {
			running++
		}
	}
	if total == 0 {
		// Codex bot PR #142 round 4 P2: when EVERY non-maintenance
		// row was intent-suppressed (e.g. a single daemon with
		// LastResult=1 + active user-stop, OR every daemon disabled),
		// the operator wants nothing running. Surfacing StateHealthy
		// here would paint a green icon over an entirely-stopped
		// system — the inverse of the truth. StateDown ("nothing
		// running") matches reality and the operator's intent.
		if suppressedCount > 0 {
			return StateDown
		}
		// No rows at all (or only maintenance daemons) — nothing
		// to classify; default to StateHealthy.
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
// `now` of zero value is permitted only when the intent map is empty:
// callers that pass non-empty intent + zero now would otherwise hit
// IsActiveStop's future-skew fail-closed branch (year 0001 < UpdatedAt
// - 5m for any realistic record), silently re-attributing every reason
// as "clock-skew-future-suspect" and dropping the chronic-failure
// carve-out. AggregateWithIntent guards against that misuse by
// promoting zero now → time.Now() before reaching this function.
// The legacy Aggregate(rows) path passes an empty intent map, so
// the lookup misses on every row and the zero-now value is never
// observed by IsActiveStop — exactly the back-compat semantic
// Aggregate needs (no rows, no suppression).
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
