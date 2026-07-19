package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
)

const (
	handoffMarkerFileLeaf = "gui-restart.json"
	handoffMarkerVersion  = "3.1"
	// handoffRecordLockRetry is the flock re-poll cadence while acquiring the
	// record lock under a bounded RecordLock budget (matches the repo's
	// TryLockContext usage in daemon_intent.go / supervisor_events.go).
	handoffRecordLockRetry = 10 * time.Millisecond
)

// RestartDeadlines is the single clock and timeout policy for the restart-v3
// handoff. Store operations use Now instead of reading the wall clock directly.
type RestartDeadlines struct {
	Now func() time.Time
	// RecordLock bounds gui-restart.json.lock acquisition so a wedged holder
	// (e.g. a DACL-hang inside a state-file write) can never block ensure-alive
	// ticks, the endpoint handler, or a `mcphub gui` entrant forever — expiry
	// is a typed fail-closed error, not an unbounded wait.
	RecordLock  time.Duration
	Freshness   time.Duration
	Reservation time.Duration
	Proof       time.Duration
	Bind        time.Duration
	Quiesce     time.Duration
	Rollback    time.Duration
	Grace       time.Duration
}

// DefaultRestartDeadlines returns the production restart-v3 timing policy.
func DefaultRestartDeadlines() RestartDeadlines {
	return RestartDeadlines{
		Now:         time.Now,
		RecordLock:  5 * time.Second,
		Freshness:   3 * time.Minute,
		Reservation: 10 * time.Second,
		Proof:       10 * time.Second,
		Bind:        2 * time.Second,
		Quiesce:     5 * time.Second,
		Rollback:    5 * time.Second,
		Grace:       5 * time.Second,
	}
}

// HandoffRoute identifies whether the replacement listener keeps or changes
// the GUI port.
type HandoffRoute string

const (
	HandoffRouteSamePort   HandoffRoute = "same-port"
	HandoffRoutePortChange HandoffRoute = "port-change"
)

// HandoffPhase is one of the four durable v3.1 handoff decisions.
type HandoffPhase string

const (
	HandoffPhaseInProgress  HandoffPhase = "in-progress"
	HandoffPhaseReserved    HandoffPhase = "reserved"
	HandoffPhaseCommitted   HandoffPhase = "committed"
	HandoffPhaseInterrupted HandoffPhase = "interrupted"
)

// HandoffMarkerRecord is the complete v3.1 gui-restart.json wire shape.
type HandoffMarkerRecord struct {
	Version              string       `json:"version"`
	Generation           string       `json:"generation"`
	Sequence             uint64       `json:"sequence"`
	Phase                HandoffPhase `json:"phase"`
	Route                HandoffRoute `json:"route"`
	OldPort              int          `json:"old_port"`
	NewPort              int          `json:"new_port"`
	OldPID               int          `json:"old_pid"`
	ChildPID             int          `json:"child_pid"`
	DesignatedChildHash  string       `json:"designated_child_hash"`
	CreatedAt            time.Time    `json:"created_at"`
	UpdatedAt            time.Time    `json:"updated_at"`
	FreshUntil           time.Time    `json:"fresh_until"`
	ReservationExpiresAt time.Time    `json:"reservation_expires_at"`
	ReasonCode           string       `json:"reason_code"`
	OperatorAction       string       `json:"operator_action"`
}

// HandoffBegin supplies the owner facts recorded when a parent begins a new
// handoff generation while retaining the GUI single-instance lease.
type HandoffBegin struct {
	Generation string
	Route      HandoffRoute
	OldPort    int
	NewPort    int
	OldPID     int
}

// HandoffMarkerFailureID is a stable caller-facing failure discriminator.
type HandoffMarkerFailureID string

const (
	HandoffMarkerFailureRead  HandoffMarkerFailureID = "gui-restart-marker-read-failed"
	HandoffMarkerFailureWrite HandoffMarkerFailureID = "gui-restart-marker-write-failed"
	HandoffMarkerFailureCAS   HandoffMarkerFailureID = "gui-restart-marker-cas-mismatch"
)

var (
	ErrHandoffMarkerCASMismatch   = errors.New("handoff marker generation or sequence changed")
	ErrHandoffMarkerStateMismatch = errors.New("handoff marker is not in the operation's required phase")
)

// HandoffMarkerError reports a durable-marker failure without terminating the
// process. Every such error is fail-closed: callers must not authorize a
// handoff, recovery write, or operator command from the failed observation.
type HandoffMarkerError struct {
	Operation string
	FailureID HandoffMarkerFailureID
	Cause     error
}

func (e *HandoffMarkerError) Error() string {
	if e == nil {
		return "handoff marker error"
	}
	return fmt.Sprintf("handoff marker %s (%s): %v", e.Operation, e.FailureID, e.Cause)
}

func (e *HandoffMarkerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// FailClosed identifies the required caller policy for marker failures.
func (*HandoffMarkerError) FailClosed() bool { return true }

// HandoffMarkerStore is the sole owner of gui-restart.json and its sibling
// record lock. Its public operations express handoff intents rather than a
// caller-selected transition graph.
type HandoffMarkerStore struct {
	stateDir    string
	path        string
	stateDirErr error
	deadlines   RestartDeadlines
}

// NewHandoffMarkerStore binds a store to exactly one state directory. The
// marker path is always <state-dir>/gui-restart.json; the store never searches
// or falls back to a marker under another state root.
func NewHandoffMarkerStore(stateDir string, deadlines RestartDeadlines) *HandoffMarkerStore {
	s := &HandoffMarkerStore{deadlines: deadlines}
	if strings.TrimSpace(stateDir) == "" {
		s.stateDirErr = errors.New("state directory is empty")
		return s
	}
	abs, err := filepath.Abs(filepath.Clean(stateDir))
	if err != nil {
		s.stateDirErr = fmt.Errorf("resolve absolute state directory: %w", err)
		return s
	}
	s.stateDir = abs
	s.path = filepath.Join(abs, handoffMarkerFileLeaf)
	return s
}

// Read returns nil,nil when no marker exists. Unknown versions, malformed
// schema, unsafe paths, and I/O failures return no record plus a typed error.
func (s *HandoffMarkerStore) Read() (*HandoffMarkerRecord, error) {
	var record *HandoffMarkerRecord
	err := s.withRecordLock("read", HandoffMarkerFailureRead, func() error {
		var err error
		record, err = s.readLockHeld()
		return err
	})
	if err != nil {
		return nil, err
	}
	return record, nil
}

// Begin replaces any prior marker with a new in-progress generation.
func (s *HandoffMarkerStore) Begin(begin HandoffBegin) (*HandoffMarkerRecord, error) {
	var written *HandoffMarkerRecord
	err := s.withRecordLock("begin", HandoffMarkerFailureWrite, func() error {
		// A new generation may replace a valid prior generation, but an unknown
		// or unreadable marker must remain fail-closed instead of being erased.
		if _, err := s.readLockHeld(); err != nil {
			return err
		}
		now, err := s.nowUTC()
		if err != nil {
			return err
		}
		record := &HandoffMarkerRecord{
			Version:    handoffMarkerVersion,
			Generation: strings.TrimSpace(begin.Generation),
			Sequence:   1,
			Phase:      HandoffPhaseInProgress,
			Route:      begin.Route,
			OldPort:    begin.OldPort,
			NewPort:    begin.NewPort,
			OldPID:     begin.OldPID,
			CreatedAt:  now,
			UpdatedAt:  now,
			FreshUntil: now.Add(s.deadlines.Freshness),
		}
		if err := validateHandoffMarker(record); err != nil {
			return err
		}
		if err := s.writeLockHeld(record); err != nil {
			return err
		}
		written = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return written, nil
}

// Reserve compare-and-swaps the matching in-progress generation and sequence
// to reserved while recording the designated child and protection deadline.
func (s *HandoffMarkerStore) Reserve(generation string, expectedSequence uint64, reservationExpiresAt time.Time, designatedChildHash string, childPID int) (*HandoffMarkerRecord, error) {
	return s.update("reserve", generation, &expectedSequence, func(record *HandoffMarkerRecord) error {
		if record.Phase != HandoffPhaseInProgress {
			return ErrHandoffMarkerStateMismatch
		}
		if reservationExpiresAt.IsZero() {
			return errors.New("reservation deadline is zero")
		}
		if strings.TrimSpace(designatedChildHash) == "" {
			return errors.New("designated child hash is empty")
		}
		record.Phase = HandoffPhaseReserved
		record.ReservationExpiresAt = reservationExpiresAt.UTC()
		record.DesignatedChildHash = designatedChildHash
		record.ChildPID = childPID
		return nil
	})
}

// Commit records that the matching reserved generation owns the flock and its
// full GUI is reachable on boundPort.
func (s *HandoffMarkerStore) Commit(generation string, boundPort int) (*HandoffMarkerRecord, error) {
	return s.update("commit", generation, nil, func(record *HandoffMarkerRecord) error {
		if record.Phase != HandoffPhaseReserved {
			return ErrHandoffMarkerStateMismatch
		}
		record.Phase = HandoffPhaseCommitted
		record.NewPort = boundPort
		return nil
	})
}

// Interrupt records a proved pre-release parent failure for the matching
// nonterminal generation.
func (s *HandoffMarkerStore) Interrupt(generation, reasonCode, operatorAction string) (*HandoffMarkerRecord, error) {
	return s.update("interrupt", generation, nil, func(record *HandoffMarkerRecord) error {
		if !record.Phase.nonterminal() {
			return ErrHandoffMarkerStateMismatch
		}
		return applyHandoffInterrupt(record, reasonCode, operatorAction)
	})
}

// InterruptFromOwnedFreeProbe compare-and-swaps the exact nonterminal record
// observed while the caller owns the free GUI single-instance probe.
func (s *HandoffMarkerStore) InterruptFromOwnedFreeProbe(generation string, expectedSequence uint64, reasonCode, operatorAction string) (*HandoffMarkerRecord, error) {
	return s.update("interrupt-from-owned-free-probe", generation, &expectedSequence, func(record *HandoffMarkerRecord) error {
		if !record.Phase.nonterminal() {
			return ErrHandoffMarkerStateMismatch
		}
		return applyHandoffInterrupt(record, reasonCode, operatorAction)
	})
}

// ClearAfterProvedPreReleaseRollback removes the matching in-progress marker
// after the parent has proved the original full GUI was restored.
func (s *HandoffMarkerStore) ClearAfterProvedPreReleaseRollback(generation string) error {
	return s.withRecordLock("clear-after-proved-pre-release-rollback", HandoffMarkerFailureWrite, func() error {
		record, err := s.readLockHeld()
		if err != nil {
			return err
		}
		if record == nil || record.Generation != generation {
			return ErrHandoffMarkerCASMismatch
		}
		if record.Phase != HandoffPhaseInProgress {
			return ErrHandoffMarkerStateMismatch
		}
		if err := os.Remove(s.path); err != nil {
			return fmt.Errorf("remove marker: %w", err)
		}
		return nil
	})
}

func (s *HandoffMarkerStore) update(operation, generation string, expectedSequence *uint64, mutate func(*HandoffMarkerRecord) error) (*HandoffMarkerRecord, error) {
	var written *HandoffMarkerRecord
	err := s.withRecordLock(operation, HandoffMarkerFailureWrite, func() error {
		record, err := s.readLockHeld()
		if err != nil {
			return err
		}
		if record == nil || record.Generation != generation {
			return ErrHandoffMarkerCASMismatch
		}
		if expectedSequence != nil && record.Sequence != *expectedSequence {
			return ErrHandoffMarkerCASMismatch
		}
		if err := mutate(record); err != nil {
			return err
		}
		now, err := s.nowUTC()
		if err != nil {
			return err
		}
		record.Sequence++
		record.UpdatedAt = now
		if err := validateHandoffMarker(record); err != nil {
			return err
		}
		if err := s.writeLockHeld(record); err != nil {
			return err
		}
		written = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return written, nil
}

func (s *HandoffMarkerStore) withRecordLock(operation string, failureID HandoffMarkerFailureID, fn func() error) error {
	if s == nil {
		return newHandoffMarkerError(operation, failureID, errors.New("store is nil"))
	}
	if s.stateDirErr != nil {
		return newHandoffMarkerError(operation, failureID, s.stateDirErr)
	}
	if s.deadlines.Now == nil {
		return newHandoffMarkerError(operation, failureID, errors.New("restart clock is nil"))
	}
	if s.deadlines.Freshness <= 0 {
		return newHandoffMarkerError(operation, failureID, errors.New("restart freshness deadline must be positive"))
	}
	if s.deadlines.RecordLock <= 0 {
		return newHandoffMarkerError(operation, failureID, errors.New("restart record-lock deadline must be positive"))
	}
	if err := os.MkdirAll(s.stateDir, 0o700); err != nil {
		return newHandoffMarkerError(operation, failureID, fmt.Errorf("create state directory: %w", err))
	}

	lockPath := s.path + ".lock"
	lock := flock.New(lockPath)
	// Bounded acquire: a wedged lock holder must fail closed after RecordLock,
	// not block this caller forever (Phase-I bounded-hold invariant).
	lockCtx, cancelLock := context.WithTimeout(context.Background(), s.deadlines.RecordLock)
	defer cancelLock()
	locked, lockErr := lock.TryLockContext(lockCtx, handoffRecordLockRetry)
	if lockErr != nil {
		return newHandoffMarkerError(operation, failureID, fmt.Errorf("acquire record lock %s: %w", lockPath, lockErr))
	}
	if !locked {
		return newHandoffMarkerError(operation, failureID, fmt.Errorf("acquire record lock %s: timed out after %s", lockPath, s.deadlines.RecordLock))
	}

	opErr := fn()
	unlockErr := lock.Unlock()
	if opErr != nil {
		return newHandoffMarkerError(operation, failureID, opErr)
	}
	if unlockErr != nil {
		return newHandoffMarkerError(operation, failureID, fmt.Errorf("release record lock %s: %w", lockPath, unlockErr))
	}
	return nil
}

func (s *HandoffMarkerStore) readLockHeld() (*HandoffMarkerRecord, error) {
	raw, err := api.ReadStateFileInodeAnchored(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var record HandoffMarkerRecord
	if err := decoder.Decode(&record); err != nil {
		return nil, fmt.Errorf("decode v3.1 marker: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	if err := validateHandoffMarker(&record); err != nil {
		return nil, err
	}
	return &record, nil
}

func (s *HandoffMarkerStore) writeLockHeld(record *HandoffMarkerRecord) error {
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal v3.1 marker: %w", err)
	}
	if err := api.WriteStateFileBytesLockHeld(s.path, raw); err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	return nil
}

func (s *HandoffMarkerStore) nowUTC() (time.Time, error) {
	if s.deadlines.Now == nil {
		return time.Time{}, errors.New("restart clock is nil")
	}
	return s.deadlines.Now().UTC(), nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode v3.1 marker: trailing JSON value")
		}
		return fmt.Errorf("decode v3.1 marker trailing data: %w", err)
	}
	return nil
}

func validateHandoffMarker(record *HandoffMarkerRecord) error {
	if record == nil {
		return errors.New("marker is nil")
	}
	if record.Version != handoffMarkerVersion {
		return fmt.Errorf("unknown marker version %q", record.Version)
	}
	if strings.TrimSpace(record.Generation) == "" {
		return errors.New("marker generation is empty")
	}
	if record.Sequence == 0 {
		return errors.New("marker sequence is zero")
	}
	if !record.Phase.valid() {
		return fmt.Errorf("unknown marker phase %q", record.Phase)
	}
	if !record.Route.valid() {
		return fmt.Errorf("unknown marker route %q", record.Route)
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.FreshUntil.IsZero() {
		return errors.New("marker timestamps are incomplete")
	}
	if record.UpdatedAt.Before(record.CreatedAt) {
		return errors.New("marker updated_at precedes created_at")
	}
	if record.FreshUntil.Before(record.CreatedAt) {
		return errors.New("marker fresh_until precedes created_at")
	}
	// Ports/PIDs are diagnostics only (never kill/spawn authority), but a
	// corrupt marker must still not round-trip nonsensical values.
	if record.OldPort < 0 || record.OldPort > 65535 || record.NewPort < 0 || record.NewPort > 65535 {
		return fmt.Errorf("marker port out of range (old_port=%d new_port=%d)", record.OldPort, record.NewPort)
	}
	if record.OldPID < 0 || record.ChildPID < 0 {
		return fmt.Errorf("marker pid negative (old_pid=%d child_pid=%d)", record.OldPID, record.ChildPID)
	}
	if record.Phase == HandoffPhaseReserved {
		if record.ReservationExpiresAt.IsZero() {
			return errors.New("reserved marker has no reservation_expires_at")
		}
		if strings.TrimSpace(record.DesignatedChildHash) == "" {
			return errors.New("reserved marker has no designated_child_hash")
		}
		if !record.ReservationExpiresAt.After(record.UpdatedAt) {
			return errors.New("reserved marker reservation_expires_at is not after updated_at")
		}
	}
	if record.Phase == HandoffPhaseInterrupted {
		if strings.TrimSpace(record.ReasonCode) == "" || strings.TrimSpace(record.OperatorAction) == "" {
			return errors.New("interrupted marker requires reason_code and operator_action")
		}
	}
	return nil
}

func (p HandoffPhase) valid() bool {
	switch p {
	case HandoffPhaseInProgress, HandoffPhaseReserved, HandoffPhaseCommitted, HandoffPhaseInterrupted:
		return true
	default:
		return false
	}
}

func (p HandoffPhase) nonterminal() bool {
	return p == HandoffPhaseInProgress || p == HandoffPhaseReserved
}

func (r HandoffRoute) valid() bool {
	return r == HandoffRouteSamePort || r == HandoffRoutePortChange
}

func applyHandoffInterrupt(record *HandoffMarkerRecord, reasonCode, operatorAction string) error {
	if strings.TrimSpace(reasonCode) == "" || strings.TrimSpace(operatorAction) == "" {
		return errors.New("interrupt requires reason_code and operator_action")
	}
	record.Phase = HandoffPhaseInterrupted
	record.ReasonCode = reasonCode
	record.OperatorAction = operatorAction
	return nil
}

func newHandoffMarkerError(operation string, failureID HandoffMarkerFailureID, cause error) error {
	var typed *HandoffMarkerError
	if errors.As(cause, &typed) {
		return cause
	}
	if errors.Is(cause, ErrHandoffMarkerCASMismatch) {
		failureID = HandoffMarkerFailureCAS
	}
	return &HandoffMarkerError{
		Operation: operation,
		FailureID: failureID,
		Cause:     cause,
	}
}
