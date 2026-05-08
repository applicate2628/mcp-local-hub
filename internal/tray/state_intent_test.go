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
// user stop). With an active user-stop intent the row must classify
// as StateDown instead.
func TestAggregateWithIntent_RealFailure_UserStop_DownNotError(t *testing.T) {
	now := time.Now().UTC()
	intent := intentFile("\\mcp-local-hub-memory-default",
		stoppedIntent(api.IntentReasonUserStop, now.Add(-1*time.Minute)))

	rows := []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, now)
	if got != StateDown {
		t.Errorf("user-stop intent within TTL → got %v, want StateDown (bug #3)", got)
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
// (likely the exit code from the stop that flipped it to disabled) must
// not surface as an error.
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
		t.Errorf("user-disabled intent → got %v, want StateDown (permanent suppress must hide failure)", got)
	}
}

// TestAggregateWithIntent_RealFailure_Uninstalled_DownNotError covers
// the cleanup-side reason. After uninstall there should be no row left
// at all, but if status enumeration races with intent record (rare,
// observable during the uninstall handshake) the aggregator must not
// flash a red icon for the in-flight removal.
func TestAggregateWithIntent_RealFailure_Uninstalled_DownNotError(t *testing.T) {
	now := time.Now().UTC()
	intent := intentFile("\\mcp-local-hub-memory-default",
		stoppedIntent(api.IntentReasonUninstalled, now.Add(-30*time.Second)))

	rows := []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "Stopped", LastResult: 1},
	}
	got := AggregateWithIntent(rows, intent, now)
	if got != StateDown {
		t.Errorf("uninstalled intent → got %v, want StateDown (removal in-flight must not flash error)", got)
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
// the alternative entry point.
func TestAggregateWithIntent_StateContainsFail_UserStop_Suppressed(t *testing.T) {
	now := time.Now().UTC()
	intent := intentFile("\\mcp-local-hub-memory-default",
		stoppedIntent(api.IntentReasonUserStop, now.Add(-1*time.Minute)))

	rows := []api.DaemonStatus{
		{Server: "memory", TaskName: "\\mcp-local-hub-memory-default", State: "FailedToLaunch"},
	}
	got := AggregateWithIntent(rows, intent, now)
	if got != StateDown {
		t.Errorf("state=FailedToLaunch + user-stop → got %v, want StateDown (state-string path must respect intent)", got)
	}
}

// TestAggregateWithIntent_TaskNameNormalization asserts the lookup
// uses the canonical leading-backslash key shape. The intent file
// stores entries under "\mcp-local-hub-X" (per WriteDaemonIntent's
// canonicalIntentTaskKey normalization); rows from Status() also
// carry the leading backslash. Mismatched normalization here would
// re-introduce the bug under a different guise (lookup misses,
// row classified as error despite recorded intent).
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
		t.Errorf("canonical leading-backslash key lookup → got %v, want StateDown (key normalization regression)", got)
	}
}

// TestAggregateWithIntent_EmptyIntent_BehavesLikeAggregate guards the
// API back-compat: with an empty intent file passed in, the function
// must produce the exact same verdict as the legacy Aggregate(rows)
// across every existing classification case.
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
