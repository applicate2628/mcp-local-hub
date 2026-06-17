package api

import "testing"

// TestDaemonState_QuarantinedIsEnumeratedNotPassthrough is the forward-looking
// guard that closes the latent fail-loud-to-fail-quiet TRAP this refactor
// targets. The legacy normalizeSupervisorIPCStatusState had NO quarantine case,
// so "Quarantined" survived only via `default: return state` verbatim
// passthrough. If a future change tightened that default into a closed enum,
// Quarantined would silently disappear — a fail-quiet regression on the
// operator's MAIN failure signal.
//
// This test asserts Quarantined is classified as the canonical
// DaemonDisplayQuarantined (an ENUMERATED value), independent of any
// passthrough default, on BOTH the producer projection (tracker raw vocabulary)
// and the IPC-consumer projection (Title-case wire vocabulary).
func TestDaemonState_QuarantinedIsEnumeratedNotPassthrough(t *testing.T) {
	// Producer side: the tracker raw vocabulary recognizes both lowercase
	// quarantine spellings (and is case-insensitive + trimming), so all of
	// these classify to the enumerated quarantine state.
	for _, raw := range []string{"quarantine", "quarantined", "QUARANTINED", "  Quarantined  "} {
		got, ok := classifyTrackerRuntimeState(raw)
		if !ok {
			t.Errorf("classifyTrackerRuntimeState(%q): ok=false — Quarantined must be ENUMERATED, not a passthrough", raw)
			continue
		}
		if got != DaemonDisplayQuarantined {
			t.Errorf("classifyTrackerRuntimeState(%q) = %v, want DaemonDisplayQuarantined", raw, got)
		}
	}
	// IPC-consumer side: the producer always emits the Title-case "Quarantined"
	// on the wire, so THAT exact word is the one the IPC projection enumerates
	// (closing the trap). The lowercase tracker spellings never reach the wire
	// in that form — they were already converted by the producer — so on the
	// IPC side they remain legacy verbatim passthrough (NOT enumerated), which
	// is the behavior the golden test pins. Enumerating only the producer's
	// actual wire word is what keeps every output byte-identical.
	if got, ok := classifyIPCWireState("Quarantined"); !ok || got != DaemonDisplayQuarantined {
		t.Errorf("classifyIPCWireState(%q) = (%v, ok=%v), want (DaemonDisplayQuarantined, true) — the producer's wire word must be ENUMERATED, not a passthrough", "Quarantined", got, ok)
	}
	// And the OUTPUT stays byte-identical to the legacy passthrough: the
	// projections must still emit the operator-visible strings.
	if got := ProjectIPCStatusState("Quarantined"); got != "Quarantined" {
		t.Errorf("ProjectIPCStatusState(Quarantined) = %q, want Quarantined (output must be byte-identical to legacy passthrough)", got)
	}
	if got := ProjectGUIState("quarantined"); got != "Quarantined" {
		t.Errorf("ProjectGUIState(quarantined) = %q, want Quarantined", got)
	}
	if got := ProjectHealthWireState("Quarantined"); got != "failed" {
		t.Errorf("ProjectHealthWireState(Quarantined) = %q, want failed (a quarantined crash-looped daemon IS a failure)", got)
	}
}

// TestDaemonState_RuntimeStatesRoundTripToFrontendCasing asserts that every
// tracker runtime state round-trips producer -> IPC-client -> the casing the
// frontend depends on (internal/gui/frontend renders DaemonStatus.state
// verbatim and branches on `=== "Running"`, so the IPC client's output casing
// IS the contract). It also asserts /api/status Title-case and /api/health
// lowercase are CONSISTENT projections of the same canonical state.
//
// Round-trip path:
//
//	tracker raw  --ProjectGUIState-->  IPC wire (Title-case)
//	             --ProjectIPCStatusState-->  /api/status display state
//	             --ProjectHealthWireState-->  /api/health lowercase enum
//
// This is the live supervisor status path (Dashboard + tray + mcphub status);
// the table is the explicit behavior contract for it.
func TestDaemonState_RuntimeStatesRoundTripToFrontendCasing(t *testing.T) {
	cases := []struct {
		rawTrackerState string // what the supervisor runtime tracker emits
		wantGUI         string // ProjectGUIState output = IPC wire state (Title-case)
		wantStatus      string // ProjectIPCStatusState output = /api/status display state
		wantHealth      string // ProjectHealthWireState output = /api/health lowercase enum
	}{
		// The four canonical tracker runtime states
		// (internal/cli/supervisor_runtime_tracker.go).
		{"running", "Running", "Running", "running"},
		{"idle", "Stopped", "Stopped", "stopped"},
		{"backoff", "Restarting", "Restarting", "starting"},
		{"quarantine", "Quarantined", "Quarantined", "failed"},
		// The supervisor-state synonyms the tracker also tolerates.
		{"backoff-waiting", "Restarting", "Restarting", "starting"},
		{"quarantined", "Quarantined", "Quarantined", "failed"},
		{"spawning", "Restarting", "Restarting", "starting"},
		// The fresh-entry empty case: the producer projects "" -> "Idle"
		// (DISTINCT from the "idle" -> "Stopped" outcome). The IPC consumer
		// then lowercases "Idle" -> "idle" -> "Stopped", and /api/health maps
		// "Stopped" -> "stopped". The producer's Idle/Stopped asymmetry
		// collapses at the IPC boundary (both not-running cases converge on
		// "Stopped"), which is exactly the legacy behavior.
		{"", "Idle", "Stopped", "stopped"},
	}
	for _, tc := range cases {
		gui := ProjectGUIState(tc.rawTrackerState)
		if gui != tc.wantGUI {
			t.Errorf("ProjectGUIState(%q) = %q, want %q", tc.rawTrackerState, gui, tc.wantGUI)
		}
		// Feed the producer's wire output through the IPC consumer — the
		// real production path (producer emits the Title-case state on the
		// wire; the IPC client re-normalizes it for /api/status).
		status := ProjectIPCStatusState(gui)
		if status != tc.wantStatus {
			t.Errorf("ProjectIPCStatusState(ProjectGUIState(%q)=%q) = %q, want %q",
				tc.rawTrackerState, gui, status, tc.wantStatus)
		}
		// And the /api/health projection of the SAME /api/status display
		// state must be a consistent lowercase enum value.
		health := ProjectHealthWireState(status)
		if health != tc.wantHealth {
			t.Errorf("ProjectHealthWireState(%q) = %q, want %q (must be a consistent projection of the canonical state)",
				status, health, tc.wantHealth)
		}
	}
}
