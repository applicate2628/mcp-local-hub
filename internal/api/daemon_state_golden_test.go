package api

import "testing"

// daemonStateGoldenInputs is the COMPLETE input domain that exercises the
// daemon-state display classifiers. It deliberately mixes:
//   - the lowercase tracker raw-runtime vocabulary the producer emits
//     ("", running, idle, stopped, backoff, backoff-waiting, restarting,
//     spawning, quarantine, quarantined), and
//   - the Title-case GUI-state vocabulary the producer (supervisorStatusGUIState)
//     and the IPC client (normalizeSupervisorIPCStatusState) can hand to the
//     /api/status + /api/health projections (Running, Stopped, Restarting,
//     Quarantined, Idle, Starting, Backoff, Spawning, Failed, Ready, Scheduled),
//   - and a couple of unknown/garbage inputs (whitespace, mixed case, junk).
//
// Every classifier under test is fed this same domain so the golden tables in
// this file (and the cli-side sibling) pin the EXACT current output of each
// function before and after the refactor. A byte-different output is a
// behavior change and fails the test.
var daemonStateGoldenInputs = []string{
	// lowercase tracker raw-runtime vocabulary
	"", "running", "idle", "stopped", "backoff", "backoff-waiting",
	"restarting", "spawning", "quarantine", "quarantined",
	// Title-case GUI-state vocabulary
	"Running", "Stopped", "Restarting", "Quarantined", "Idle",
	"Starting", "Backoff", "Spawning", "Failed", "Ready", "Scheduled",
	// unknown / garbage
	"  running  ", "RUNNING", "Disabled", "Queued", "garbage", "  ",
}

// TestGolden_normalizeSupervisorIPCStatusState pins the EXACT current output of
// the IPC-consumer mapper (Title-case wire state -> /api/status form) over the
// complete input domain. This is the behavior baseline for STEP D: after the
// refactor repoints this function to the canonical classifier, every output
// here MUST stay byte-identical.
//
// NOTE the documented quirks this table encodes:
//   - it lowercase+trims first (so "  running  " and "RUNNING" both -> "Running");
//   - "idle" AND "stopped" both -> "Stopped" (unlike the producer, "" has NO
//     case here, so "" passes through verbatim as "");
//   - "restarting" is a recognized input here (the producer never emits it, but
//     the wire can carry the producer's "Restarting" output re-fed; lowercased
//     it hits this case);
//   - everything else (including "Quarantined" — the LATENT TRAP) passes through
//     VERBATIM in its ORIGINAL casing. "Quarantined" -> "Quarantined".
func TestGolden_normalizeSupervisorIPCStatusState(t *testing.T) {
	want := map[string]string{
		"":                "",
		"running":         "Running",
		"idle":            "Stopped",
		"stopped":         "Stopped",
		"backoff":         "Restarting",
		"backoff-waiting": "Restarting",
		"restarting":      "Restarting",
		"spawning":        "spawning",
		"quarantine":      "quarantine",
		"quarantined":     "quarantined",
		"Running":         "Running",
		"Stopped":         "Stopped",
		"Restarting":      "Restarting",
		"Quarantined":     "Quarantined",
		"Idle":            "Stopped",
		"Starting":        "Starting",
		"Backoff":         "Restarting",
		"Spawning":        "Spawning",
		"Failed":          "Failed",
		"Ready":           "Ready",
		"Scheduled":       "Scheduled",
		"  running  ":     "Running",
		"RUNNING":         "Running",
		"Disabled":        "Disabled",
		"Queued":          "Queued",
		"garbage":         "garbage",
		"  ":              "  ",
	}
	for _, in := range daemonStateGoldenInputs {
		exp, ok := want[in]
		if !ok {
			t.Fatalf("golden table missing input %q — extend daemonStateGoldenInputs and want together", in)
		}
		if got := normalizeSupervisorIPCStatusState(in); got != exp {
			t.Errorf("normalizeSupervisorIPCStatusState(%q) = %q, want %q", in, got, exp)
		}
	}
}

// TestGolden_normalizeDaemonState pins the EXACT current output of the
// /api/health projection (Title-case state -> lowercase wire enum) over the
// complete input domain. Behavior baseline for STEP D.
//
// NOTE the documented quirks this table encodes:
//   - it is CASE-SENSITIVE (does NOT lowercase first), so "running" (lowercase)
//     and "  running  " and "RUNNING" all fall to the default "unknown" — only
//     the exact "Running" maps to "running";
//   - the supervisor's KNOWN degraded/terminal vocabulary is enumerated:
//     Starting/Restarting/Backoff/Spawning -> "starting";
//     Failed/Quarantined -> "failed"; Ready/Scheduled/Stopped -> "stopped";
//   - everything genuinely unrecognized/blank (including "", lowercase forms,
//     "Idle", "Disabled", "Queued", garbage) -> "unknown" (NOT "failed").
func TestGolden_normalizeDaemonState(t *testing.T) {
	want := map[string]string{
		"":                "unknown",
		"running":         "unknown",
		"idle":            "unknown",
		"stopped":         "unknown",
		"backoff":         "unknown",
		"backoff-waiting": "unknown",
		"restarting":      "unknown",
		"spawning":        "unknown",
		"quarantine":      "unknown",
		"quarantined":     "unknown",
		"Running":         "running",
		"Stopped":         "stopped",
		"Restarting":      "starting",
		"Quarantined":     "failed",
		"Idle":            "unknown",
		"Starting":        "starting",
		"Backoff":         "starting",
		"Spawning":        "starting",
		"Failed":          "failed",
		"Ready":           "stopped",
		"Scheduled":       "stopped",
		"  running  ":     "unknown",
		"RUNNING":         "unknown",
		"Disabled":        "unknown",
		"Queued":          "unknown",
		"garbage":         "unknown",
		"  ":              "unknown",
	}
	for _, in := range daemonStateGoldenInputs {
		exp, ok := want[in]
		if !ok {
			t.Fatalf("golden table missing input %q — extend daemonStateGoldenInputs and want together", in)
		}
		if got := normalizeDaemonState(in); got != exp {
			t.Errorf("normalizeDaemonState(%q) = %q, want %q", in, got, exp)
		}
	}
}
