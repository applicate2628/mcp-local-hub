package api

import (
	"encoding/json"
	"fmt"
	"os"
)

// SupervisorStateFile is the on-disk schema for <state-dir>/supervisor-state.json.
// Spec §"supervisor-state.json (NEW)".
type SupervisorStateFile struct {
	Version            int                              `json:"version"`
	Daemons            map[string]SupervisorDaemonState `json:"daemons"`
	TransientPIDs      []TransientPID                   `json:"transient_pids,omitempty"`
	MaintenanceFiredAt map[string]string                `json:"maintenance_fired_at,omitempty"`
}

// SupervisorDaemonState is per-daemon Hybrid C state.
type SupervisorDaemonState struct {
	State           string         `json:"state"` // idle|spawning|running|exiting|backoff-waiting|quarantined
	CurrentPID      int            `json:"current_pid"`
	PIDGeneration   int            `json:"pid_generation"`
	StartedAt       string         `json:"started_at,omitempty"`
	RestartHistory  []RestartEvent `json:"restart_history,omitempty"`
	BackoffUntil    string         `json:"backoff_until,omitempty"`
	QuarantineSince string         `json:"quarantine_since,omitempty"`
	QueuedAction    *QueuedAction  `json:"queued_action,omitempty"`
	// OrphanPID records a Windows post-create orphan PID when the
	// supervisor's best-effort kill failed. Operator-visible via
	// supervisor-state.json AND IPC status response; SEPARATE from
	// CurrentPID because terminate-by-PID code reads CurrentPID and
	// conflating an orphan would silently nil-success the terminate
	// (StartedAt empty -> ErrProcessIdentityMismatch). Manual cleanup
	// on Windows: `taskkill /F /T /PID <OrphanPID>`.
	//
	// Closes bot finding on PR #238 fd51536 (P2 persist-the-preserved-
	// orphan-PID): in-memory tracker.OrphanPID without this serialized
	// field meant `mcphub status` and supervisor-state.json could not
	// surface it after supervisor restart.
	OrphanPID int `json:"orphan_pid,omitempty"`

	// JobProtection records whether the per-spawn Windows Job Object
	// (ADR #239) was successfully allocated for this daemon's CURRENT
	// spawn attempt. Tri-state via *bool with backward-compatible
	// default:
	//
	//   - nil       — unknown / legacy state file / not yet probed.
	//                 Operator UI treats this as "no warning" (default-
	//                 trust). Required to avoid retroactively marking
	//                 every legacy daemon as unprotected when older
	//                 supervisor-state.json files (without the field)
	//                 are read after upgrade. Codex deep-sec flagged
	//                 this as the load-bearing trap on PR #242
	//                 sequencing review (2026-05-28).
	//
	//   - &true     — supervisor successfully called
	//                 process.NewKillOnCloseJob and assigned the spawn
	//                 to the job. Orphan-cleanup invariant holds: the
	//                 daemon's descendant tree dies on supervisor exit
	//                 via KILL_ON_JOB_CLOSE; the supervisor's orphan
	//                 branch can task-scope kill via
	//                 daemonJob.TerminateAll.
	//
	//   - &false    — NewKillOnCloseJob failed (rare; typically AppLocker
	//                 / nested-job constraints / handle exhaustion on
	//                 restrictive corp-managed hosts) AND the supervisor
	//                 fell through to the documented non-fatal fallback:
	//                 plain cmd.Start without StartWithJob. The daemon
	//                 runs without Job Object orphan-protection — its
	//                 descendant tree may survive supervisor crash, and
	//                 the orphan-cleanup branch downgrades to per-PID
	//                 BestEffortKillByPID. Operator visibility is the
	//                 PR #242 raison d'être — consultant's #1 strategic
	//                 concern on PR #241 was that this branch leaves no
	//                 status-surface signal between the warn-event log
	//                 entry (post-incident only) and the actual daemon
	//                 state.
	JobProtection *bool `json:"job_protection,omitempty"`
}

// RestartEvent is one entry in the 30-min sliding window failure-count
// store. Pruned on every state persist to entries with at >= now-30m.
type RestartEvent struct {
	At       string `json:"at"`
	ExitCode int    `json:"exit_code"`
	Signal   *int   `json:"signal,omitempty"`
}

// QueuedAction records the pending side-effect after the current
// `exiting` transition completes. Cleared on transition out of exiting.
type QueuedAction struct {
	Kind   string `json:"kind"` // "respawn" | "none"
	Reason string `json:"reason"`
}

// TransientPID tracks maintenance-timer fire-and-forget children that
// are NOT Job Object members. Used by quiesce-timers IPC + graceful
// exit + cold-start reaper.
type TransientPID struct {
	PID       int    `json:"pid"`
	Kind      string `json:"kind"`
	StartedAt string `json:"started_at"`
}

// ReadSupervisorState reads + parses the supervisor's runtime state. Unknown
// fields are ignored deliberately: this file is rewritten by newer binaries,
// and a rollback must not brick supervisor startup just because it sees a
// future additive field. JSON type/shape errors still fail through Unmarshal.
func ReadSupervisorState(path string) (*SupervisorStateFile, error) {
	raw, err := readStateFileInodeAnchored(path)
	if err != nil {
		if isHubMcpStateMissingErr(err) {
			return nil, fmt.Errorf("read %s: %w", path, os.ErrNotExist)
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f SupervisorStateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return &f, nil
}

// WriteSupervisorState goes through WriteStateFileAtomic (Task 1.1).
func WriteSupervisorState(path string, f *SupervisorStateFile) error {
	return WriteStateFileAtomic(path, f)
}
