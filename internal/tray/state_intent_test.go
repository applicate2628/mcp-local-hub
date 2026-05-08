package tray

import (
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// helpers ---------------------------------------------------------------

// stoppedIntent builds a Desired=stopped intent with the supplied
// reason, anchored at `updatedAt`.
func stoppedIntent(reason string, updatedAt time.Time) api.DaemonIntent {
	return api.DaemonIntent{
		Desired:   api.IntentDesiredStopped,
		Reason:    reason,
		UpdatedAt: updatedAt.UTC(),
	}
}

// intentFile bundles a one-task intent file keyed by `taskName`. The
// real on-disk reader (api.DaemonIntentFile) keys entries by canonical
// leading-backslash form; tests build the file the same way the
// production lookup expects to avoid normalization drift.
func intentFile(taskName string, intent api.DaemonIntent) api.DaemonIntentFile {
	return api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{taskName: intent},
	}
}

// TestAggregateWithIntent_RealFailure_NoIntent_StillError preserves
// the pre-bug-fix behavior: a real-failure exit code with NO intent
// recorded must still classify as StateError. The fix is a narrowing
// (suppress when intent says user-stopped), not a broadening — daemons
// that crash on their own with no operator stop must keep raising the
// red badge.
func TestAggregateWithIntent_RealFailure_NoIntent_StillError(t *testing.T) {
	rows := []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{}}, time.Now().UTC())
	if got != StateError {
		t.Errorf("no intent recorded → got %v, want StateError (current behavior must be preserved)", got)
	}
}

// TestAggregateWithIntent_RealFailure_UserStop_DownNotError is the
// core regression guard for bug #3. Node-based MCP servers exit with
// code 1 on graceful stdin-close; after `mcphub stop --server memory`
// LastResult=1 used to classify as StateError (red icon for a clean
// user stop). With an active user-stop intent the row is excluded
// from BOTH the error path and the running/total denominator
// (codex deep-sec round 1 MED). When the suppressed row is the SOLE
// non-maintenance row, total==0 + suppressedCount==1 classifies as
// StateDown (codex bot PR #142 round 4 P2: operator-stopped sole
// daemon must surface "nothing running" — gray-icon honesty —
// rather than the original wrong "green icon over stopped system").
func TestAggregateWithIntent_RealFailure_UserStop_DownNotError(t *testing.T) {
	now := time.Now().UTC()
	intent := intentFile("\\mcp-local-hub-memory-default",
		stoppedIntent(api.IntentReasonUserStop, now.Add(-1*time.Minute)))

	rows := []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, now)
	// Codex bot PR #142 round 4 P2: a sole user-stopped row must
	// classify as StateDown ("nothing running"), NOT StateHealthy
	// ("everything fine"). Suppression hides the red badge so the
	// icon does not flash error on a clean stop, but a green icon
	// over an entirely-stopped system is the inverse of truth.
	if got != StateDown {
		t.Errorf("user-stop intent within TTL (sole row) → got %v, want StateDown (suppression hides red, but operator-stopped sole row is down, not healthy)", got)
	}
}

// TestAggregateWithIntent_RealFailure_UserStop_TTLExpired_BackToError
// guards the TTL boundary. user-stop intent decays to "ineligible"
// after StopIntentTTL (24h); past that window a real failure must
// once again classify as StateError so a daemon that crashes 25h
// after the operator stopped it still surfaces the red badge.
func TestAggregateWithIntent_RealFailure_UserStop_TTLExpired_BackToError(t *testing.T) {
	now := time.Now().UTC()
	// 25h ago — past StopIntentTTL (24h).
	intent := intentFile("\\mcp-local-hub-memory-default",
		stoppedIntent(api.IntentReasonUserStop, now.Add(-25*time.Hour)))

	rows := []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, now)
	if got != StateError {
		t.Errorf("user-stop intent past TTL → got %v, want StateError (TTL expiry must restore failure visibility)", got)
	}
}

// TestAggregateWithIntent_RealFailure_UserDisabled_DownNotError covers
// the permanent-suppress reason. user-disabled never expires; the
// operator deliberately turned this daemon off, so a non-zero LastResult
// (likely the exit code from the stop that flipped it to disabled)
// must not surface as a red error badge. Sole row → after suppression
// total=0 + suppressedCount=1 → StateDown (codex bot PR #142 round 4
// P2: an entirely-suppressed system is operator-stopped, not healthy).
func TestAggregateWithIntent_RealFailure_UserDisabled_DownNotError(t *testing.T) {
	now := time.Now().UTC()
	// Even very old user-disabled intents stay active (no TTL).
	intent := intentFile("\\mcp-local-hub-memory-default",
		stoppedIntent(api.IntentReasonUserDisabled, now.Add(-30*24*time.Hour)))

	rows := []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, now)
	if got != StateDown {
		t.Errorf("user-disabled intent (sole row) → got %v, want StateDown (permanent suppression hides red badge; sole-row stopped system is down, not healthy)", got)
	}
}

// TestAggregateWithIntent_RealFailure_Uninstalled_DownNotError covers
// the cleanup-side reason. After uninstall there should be no row
// left at all, but if status enumeration races with intent record
// (rare, observable during the uninstall handshake) the aggregator
// must not flash a red icon for the in-flight removal. Sole row →
// after suppression total=0 + suppressedCount=1 → StateDown (codex
// bot PR #142 round 4 P2: an in-flight uninstall is "the operator
// wants this gone" — not running, not healthy).
func TestAggregateWithIntent_RealFailure_Uninstalled_DownNotError(t *testing.T) {
	now := time.Now().UTC()
	intent := intentFile("\\mcp-local-hub-memory-default",
		stoppedIntent(api.IntentReasonUninstalled, now.Add(-30*time.Second)))

	rows := []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, now)
	if got != StateDown {
		t.Errorf("uninstalled intent (sole row) → got %v, want StateDown (in-flight removal hides red badge; sole-row stopped system is down, not healthy)", got)
	}
}

// TestAggregateWithIntent_RealFailure_ChronicFailure_StaysError guards
// the case where the watchdog itself fail-closed-quarantined the
// daemon (chronic-failure reason). Even though it is "Desired=stopped",
// chronic-failure is precisely the case the operator NEEDS to see —
// the red badge must remain because it is the watchdog telling the
// operator something is wrong. This is the only Desired=stopped reason
// that keeps StateError.
func TestAggregateWithIntent_RealFailure_ChronicFailure_StaysError(t *testing.T) {
	now := time.Now().UTC()
	intent := intentFile("\\mcp-local-hub-memory-default",
		stoppedIntent(api.IntentReasonChronicFailure, now.Add(-1*time.Minute)))

	rows := []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, now)
	if got != StateError {
		t.Errorf("chronic-failure intent → got %v, want StateError (watchdog quarantine must STAY visible)", got)
	}
}

// TestAggregateWithIntent_Running_IgnoresIntent: a Running row with
// LastResult=0 contributes no failure and the running/total counter
// drives the aggregate. Active intent must not change that classification
// — intent is only consulted when something looks like a failure.
func TestAggregateWithIntent_Running_IgnoresIntent(t *testing.T) {
	now := time.Now().UTC()
	intent := intentFile("\\mcp-local-hub-memory-default",
		stoppedIntent(api.IntentReasonUserStop, now.Add(-1*time.Minute)))
	rows := []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Running"},
	}
	got := AggregateWithIntent(rows, intent, now)
	if got != StateHealthy {
		t.Errorf("Running row with active intent → got %v, want StateHealthy (intent must not down-grade healthy rows)", got)
	}
}

// TestAggregateWithIntent_StateContainsFail_UserStop_Suppressed mirrors
// the LastResult-based path for the state-string predicate. Some daemon
// paths emit "Failed" without a matching LastResult update; the same
// user-stop suppression must apply, otherwise the bug recurs through
// the alternative entry point. Sole row → after exclusion total=0 →
// Healthy (codex deep-sec round 1 MED).
func TestAggregateWithIntent_StateContainsFail_UserStop_Suppressed(t *testing.T) {
	now := time.Now().UTC()
	intent := intentFile("\\mcp-local-hub-memory-default",
		stoppedIntent(api.IntentReasonUserStop, now.Add(-1*time.Minute)))

	rows := []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "FailedToLaunch"},
	}
	got := AggregateWithIntent(rows, intent, now)
	// Codex bot PR #142 round 4 P2: a sole user-stopped row must not
	// surface StateHealthy — the operator deliberately stopped it, so
	// "nothing running" (StateDown) is the truthful classification.
	// The state-string failure path is still suppressed (no red badge),
	// but a green badge over an entirely-stopped system was wrong.
	if got != StateDown {
		t.Errorf("state=FailedToLaunch + user-stop (sole row) → got %v, want StateDown (suppression hides red, but operator-stopped system is down, not healthy)", got)
	}
}

// TestAggregateWithIntent_TaskNameNormalization asserts the lookup
// uses the canonical leading-backslash key shape. The intent file
// stores entries under "\mcp-local-hub-X" (per WriteDaemonIntent's
// canonicalIntentTaskKey normalization); rows from Status() also
// carry the leading backslash. Mismatched normalization here would
// re-introduce the bug under a different guise (lookup misses,
// row classified as error despite recorded intent). Sole row →
// after suppression+exclusion total=0 + suppressedCount=1 →
// StateDown (codex bot PR #142 round 4 P2: an entirely-suppressed
// system is operator-stopped, not healthy).
func TestAggregateWithIntent_TaskNameNormalization(t *testing.T) {
	now := time.Now().UTC()
	intent := intentFile("\\mcp-local-hub-memory-default",
		stoppedIntent(api.IntentReasonUserStop, now.Add(-1*time.Minute)))

	rows := []api.DaemonStatus{
		// TaskName matches the canonical (leading-backslash) form
		// used by both scheduler.List() and WriteDaemonIntent.
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, now)
	if got != StateDown {
		t.Errorf("canonical leading-backslash key lookup → got %v, want StateDown (suppression applies via canonical key; sole-row stopped system is down, not healthy)", got)
	}
}

// TestAggregateWithIntent_MixedRunningPlusUserStopped_Healthy is the
// codex deep-sec round-1 finding (MED). With 2 Running rows + 1
// LastResult=1 row that has an active user-stop intent, the suppressed
// row was still counted in the running/total denominator, producing
// StatePartial (running=2, total=3). The operator's intent says "this
// daemon is intentionally not running" so it must be invisible to the
// healthy-ratio calculation as well — every row with an active
// suppressed-stop intent is excluded from BOTH the error AND the ratio
// counters. Expected: StateHealthy (running=2, total=2 after exclusion).
func TestAggregateWithIntent_MixedRunningPlusUserStopped_Healthy(t *testing.T) {
	now := time.Now().UTC()
	intent := api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		"\\mcp-local-hub-memory-default": stoppedIntent(api.IntentReasonUserStop, now.Add(-1*time.Minute)),
	}}

	rows := []api.DaemonStatus{
		{Server: "fs", TaskName: "\\mcp-local-hub-fs-default", State: "Running"},
		{Server: "git", TaskName: "\\mcp-local-hub-git-default", State: "Running"},
		// User-stopped row with the canonical Node-MCP exit-1 signature.
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, now)
	if got != StateHealthy {
		t.Errorf("2 Running + 1 user-stopped (suppressed) → got %v, want StateHealthy "+
			"(suppressed row must be excluded from running/total ratio, not just the error path)", got)
	}
}

// TestAggregateWithIntent_MultiRow_OneUserStoppedPlusOneRealFailure
// guards the inverse: even with one row legitimately suppressed, a
// SECOND row with a real failure but NO intent must still dominate
// to StateError. The new exclusion rule must not weaken error
// detection on adjacent rows.
func TestAggregateWithIntent_MultiRow_OneUserStoppedPlusOneRealFailure(t *testing.T) {
	now := time.Now().UTC()
	intent := api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		"\\mcp-local-hub-memory-default": stoppedIntent(api.IntentReasonUserStop, now.Add(-1*time.Minute)),
	}}

	rows := []api.DaemonStatus{
		{Server: "fs", TaskName: "\\mcp-local-hub-fs-default", State: "Running"},
		// User-stopped + suppressed.
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
		// Real failure with NO recorded intent → must dominate.
		{Server: "git", TaskName: "\\mcp-local-hub-git-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, now)
	if got != StateError {
		t.Errorf("1 Running + 1 suppressed + 1 real failure (no intent) → got %v, want StateError "+
			"(real failure on a non-suppressed row must still raise red badge)", got)
	}
}

// TestAggregateWithIntent_ZeroNow_FallsBackToTimeNow is the codex
// deep-sec round-1 finding (LOW): the comment claimed
// IsActiveStop(time.Time{}) returns false for realistic intents, but
// in fact it hits the future-skew fail-closed branch (because
// time.Time{} < UpdatedAt - 5m for any realistic UpdatedAt) and
// returns (true, "clock-skew-future-suspect") — exactly the OPPOSITE
// of the comment's claim. With now=zero AND a non-empty intent file,
// AggregateWithIntent would mis-suppress the row.
//
// Defense: AggregateWithIntent must promote zero `now` to time.Now()
// internally, making the back-compat Aggregate(rows) path implicitly
// safe (empty intent + now=time.Now() → no suppression) AND making
// AggregateWithIntent(non-empty, zero) behave like a sensible "use
// current wall-clock" call instead of an exported foot-gun.
//
// Multi-row variant: a Running peer + a user-stopped (suppressed) row
// is the cleanest assertion target. Without the fallback the future-skew
// suppression still fires on the user-stop reason (same outcome by
// accident); the dangerous reason-confusion case is in the
// ZeroNow_ChronicFailure test below.
func TestAggregateWithIntent_ZeroNow_FallsBackToTimeNow(t *testing.T) {
	// UpdatedAt anchored "now-ish" so a sane wall-clock would classify
	// it as an active user-stop within TTL → suppression should fire.
	intent := api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		"\\mcp-local-hub-memory-default": stoppedIntent(
			api.IntentReasonUserStop, time.Now().UTC().Add(-1*time.Minute)),
	}}

	rows := []api.DaemonStatus{
		{Server: "fs", TaskName: "\\mcp-local-hub-fs-default", State: "Running"},
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, time.Time{})
	if got != StateHealthy {
		t.Errorf("zero now + active user-stop intent (1 Running peer) → got %v, want StateHealthy "+
			"(zero-now must promote to time.Now(); suppressed row excluded from denominator)", got)
	}
}

// TestAggregateWithIntent_ZeroNow_ChronicFailure_StaysError exercises
// the dangerous reason-confusion path the zero-now defense actually
// closes. Without the defense, IsActiveStop(time.Time{}) returns
// (true, "clock-skew-future-suspect") regardless of the recorded
// reason, so chronic-failure (which the operator NEEDS to see) would
// be silently re-classified as a clock-skew suppression. With the
// defense, IsActiveStop runs against real wall-clock and the
// chronic-failure carve-out fires correctly → StateError.
func TestAggregateWithIntent_ZeroNow_ChronicFailure_StaysError(t *testing.T) {
	intent := api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		"\\mcp-local-hub-memory-default": stoppedIntent(
			api.IntentReasonChronicFailure, time.Now().UTC().Add(-1*time.Minute)),
	}}

	rows := []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, time.Time{})
	if got != StateError {
		t.Errorf("zero now + chronic-failure intent → got %v, want StateError "+
			"(chronic-failure carve-out must survive the zero-now fallback; "+
			"without the fallback, IsActiveStop misreports clock-skew-future-suspect "+
			"and the watchdog quarantine vanishes from the icon)", got)
	}
}

// TestAggregateWithIntent_EmptyIntent_BehavesLikeAggregate guards the
// API back-compat: with an empty intent file passed in, the function
// must produce the exact same verdict as the legacy Aggregate(rows)
// across every existing classification case.
// TestAggregateWithIntent_AllUserStopped_StateDown is the codex bot
// PR #142 round 4 P2 regression guard. A single daemon with
// LastResult=1 (Node MCP graceful stdin-close after `mcphub stop`)
// plus active user-stop intent must classify as StateDown — the
// operator deliberately stopped everything. The PR #142 round 1 fix
// excluded user-stopped rows from the running/total denominator
// (correct for "2 Running + 1 user-stopped → Healthy" — kept healthy
// even when one is intentionally stopped) but accidentally regressed
// "1 user-stopped (only row) → StateHealthy" through the
// `if total == 0 { return StateHealthy }` fast path. The fix tracks
// suppressed count and returns StateDown when total==0 + suppressed>0.
func TestAggregateWithIntent_AllUserStopped_StateDown(t *testing.T) {
	now := time.Now().UTC()
	intent := api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		"\\mcp-local-hub-memory-default": stoppedIntent(api.IntentReasonUserStop, now.Add(-1*time.Minute)),
	}}
	rows := []api.DaemonStatus{
		// The exact bot scenario: one daemon, LastResult=1, active user-stop.
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, now)
	if got != StateDown {
		t.Errorf("single user-stopped daemon (LastResult=1) → got %v, want StateDown "+
			"(operator stopped everything; tray must show down, NOT healthy)", got)
	}
}

// TestAggregateWithIntent_AllUserDisabled_StateDown covers the
// second bot scenario: every daemon has user-disabled intent. The
// suppression carve-out for user-disabled is identical to user-stop
// at the StateError gate, but the down-not-healthy invariant must
// hold across all stop reasons.
func TestAggregateWithIntent_AllUserDisabled_StateDown(t *testing.T) {
	now := time.Now().UTC()
	intent := api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		"\\mcp-local-hub-fs-default":     stoppedIntent(api.IntentReasonUserDisabled, now.Add(-1*time.Hour)),
		"\\mcp-local-hub-memory-default": stoppedIntent(api.IntentReasonUserDisabled, now.Add(-1*time.Hour)),
	}}
	rows := []api.DaemonStatus{
		{Server: "fs", TaskName: "\\mcp-local-hub-fs-default", State: "Stopped", LastResult: 1},
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, now)
	if got != StateDown {
		t.Errorf("two user-disabled daemons (both stopped, LastResult=1) → got %v, want StateDown", got)
	}
}

// TestAggregateWithIntent_PlainStoppedPlusUserStopped_StateDown covers
// the codex r5 QA finding: a plain-Stopped row (no recorded intent)
// alongside a user-stopped suppressed row must classify as StateDown.
// The plain row is NOT suppressed (no intent → no exclusion), so
// total=1, running=0 → StateDown via the running==0 path. The
// suppressed peer is excluded from the denominator; if the
// suppression branch had bled into the plain row, the test would
// fail because total=0 + suppressed>0 would also produce StateDown
// — but the test is specifically about the running==0 fork being
// reached, not the total==0 fallback.
func TestAggregateWithIntent_PlainStoppedPlusUserStopped_StateDown(t *testing.T) {
	now := time.Now().UTC()
	intent := api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		"\\mcp-local-hub-memory-default": stoppedIntent(api.IntentReasonUserStop, now.Add(-1*time.Minute)),
	}}
	rows := []api.DaemonStatus{
		// Plain-Stopped, no intent: contributes to total but not running.
		{Server: "fs", TaskName: "\\mcp-local-hub-fs-default", State: "Stopped"},
		// User-stopped + suppressed: excluded from total/running.
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, now)
	if got != StateDown {
		t.Errorf("plain-stopped + user-stopped → got %v, want StateDown (running==0 fork; not the total==0 + suppressed fork)", got)
	}
}

// TestAggregateWithIntent_NoRows_StateHealthy pins the OTHER half of
// the total==0 fork: when no rows exist at all (or every row is
// IsMaintenance), suppressedCount stays 0 and the function returns
// StateHealthy ("nothing to classify"). Catches a future regression
// that might fold the no-rows case into the all-suppressed branch.
func TestAggregateWithIntent_NoRows_StateHealthy(t *testing.T) {
	now := time.Now().UTC()
	emptyIntent := api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{}}
	if got := AggregateWithIntent(nil, emptyIntent, now); got != StateHealthy {
		t.Errorf("no rows, empty intent → got %v, want StateHealthy", got)
	}
	// Maintenance-only rows must also classify as healthy.
	rows := []api.DaemonStatus{
		{Server: "fs", TaskName: "\\mcp-local-hub-fs-default", State: "Stopped", IsMaintenance: true},
	}
	if got := AggregateWithIntent(rows, emptyIntent, now); got != StateHealthy {
		t.Errorf("maintenance-only rows → got %v, want StateHealthy", got)
	}
}

func TestAggregateWithIntent_EmptyIntent_BehavesLikeAggregate(t *testing.T) {
	cases := []struct {
		name string
		rows []api.DaemonStatus
	}{
		{"empty", nil},
		{"all running", []api.DaemonStatus{{Server: "x", State: "Running"}}},
		{"some stopped", []api.DaemonStatus{
			{Server: "x", State: "Running"},
			{Server: "y", State: "Stopped"},
		}},
		{"all stopped", []api.DaemonStatus{{Server: "x", State: "Stopped"}}},
		{"failure", []api.DaemonStatus{{Server: "x", State: "Stopped", LastResult: 1}}},
		{"info code", []api.DaemonStatus{{Server: "x", State: "Ready", LastResult: 0x41303}}},
	}
	emptyIntent := api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			legacy := Aggregate(tc.rows)
			got := AggregateWithIntent(tc.rows, emptyIntent, time.Now().UTC())
			if got != legacy {
				t.Errorf("AggregateWithIntent(empty intent) = %v, Aggregate = %v (back-compat break)", got, legacy)
			}
		})
	}
}
