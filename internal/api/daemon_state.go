package api

import "strings"

// daemon_state.go is the SINGLE canonical owner of the daemon display-state
// vocabulary that the three hand-maintained status mappers used to encode
// independently:
//
//   - internal/cli/supervise_status.go    supervisorStatusGUIState(raw)
//         producer: tracker raw-runtime state -> Title-case GUI/IPC-wire state
//   - internal/api/supervisor_ipc_status_client.go
//                                          normalizeSupervisorIPCStatusState(s)
//         IPC consumer: Title-case wire state -> /api/status display form
//   - internal/api/health.go              normalizeDaemonState(s)
//         /api/health projection: Title-case state -> lowercase wire enum
//
// Centralizing the vocabulary here closes a LATENT fail-loud-to-fail-quiet
// TRAP: normalizeSupervisorIPCStatusState had NO quarantine case, so
// "Quarantined" hit a `default: return state` verbatim passthrough. If anyone
// later tightened that default into a closed enum, Quarantined daemons would
// silently render benign — a fail-quiet regression on the operator's main
// failure signal. The canonical classifier below ENUMERATES Quarantined
// explicitly on every projection, so the operator-visible vocabulary no longer
// depends on a passthrough default.
//
// The three call sites genuinely DIVERGE on the same input — that divergence
// is the reason to centralize, not a reason to flatten. They are modeled as
// DISTINCT projection methods of one canonical state, each reproducing the
// historical output byte-for-byte (proven by the golden tests in
// daemon_state_golden_test.go + the cli sibling). The classifiers preserve the
// historical quirks exactly:
//   - the producer + IPC-consumer projections lowercase+trim their input;
//     the /api/health projection is CASE-SENSITIVE (exact Title-case match);
//   - the producer maps "idle" -> Stopped but "" -> Idle (two distinct
//     not-running outcomes); the IPC consumer maps both "idle" and "stopped"
//     -> Stopped and has no empty-string special case;
//   - genuinely-unrecognized inputs pass through VERBATIM (original casing)
//     in the producer + IPC-consumer projections, and map to the honest
//     "unknown" lowercase value in the /api/health projection.
//
// SCOPE NOTE: this file owns the DISPLAY-state vocabulary only. The lower-level
// tracker-internal vocabulary (internal/cli/supervisor_runtime_tracker.go:
// runtimeStateFromSupervisorState / supervisorStateFromRuntimeState) is a
// SEPARATE concern (the supervisor's own persisted state machine) and is
// intentionally out of scope.

// DaemonDisplayState is the canonical, projection-independent daemon state
// vocabulary. Every known operator-visible daemon condition has exactly one
// value here; the three wire/display forms are PROJECTIONS of these values.
type DaemonDisplayState int

const (
	// DaemonDisplayUnknown is the catch-all for vocabulary none of the
	// classifiers recognize. It carries the original raw input so the
	// passthrough-style projections (producer + IPC consumer) can return it
	// verbatim, exactly as the legacy `default: return raw/state` cases did.
	DaemonDisplayUnknown DaemonDisplayState = iota
	// DaemonDisplayRunning — the daemon process is live and serving.
	DaemonDisplayRunning
	// DaemonDisplayIdle — no live process AND no record of one having run
	// (the producer's "" / fresh-entry case). Distinct from Stopped.
	DaemonDisplayIdle
	// DaemonDisplayStopped — the daemon is not running but was tracked
	// (the producer's "idle" case; the IPC consumer's "idle"/"stopped" case).
	DaemonDisplayStopped
	// DaemonDisplayRestarting — the supervisor is actively recovering the
	// daemon (backoff / backoff-waiting / spawning / port-stale wedge).
	DaemonDisplayRestarting
	// DaemonDisplayQuarantined — the supervisor PERMANENTLY gave up after a
	// crash-loop / 4-strike quarantine. A real hard failure. ENUMERATED
	// EXPLICITLY here so it can never silently fall to a passthrough default.
	DaemonDisplayQuarantined
)

// classifyTrackerRuntimeState maps the tracker's raw-runtime vocabulary (the
// lowercase strings the supervisor runtime tracker emits) to a canonical
// display state. It lowercase+trims first, matching the legacy producer.
//
// On an unrecognized input it returns (DaemonDisplayUnknown, false) so the
// caller can fall back to verbatim passthrough — preserving the legacy
// `default: return raw` behavior with the ORIGINAL (un-lowercased) casing.
func classifyTrackerRuntimeState(raw string) (DaemonDisplayState, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "running":
		return DaemonDisplayRunning, true
	case "idle":
		return DaemonDisplayStopped, true
	case "backoff", "backoff-waiting", "spawning":
		return DaemonDisplayRestarting, true
	case "quarantine", "quarantined":
		return DaemonDisplayQuarantined, true
	case "":
		return DaemonDisplayIdle, true
	default:
		return DaemonDisplayUnknown, false
	}
}

// classifyIPCWireState maps the Title-case GUI/IPC-wire vocabulary (what the
// producer projection emits, possibly re-fed) to a canonical display state.
//
// IMPORTANT byte-for-byte fidelity to the legacy normalizeSupervisorIPCStatusState:
// the legacy mapper lowercase+trimmed first and recognized ONLY these inputs —
//   - running                                  -> Running
//   - idle, stopped                            -> Stopped
//   - backoff, backoff-waiting, restarting     -> Restarting
//
// EVERYTHING ELSE (including the lowercase "spawning", "quarantine",
// "quarantined" tracker words, and the Title-case "Spawning") fell to
// `default: return state` and passed through VERBATIM in its ORIGINAL casing.
// Reproducing that exactly is mandatory — the golden test pins it.
//
// The ONE intended structural change (NOT an output change) is enumerating the
// Title-case "Quarantined" the producer actually emits on the wire, so the
// operator's main failure signal no longer depends on the passthrough default.
// That is matched CASE-SENSITIVELY on the exact "Quarantined" string BEFORE the
// lowercase switch, so its output stays byte-identical to the legacy passthrough
// ("Quarantined" -> "Quarantined") while the lowercase "quarantine"/"quarantined"
// tracker variants keep passing through verbatim exactly as before.
//
// On an unrecognized input it returns (DaemonDisplayUnknown, false) so the
// caller can fall back to verbatim passthrough with ORIGINAL casing.
func classifyIPCWireState(state string) (DaemonDisplayState, bool) {
	// Enumerate the producer's actual Title-case wire word for the quarantine
	// terminal state — matched exactly (not lowercased) so lowercase variants
	// keep their legacy verbatim-passthrough output. This is what closes the
	// latent fail-quiet trap without altering any output.
	if state == "Quarantined" {
		return DaemonDisplayQuarantined, true
	}
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running":
		return DaemonDisplayRunning, true
	case "idle", "stopped":
		return DaemonDisplayStopped, true
	case "backoff", "backoff-waiting", "restarting":
		return DaemonDisplayRestarting, true
	default:
		// Matches the legacy `default: return state`: lowercase "spawning",
		// "quarantine", "quarantined", Title-case "Spawning", "", garbage, etc.
		// all pass through verbatim in their ORIGINAL casing.
		return DaemonDisplayUnknown, false
	}
}

// ProjectGUIState reproduces internal/cli/supervise_status.go's
// supervisorStatusGUIState EXACTLY: the producer projection (tracker raw ->
// Title-case GUI/IPC-wire state). Unrecognized inputs pass through verbatim in
// their ORIGINAL casing.
func ProjectGUIState(raw string) string {
	state, ok := classifyTrackerRuntimeState(raw)
	if !ok {
		return raw
	}
	switch state {
	case DaemonDisplayRunning:
		return "Running"
	case DaemonDisplayRestarting:
		return "Restarting"
	case DaemonDisplayQuarantined:
		return "Quarantined"
	case DaemonDisplayStopped:
		// The producer's "idle" raw input is the ONLY recognized input that
		// resolves to Stopped here.
		return "Stopped"
	case DaemonDisplayIdle:
		return "Idle"
	default:
		// Unreachable: classifyTrackerRuntimeState only returns ok=true for
		// the cases above. Fail safe to verbatim rather than inventing output.
		return raw
	}
}

// ProjectIPCStatusState reproduces internal/api/supervisor_ipc_status_client.go's
// normalizeSupervisorIPCStatusState EXACTLY: the IPC-consumer projection
// (Title-case wire -> /api/status display form). Unrecognized inputs pass
// through verbatim in their ORIGINAL casing.
func ProjectIPCStatusState(state string) string {
	canonical, ok := classifyIPCWireState(state)
	if !ok {
		return state
	}
	switch canonical {
	case DaemonDisplayRunning:
		return "Running"
	case DaemonDisplayStopped:
		return "Stopped"
	case DaemonDisplayRestarting:
		return "Restarting"
	case DaemonDisplayQuarantined:
		// Enumerated explicitly (closing the latent trap). The OUTPUT stays
		// "Quarantined" — byte-identical to the legacy verbatim passthrough.
		return "Quarantined"
	default:
		// Unreachable for ok=true; fail safe to verbatim.
		return state
	}
}

// ProjectHealthWireState reproduces internal/api/health.go's normalizeDaemonState
// EXACTLY: the /api/health projection (Title-case state -> lowercase wire enum).
//
// This projection is CASE-SENSITIVE (it does NOT lowercase first) — only the
// exact Title-case vocabulary the IPC consumer / producer emit is recognized.
// Genuinely-unrecognized/blank inputs map to the honest "unknown" lowercase
// value, NEVER to the misleading "failed" (the false negative Workstream B
// removed). Known degraded states map to "starting"; known terminal failures
// (Failed, Quarantined) map to "failed" so a monitor on state=="failed" keeps
// firing.
//
// Because it is case-sensitive, it does NOT go through the lowercase-first
// canonical classifiers above; instead it switches on the exact Title-case
// canonical vocabulary directly. The canonical DaemonDisplayState values are
// the shared meaning, so this stays consistent with the other two projections
// (e.g. Quarantined is enumerated here too, never a silent default).
func ProjectHealthWireState(s string) string {
	switch s {
	case "Running":
		return "running"
	case "Starting", "Restarting", "Backoff", "Spawning":
		return "starting"
	case "Failed", "Quarantined":
		return "failed"
	case "Ready", "Scheduled", "Stopped":
		return "stopped"
	default:
		// Honest classification: an unrecognized (or blank) source state is
		// "unknown", NOT "failed". KNOWN degraded/terminal supervisor states
		// are handled above so they never silently fall to "unknown"
		// (fail-quiet weakening). See the historical rationale that lived on
		// normalizeDaemonState (Workstream B §3.1 + PR #281 review P2).
		return "unknown"
	}
}
