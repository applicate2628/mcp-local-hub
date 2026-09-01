package vtune

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/hubtemp"
	"mcp-local-hub/internal/process"
)

const vtuneRunSchemaV1 = "vtune-run-v1"

const (
	vtunePhaseTimeout     = 30 * time.Second
	vtuneArtifactSweepTTL = 24 * time.Hour
	maxDurableVTuneRuns   = 2
	maxDurableTimeoutSec  = 3600
)

var vtuneGenerationSnapshotName = regexp.MustCompile(`^[0-9]{8}\.json$`)

const (
	vtuneRunPrepared         = "prepared"
	vtuneRunCollecting       = "collecting"
	vtuneRunStopRequested    = "stop_requested"
	vtuneRunFinalizing       = "finalizing"
	vtuneRunReporting        = "reporting"
	vtuneRunCompleted        = "completed"
	vtuneRunCompletedNonzero = "completed_nonzero"
	vtuneRunStopped          = "stopped"
	vtuneRunFailed           = "failed"
	vtuneRunHostLost         = "host_lost"
)

const (
	failureCallerDeadlineTooShort = "VTUNE_CALLER_DEADLINE_TOO_SHORT"
	failureRunNotFound            = "RUN_NOT_FOUND"
	failureIdempotencyConflict    = "IDEMPOTENCY_CONFLICT"
	failureContainmentUnavailable = "CONTAINMENT_UNAVAILABLE"
	failureCollectStartFailed     = "COLLECT_START_FAILED"
	failureStopFailed             = "STOP_FAILED"
	failureStopSettlementTimeout  = "STOP_SETTLEMENT_TIMEOUT"
	failureFinalizeFailed         = "FINALIZE_FAILED"
	failureReportFailed           = "REPORT_FAILED"
	failureReportOutputMissing    = "REPORT_OUTPUT_MISSING"
	failureResultNonReportable    = "RESULT_NONREPORTABLE"
	failureCleanupFailed          = "CLEANUP_FAILED"
	failureOwnerRestarted         = "OWNER_RESTARTED"
	failureAdmissionLimited       = "ADMISSION_LIMITED"
	failureTimeoutOutOfRange      = "TIMEOUT_OUT_OF_RANGE"
)

var (
	errVTuneIdempotencyConflict = errors.New(failureIdempotencyConflict)
	errVTuneRunNotFound         = errors.New(failureRunNotFound)
	errVTuneAdmissionLimited    = errors.New(failureAdmissionLimited)
	errVTuneTimeoutOutOfRange   = errors.New(failureTimeoutOutOfRange)
)

// vtuneRunRequest is the durable, caller-independent collection request. It
// contains only the validated fields needed to resume status and cleanup; it
// never stores a caller context or transport handle.
type vtuneRunRequest struct {
	Target         string   `json:"target"`
	Args           []string `json:"args,omitempty"`
	Cwd            string   `json:"cwd,omitempty"`
	AnalysisType   string   `json:"analysis_type"`
	TimeoutSec     int      `json:"timeout_sec"`
	KeepResult     bool     `json:"keep_result"`
	IdempotencyKey string   `json:"idempotency_key,omitempty"`
	VTunePath      string   `json:"vtune_path"`
	RunID          string   `json:"run_id"`
	ResultDir      string   `json:"result_dir"`
}

type vtuneStopOperation struct {
	Disposition string    `json:"disposition"`
	At          time.Time `json:"at"`
}

type vtuneReportReceiptV1 struct {
	SchemaVersion string    `json:"schema_version"`
	RunID         string    `json:"run_id"`
	ReportPath    string    `json:"report_path"`
	ResultDir     string    `json:"result_dir"`
	ReportSHA256  string    `json:"report_sha256"`
	SummarySHA256 string    `json:"summary_sha256"`
	ExitCode      int       `json:"exit_code"`
	CreatedAt     time.Time `json:"created_at"`
}

// vtuneRunRecord is the one public snapshot shape. The immutable receipt
// hash is populated only after finalization and both reports are durable.
type vtuneRunRecord struct {
	SchemaVersion      string                        `json:"schema_version"`
	Generation         int                           `json:"generation"`
	RunID              string                        `json:"run_id"`
	State              string                        `json:"state"`
	Phase              string                        `json:"phase"`
	RequestDisposition string                        `json:"request_disposition,omitempty"`
	FailureID          string                        `json:"failure_id,omitempty"`
	Reportable         bool                          `json:"reportable"`
	StopReason         string                        `json:"stop_reason,omitempty"`
	PhaseExitCodes     map[string]int                `json:"phase_exit_codes,omitempty"`
	CreatedAt          time.Time                     `json:"created_at"`
	UpdatedAt          time.Time                     `json:"updated_at"`
	CompletedAt        *time.Time                    `json:"completed_at,omitempty"`
	ReceiptSHA256      string                        `json:"receipt_sha256,omitempty"`
	Quarantined        bool                          `json:"quarantined"`
	ExitCode           int                           `json:"exit_code"`
	Error              string                        `json:"error,omitempty"`
	Request            vtuneRunRequest               `json:"request"`
	Output             *runOutput                    `json:"output,omitempty"`
	StopOperations     map[string]vtuneStopOperation `json:"stop_operations,omitempty"`
}

func (r vtuneRunRecord) Terminal() bool {
	switch r.State {
	case vtuneRunCompleted, vtuneRunCompletedNonzero, vtuneRunStopped, vtuneRunFailed, vtuneRunHostLost:
		return true
	default:
		return false
	}
}

type vtunePhaseDriver interface {
	collect(context.Context, vtuneRunRequest) (*runOutput, error)
	stop(context.Context, vtuneRunRequest) error
	finalize(context.Context, vtuneRunRequest) error
	report(context.Context, vtuneRunRequest) (*runOutput, error)
	forceStop(context.Context, vtuneRunRequest) error
}

// vtuneRunOwnerV1 owns worker lifetime and the append-only on-disk snapshot
// store. No handler owns a worker: closing an MCP request cannot cancel one.
type vtuneRunOwnerV1 struct {
	mu                sync.Mutex
	root              string
	driver            vtunePhaseDriver
	runs              map[string]*vtuneRunRecord
	keys              map[string]string
	collectCancels    map[string]context.CancelFunc
	shutdownCancels   map[string]context.CancelFunc
	phaseActive       map[string]string
	done              map[string]chan struct{}
	settlementTimeout time.Duration
	storeLease        *flock.Flock
	unlockStoreLease  func(*flock.Flock) error
	leaseReleaseErr   error
	leaseStateChanged chan struct{}
	snapshotWriter    func(string, []byte) error
	storageFailed     map[string]bool
	durable           map[string]vtuneRunRecord
	wg                sync.WaitGroup
	closed            bool
}

func newVTuneRunOwnerV1(root string, driver vtunePhaseDriver) (*vtuneRunOwnerV1, error) {
	if driver == nil {
		return nil, fmt.Errorf("%s", failureContainmentUnavailable)
	}
	if err := hubtemp.EnsurePrivateDir(root); err != nil {
		return nil, fmt.Errorf("create VTune run store: %w", err)
	}
	lease := flock.New(filepath.Join(root, "durable-runs.lock"))
	locked, err := lease.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire VTune run store lease: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("%s: VTune run store already owned", failureContainmentUnavailable)
	}
	o := &vtuneRunOwnerV1{root: root, driver: driver, runs: map[string]*vtuneRunRecord{}, keys: map[string]string{}, collectCancels: map[string]context.CancelFunc{}, shutdownCancels: map[string]context.CancelFunc{}, phaseActive: map[string]string{}, done: map[string]chan struct{}{}, settlementTimeout: 30 * time.Second, storeLease: lease, unlockStoreLease: func(lease *flock.Flock) error { return lease.Unlock() }, leaseStateChanged: make(chan struct{}), snapshotWriter: atomicWrite, storageFailed: map[string]bool{}, durable: map[string]vtuneRunRecord{}}
	o.sweepArtifacts()
	if err := o.load(); err != nil {
		_ = lease.Unlock()
		return nil, err
	}
	return o, nil
}

func (o *vtuneRunOwnerV1) Start(req vtuneRunRequest) (vtuneRunRecord, string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return vtuneRunRecord{}, "", fmt.Errorf("VTune run owner is closed")
	}
	if req.TimeoutSec <= 0 || req.TimeoutSec > maxDurableTimeoutSec {
		return vtuneRunRecord{}, "", fmt.Errorf("%w: timeout_sec must be between 1 and %d", errVTuneTimeoutOutOfRange, maxDurableTimeoutSec)
	}
	fp := requestFingerprint(req)
	if req.IdempotencyKey != "" {
		if id, ok := o.keys[req.IdempotencyKey]; ok {
			existing := *o.runs[id]
			if requestFingerprint(existing.Request) != fp {
				return vtuneRunRecord{}, "", errVTuneIdempotencyConflict
			}
			return existing, "replayed", nil
		}
	}
	active := 0
	for _, run := range o.runs {
		if !run.Terminal() {
			active++
		}
	}
	if active >= maxDurableVTuneRuns {
		return vtuneRunRecord{}, "", fmt.Errorf("%w: at most %d durable VTune runs may be active", errVTuneAdmissionLimited, maxDurableVTuneRuns)
	}
	if req.RunID == "" {
		req.RunID = newVTuneRunID()
	}
	if req.ResultDir == "" {
		req.ResultDir = filepath.Join(o.root, "runs", req.RunID, "result")
	}
	now := time.Now().UTC()
	record := &vtuneRunRecord{SchemaVersion: vtuneRunSchemaV1, Generation: 1, RunID: req.RunID, State: vtuneRunPrepared, Phase: vtuneRunPrepared, RequestDisposition: "started", CreatedAt: now, UpdatedAt: now, PhaseExitCodes: map[string]int{}, Request: req, StopOperations: map[string]vtuneStopOperation{}}
	if err := o.persistLocked(record); err != nil {
		return vtuneRunRecord{}, "", err
	}
	o.durable[record.RunID] = cloneVTuneRunRecord(*record)
	o.runs[record.RunID] = record
	if req.IdempotencyKey != "" {
		o.keys[req.IdempotencyKey] = record.RunID
	}
	collectCtx, cancelCollect := context.WithTimeout(context.Background(), time.Duration(req.TimeoutSec)*time.Second)
	shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
	o.collectCancels[record.RunID] = cancelCollect
	o.shutdownCancels[record.RunID] = cancelShutdown
	o.done[record.RunID] = make(chan struct{})
	o.wg.Add(1)
	go o.worker(collectCtx, shutdownCtx, record.RunID)
	return *record, "started", nil
}

func (o *vtuneRunOwnerV1) Status(runID string) (vtuneRunRecord, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, ok := o.runs[runID]
	if !ok {
		return vtuneRunRecord{}, false
	}
	if o.storageFailed[runID] {
		out := cloneVTuneRunRecord(*r)
		out.State, out.Phase, out.FailureID, out.Error, out.Reportable, out.Quarantined, out.ExitCode = vtuneRunFailed, "failed", failureCleanupFailed, "durable VTune state write failed", false, true, -1
		return out, true
	}
	if r.Reportable && !o.validReceiptLocked(r) {
		if err := o.invalidateReceiptLocked(r); err != nil {
			return nonreportableVTuneSnapshot(*r), true
		}
	}
	return *r, true
}

func (o *vtuneRunOwnerV1) Stop(runID, operationID string, positive bool) (vtuneRunRecord, string, error) {
	if !positive {
		return vtuneRunRecord{}, "", fmt.Errorf("stop must be true")
	}
	if strings.TrimSpace(operationID) == "" {
		return vtuneRunRecord{}, "", fmt.Errorf("missing required parameter: operation_id")
	}
	o.mu.Lock()
	r, ok := o.runs[runID]
	if !ok {
		o.mu.Unlock()
		return vtuneRunRecord{}, "", errVTuneRunNotFound
	}
	if prior, ok := r.StopOperations[operationID]; ok {
		out := *r
		o.mu.Unlock()
		return out, prior.Disposition, nil
	}
	if r.Terminal() {
		r.StopOperations[operationID] = vtuneStopOperation{Disposition: "already_terminal", At: time.Now().UTC()}
		_ = o.advanceLocked(r)
		out := *r
		o.mu.Unlock()
		return out, "already_terminal", nil
	}
	if r.State == vtuneRunPrepared {
		r.State, r.Phase, r.StopReason, r.Reportable, r.Quarantined, r.ExitCode = vtuneRunStopped, vtuneRunStopped, "operator_request_before_collect", false, true, -1
		r.StopOperations[operationID] = vtuneStopOperation{Disposition: "stopped_before_collect", At: time.Now().UTC()}
		now := time.Now().UTC()
		r.CompletedAt = &now
		if err := o.removeUnretainedRawLocked(r); err != nil {
			r.State, r.Phase, r.FailureID, r.Error, r.Reportable, r.Quarantined, r.ExitCode = vtuneRunFailed, "failed", failureCleanupFailed, err.Error(), false, true, -1
			o.settleStopOperationsLocked(r, "failed")
		}
		if err := o.advanceLocked(r); err != nil {
			o.mu.Unlock()
			return vtuneRunRecord{}, "", err
		}
		out := *r
		disposition := r.StopOperations[operationID].Disposition
		o.mu.Unlock()
		return out, disposition, nil
	}
	if r.State == vtuneRunFinalizing || r.State == vtuneRunReporting {
		r.StopOperations[operationID] = vtuneStopOperation{Disposition: "already_settling", At: time.Now().UTC()}
		if err := o.advanceLocked(r); err != nil {
			o.mu.Unlock()
			return vtuneRunRecord{}, "", err
		}
		out := *r
		o.mu.Unlock()
		return out, "already_settling", nil
	}
	if r.State != vtuneRunPrepared && r.State != vtuneRunCollecting && r.State != vtuneRunStopRequested {
		o.mu.Unlock()
		return vtuneRunRecord{}, "", fmt.Errorf("stop is not legal from VTune run state %q", r.State)
	}
	r.State, r.Phase, r.StopReason = vtuneRunStopRequested, vtuneRunStopRequested, "operator_request"
	r.StopOperations[operationID] = vtuneStopOperation{Disposition: "stop_requested", At: time.Now().UTC()}
	if err := o.advanceLocked(r); err != nil {
		o.mu.Unlock()
		return vtuneRunRecord{}, "", err
	}
	req := r.Request
	o.mu.Unlock()

	stopCtx, cancel := context.WithTimeout(context.Background(), vtunePhaseTimeout)
	defer cancel()
	if err := o.driver.stop(stopCtx, req); err != nil {
		// The driver may be unable to issue VTune's native command. Cancellation
		// invokes the pre-existing Job-contained collect runner as the last resort.
		_ = o.driver.forceStop(stopCtx, req)
		o.mu.Lock()
		if c := o.collectCancels[runID]; c != nil {
			c()
		}
		r := o.runs[runID]
		r.FailureID, r.Error = failureStopFailed, err.Error()
		r.StopOperations[operationID] = vtuneStopOperation{Disposition: "stop_fallback_requested", At: time.Now().UTC()}
		if persistErr := o.advanceLocked(r); persistErr != nil {
			o.mu.Unlock()
			return vtuneRunRecord{}, "", persistErr
		}
		out := *r
		disposition := r.StopOperations[operationID].Disposition
		o.mu.Unlock()
		return out, disposition, nil
	}
	if o.waitSettled(runID, o.settlementTimeout) {
		return o.stopOperationReceipt(runID, operationID)
	}
	// Native stop acknowledgement is not completion proof. Cancel the context
	// that owns RunUnderKillJob, ask the driver's explicit fallback, then wait
	// once more for the contained process tree to reap.
	_ = o.driver.forceStop(stopCtx, req)
	o.mu.Lock()
	if c := o.collectCancels[runID]; c != nil {
		c()
	}
	o.mu.Unlock()
	if o.waitSettled(runID, o.settlementTimeout) {
		return o.stopOperationReceipt(runID, operationID)
	}
	o.mu.Lock()
	r = o.runs[runID]
	r.State, r.Phase, r.FailureID, r.Error, r.Reportable, r.Quarantined, r.ExitCode = vtuneRunFailed, "failed", failureStopSettlementTimeout, "VTune collect did not settle after native stop and contained fallback", false, true, -1
	now := time.Now().UTC()
	r.CompletedAt = &now
	o.settleStopOperationsLocked(r, "failed")
	if cleanupErr := o.removeUnretainedRawLocked(r); cleanupErr != nil {
		r.FailureID, r.Error, r.ExitCode = failureCleanupFailed, cleanupErr.Error(), -1
	}
	if persistErr := o.advanceLocked(r); persistErr != nil {
		o.mu.Unlock()
		return vtuneRunRecord{}, "", persistErr
	}
	out := *r
	o.mu.Unlock()
	return out, r.StopOperations[operationID].Disposition, nil
}

// stopOperationReceipt returns the exact persisted operation disposition. A
// Stop response must never claim success after finalization/reporting has
// durably settled the same operation as failed.
func (o *vtuneRunOwnerV1) stopOperationReceipt(runID, operationID string) (vtuneRunRecord, string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.runs[runID]
	if r == nil {
		return vtuneRunRecord{}, "", errVTuneRunNotFound
	}
	op, ok := r.StopOperations[operationID]
	if !ok {
		return vtuneRunRecord{}, "", fmt.Errorf("missing persisted stop operation %q", operationID)
	}
	return *r, op.Disposition, nil
}

func (o *vtuneRunOwnerV1) waitSettled(runID string, timeout time.Duration) bool {
	o.mu.Lock()
	done := o.done[runID]
	o.mu.Unlock()
	if done == nil {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (o *vtuneRunOwnerV1) Close(ctx context.Context) error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		if err := o.releaseStoreLeaseWhenSettled(); err != nil {
			return err
		}
		o.mu.Lock()
		leaseHeld := o.storeLease != nil
		leaseReleaseErr := o.leaseReleaseErr
		o.mu.Unlock()
		if leaseReleaseErr != nil {
			return cleanupLeaseError(leaseReleaseErr)
		}
		if leaseHeld {
			return fmt.Errorf("%s: VTune worker settlement is still required", failureCleanupFailed)
		}
		return nil
	}
	o.closed = true
	var active []vtuneRunRecord
	for _, r := range o.runs {
		if !r.Terminal() {
			active = append(active, *r)
		}
	}
	o.mu.Unlock()
	var closeErr error
	for _, r := range active {
		// Shutdown owns a bounded stop directly: it cannot call Stop (whose
		// normal settlement budget may outlive this server shutdown context). Every
		// phase is derived from the caller's one shutdown context, so multiple runs
		// cannot turn a short deadline into N fresh thirty-second budgets.
		shutdownCtx, cancel := boundedVTuneContext(ctx, vtunePhaseTimeout)
		_ = o.driver.stop(shutdownCtx, r.Request)
		_ = o.driver.forceStop(shutdownCtx, r.Request)
		cancel()
		o.mu.Lock()
		if cancel := o.collectCancels[r.RunID]; cancel != nil {
			cancel()
		}
		if cancel := o.shutdownCancels[r.RunID]; cancel != nil {
			cancel()
		}
		o.mu.Unlock()
	}
	for _, r := range active {
		o.mu.Lock()
		done := o.done[r.RunID]
		leaseStateChanged := o.leaseStateChanged
		leaseReleaseErr := o.leaseReleaseErr
		o.mu.Unlock()
		if leaseReleaseErr != nil {
			return cleanupLeaseError(leaseReleaseErr)
		}
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-leaseStateChanged:
			o.mu.Lock()
			leaseReleaseErr := o.leaseReleaseErr
			o.mu.Unlock()
			if leaseReleaseErr != nil {
				return cleanupLeaseError(leaseReleaseErr)
			}
			// A successful lease release closes every pending handoff channel.
			continue
		case <-ctx.Done():
			o.mu.Lock()
			if current := o.runs[r.RunID]; current != nil && !current.Terminal() {
				current.State, current.Phase, current.FailureID, current.Error, current.Reportable, current.Quarantined, current.ExitCode = vtuneRunFailed, "failed", failureCleanupFailed, "VTune worker did not reap before shutdown deadline", false, true, -1
				now := time.Now().UTC()
				current.CompletedAt = &now
				_ = o.advanceLocked(current)
			}
			o.mu.Unlock()
			closeErr = fmt.Errorf("%s: %w", failureCleanupFailed, ctx.Err())
			break
		}
	}
	if closeErr != nil {
		// The lease is intentionally retained.  A second owner must not replay
		// or mutate durable state while the original contained worker can still
		// write its terminal receipt/snapshot.  worker's final defer releases it
		// only after the exact process/phase settlement has completed.
		return closeErr
	}
	if err := o.releaseStoreLeaseWhenSettled(); err != nil {
		return err
	}
	return nil
}

func cleanupLeaseError(err error) error {
	return fmt.Errorf("%s: durable VTune store lease release failed; recovery retry required: %w", failureCleanupFailed, err)
}

// releaseStoreLeaseWhenSettled is the only owner of the durable-store handoff.
// It keeps the lease handle on failure, so a successor cannot race a writer
// whose lock release has not been proven.
func (o *vtuneRunOwnerV1) releaseStoreLeaseWhenSettled() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed || len(o.collectCancels) != 0 {
		return nil
	}
	lease := o.storeLease
	if lease == nil {
		return nil
	}
	if err := o.unlockStoreLease(lease); err != nil {
		o.leaseReleaseErr = err
		o.signalLeaseStateChangedLocked()
		return cleanupLeaseError(err)
	}
	o.storeLease = nil
	o.leaseReleaseErr = nil
	o.closeDoneLocked()
	o.signalLeaseStateChangedLocked()
	return nil
}

func (o *vtuneRunOwnerV1) signalLeaseStateChangedLocked() {
	close(o.leaseStateChanged)
	o.leaseStateChanged = make(chan struct{})
}

func (o *vtuneRunOwnerV1) closeDoneLocked() {
	for _, done := range o.done {
		select {
		case <-done:
		default:
			close(done)
		}
	}
}

func (o *vtuneRunOwnerV1) worker(collectCtx, shutdownCtx context.Context, id string) {
	defer o.wg.Done()
	defer func() {
		o.mu.Lock()
		delete(o.collectCancels, id)
		delete(o.shutdownCancels, id)
		closed := o.closed
		o.mu.Unlock()
		if closed {
			// The final shutdown worker releases the durable-store lease before
			// publishing any settled handoff. An unlock error is retained for
			// Close's recovery retry.
			_ = o.releaseStoreLeaseWhenSettled()
			return
		}
		// waitSettled is the public handoff edge: close it only after the
		// current worker has settled. While the owner is live, its store lease
		// remains intentional and no successor handoff is advertised.
		o.mu.Lock()
		if done := o.done[id]; done != nil {
			select {
			case <-done:
			default:
				close(done)
			}
		}
		o.mu.Unlock()
	}()
	req, launch, err := o.claimCollect(id)
	if err != nil || !launch {
		return
	}
	out, err := o.driver.collect(collectCtx, req)
	if err != nil && !isVTuneExitError(err) {
		failureID := failureCollectStartFailed
		if errors.Is(err, process.ErrContainmentUnavailable) {
			failureID = failureContainmentUnavailable
		} else if hasContainedCleanupFailure(err) {
			failureID = failureCleanupFailed
		}
		o.fail(id, failureID, err, out)
		return
	}
	o.mu.Lock()
	r := o.runs[id]
	if r == nil || r.Terminal() {
		o.mu.Unlock()
		return
	}
	r.Output = out
	if out != nil {
		r.PhaseExitCodes["collect"] = out.ExitCode
	}
	if out != nil && out.ResultDir != "" {
		r.Request.ResultDir = out.ResultDir
	}
	if r.State != vtuneRunStopRequested {
		r.State = vtuneRunFinalizing
		r.Phase = vtuneRunFinalizing
		if o.advanceLocked(r) != nil {
			o.mu.Unlock()
			return
		}
	} else {
		r.Phase = vtuneRunFinalizing
		if o.advanceLocked(r) != nil {
			o.mu.Unlock()
			return
		}
	}
	req = r.Request
	o.mu.Unlock()
	if _, launch := o.claimPhase(id, vtuneRunFinalizing); !launch {
		return
	}
	finalizeCtx, cancelFinalize := boundedVTuneContext(shutdownCtx, vtunePhaseTimeout)
	err = o.driver.finalize(finalizeCtx, req)
	cancelFinalize()
	o.releasePhase(id)
	if err != nil {
		_ = o.recordPhaseExit(id, "finalize", phaseExitCode(err))
		o.fail(id, failureFinalizeFailed, err, out)
		return
	}
	if o.recordPhaseExit(id, "finalize", 0) != nil {
		return
	}
	if o.transition(id, vtuneRunReporting, "", "") != nil {
		return
	}
	if _, launch := o.claimPhase(id, vtuneRunReporting); !launch {
		return
	}
	reportCtx, cancelReport := boundedVTuneContext(shutdownCtx, vtunePhaseTimeout)
	reported, err := o.driver.report(reportCtx, req)
	cancelReport()
	o.releasePhase(id)
	if err != nil {
		_ = o.recordPhaseExit(id, "report", phaseExitCode(err))
		o.fail(id, failureReportFailed, err, reported)
		return
	}
	if reported == nil || strings.TrimSpace(reported.ReportCSV) == "" || strings.TrimSpace(reported.Summary) == "" {
		o.fail(id, failureReportOutputMissing, errors.New("VTune report output missing"), reported)
		return
	}
	if o.recordPhaseExit(id, "report", 0) != nil {
		return
	}
	o.complete(id, reported)
}

// claimCollect is the one prepared-to-collect transition.  Stop may settle a
// prepared record without any native process ever being started; therefore a
// worker must claim this transition atomically before invoking its driver.
func (o *vtuneRunOwnerV1) claimCollect(id string) (vtuneRunRequest, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.runs[id]
	if r == nil || r.Terminal() || r.State != vtuneRunPrepared {
		return vtuneRunRequest{}, false, nil
	}
	r.State, r.Phase = vtuneRunCollecting, vtuneRunCollecting
	if err := o.advanceLocked(r); err != nil {
		return vtuneRunRequest{}, false, err
	}
	return r.Request, true, nil
}

func (o *vtuneRunOwnerV1) claimPhase(id, phase string) (vtuneRunRequest, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.runs[id]
	if r == nil || r.Terminal() || r.Phase != phase || o.phaseActive[id] != "" {
		return vtuneRunRequest{}, false
	}
	o.phaseActive[id] = phase
	return r.Request, true
}

func (o *vtuneRunOwnerV1) releasePhase(id string) {
	o.mu.Lock()
	delete(o.phaseActive, id)
	o.mu.Unlock()
}

func isVTuneExitError(err error) bool {
	var contained *process.ContainedRunError
	return errors.As(err, &contained) && contained.Stage == process.ContainedStageExit && contained.ExitCode != nil && contained.CleanupCause == nil
}

func hasContainedCleanupFailure(err error) bool {
	var contained *process.ContainedRunError
	return errors.As(err, &contained) && contained.CleanupCause != nil
}

func (o *vtuneRunOwnerV1) recordPhaseExit(id, phase string, code int) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if r := o.runs[id]; r != nil && !r.Terminal() {
		r.PhaseExitCodes[phase] = code
		return o.advanceLocked(r)
	}
	return nil
}

func (o *vtuneRunOwnerV1) transition(id, state, failureID, message string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.runs[id]
	if r == nil || r.Terminal() {
		return nil
	}
	// A requested native stop is an outcome, not merely the current phase.
	// Keep it while finalization/reporting run so completion can distinguish a
	// deliberately stopped reportable database from a naturally completed run.
	if r.State == vtuneRunStopRequested && (state == vtuneRunFinalizing || state == vtuneRunReporting) {
		r.Phase = state
	} else {
		r.State, r.Phase = state, state
	}
	if failureID != "" {
		r.FailureID, r.Error = failureID, message
	}
	return o.advanceLocked(r)
}

func (o *vtuneRunOwnerV1) fail(id, failureID string, err error, out *runOutput) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.runs[id]
	if r == nil || r.Terminal() {
		return
	}
	r.State, r.Phase, r.Reportable, r.Quarantined = vtuneRunFailed, "failed", false, true
	// A stop fallback already recorded its native-command failure.  The
	// subsequent cancelled collect is evidence of containment cleanup, not a
	// replacement diagnosis for that operation.
	if r.FailureID == "" {
		r.FailureID, r.Error = failureID, err.Error()
	}
	if out != nil {
		r.Output = out
		r.ExitCode = out.ExitCode
	}
	if r.ExitCode == 0 {
		r.ExitCode = -1
	}
	now := time.Now().UTC()
	r.CompletedAt = &now
	o.settleStopOperationsLocked(r, "failed")
	if cleanupErr := o.removeUnretainedRawLocked(r); cleanupErr != nil {
		r.FailureID, r.Error, r.ExitCode = failureCleanupFailed, cleanupErr.Error(), -1
	}
	_ = o.advanceLocked(r)
}

func (o *vtuneRunOwnerV1) complete(id string, out *runOutput) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.runs[id]
	if r == nil || r.Terminal() {
		return
	}
	r.Output, r.Reportable = out, true
	r.ExitCode = out.ExitCode
	if collectExit, ok := r.PhaseExitCodes["collect"]; ok && collectExit != 0 {
		r.ExitCode = collectExit
	}
	if r.State == vtuneRunStopRequested {
		r.State = vtuneRunStopped
	} else if r.ExitCode != 0 {
		r.State = vtuneRunCompletedNonzero
	} else {
		r.State = vtuneRunCompleted
	}
	r.Phase = r.State
	now := time.Now().UTC()
	r.CompletedAt = &now
	if !r.Request.KeepResult {
		// Findings remain in the signed receipt/snapshot, but neither the raw
		// database nor a dangling report path survives a non-retained run.
		r.Output.ReportPath = ""
	}
	if hash, err := o.writeReceiptLocked(r); err != nil {
		r.State, r.Phase, r.Reportable, r.Quarantined, r.FailureID, r.Error, r.ExitCode = vtuneRunFailed, "failed", false, true, failureCleanupFailed, err.Error(), -1
	} else {
		r.ReceiptSHA256 = hash
	}
	o.settleStopOperationsLocked(r, r.State)
	if err := o.removeUnretainedRawLocked(r); err != nil {
		r.State, r.Phase, r.Reportable, r.Quarantined, r.FailureID, r.Error, r.ExitCode = vtuneRunFailed, "failed", false, true, failureCleanupFailed, err.Error(), -1
		o.settleStopOperationsLocked(r, "failed")
	}
	_ = o.advanceLocked(r)
}

func (o *vtuneRunOwnerV1) settleStopOperationsLocked(r *vtuneRunRecord, disposition string) {
	for id, op := range r.StopOperations {
		op.Disposition = disposition
		r.StopOperations[id] = op
	}
}

// removeUnretainedRawLocked deletes only the owner-derived run directory.
// A request may carry a corrupted persisted ResultDir, so cleanup never trusts
// it to select a deletion target.
func (o *vtuneRunOwnerV1) removeUnretainedRawLocked(r *vtuneRunRecord) error {
	if r.Request.KeepResult {
		return nil
	}
	return os.RemoveAll(filepath.Join(o.root, "runs", r.RunID))
}

func (o *vtuneRunOwnerV1) advanceLocked(r *vtuneRunRecord) error {
	if o.storageFailed[r.RunID] {
		return fmt.Errorf("durable VTune state unavailable")
	}
	oldGeneration, oldUpdated := r.Generation, r.UpdatedAt
	r.Generation++
	r.UpdatedAt = time.Now().UTC()
	if err := o.persistLocked(r); err != nil {
		if durable, ok := o.durable[r.RunID]; ok {
			*r = cloneVTuneRunRecord(durable)
		} else {
			r.Generation, r.UpdatedAt = oldGeneration, oldUpdated
		}
		o.storageFailed[r.RunID] = true
		return err
	}
	o.durable[r.RunID] = cloneVTuneRunRecord(*r)
	return nil
}

func cloneVTuneRunRecord(in vtuneRunRecord) vtuneRunRecord {
	body, _ := json.Marshal(in)
	var out vtuneRunRecord
	_ = json.Unmarshal(body, &out)
	return out
}

func (o *vtuneRunOwnerV1) persistLocked(r *vtuneRunRecord) error {
	dir := filepath.Join(o.root, "durable-runs", r.RunID)
	if err := hubtemp.EnsurePrivateDir(dir); err != nil {
		return err
	}
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("%08d.json", r.Generation))
	return o.snapshotWriter(path, body)
}

func (o *vtuneRunOwnerV1) writeReceiptLocked(r *vtuneRunRecord) (string, error) {
	dir := filepath.Join(o.root, "durable-runs", r.RunID)
	if err := hubtemp.EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	body, err := json.Marshal(vtuneReportReceiptV1{
		SchemaVersion: "report-receipt-v1", RunID: r.RunID, ReportPath: r.Output.ReportPath, ResultDir: r.Request.ResultDir,
		ReportSHA256: contentSHA256(r.Output.ReportCSV), SummarySHA256: contentSHA256(r.Output.Summary), ExitCode: r.ExitCode, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return "", err
	}
	if err := atomicWrite(filepath.Join(dir, "report-receipt-v1.json"), body); err != nil {
		return "", err
	}
	s := sha256.Sum256(body)
	return hex.EncodeToString(s[:]), nil
}

func contentSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// validReceiptLocked is the sole durable publication gate for result_dir.
// It requires a regular bounded receipt, matching receipt hash, and matching
// bounded report payload hashes before a result can be exposed.
func (o *vtuneRunOwnerV1) validReceiptLocked(r *vtuneRunRecord) bool {
	if r.ReceiptSHA256 == "" || r.Output == nil {
		return false
	}
	path := filepath.Join(o.root, "durable-runs", r.RunID, "report-receipt-v1.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > vtuneOutputCap {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, vtuneOutputCap+1))
	if err != nil || len(body) == 0 || len(body) > vtuneOutputCap {
		return false
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != r.ReceiptSHA256 {
		return false
	}
	var receipt vtuneReportReceiptV1
	if json.Unmarshal(body, &receipt) != nil {
		return false
	}
	if receipt.SchemaVersion != "report-receipt-v1" || receipt.RunID != r.RunID || receipt.ResultDir != r.Request.ResultDir || receipt.ReportPath != r.Output.ReportPath || receipt.ExitCode != r.ExitCode || receipt.ReportSHA256 != contentSHA256(r.Output.ReportCSV) || receipt.SummarySHA256 != contentSHA256(r.Output.Summary) {
		return false
	}
	// Non-retained results intentionally delete the raw database and report
	// files after their receipt is written.  Retained results are different: a
	// reportable receipt must still point at the owner-created result directory
	// and regular CSV artifact it claims to retain.
	if !r.Request.KeepResult {
		return true
	}
	resultInfo, err := os.Lstat(r.Request.ResultDir)
	if err != nil || !resultInfo.IsDir() {
		return false
	}
	resultEntries, err := os.ReadDir(r.Request.ResultDir)
	if err != nil || len(resultEntries) == 0 {
		return false
	}
	reportInfo, err := os.Lstat(r.Output.ReportPath)
	if err != nil || !reportInfo.Mode().IsRegular() || reportInfo.Size() <= 0 || reportInfo.Size() > vtuneOutputCap {
		return false
	}
	f, err = os.Open(r.Output.ReportPath)
	if err != nil {
		return false
	}
	defer f.Close()
	reportBody, err := io.ReadAll(io.LimitReader(f, vtuneOutputCap+1))
	return err == nil && len(reportBody) > 0 && len(reportBody) <= vtuneOutputCap && contentSHA256(string(reportBody)) == receipt.ReportSHA256
}

func (o *vtuneRunOwnerV1) invalidateReceiptLocked(r *vtuneRunRecord) error {
	r.State, r.Phase, r.FailureID, r.Error, r.Reportable, r.Quarantined = vtuneRunFailed, "failed", failureResultNonReportable, "VTune report receipt is missing, malformed, or does not match the report output", false, true
	if r.ExitCode == 0 {
		r.ExitCode = -1
	}
	now := time.Now().UTC()
	r.CompletedAt = &now
	return o.advanceLocked(r)
}

func nonreportableVTuneSnapshot(r vtuneRunRecord) vtuneRunRecord {
	r.State, r.Phase, r.FailureID, r.Error, r.Reportable, r.Quarantined = vtuneRunFailed, "failed", failureResultNonReportable, "VTune report receipt is missing, malformed, or does not match the report output", false, true
	if r.ExitCode == 0 {
		r.ExitCode = -1
	}
	return r
}

func (o *vtuneRunOwnerV1) load() error {
	base := filepath.Join(o.root, "durable-runs")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(base, entry.Name()))
		if err != nil {
			return err
		}
		var files []string
		for _, candidate := range entries {
			if !candidate.Type().IsRegular() || !vtuneGenerationSnapshotName.MatchString(candidate.Name()) {
				continue
			}
			files = append(files, filepath.Join(base, entry.Name(), candidate.Name()))
		}
		sort.Strings(files)
		if len(files) == 0 {
			continue
		}
		body, err := os.ReadFile(files[len(files)-1])
		if err != nil {
			return err
		}
		var r vtuneRunRecord
		if err := json.Unmarshal(body, &r); err != nil || r.SchemaVersion != vtuneRunSchemaV1 {
			continue
		}
		o.durable[r.RunID] = cloneVTuneRunRecord(r)
		if !r.Terminal() {
			r.State, r.Phase, r.FailureID, r.Error, r.Reportable, r.Quarantined, r.ExitCode = vtuneRunHostLost, vtuneRunHostLost, failureOwnerRestarted, "owner restarted before terminal settlement", false, true, -1
			now := time.Now().UTC()
			r.CompletedAt = &now
			if err := o.removeUnretainedRawLocked(&r); err != nil {
				return err
			}
			if err := o.advanceLocked(&r); err != nil {
				return err
			}
		}
		if r.Reportable && !o.validReceiptLocked(&r) {
			if err := o.invalidateReceiptLocked(&r); err != nil {
				return err
			}
		}
		o.runs[r.RunID] = &r
		o.durable[r.RunID] = cloneVTuneRunRecord(r)
		if r.Request.IdempotencyKey != "" {
			o.keys[r.Request.IdempotencyKey] = r.RunID
		}
	}
	return nil
}

// sweepArtifacts bounds durable-run residue from crashes and hard kills.  The
// runs directory is exclusively owner-created, so an empty prefix is safe and
// covers retained, failed, and quarantined result trees alike.  Generation
// snapshots and receipts live elsewhere and are never removed here.
func (o *vtuneRunOwnerV1) sweepArtifacts() {
	runsDir := filepath.Join(o.root, "runs")
	if hubtemp.EnsurePrivateDir(runsDir) == nil {
		hubtemp.SweepStale(runsDir, "", vtuneArtifactSweepTTL)
	}
	durableDir := filepath.Join(o.root, "durable-runs")
	entries, err := os.ReadDir(durableDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-vtuneArtifactSweepTTL)
	for _, runDir := range entries {
		if !runDir.IsDir() {
			continue
		}
		files, readErr := os.ReadDir(filepath.Join(durableDir, runDir.Name()))
		if readErr != nil {
			continue
		}
		for _, file := range files {
			if !file.Type().IsRegular() || !strings.HasSuffix(file.Name(), ".tmp") {
				continue
			}
			if info, statErr := file.Info(); statErr == nil && info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(durableDir, runDir.Name(), file.Name()))
			}
		}
	}
}

func boundedVTuneContext(parent context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, limit)
}

func atomicWrite(path string, body []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func requestFingerprint(req vtuneRunRequest) string {
	req.RunID = ""
	req.ResultDir = ""
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func newVTuneRunID() string { return fmt.Sprintf("vtune-%d", time.Now().UTC().UnixNano()) }

// defaultVTunePhaseDriver deliberately uses the existing Job-contained phase
// runner. A native stop is attempted first; only its failure causes the owner
// to cancel the collect context, which makes RunUnderKillJob reap descendants.
type defaultVTunePhaseDriver struct{}

func newDefaultVTunePhaseDriver() vtunePhaseDriver { return defaultVTunePhaseDriver{} }

func (defaultVTunePhaseDriver) collect(ctx context.Context, req vtuneRunRequest) (*runOutput, error) {
	if err := hubtemp.EnsurePrivateDir(filepath.Dir(req.ResultDir)); err != nil {
		return nil, err
	}
	args := buildDurableCollectArgs(req.AnalysisType, req.ResultDir, req.TimeoutSec, req.Target, req.Args)
	stderr := newCappedBuffer(vtuneOutputCap)
	err := runVTunePhase(ctx, req.VTunePath, args, req.Cwd, vtuneEnv(), stderr)
	out := &runOutput{ExitCode: phaseExitCode(err), ResultDir: req.ResultDir, CommandLine: formatCommandLine(req.VTunePath, args), Stderr: truncate(stderr.String(), vtuneOutputCap), TimedOut: errors.Is(err, context.DeadlineExceeded)}
	return out, err
}

func (defaultVTunePhaseDriver) stop(ctx context.Context, req vtuneRunRequest) error {
	return runVTunePhase(ctx, req.VTunePath, []string{"-command", "stop", "-r", req.ResultDir}, req.Cwd, vtuneEnv(), newCappedBuffer(vtuneOutputCap))
}

func (defaultVTunePhaseDriver) finalize(ctx context.Context, req vtuneRunRequest) error {
	return runVTunePhase(ctx, req.VTunePath, []string{"-finalize", "-finalization-mode=fast", "-r", req.ResultDir}, req.Cwd, vtuneEnv(), newCappedBuffer(vtuneOutputCap))
}

func (defaultVTunePhaseDriver) report(ctx context.Context, req vtuneRunRequest) (*runOutput, error) {
	outDir := filepath.Join(filepath.Dir(req.ResultDir), "reports")
	if err := hubtemp.EnsurePrivateDir(outDir); err != nil {
		return nil, err
	}
	csvPath, summaryPath := filepath.Join(outDir, "report.csv"), filepath.Join(outDir, "summary.txt")
	stderr := newCappedBuffer(vtuneOutputCap)
	first := buildReportArgs(reportName(req.AnalysisType), req.ResultDir, csvPath)
	if err := runVTunePhase(ctx, req.VTunePath, first, req.Cwd, vtuneEnv(), stderr); err != nil {
		return &runOutput{ExitCode: phaseExitCode(err), ResultDir: req.ResultDir, CommandLine: formatCommandLine(req.VTunePath, first), Stderr: stderr.String()}, err
	}
	second := buildReportArgs("summary", req.ResultDir, summaryPath)
	if err := runVTunePhase(ctx, req.VTunePath, second, req.Cwd, vtuneEnv(), stderr); err != nil {
		return &runOutput{ExitCode: phaseExitCode(err), ResultDir: req.ResultDir, CommandLine: formatCommandLine(req.VTunePath, first) + "\n" + formatCommandLine(req.VTunePath, second), Stderr: stderr.String()}, err
	}
	return &runOutput{ExitCode: 0, ResultDir: req.ResultDir, ReportCSV: readReportFile(csvPath), Summary: readReportFile(summaryPath), ReportPath: reportPathIfPresent(csvPath), CommandLine: formatCommandLine(req.VTunePath, first) + "\n" + formatCommandLine(req.VTunePath, second), Stderr: truncate(stderr.String(), vtuneOutputCap)}, nil
}

func (defaultVTunePhaseDriver) forceStop(ctx context.Context, req vtuneRunRequest) error {
	// A second native stop is cheap and can catch a collection that reached the
	// result DB between the first command and fallback. The owner then cancels
	// the collect context, whose RunContainedStream containment is the decisive
	// reap mechanism.
	return runVTunePhase(ctx, req.VTunePath, []string{"-command", "stop", "-r", req.ResultDir}, req.Cwd, vtuneEnv(), newCappedBuffer(vtuneOutputCap))
}

func phaseExitCode(err error) int {
	if err == nil {
		return 0
	}
	var contained *process.ContainedRunError
	if errors.As(err, &contained) && contained.ExitCode != nil {
		return *contained.ExitCode
	}
	return -1
}
