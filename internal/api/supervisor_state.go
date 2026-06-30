package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
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
//
// Restart-policy runtime state (the 30-min crash sliding window, the
// active backoff deadline, the quarantine timestamp, and any queued
// post-exit action) is deliberately NOT persisted here: it lives only
// in-memory in DaemonRuntimeTracker (the sliding window in
// `failures map[string][]time.Time`, the backoff in a live time.Timer,
// the quarantine/queued-action in the SM's in-memory SMContext) and is
// RESET on every supervisor cold restart. That is the documented design
// intent — pre-restart crashes are not relevant to runtime respawn
// decisions, because a cold restart is an operator-initiated reset of
// runtime state (see DaemonRuntimeTracker.RecordCrashAndCountInWindow).
// Earlier revisions carried vestigial restart_history / backoff_until /
// quarantine_since / queued_action fields here, but no production path
// ever wrote a non-empty value into them; they were removed (2026-06-20
// supervisor audit P3) so the persisted schema matches what the code
// actually writes.
type SupervisorDaemonState struct {
	State         string `json:"state"` // durable persisted state: idle|running
	CurrentPID    int    `json:"current_pid"`
	PIDGeneration int    `json:"pid_generation"`
	StartedAt     string `json:"started_at,omitempty"`
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

// MutateSupervisorState serializes a supervisor-state.json read/modify/write
// under the same sibling flock used by WriteSupervisorState's final write.
func MutateSupervisorState(path string, mutate func(*SupervisorStateFile) error) error {
	return MutateSupervisorStateIfChanged(path, func(file *SupervisorStateFile) (bool, error) {
		if mutate == nil {
			return true, nil
		}
		if err := mutate(file); err != nil {
			return false, err
		}
		return true, nil
	})
}

// MutateSupervisorStateIfChanged is MutateSupervisorState with an explicit
// changed flag so callers can keep read-only/no-op cases from rewriting the
// state file. The caller's mutate function runs while path+".lock" is held.
func MutateSupervisorStateIfChanged(path string, mutate func(*SupervisorStateFile) (bool, error)) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("empty supervisor state path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir supervisor state dir: %w", err)
	}

	lockPath := path + ".lock"
	lk := flock.New(lockPath)
	if err := lk.Lock(); err != nil {
		return fmt.Errorf("supervisor-state flock %s: %w", lockPath, err)
	}
	defer func() { _ = lk.Unlock() }()

	file, err := ReadSupervisorState(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read existing supervisor state: %w", err)
		}
		file = &SupervisorStateFile{Version: 1}
	}
	normalizeSupervisorStateForMutation(file)
	changed := true
	if mutate != nil {
		changed, err = mutate(file)
		if err != nil {
			return err
		}
	}
	if !changed {
		return nil
	}
	normalizeSupervisorStateForMutation(file)
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal supervisor state: %w", err)
	}
	return WriteStateFileBytesLockHeld(path, raw)
}

func normalizeSupervisorStateForMutation(file *SupervisorStateFile) {
	if file == nil {
		return
	}
	if file.Version == 0 {
		file.Version = 1
	}
	if file.Daemons == nil {
		file.Daemons = map[string]SupervisorDaemonState{}
	}
}
