package oneapirun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
)

const (
	asyncStateAccepted        = "accepted"
	asyncStateRunning         = "running"
	asyncStateCancelRequested = "cancel_requested"
	asyncStateCompleted       = "completed"
	asyncStateFailed          = "failed"
	asyncStateTimedOut        = "timed_out"
	asyncStateCancelled       = "cancelled"
	asyncStateInterrupted     = "interrupted"
	asyncRunSnapshotVersion   = 1
	maxAsyncTerminalRuns      = 64
	// This owner retains up to 64 terminal receipts, each with two capped
	// streams. It owns this explicit ceiling; the API reader only enforces it.
	maxAsyncPersistedStateBytes  int64 = 64 << 20
	maxLegacyExpandedStreamBytes       = 3*maxStreamBytes + len(truncationMarker)
)

var (
	errAsyncInvalidRequest      = errors.New("oneapi_run_invalid_request")
	errAsyncBusy                = errors.New("oneapi_run_busy")
	errAsyncNotFound            = errors.New("oneapi_run_not_found")
	errAsyncIdempotencyConflict = errors.New("oneapi_run_idempotency_conflict")
	errAsyncPersist             = errors.New("oneapi_run_persist_failed")
	errAsyncTerminalPersist     = errors.New("oneapi_run_terminal_persist_failed")
	errAsyncRecoveryUnsettled   = errors.New("oneapi_run_recovery_unsettled")
	errAsyncOwnerClosed         = errors.New("oneapi_run_server_shutdown")
	errAsyncOwnerBusy           = errors.New("oneapi_run_owner_busy")
)

// oneAPIRunRequest is runtime-only execution input. Its fixed canonical digest
// is the only request identity persisted with an accepted asynchronous run.
type oneAPIRunRequest struct {
	Command        string
	Args           []string
	Cwd            string
	Timeout        time.Duration
	IdempotencyKey string
}

type asyncExecution struct {
	Result    runResult
	FailureID string
	Err       error
}

type asyncRunRecord struct {
	RunID          string     `json:"run_id"`
	IdempotencyKey string     `json:"idempotency_key"`
	RequestDigest  string     `json:"request_digest"`
	State          string     `json:"state"`
	AcceptedAt     time.Time  `json:"accepted_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	FinishSeq      uint64     `json:"finish_seq,omitempty"`
	FailureID      string     `json:"failure_id,omitempty"`
	Result         *runResult `json:"result,omitempty"`

	// These runtime-only fields are deliberately never persisted. A restart
	// therefore cannot attach to an old process or reuse its cancellation port.
	request    oneAPIRunRequest   `json:"-"`
	cancel     context.CancelFunc `json:"-"`
	done       chan struct{}      `json:"-"`
	workerDone chan struct{}      `json:"-"`
	unsettled  bool               `json:"-"`
	// recoveryUnsettled means a nonterminal persisted receipt was reloaded
	// without a runtime handle that can prove its prior process settled.
	recoveryUnsettled bool  `json:"-"`
	persistErr        error `json:"-"`
	// Legacy expansion is set only while migrating an old JSON text stream
	// whose replacement runes expanded beyond the normal raw-capture bound.
	stdoutLegacyExpanded bool `json:"-"`
	stderrLegacyExpanded bool `json:"-"`
}

func (r *asyncRunRecord) terminal() bool {
	switch r.State {
	case asyncStateCompleted, asyncStateFailed, asyncStateTimedOut, asyncStateCancelled, asyncStateInterrupted:
		return true
	default:
		return false
	}
}

type asyncRunSnapshot struct {
	Version          int              `json:"version"`
	TerminalSequence uint64           `json:"terminal_sequence"`
	Runs             []asyncRunRecord `json:"runs"`
}

// asyncPersistedRunResult is the durable-only representation. Captured
// process output is arbitrary bytes, while JSON strings must be UTF-8 and can
// expand control or invalid bytes. Base64 keeps the exact captured byte count
// and truncation marker stable across persistence and reload.
type asyncPersistedRunResult struct {
	ExitCode             int    `json:"exit_code"`
	Stdout               string `json:"stdout,omitempty"`
	StdoutBase64         string `json:"stdout_base64,omitempty"`
	StdoutLegacyExpanded bool   `json:"stdout_legacy_expanded,omitempty"`
	Stderr               string `json:"stderr,omitempty"`
	StderrBase64         string `json:"stderr_base64,omitempty"`
	StderrLegacyExpanded bool   `json:"stderr_legacy_expanded,omitempty"`
	EnvSource            string `json:"env_source"`
	DurationMs           int64  `json:"duration_ms"`
	TimedOut             bool   `json:"timed_out,omitempty"`
}

type asyncPersistedRunRecord struct {
	RunID          string                   `json:"run_id"`
	IdempotencyKey string                   `json:"idempotency_key"`
	RequestDigest  string                   `json:"request_digest"`
	State          string                   `json:"state"`
	AcceptedAt     time.Time                `json:"accepted_at"`
	StartedAt      *time.Time               `json:"started_at,omitempty"`
	FinishedAt     *time.Time               `json:"finished_at,omitempty"`
	FinishSeq      uint64                   `json:"finish_seq,omitempty"`
	FailureID      string                   `json:"failure_id,omitempty"`
	Result         *asyncPersistedRunResult `json:"result,omitempty"`
}

type asyncPersistedRunSnapshot struct {
	Version          int                       `json:"version"`
	TerminalSequence uint64                    `json:"terminal_sequence"`
	Runs             []asyncPersistedRunRecord `json:"runs"`
}

type asyncOwnerOptions struct {
	StatePath string
	Execute   func(context.Context, oneAPIRunRequest) asyncExecution
	Now       func() time.Time
	NextID    func() string
	Read      func(string) ([]byte, error)
	Write     func(string, []byte) error
	NewLease  func(string) asyncRunLease
	// BeforeWorkerDone is an owner-instance test seam for the terminal-to-done
	// window. Production leaves it nil.
	BeforeWorkerDone func()
}

type asyncRunLease interface {
	TryLock() (bool, error)
	Unlock() error
}

type asyncStartReceipt struct {
	Action   string `json:"action"`
	RunID    string `json:"run_id"`
	State    string `json:"state"`
	Terminal bool   `json:"terminal"`
	Replayed bool   `json:"replayed"`
}

type asyncStatusReceipt struct {
	Action     string     `json:"action"`
	RunID      string     `json:"run_id"`
	State      string     `json:"state"`
	Terminal   bool       `json:"terminal"`
	AcceptedAt time.Time  `json:"accepted_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	FailureID  string     `json:"failure_id,omitempty"`
}

type asyncResultReceipt struct {
	Action    string     `json:"action"`
	RunID     string     `json:"run_id"`
	State     string     `json:"state"`
	Terminal  bool       `json:"terminal"`
	Available bool       `json:"available"`
	Result    *runResult `json:"result,omitempty"`
}

// asyncRunOwner is the single owner of both the in-memory index and the v1
// receipt snapshot. Its mutex is intentionally held while persisting the
// candidate, so readers cannot observe a state that was not durably committed.
type asyncRunOwner struct {
	mu               sync.Mutex
	ctx              context.Context
	cancel           context.CancelFunc
	closed           bool
	closing          bool
	closeDone        chan struct{}
	closeErr         error
	lease            asyncRunLease
	workerGroup      sync.WaitGroup
	runs             map[string]*asyncRunRecord
	keyToRun         map[string]string
	seq              uint64
	statePath        string
	execute          func(context.Context, oneAPIRunRequest) asyncExecution
	now              func() time.Time
	nextID           func() string
	read             func(string) ([]byte, error)
	write            func(string, []byte) error
	beforeWorkerDone func()
}

func newAsyncRunOwner(parent context.Context, options asyncOwnerOptions) (*asyncRunOwner, error) {
	if options.StatePath == "" || options.Execute == nil {
		return nil, fmt.Errorf("oneapi async owner: %w", errAsyncInvalidRequest)
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NextID == nil {
		var next uint64
		options.NextID = func() string {
			next++
			return fmt.Sprintf("oneapi-%d-%d", options.Now().UTC().UnixNano(), next)
		}
	}
	if options.Read == nil {
		options.Read = func(path string) ([]byte, error) {
			return api.ReadStateFileInodeAnchoredWithMaxBytes(path, maxAsyncPersistedStateBytes)
		}
	}
	if options.Write == nil {
		options.Write = api.WriteStateFileBytesAtomic
	}
	if options.NewLease == nil {
		options.NewLease = func(path string) asyncRunLease { return flock.New(path) }
	}
	ctx, cancel := context.WithCancel(parent)
	if err := os.MkdirAll(filepath.Dir(options.StatePath), 0o700); err != nil {
		cancel()
		return nil, fmt.Errorf("oneapi async lease directory: %w", err)
	}
	lease := options.NewLease(filepath.Join(filepath.Dir(options.StatePath), "runs-v1.owner.lock"))
	if lease == nil {
		cancel()
		return nil, fmt.Errorf("oneapi async lease: %w", errAsyncInvalidRequest)
	}
	locked, err := lease.TryLock()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("oneapi async lease: %w", err)
	}
	if !locked {
		cancel()
		return nil, errAsyncOwnerBusy
	}
	o := &asyncRunOwner{
		ctx: ctx, cancel: cancel, runs: make(map[string]*asyncRunRecord), keyToRun: make(map[string]string),
		statePath: options.StatePath, execute: options.Execute, now: options.Now, nextID: options.NextID,
		read: options.Read, write: options.Write, lease: lease, beforeWorkerDone: options.BeforeWorkerDone,
	}
	if err := o.load(); err != nil {
		cancel()
		return nil, errors.Join(err, lease.Unlock())
	}
	return o, nil
}

func (o *asyncRunOwner) load() error {
	raw, err := o.read(o.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("oneapi async state read: %w", err)
	}
	var snapshot asyncPersistedRunSnapshot
	decoder := json.NewDecoder(bytesReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return fmt.Errorf("oneapi async state decode: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("oneapi async state trailing data: %w", err)
	}
	if snapshot.Version != asyncRunSnapshotVersion {
		return fmt.Errorf("oneapi async state version: %w", errAsyncInvalidRequest)
	}
	legacyText := false
	for i := range snapshot.Runs {
		record, usedLegacyText, err := asyncRecordFromPersisted(snapshot.Runs[i])
		if err != nil {
			return fmt.Errorf("oneapi async state record: %w", err)
		}
		legacyText = legacyText || usedLegacyText
		if err := validateAsyncRecord(record); err != nil {
			return fmt.Errorf("oneapi async state record: %w", err)
		}
		if _, exists := o.runs[record.RunID]; exists {
			return fmt.Errorf("oneapi async state duplicate run: %w", errAsyncInvalidRequest)
		}
		if _, exists := o.keyToRun[record.IdempotencyKey]; exists {
			return fmt.Errorf("oneapi async state duplicate key: %w", errAsyncInvalidRequest)
		}
		o.runs[record.RunID] = record
		o.keyToRun[record.IdempotencyKey] = record.RunID
		if record.FinishSeq > o.seq {
			o.seq = record.FinishSeq
		}
	}
	if snapshot.TerminalSequence > o.seq {
		o.seq = snapshot.TerminalSequence
	}

	changed := legacyText
	beforeEviction := len(o.runs)
	evictAsyncTerminals(o.runs)
	if len(o.runs) != beforeEviction {
		o.rebuildKeysLocked()
		changed = true
	}
	for _, record := range o.runs {
		switch record.State {
		case asyncStateAccepted, asyncStateRunning, asyncStateCancelRequested:
			record.recoveryUnsettled = true
		}
	}
	if changed {
		if err := o.persistLocked(o.runs, o.seq); err != nil {
			return fmt.Errorf("oneapi async state recovery: %w", err)
		}
	}
	return nil
}

func (o *asyncRunOwner) Start(request oneAPIRunRequest) (asyncStartReceipt, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := validateAsyncRequest(request); err != nil {
		return asyncStartReceipt{}, err
	}
	digest, err := asyncRequestDigest(request)
	if err != nil {
		return asyncStartReceipt{}, err
	}
	if o.closed {
		return asyncStartReceipt{}, errAsyncOwnerClosed
	}
	if runID, ok := o.keyToRun[request.IdempotencyKey]; ok {
		record := o.runs[runID]
		if record != nil && record.RequestDigest == digest {
			return asyncStartReceipt{Action: "start", RunID: record.RunID, State: record.State, Terminal: record.terminal(), Replayed: true}, nil
		}
		return asyncStartReceipt{}, errAsyncIdempotencyConflict
	}
	for _, record := range o.runs {
		if !record.terminal() || record.unsettled {
			return asyncStartReceipt{}, errAsyncBusy
		}
	}
	runCtx, cancel := context.WithCancel(o.ctx)
	now := o.now().UTC()
	record := &asyncRunRecord{
		RunID: o.nextID(), IdempotencyKey: request.IdempotencyKey, RequestDigest: digest,
		State: asyncStateAccepted, AcceptedAt: now, request: cloneAsyncRequest(request),
		cancel: cancel, done: make(chan struct{}), workerDone: make(chan struct{}),
	}
	if record.RunID == "" || o.runs[record.RunID] != nil {
		cancel()
		return asyncStartReceipt{}, errAsyncPersist
	}
	candidate := cloneAsyncRunMap(o.runs)
	candidate[record.RunID] = cloneAsyncRecord(record)
	if err := o.persistLocked(candidate, o.seq); err != nil {
		cancel()
		return asyncStartReceipt{}, fmt.Errorf("%w: %v", errAsyncPersist, err)
	}
	o.runs[record.RunID] = record
	o.keyToRun[record.IdempotencyKey] = record.RunID
	o.workerGroup.Add(1)
	go o.runWorker(runCtx, record.RunID, record.workerDone)
	return asyncStartReceipt{Action: "start", RunID: record.RunID, State: asyncStateAccepted, Terminal: false, Replayed: false}, nil
}

func (o *asyncRunOwner) runWorker(runCtx context.Context, runID string, workerDone chan struct{}) {
	defer func() {
		if o.beforeWorkerDone != nil {
			o.beforeWorkerDone()
		}
		close(workerDone)
		o.workerGroup.Done()
	}()

	o.mu.Lock()
	record := o.runs[runID]
	if record == nil {
		o.mu.Unlock()
		return
	}
	if err := o.transitionRunningLocked(record); err != nil {
		record.unsettled = true
		record.persistErr = err
		o.mu.Unlock()
		return
	}
	request := cloneAsyncRequest(record.request)
	o.mu.Unlock()

	timedCtx, timeoutCancel := context.WithTimeout(runCtx, request.Timeout)
	execution := o.execute(timedCtx, request)
	timeoutCancel()

	o.mu.Lock()
	defer o.mu.Unlock()
	record = o.runs[runID]
	if record == nil {
		return
	}
	state, failureID := classifyAsyncExecution(record, runCtx, execution)
	if err := o.transitionTerminalLocked(record, state, failureID, execution.Result); err != nil {
		record.unsettled = true
		record.persistErr = err
	}
}

func (o *asyncRunOwner) transitionRunningLocked(record *asyncRunRecord) error {
	if record.State == asyncStateCancelRequested {
		return nil
	}
	next := cloneAsyncRecord(record)
	now := o.now().UTC()
	next.State = asyncStateRunning
	next.StartedAt = &now
	candidate := cloneAsyncRunMap(o.runs)
	candidate[next.RunID] = next
	if err := o.persistLocked(candidate, o.seq); err != nil {
		return fmt.Errorf("%w: %v", errAsyncPersist, err)
	}
	record.State = next.State
	record.StartedAt = next.StartedAt
	return nil
}

func (o *asyncRunOwner) transitionTerminalLocked(record *asyncRunRecord, state, failureID string, result runResult) error {
	next := cloneAsyncRecord(record)
	now := o.now().UTC()
	next.State = state
	next.FailureID = failureID
	next.Result = &result
	next.FinishedAt = &now
	next.FinishSeq = o.nextFinishSeq()
	candidate := cloneAsyncRunMap(o.runs)
	candidate[next.RunID] = next
	evictAsyncTerminals(candidate)
	if err := o.persistLocked(candidate, next.FinishSeq); err != nil {
		return fmt.Errorf("%w: %v", errAsyncTerminalPersist, err)
	}
	o.runs = candidate
	o.rebuildKeysLocked()
	if committed := o.runs[next.RunID]; committed != nil {
		committed.cancel = record.cancel
		committed.done = record.done
		committed.workerDone = record.workerDone
		close(committed.done)
	}
	o.seq = next.FinishSeq
	return nil
}

func (o *asyncRunOwner) Status(runID string) (asyncStatusReceipt, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	record, err := o.lookupLocked(runID)
	if err != nil {
		return asyncStatusReceipt{}, err
	}
	if record.unsettled {
		return asyncStatusReceipt{}, errAsyncTerminalPersist
	}
	return statusReceipt(record), nil
}

func (o *asyncRunOwner) Result(runID string) (asyncResultReceipt, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	record, err := o.lookupLocked(runID)
	if err != nil {
		return asyncResultReceipt{}, err
	}
	if record.unsettled {
		return asyncResultReceipt{}, errAsyncTerminalPersist
	}
	if record.recoveryUnsettled {
		return asyncResultReceipt{Action: "result", RunID: record.RunID, State: record.State}, nil
	}
	return asyncResultReceipt{Action: "result", RunID: record.RunID, State: record.State, Terminal: record.terminal(), Available: record.terminal(), Result: cloneRunResult(record.Result)}, nil
}

func (o *asyncRunOwner) Cancel(runID string, confirm bool) (asyncStatusReceipt, error) {
	if !confirm {
		return asyncStatusReceipt{}, errAsyncInvalidRequest
	}
	o.mu.Lock()
	record, err := o.lookupLocked(runID)
	if err != nil {
		o.mu.Unlock()
		return asyncStatusReceipt{}, err
	}
	if record.unsettled {
		o.mu.Unlock()
		return asyncStatusReceipt{}, errAsyncTerminalPersist
	}
	if record.recoveryUnsettled {
		o.mu.Unlock()
		return asyncStatusReceipt{}, errAsyncRecoveryUnsettled
	}
	if record.terminal() {
		receipt := cancelReceipt(record)
		o.mu.Unlock()
		return receipt, nil
	}
	if record.State != asyncStateCancelRequested {
		next := cloneAsyncRecord(record)
		next.State = asyncStateCancelRequested
		candidate := cloneAsyncRunMap(o.runs)
		candidate[next.RunID] = next
		if err := o.persistLocked(candidate, o.seq); err != nil {
			o.mu.Unlock()
			return asyncStatusReceipt{}, fmt.Errorf("%w: %v", errAsyncPersist, err)
		}
		record.State = next.State
	}
	cancel := record.cancel
	workerDone := record.workerDone
	o.mu.Unlock()
	cancel()
	<-workerDone
	o.mu.Lock()
	defer o.mu.Unlock()
	record, err = o.lookupLocked(runID)
	if err != nil {
		return asyncStatusReceipt{}, err
	}
	if record.unsettled {
		return asyncStatusReceipt{}, errAsyncTerminalPersist
	}
	return cancelReceipt(record), nil
}

// Close prevents new starts, cancels every active run, and waits for each
// contained worker to settle. A failed terminal write is surfaced to Run; it
// is never converted into a clean shutdown.
func (o *asyncRunOwner) Close() error {
	o.mu.Lock()
	if o.closing {
		done := o.closeDone
		o.mu.Unlock()
		<-done
		o.mu.Lock()
		err := o.closeErr
		o.mu.Unlock()
		return err
	}
	if o.closed && o.lease == nil {
		err := o.closeErr
		o.mu.Unlock()
		return err
	}
	o.closed = true
	o.closing = true
	o.closeDone = make(chan struct{})
	done := o.closeDone
	o.cancel()
	o.mu.Unlock()
	o.workerGroup.Wait()
	o.mu.Lock()
	var closeErr error
	for _, record := range o.runs {
		if record.unsettled {
			closeErr = errors.Join(closeErr, errAsyncTerminalPersist)
		}
		if record.recoveryUnsettled {
			closeErr = errors.Join(closeErr, errAsyncRecoveryUnsettled)
		}
	}
	lease := o.lease
	o.mu.Unlock()
	if lease != nil {
		if err := lease.Unlock(); err != nil {
			closeErr = errors.Join(closeErr, err)
		} else {
			o.mu.Lock()
			if o.lease == lease {
				o.lease = nil
			}
			o.mu.Unlock()
		}
	}
	o.mu.Lock()
	o.closeErr = closeErr
	o.closing = false
	close(done)
	o.mu.Unlock()
	return closeErr
}

func (o *asyncRunOwner) persistLocked(records map[string]*asyncRunRecord, sequence uint64) error {
	raw, err := marshalAsyncPersistedSnapshot(records, sequence, maxAsyncPersistedStateBytes)
	if err != nil {
		return err
	}
	return o.write(o.statePath, raw)
}

func marshalAsyncPersistedSnapshot(records map[string]*asyncRunRecord, sequence uint64, byteLimit int64) ([]byte, error) {
	if byteLimit <= 0 {
		return nil, errAsyncInvalidRequest
	}
	raw, err := json.MarshalIndent(asyncPersistedSnapshotFrom(records, sequence), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("oneapi async state marshal: %w", err)
	}
	if int64(len(raw)) > byteLimit {
		return nil, fmt.Errorf("oneapi async state exceeds persisted receipt bound: %w", errAsyncPersist)
	}
	return raw, nil
}

func asyncPersistedSnapshotFrom(records map[string]*asyncRunRecord, sequence uint64) asyncPersistedRunSnapshot {
	snapshot := asyncPersistedRunSnapshot{Version: asyncRunSnapshotVersion, TerminalSequence: sequence, Runs: make([]asyncPersistedRunRecord, 0, len(records))}
	for _, record := range records {
		snapshot.Runs = append(snapshot.Runs, asyncPersistedRecordFrom(record))
	}
	sort.Slice(snapshot.Runs, func(i, j int) bool { return snapshot.Runs[i].RunID < snapshot.Runs[j].RunID })
	return snapshot
}

func asyncPersistedRecordFrom(record *asyncRunRecord) asyncPersistedRunRecord {
	persisted := asyncPersistedRunRecord{
		RunID: record.RunID, IdempotencyKey: record.IdempotencyKey, RequestDigest: record.RequestDigest,
		State: record.State, AcceptedAt: record.AcceptedAt, StartedAt: record.StartedAt,
		FinishedAt: record.FinishedAt, FinishSeq: record.FinishSeq, FailureID: record.FailureID,
	}
	if record.Result != nil {
		persisted.Result = &asyncPersistedRunResult{
			ExitCode: record.Result.ExitCode, StdoutBase64: base64.StdEncoding.EncodeToString([]byte(record.Result.Stdout)),
			StdoutLegacyExpanded: record.stdoutLegacyExpanded,
			StderrBase64:         base64.StdEncoding.EncodeToString([]byte(record.Result.Stderr)), StderrLegacyExpanded: record.stderrLegacyExpanded,
			EnvSource:  record.Result.EnvSource,
			DurationMs: record.Result.DurationMs, TimedOut: record.Result.TimedOut,
		}
	}
	return persisted
}

func asyncRecordFromPersisted(persisted asyncPersistedRunRecord) (*asyncRunRecord, bool, error) {
	record := &asyncRunRecord{
		RunID: persisted.RunID, IdempotencyKey: persisted.IdempotencyKey, RequestDigest: persisted.RequestDigest,
		State: persisted.State, AcceptedAt: persisted.AcceptedAt, StartedAt: persisted.StartedAt,
		FinishedAt: persisted.FinishedAt, FinishSeq: persisted.FinishSeq, FailureID: persisted.FailureID,
	}
	if persisted.Result != nil {
		stdout, stdoutLegacyExpanded, stdoutLegacyText, err := decodeAsyncPersistedOutput(persisted.Result.Stdout, persisted.Result.StdoutBase64, persisted.Result.StdoutLegacyExpanded)
		if err != nil {
			return nil, false, err
		}
		stderr, stderrLegacyExpanded, stderrLegacyText, err := decodeAsyncPersistedOutput(persisted.Result.Stderr, persisted.Result.StderrBase64, persisted.Result.StderrLegacyExpanded)
		if err != nil {
			return nil, false, err
		}
		record.Result = &runResult{ExitCode: persisted.Result.ExitCode, Stdout: stdout, Stderr: stderr, EnvSource: persisted.Result.EnvSource, DurationMs: persisted.Result.DurationMs, TimedOut: persisted.Result.TimedOut}
		record.stdoutLegacyExpanded = stdoutLegacyExpanded
		record.stderrLegacyExpanded = stderrLegacyExpanded
		return record, stdoutLegacyText || stderrLegacyText, nil
	}
	return record, false, nil
}

func decodeAsyncPersistedOutput(legacyText, encoded string, persistedLegacyExpanded bool) (string, bool, bool, error) {
	if encoded == "" {
		if persistedLegacyExpanded {
			return "", false, false, errAsyncInvalidRequest
		}
		return legacyText, len(legacyText) > maxStreamBytes+len(truncationMarker), legacyText != "", nil
	}
	if legacyText != "" {
		return "", false, false, errAsyncInvalidRequest
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || base64.StdEncoding.EncodeToString(raw) != encoded {
		return "", false, false, errAsyncInvalidRequest
	}
	return string(raw), persistedLegacyExpanded, false, nil
}

func (o *asyncRunOwner) lookupLocked(runID string) (*asyncRunRecord, error) {
	if runID == "" {
		return nil, errAsyncInvalidRequest
	}
	record := o.runs[runID]
	if record == nil {
		return nil, errAsyncNotFound
	}
	return record, nil
}

func (o *asyncRunOwner) rebuildKeysLocked() {
	o.keyToRun = make(map[string]string, len(o.runs))
	for runID, record := range o.runs {
		o.keyToRun[record.IdempotencyKey] = runID
	}
}

func (o *asyncRunOwner) nextFinishSeq() uint64 { return o.seq + 1 }

func validateAsyncRequest(request oneAPIRunRequest) error {
	if request.Command == "" || request.Timeout <= 0 || request.Timeout%time.Second != 0 || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 || !utf8.ValidString(request.IdempotencyKey) {
		return errAsyncInvalidRequest
	}
	return nil
}

func asyncRequestDigest(request oneAPIRunRequest) (string, error) {
	if err := validateAsyncRequest(request); err != nil {
		return "", err
	}
	canonical := struct {
		Command    string   `json:"command"`
		Args       []string `json:"args"`
		Cwd        string   `json:"cwd"`
		TimeoutSec int      `json:"timeout_sec"`
	}{
		Command: request.Command, Args: append([]string{}, request.Args...), Cwd: request.Cwd,
		TimeoutSec: int(request.Timeout / time.Second),
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("oneapi async digest: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validateAsyncRecord(record *asyncRunRecord) error {
	if record == nil || record.RunID == "" || record.IdempotencyKey == "" || len(record.IdempotencyKey) > 128 || !utf8.ValidString(record.IdempotencyKey) || !validAsyncRequestDigest(record.RequestDigest) || !validAsyncState(record.State) {
		return errAsyncInvalidRequest
	}
	if record.terminal() && (record.FinishedAt == nil || record.FinishSeq == 0 || record.Result == nil) {
		return errAsyncInvalidRequest
	}
	if record.Result != nil && (!validAsyncPersistedStream(record.Result.Stdout, record.stdoutLegacyExpanded) || !validAsyncPersistedStream(record.Result.Stderr, record.stderrLegacyExpanded)) {
		return errAsyncInvalidRequest
	}
	return nil
}

func validAsyncPersistedStream(value string, legacyExpanded bool) bool {
	length := len(value)
	normalLimit := maxStreamBytes + len(truncationMarker)
	if legacyExpanded {
		return length > normalLimit && length <= maxLegacyExpandedStreamBytes
	}
	return length <= normalLimit
}

func validAsyncRequestDigest(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size
}

func validAsyncState(state string) bool {
	switch state {
	case asyncStateAccepted, asyncStateRunning, asyncStateCancelRequested, asyncStateCompleted, asyncStateFailed, asyncStateTimedOut, asyncStateCancelled, asyncStateInterrupted:
		return true
	}
	return false
}

func classifyAsyncExecution(record *asyncRunRecord, runCtx context.Context, execution asyncExecution) (string, string) {
	if record.State == asyncStateCancelRequested {
		return asyncStateCancelled, "oneapi_run_cancelled"
	}
	if errors.Is(runCtx.Err(), context.Canceled) {
		return asyncStateCancelled, "oneapi_run_server_shutdown"
	}
	if execution.Result.TimedOut || errors.Is(execution.Err, context.DeadlineExceeded) {
		return asyncStateTimedOut, ""
	}
	if execution.FailureID != "" {
		return asyncStateFailed, execution.FailureID
	}
	if execution.Err != nil {
		return asyncStateFailed, "oneapi_run_containment_failed"
	}
	return asyncStateCompleted, ""
}

func statusReceipt(record *asyncRunRecord) asyncStatusReceipt {
	receipt := asyncStatusReceipt{Action: "status", RunID: record.RunID, State: record.State, Terminal: record.terminal(), AcceptedAt: record.AcceptedAt, StartedAt: record.StartedAt, FinishedAt: record.FinishedAt, FailureID: record.FailureID}
	if record.recoveryUnsettled {
		receipt.FailureID = errAsyncRecoveryUnsettled.Error()
	}
	return receipt
}

func cancelReceipt(record *asyncRunRecord) asyncStatusReceipt {
	receipt := statusReceipt(record)
	receipt.Action = "cancel"
	return receipt
}

func cloneAsyncRunMap(in map[string]*asyncRunRecord) map[string]*asyncRunRecord {
	out := make(map[string]*asyncRunRecord, len(in))
	for id, record := range in {
		out[id] = cloneAsyncRecord(record)
	}
	return out
}

func cloneAsyncRecord(in *asyncRunRecord) *asyncRunRecord {
	if in == nil {
		return nil
	}
	out := *in
	out.Result = cloneRunResult(in.Result)
	return &out
}

func cloneAsyncRequest(in oneAPIRunRequest) oneAPIRunRequest {
	out := in
	out.Args = append([]string(nil), in.Args...)
	return out
}

func cloneRunResult(in *runResult) *runResult {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func evictAsyncTerminals(records map[string]*asyncRunRecord) {
	for terminalCount(records) > maxAsyncTerminalRuns {
		var oldest *asyncRunRecord
		for _, record := range records {
			if record.terminal() && (oldest == nil || record.FinishSeq < oldest.FinishSeq) {
				oldest = record
			}
		}
		if oldest == nil {
			return
		}
		delete(records, oldest.RunID)
	}
}

func terminalCount(records map[string]*asyncRunRecord) int {
	count := 0
	for _, record := range records {
		if record.terminal() {
			count++
		}
	}
	return count
}

// bytesReader is a narrow indirection that keeps the strict state decoder
// local to this owner without exposing a second state-file abstraction.
func bytesReader(raw []byte) *bytes.Reader { return bytes.NewReader(raw) }
