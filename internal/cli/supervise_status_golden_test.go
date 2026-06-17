package cli

import "testing"

// TestGolden_supervisorStatusGUIState pins the EXACT current output of the
// producer mapper (tracker raw-runtime state -> Title-case GUI/IPC-wire state)
// over the complete input domain. This is the behavior baseline for STEP D:
// after the refactor repoints this function to the canonical api classifier,
// every output here MUST stay byte-identical.
//
// The input domain matches the api-side golden sibling
// (internal/api/daemon_state_golden_test.go) so all three classifiers are
// exercised over the same vocabulary.
//
// NOTE the documented quirks this table encodes:
//   - it lowercase+trims first (so "  running  " and "RUNNING" both -> "Running",
//     and "Idle" -> "idle" -> "Stopped");
//   - "idle"/"" map to DISTINCT outcomes: "idle" -> "Stopped" but "" -> "Idle"
//     (the producer's two distinct not-running outcomes — this asymmetry is
//     exactly why centralizing the vocabulary needs DISTINCT projection methods,
//     not one output);
//   - backoff / backoff-waiting / spawning -> "Restarting";
//   - quarantine / quarantined -> "Quarantined";
//   - everything else passes through VERBATIM in its ORIGINAL casing
//     (e.g. "stopped" -> "stopped", "Stopped" -> "Stopped", "Failed" -> "Failed",
//     "restarting" -> "restarting", "garbage" -> "garbage").
func TestGolden_supervisorStatusGUIState(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// lowercase tracker raw-runtime vocabulary
		{"", "Idle"},
		{"running", "Running"},
		{"idle", "Stopped"},
		{"stopped", "stopped"}, // not recognized (only "idle"/"" are not-running cases) -> verbatim
		{"backoff", "Restarting"},
		{"backoff-waiting", "Restarting"},
		{"restarting", "restarting"}, // not recognized by the producer -> verbatim lowercase
		{"spawning", "Restarting"},
		{"quarantine", "Quarantined"},
		{"quarantined", "Quarantined"},
		// Title-case GUI-state vocabulary
		{"Running", "Running"},
		{"Stopped", "Stopped"}, // lowercases to "stopped" -> not a case -> verbatim ORIGINAL casing
		{"Restarting", "Restarting"},
		{"Quarantined", "Quarantined"},
		{"Idle", "Stopped"}, // lowercases to "idle" -> the "idle" case fires -> "Stopped"
		{"Starting", "Starting"},
		{"Backoff", "Restarting"},
		{"Spawning", "Restarting"},
		{"Failed", "Failed"},
		{"Ready", "Ready"},
		{"Scheduled", "Scheduled"},
		// unknown / garbage
		{"  running  ", "Running"},
		{"RUNNING", "Running"},
		{"Disabled", "Disabled"},
		{"Queued", "Queued"},
		{"garbage", "garbage"},
		{"  ", "Idle"}, // trims to "" -> "Idle"
	}
	for _, tc := range cases {
		if got := supervisorStatusGUIState(tc.in); got != tc.want {
			t.Errorf("supervisorStatusGUIState(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
