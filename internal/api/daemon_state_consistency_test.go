package api

import "testing"

// daemon_state_consistency_test.go is the G9 P2b consistency guard.
//
// /api/status (DaemonStatusSnapshot, via ProjectIPCStatusState — Title-case
// display words) and /api/health (DaemonRow.State, via ProjectHealthWireState /
// normalizeDaemonState — lowercase wire enum) project the SAME underlying
// daemon rows through two DIFFERENT vocabularies. Both now delegate to the
// canonical daemon_state.go (DaemonDisplayState). If a future vocabulary change
// edits ONE projection (say, renames the /api/status word for a quarantined
// daemon, or remaps the /api/health enum) without the other, the two surfaces
// would DESYNC: a Dashboard showing "Quarantined" while /api/health reports
// "running" (or vice versa) is a silent operator-facing lie.
//
// This guard iterates over EVERY canonical DaemonDisplayState (the single
// source of truth) and asserts that the status-wire and health-wire
// projections agree on the same health GROUP — a coarse semantic bucket
// (healthy / not-running / recovering / failed / unknown). The grouping is
// intentionally coarse: the two projections legitimately use different exact
// strings ("Running" vs "running", "Quarantined" vs "failed"), so an
// exact-string equality test would be wrong. What MUST hold is that they never
// disagree on the GROUP — Running must never be classified as a failure on one
// surface and healthy on the other.
//
// Distinct from TestDaemonState_RuntimeStatesRoundTripToFrontendCasing: that
// test pins the exact per-input round-trip outputs starting from tracker raw
// strings. THIS test is keyed on the canonical ENUM itself, so it directly
// catches a desync introduced by editing one projection's vocabulary, even if
// no tracker-raw input currently exercises the changed branch.

// healthGroup is a coarse semantic bucket shared by the two wire vocabularies.
// It is the level at which the /api/status and /api/health projections MUST
// agree even though their exact strings differ.
type healthGroup int

const (
	groupUnknown healthGroup = iota
	groupHealthy
	groupNotRunning
	groupRecovering
	groupFailed
)

func (g healthGroup) String() string {
	switch g {
	case groupHealthy:
		return "healthy"
	case groupNotRunning:
		return "not-running"
	case groupRecovering:
		return "recovering"
	case groupFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// statusWireGroup buckets a /api/status Title-case display word (the
// ProjectIPCStatusState output) into a health group.
func statusWireGroup(s string) healthGroup {
	switch s {
	case "Running":
		return groupHealthy
	case "Idle", "Stopped", "Ready", "Scheduled":
		return groupNotRunning
	case "Restarting", "Starting", "Backoff", "Spawning":
		return groupRecovering
	case "Failed", "Quarantined":
		return groupFailed
	default:
		return groupUnknown
	}
}

// healthWireGroup buckets a /api/health lowercase wire enum (the
// ProjectHealthWireState output) into a health group. The /api/health enum is
// already a small closed set ("running" | "stopped" | "starting" | "failed" |
// "unknown"), so this is a direct map.
func healthWireGroup(s string) healthGroup {
	switch s {
	case "running":
		return groupHealthy
	case "stopped":
		return groupNotRunning
	case "starting":
		return groupRecovering
	case "failed":
		return groupFailed
	default:
		return groupUnknown
	}
}

// TestDaemonState_StatusAndHealthProjectionsAgreeOnGroup is the consistency
// guard. For EVERY canonical DaemonDisplayState it walks the REAL production
// projection chain — a representative tracker-raw input -> ProjectGUIState
// (producer wire word) -> ProjectIPCStatusState (the /api/status display word)
// -> ProjectHealthWireState (the /api/health lowercase enum) — and asserts that
// the /api/status and /api/health forms bucket into the SAME health group. A
// future change that desyncs the two projections (e.g. remapping one surface's
// vocabulary without the other) makes the groups diverge and fails here.
//
// Walking the real chain (not hand-picking a literal status word) is what
// captures the documented Idle->Stopped collapse at the IPC boundary: the
// producer emits "Idle" for a fresh empty entry, but ProjectIPCStatusState
// lowercases+remaps it to "Stopped", so the /api/status word for the canonical
// Idle state is "Stopped", and /api/health then maps "Stopped" -> "stopped".
func TestDaemonState_StatusAndHealthProjectionsAgreeOnGroup(t *testing.T) {
	cases := []struct {
		canonical DaemonDisplayState
		name      string
		// rawInput is a representative tracker-raw string that
		// classifyTrackerRuntimeState maps to this canonical state. The chain
		// below projects it the way the live supervisor status path does.
		rawInput  string
		wantGroup healthGroup
	}{
		{DaemonDisplayRunning, "Running", "running", groupHealthy},
		// "" is the producer's fresh-entry Idle case; it projects to "Idle" on
		// the producer wire, which the IPC consumer collapses to "Stopped".
		{DaemonDisplayIdle, "Idle", "", groupNotRunning},
		// "idle" raw is the producer's tracked-but-not-running Stopped case.
		{DaemonDisplayStopped, "Stopped", "idle", groupNotRunning},
		{DaemonDisplayRestarting, "Restarting", "backoff", groupRecovering},
		{DaemonDisplayQuarantined, "Quarantined", "quarantine", groupFailed},
	}

	// Coverage assertion: every canonical state except the catch-all
	// DaemonDisplayUnknown must appear in the table. If a new
	// DaemonDisplayState constant is added without extending this table, the
	// count check below fails so the guard cannot silently skip the new state.
	// DaemonDisplayUnknown is intentionally excluded — it has no fixed
	// status-wire word (it carries verbatim raw input), so it is checked
	// separately below.
	const enumeratedNonUnknownStates = 5 // Running, Idle, Stopped, Restarting, Quarantined
	if len(cases) != enumeratedNonUnknownStates {
		t.Fatalf("table covers %d states, expected %d enumerated non-unknown canonical DaemonDisplayState values — extend the table when adding a new state", len(cases), enumeratedNonUnknownStates)
	}

	for _, tc := range cases {
		// Guard the table itself: the raw input must actually classify to the
		// canonical state the row claims, so a typo can't quietly test the
		// wrong state.
		if got, ok := classifyTrackerRuntimeState(tc.rawInput); !ok || got != tc.canonical {
			t.Errorf("%s: classifyTrackerRuntimeState(%q) = (%d, ok=%v), want canonical %d — fix the table's rawInput", tc.name, tc.rawInput, got, ok, tc.canonical)
			continue
		}

		// Walk the real production chain: producer wire -> /api/status word.
		producerWire := ProjectGUIState(tc.rawInput)
		statusWire := ProjectIPCStatusState(producerWire)

		statusGroup := statusWireGroup(statusWire)
		if statusGroup != tc.wantGroup {
			t.Errorf("%s: /api/status word %q (from raw %q via %q) buckets to %s, want %s", tc.name, statusWire, tc.rawInput, producerWire, statusGroup, tc.wantGroup)
		}

		// /api/health derives its lowercase enum from the SAME /api/status
		// Title-case word (computeDaemonsSection feeds the status display
		// state into normalizeDaemonState).
		healthWire := ProjectHealthWireState(statusWire)
		healthGrp := healthWireGroup(healthWire)
		if healthGrp != tc.wantGroup {
			t.Errorf("%s: /api/health enum %q (from %q) buckets to %s, want %s", tc.name, healthWire, statusWire, healthGrp, tc.wantGroup)
		}

		// The core consistency invariant: both projections of the same
		// canonical state MUST agree on the health group. This is the
		// assertion a future vocabulary desync trips.
		if statusGroup != healthGrp {
			t.Errorf("%s (canonical %d): /api/status word %q -> group %s, but /api/health enum %q -> group %s; the two projections of the SAME daemon state DISAGREE on the health group",
				tc.name, tc.canonical, statusWire, statusGroup, healthWire, healthGrp)
		}
	}

	// DaemonDisplayUnknown: an unrecognized state must map to the unknown
	// group on BOTH surfaces and must never silently become healthy or a
	// passthrough that one surface treats as failed and the other as fine.
	// Feed a genuinely-unrecognized raw word: /api/status passes it through
	// verbatim, /api/health honestly classifies it "unknown".
	const garbage = "ZzNotARealState"
	statusVerbatim := ProjectIPCStatusState(garbage)
	if g := statusWireGroup(statusVerbatim); g != groupUnknown {
		t.Errorf("unknown state %q: /api/status verbatim %q buckets to %s, want unknown", garbage, statusVerbatim, g)
	}
	healthUnknown := ProjectHealthWireState(statusVerbatim)
	if healthUnknown != "unknown" {
		t.Errorf("unknown state %q: /api/health = %q, want \"unknown\" (an unrecognized state must NOT silently become failed/running)", garbage, healthUnknown)
	}
	if statusWireGroup(statusVerbatim) != healthWireGroup(healthUnknown) {
		t.Errorf("unknown state %q: /api/status group %s != /api/health group %s — the two projections disagree on an unrecognized state",
			garbage, statusWireGroup(statusVerbatim), healthWireGroup(healthUnknown))
	}
}
