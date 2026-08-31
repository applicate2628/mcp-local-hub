package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// SupervisorStateFile is the on-disk schema for <state-dir>/supervisor-state.json.
// Spec §"supervisor-state.json (NEW)".
type SupervisorStateFile struct {
	Version            int                              `json:"version"`
	Daemons            map[string]SupervisorDaemonState `json:"daemons"`
	TransientPIDs      []TransientPID                   `json:"transient_pids,omitempty"`
	MaintenanceFiredAt map[string]string                `json:"maintenance_fired_at,omitempty"`
	// StopSettlementEpoch is a fleet-wide, monotonically increasing fence for
	// durable stop transactions. It is deliberately separate from a daemon's
	// PIDGeneration: a task can be stopped repeatedly without spawning a new
	// generation, and each stop must still have a unique durable identity.
	StopSettlementEpoch uint64 `json:"stop_settlement_epoch,omitempty"`
	// StopSettlementMapGeneration and StopSettlementDigest bind the complete
	// durable receipt map. A restart treats a malformed or mismatched pair as
	// unavailable lifecycle state, never as permission to spawn over it.
	StopSettlementMapGeneration uint64 `json:"stop_settlement_map_generation,omitempty"`
	StopSettlementDigest        string `json:"stop_settlement_digest,omitempty"`
	// StopSettlements retains only non-terminal stop receipts. A successful
	// settlement removes its exact receipt as the final durable commit; a
	// failed/incomplete settlement remains present across supervisor restart and
	// blocks a new spawn until the controller settles that same generation.
	StopSettlements map[string]StopSettlementReceiptV1 `json:"stop_settlements,omitempty"`
}

// StopSettlementPhase is the durable stage of one exact daemon-stop attempt.
// It is diagnostic and recovery state, never a substitute for the runtime
// identity checks performed by the supervisor controller.
type StopSettlementPhase string

const (
	// StopRequested is durable before the controller receives the one batch
	// command.  There is deliberately no enqueue-only phase: a FIFO post is not
	// terminal lifecycle evidence.
	StopSettlementPhaseStopRequested StopSettlementPhase = "stop_requested"
	StopSettlementPhaseExitObserved  StopSettlementPhase = "exit_observed"
	StopSettlementPhasePortReleased  StopSettlementPhase = "port_released"
	StopSettlementPhaseFailed        StopSettlementPhase = "failed"
)

// StopSettlementFailureClass is the closed machine-readable reason for a
// failed durable stop settlement. FailureDetail is deliberately separate so
// retry policy never parses human/debug text.
type StopSettlementFailureClass string

const (
	StopSettlementFailureProcessAlive       StopSettlementFailureClass = "process_alive"
	StopSettlementFailureListenerAlive      StopSettlementFailureClass = "listener_alive"
	StopSettlementFailureIdentityUnverified StopSettlementFailureClass = "identity_unverified"
	StopSettlementFailureSettlementTimeout  StopSettlementFailureClass = "settlement_timeout"
	StopSettlementFailureSettlementCanceled StopSettlementFailureClass = "settlement_cancelled"
	StopSettlementFailureRuntimeReplaced    StopSettlementFailureClass = "runtime_generation_replaced"
	StopSettlementFailureTerminationFailed  StopSettlementFailureClass = "termination_failed"
	StopSettlementFailurePersistence        StopSettlementFailureClass = "persistence_failed"
)

func (c StopSettlementFailureClass) Valid() bool {
	switch c {
	case StopSettlementFailureProcessAlive,
		StopSettlementFailureListenerAlive,
		StopSettlementFailureIdentityUnverified,
		StopSettlementFailureSettlementTimeout,
		StopSettlementFailureSettlementCanceled,
		StopSettlementFailureRuntimeReplaced,
		StopSettlementFailureTerminationFailed,
		StopSettlementFailurePersistence:
		return true
	default:
		return false
	}
}

// StopSettlementReceiptV1 is the immutable identity token and mutable durable
// progress record for a stop transaction. TaskName, Epoch, PID, StartedAt and
// PIDGeneration are the token and must be compared as one unit before any
// transition or removal. Revision changes on each durable transition so a
// stale async completion cannot commit over a newer receipt.
type StopSettlementReceiptV1 struct {
	Version       int                        `json:"version"`
	BatchID       string                     `json:"batch_id"`
	TaskName      string                     `json:"task_name"`
	Epoch         uint64                     `json:"epoch"`
	PID           int                        `json:"pid"`
	StartedAt     string                     `json:"started_at"`
	PIDGeneration int                        `json:"pid_generation"`
	BatchIndex    int                        `json:"batch_index"`
	Mode          string                     `json:"mode"`
	Port          int                        `json:"port"`
	Revision      uint64                     `json:"revision"`
	Attempt       uint64                     `json:"attempt"`
	Phase         StopSettlementPhase        `json:"phase"`
	FailureClass  StopSettlementFailureClass `json:"failure_class,omitempty"`
	FailureDetail string                     `json:"failure_detail,omitempty"`
	ResumePhase   StopSettlementPhase        `json:"resume_phase,omitempty"`
	OperationID   string                     `json:"operation_id"`
}

// StopSettlementMapDigest returns a stable digest over the full receipt map
// and its generation. encoding/json orders map keys deterministically, so the
// same durable map yields the same digest across restart.
func StopSettlementMapDigest(epoch, generation uint64, rows map[string]StopSettlementReceiptV1) (string, error) {
	if rows == nil {
		rows = map[string]StopSettlementReceiptV1{}
	}
	payload, err := json.Marshal(struct {
		Epoch      uint64                             `json:"epoch"`
		Generation uint64                             `json:"generation"`
		Rows       map[string]StopSettlementReceiptV1 `json:"rows"`
	}{Epoch: epoch, Generation: generation, Rows: rows})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// StopSettlementReceiptDigest is the canonical compare-and-swap identity for a
// receipt revision. It intentionally covers every semantic field rather than
// relying on a hand-maintained subset: stale writers must not overwrite a
// newer attempt, phase, failure classification/detail, resume point, or
// operation identity that happens to retain the same revision number.
func StopSettlementReceiptDigest(receipt StopSettlementReceiptV1) (string, error) {
	payload, err := json.Marshal(struct {
		Version       int                        `json:"version"`
		BatchID       string                     `json:"batch_id"`
		TaskName      string                     `json:"task_name"`
		Epoch         uint64                     `json:"epoch"`
		PID           int                        `json:"pid"`
		StartedAt     string                     `json:"started_at"`
		PIDGeneration int                        `json:"pid_generation"`
		BatchIndex    int                        `json:"batch_index"`
		Mode          string                     `json:"mode"`
		Port          int                        `json:"port"`
		Revision      uint64                     `json:"revision"`
		Attempt       uint64                     `json:"attempt"`
		Phase         StopSettlementPhase        `json:"phase"`
		FailureClass  StopSettlementFailureClass `json:"failure_class"`
		FailureDetail string                     `json:"failure_detail"`
		ResumePhase   StopSettlementPhase        `json:"resume_phase"`
		OperationID   string                     `json:"operation_id"`
	}{
		Version: receipt.Version, BatchID: receipt.BatchID, TaskName: receipt.TaskName,
		Epoch: receipt.Epoch, PID: receipt.PID, StartedAt: receipt.StartedAt,
		PIDGeneration: receipt.PIDGeneration, BatchIndex: receipt.BatchIndex,
		Mode: receipt.Mode, Port: receipt.Port, Revision: receipt.Revision,
		Attempt: receipt.Attempt, Phase: receipt.Phase, FailureClass: receipt.FailureClass,
		FailureDetail: receipt.FailureDetail, ResumePhase: receipt.ResumePhase,
		OperationID: receipt.OperationID,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// StopSettlementDiagnosticV1 is the public, diagnostic-only projection of a
// non-terminal stop receipt. It carries the owner identity and an optional
// observed listener owner; it never authorizes a process operation.
type StopSettlementDiagnosticV1 struct {
	Version              int                 `json:"version"`
	TaskName             string              `json:"task_name"`
	Epoch                uint64              `json:"epoch"`
	OwnedPID             int                 `json:"owned_pid"`
	StartedAt            string              `json:"started_at"`
	PIDGeneration        int                 `json:"pid_generation"`
	Revision             uint64              `json:"revision"`
	Phase                StopSettlementPhase `json:"phase"`
	Failure              string              `json:"failure,omitempty"`
	ObservedPortOwnerPID int                 `json:"observed_port_owner_pid,omitempty"`
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

	// SpawnHoldReason / SpawnHoldPath record that the pre-spawn existence gate
	// (P1.1) is HOLDING this daemon because a path it needs is absent.
	// SpawnHoldReason is a stable id ("missing-binary" / "missing-workspace");
	// SpawnHoldPath is the exact absent path. Empty on the happy path.
	//
	// This is a HOLD, not a quarantine: no crash budget was consumed, the
	// supervisor re-probes on an armed timer, and the daemon starts by itself
	// the moment the path exists again. The pair exists so the cause and the
	// remedy reach a NON-TECHNICAL operator on the GUI Dashboard — the incident
	// that motivated it cost a working day precisely because the only signal was
	// a threshold message in a log file the person could not read.
	//
	// DECISION-INERT (mandatory). Nothing may read these fields to make a
	// restart, backoff, quarantine or spawn decision — the gate always re-probes
	// the filesystem and never consults persisted state. They are surfaced by
	// `mcphub status --json` and the GUI only. This preserves the threat model
	// documented in CLAUDE.md: a co-resident attacker who swaps
	// supervisor-state.json can at worst produce a wrong dashboard string, never
	// a control-flow change. A stale pair left by a cold restart is corrected by
	// the first gate pass for that daemon (which clears it).
	SpawnHoldReason string `json:"spawn_hold_reason,omitempty"`
	SpawnHoldPath   string `json:"spawn_hold_path,omitempty"`
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

// WithEmptyStopSettlementFence executes critical while the canonical
// supervisor-state flock is held, but only when the durable stop-settlement
// map is proven empty. It is deliberately read-only: rollback must neither
// repair a malformed receipt tuple nor rewrite a virgin state as a side effect
// of deciding whether a destructive successor kill is safe.
//
// An all-zero receipt tuple is the only virgin form. Once any receipt-map
// field is established, epoch and generation must both be positive and digest
// must be the lowercase SHA-256 of the canonical complete receipt map.
func WithEmptyStopSettlementFence(ctx context.Context, path string, critical func() error) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("empty supervisor state path")
	}
	if critical == nil {
		return fmt.Errorf("nil stop-settlement fence critical callback")
	}

	lockPath := path + ".lock"
	lk := flock.New(lockPath)
	if err := lockFlockContext(ctx, lk, lockPath, "supervisor-state"); err != nil {
		return err
	}
	defer func() {
		if unlockErr := lk.Unlock(); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("unlock supervisor-state fence %s: %w", lockPath, unlockErr))
		}
	}()

	file, err := ReadSupervisorState(path)
	if err != nil {
		return fmt.Errorf("read supervisor state under stop-settlement fence: %w", err)
	}
	if file == nil {
		return fmt.Errorf("read supervisor state under stop-settlement fence: nil state")
	}
	if file.Version != 1 {
		return fmt.Errorf("unsupported supervisor state version %d under stop-settlement fence", file.Version)
	}
	rows := file.StopSettlements
	if rows == nil {
		// Canonicalize only the in-memory comparison input. The fence is a
		// read-only admission check and must not materialize this map on disk.
		rows = map[string]StopSettlementReceiptV1{}
	}
	if err := validateEmptyStopSettlementFence(file, rows); err != nil {
		return err
	}
	return critical()
}

func validateEmptyStopSettlementFence(file *SupervisorStateFile, rows map[string]StopSettlementReceiptV1) error {
	virgin := file.StopSettlementEpoch == 0 &&
		file.StopSettlementMapGeneration == 0 &&
		file.StopSettlementDigest == ""
	if !virgin {
		if file.StopSettlementEpoch == 0 || file.StopSettlementMapGeneration == 0 {
			return fmt.Errorf("invalid stop-settlement fence tuple: established epoch and generation must both be positive")
		}
		if len(file.StopSettlementDigest) != sha256.Size*2 || file.StopSettlementDigest != strings.ToLower(file.StopSettlementDigest) {
			return fmt.Errorf("invalid stop-settlement fence digest shape")
		}
		decoded, decodeErr := hex.DecodeString(file.StopSettlementDigest)
		if decodeErr != nil || len(decoded) != sha256.Size {
			return fmt.Errorf("invalid stop-settlement fence digest encoding")
		}
		want, digestErr := StopSettlementMapDigest(file.StopSettlementEpoch, file.StopSettlementMapGeneration, rows)
		if digestErr != nil {
			return fmt.Errorf("digest stop-settlement fence rows: %w", digestErr)
		}
		if file.StopSettlementDigest != want {
			return fmt.Errorf("invalid stop-settlement fence digest")
		}
	}
	if len(rows) != 0 {
		return fmt.Errorf("pending stop settlement remains durable")
	}
	return nil
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
	return MutateSupervisorStateIfChangedContext(context.Background(), path, mutate)
}

// MutateSupervisorStateIfChangedContext is MutateSupervisorStateIfChanged with
// a context-aware flock acquire. The caller's mutate function runs while
// path+".lock" is held.
func MutateSupervisorStateIfChangedContext(ctx context.Context, path string, mutate func(*SupervisorStateFile) (bool, error)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("empty supervisor state path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir supervisor state dir: %w", err)
	}

	lockPath := path + ".lock"
	lk := flock.New(lockPath)
	if err := lockFlockContext(ctx, lk, lockPath, "supervisor-state"); err != nil {
		return err
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

const stateFileLockPollInterval = 25 * time.Millisecond

func lockFlockContext(ctx context.Context, lk *flock.Flock, lockPath, label string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		locked, err := lk.TryLock()
		if err != nil {
			return fmt.Errorf("%s flock %s: %w", label, lockPath, err)
		}
		if locked {
			return nil
		}
		timer := time.NewTimer(stateFileLockPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
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
	if file.StopSettlements == nil {
		file.StopSettlements = map[string]StopSettlementReceiptV1{}
	}
}
