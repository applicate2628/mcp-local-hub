package gui

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemonrecovery"
	"mcp-local-hub/internal/process"
)

const (
	auditLockScope                        = "supervisor_events_log"
	auditLockOccurrenceCapacity           = 64
	auditLockOccurrenceStoreVersion       = 1
	auditLockOccurrenceFileLeaf           = "daemon-recovery-occurrences.json"
	auditLockOccurrenceStoreMaxBytes      = 1 << 20
	auditLockStoreLockTimeout             = 2 * time.Second
	auditLockStoreLockRetry               = 10 * time.Millisecond
	auditLockTaskNameMaxBytes             = 1024
	auditLockTerminationStateCommitted    = "committed"
	auditLockTerminationStateNotCommitted = "not_committed"
	auditLockTerminationStateUnknown      = "unknown"
	auditLockAuthorizationNone            = "none"
	auditLockAuthorizationCurrentTruth    = "current_truth"
	auditLockAuthorizationUncertain       = "uncertain"
)

type auditLockReceiptDTO struct {
	AttemptID              string `json:"attempt_id"`
	OccurrenceID           string `json:"occurrence_id"`
	ServerInstance         string `json:"server_instance"`
	TaskName               string `json:"task_name"`
	Status                 string `json:"status"`
	LockAuthorization      string `json:"lock_authorization"`
	TerminationCommitState string `json:"termination_commit_state"`
}

type auditLockStateDTO struct {
	Scope                         string                       `json:"scope"`
	ServerInstance                string                       `json:"server_instance"`
	Revision                      uint64                       `json:"revision"`
	State                         api.SupervisorEventLockState `json:"state"`
	OccurrenceStoreHealth         occurrenceStoreLockState     `json:"occurrence_store_health"`
	OccurrenceStoreHealthRevision uint64                       `json:"occurrence_store_health_revision"`
	RestartRequired               bool                         `json:"restart_required"`
	RecoveryReceipt               *auditLockReceiptDTO         `json:"recovery_receipt"`
	RecoveryReceipts              []auditLockReceiptDTO        `json:"recovery_receipts"`
}

type auditLockCorrelation struct {
	AttemptID      string
	OccurrenceID   string
	ServerInstance string
}

// auditLockExpectedPhysical is a compare operand, never a client-selected
// policy. The physical owner supplies the actual snapshot at commit time.
type auditLockExpectedPhysical struct {
	ServerInstance string
	Revision       uint64
	State          api.SupervisorEventLockState
}

type auditLockAcknowledgeRequest struct {
	Correlation      auditLockCorrelation
	ExpectedPhysical *auditLockExpectedPhysical
}

type auditLockOccurrenceBinding struct {
	serverInstance string
	taskName       string
	confirm        bool
}

const (
	auditLockOccurrenceInFlight         = "in_flight"
	auditLockOccurrenceCommittedSuccess = "committed_success"
	auditLockOccurrenceCommittedError   = "committed_error"
	auditLockOccurrenceNotCommitted     = "not_committed"
	auditLockOccurrenceUncertain        = "uncertain"
	auditLockOccurrenceConsumed         = "consumed"
)

var auditLockOccurrenceStatuses = [...]string{
	auditLockOccurrenceInFlight,
	auditLockOccurrenceCommittedSuccess,
	auditLockOccurrenceCommittedError,
	auditLockOccurrenceNotCommitted,
	auditLockOccurrenceUncertain,
	auditLockOccurrenceConsumed,
}

var auditLockAuthorizations = [...]string{
	auditLockAuthorizationNone,
	auditLockAuthorizationCurrentTruth,
	auditLockAuthorizationUncertain,
}

var auditLockTerminationStates = [...]string{
	auditLockTerminationStateCommitted,
	auditLockTerminationStateNotCommitted,
	auditLockTerminationStateUnknown,
}

func auditLockOccurrenceStatusValues() []string {
	return append([]string(nil), auditLockOccurrenceStatuses[:]...)
}

func auditLockAuthorizationValues() []string {
	return append([]string(nil), auditLockAuthorizations[:]...)
}

func auditLockTerminationStateValues() []string {
	return append([]string(nil), auditLockTerminationStates[:]...)
}

func auditLockKnownValue(values []string, value string) bool {
	for _, known := range values {
		if value == known {
			return true
		}
	}
	return false
}

type daemonRecoverSuccessEvidence struct {
	TaskName             string `json:"task_name"`
	Reaped               bool   `json:"reaped"`
	PortOwnerCheck       string `json:"port_owner_check"`
	PortWaitOutcome      string `json:"port_wait_outcome"`
	AuditHandoff         string `json:"audit_handoff"`
	TerminationCommitted bool   `json:"termination_committed"`
}

type auditLockTerminalEvidence struct {
	HTTPStatus           int
	ErrorCode            string
	TerminationCommitted bool
	Success              *daemonRecoverSuccessEvidence
}

type auditLockOccurrenceRecord struct {
	OriginServerInstance string                        `json:"origin_server_instance"`
	AttemptID            string                        `json:"attempt_id"`
	OccurrenceID         string                        `json:"occurrence_id"`
	TaskName             string                        `json:"task_name"`
	Confirm              bool                          `json:"confirm"`
	Status               string                        `json:"status"`
	HTTPStatus           int                           `json:"http_status"`
	ErrorCode            string                        `json:"error_code"`
	LockAuthorization    string                        `json:"lock_authorization"`
	Success              *daemonRecoverSuccessEvidence `json:"success"`
}

type auditLockOccurrenceStore struct {
	Version              int                         `json:"version"`
	Generation           uint64                      `json:"generation"`
	ActiveServerInstance string                      `json:"active_server_instance"`
	Records              []auditLockOccurrenceRecord `json:"records"`
}

type auditLockMutationEpoch struct {
	ServerInstance string
	Generation     uint64
}

func (e auditLockMutationEpoch) matchesStore(store auditLockOccurrenceStore) bool {
	return store.Generation == e.Generation && store.ActiveServerInstance == e.ServerInstance
}

type auditLockReservation struct {
	Novel         bool
	Receipt       auditLockReceiptDTO
	Terminal      *auditLockTerminalEvidence
	Binding       auditLockOccurrenceBinding
	MutationEpoch auditLockMutationEpoch
	Settlement    *auditLockSettlementCell
}

type auditLockSettlementKey struct {
	Generation     uint64
	ServerInstance string
	AttemptID      string
	OccurrenceID   string
}

type auditLockSettlementPhase string

const (
	auditLockSettlementInFlight        auditLockSettlementPhase = "in_flight"
	auditLockSettlementDurableTerminal auditLockSettlementPhase = "durable_terminal"
	auditLockSettlementUncertain       auditLockSettlementPhase = "uncertain"
)

type auditLockSettlementSnapshot struct {
	phase   auditLockSettlementPhase
	receipt auditLockReceiptDTO
}

type auditLockSettlementCell struct {
	snapshot atomic.Pointer[auditLockSettlementSnapshot]
}

func newAuditLockSettlementCell(receipt auditLockReceiptDTO) *auditLockSettlementCell {
	cell := &auditLockSettlementCell{}
	cell.snapshot.Store(&auditLockSettlementSnapshot{
		phase:   auditLockSettlementInFlight,
		receipt: receipt,
	})
	return cell
}

func (c *auditLockSettlementCell) load() *auditLockSettlementSnapshot {
	if c == nil {
		return nil
	}
	return c.snapshot.Load()
}

func (c *auditLockSettlementCell) publish(phase auditLockSettlementPhase, receipt auditLockReceiptDTO) *auditLockSettlementSnapshot {
	if c == nil {
		return nil
	}
	next := &auditLockSettlementSnapshot{phase: phase, receipt: receipt}
	for {
		current := c.snapshot.Load()
		if current == nil {
			return nil
		}
		if current.phase != auditLockSettlementInFlight {
			return current
		}
		if c.snapshot.CompareAndSwap(current, next) {
			return next
		}
	}
}

type auditLockRouteError struct {
	status                   int
	code                     string
	cause                    error
	occurrenceStoreHealth    *occurrenceStoreLockHealthSnapshot
	auditLockStateProjection *auditLockStateDTO
}

func (e *auditLockRouteError) Error() string { return e.code }

func (e *auditLockRouteError) Unwrap() error { return e.cause }

type auditLockAdapter struct {
	mu               sync.Mutex
	storeMu          sync.Mutex
	activationMu     sync.Mutex
	serverInstance   string
	generation       uint64
	storeClaimed     bool
	storePath        string
	logPath          string
	initErr          error
	closing          bool
	closed           bool
	watching         bool
	observedRevision uint64
	settlements      sync.Map
	storeLockHealth  *occurrenceStoreLockHealth
	subscription     *api.SupervisorEventLockSubscription
	events           *Broadcaster
	lockTimeout      time.Duration
	// terminalizationBudget is resolved once by the composition root. It bounds
	// only the contained terminal transaction; lockTimeout remains the ordinary
	// occurrence-store operation allowance.
	terminalizationBudget time.Duration
	// writeStateFileLockHeld is per-instance only so the worker transaction can
	// prove the hardened writer's post-error reread path without a package-global
	// hook. Production wiring is api.WriteStateFileBytesLockHeld.
	writeStateFileLockHeld func(string, []byte) error
	terminalization        auditLockTerminalizationConfig
}

// auditLockTerminalRunner is the narrow parent-to-contained-worker seam. The
// production constructor injects the current-binary runner; tests inject a real
// contained helper or a bounded protocol result without changing store policy.
type auditLockTerminalRunner func(context.Context, auditLockTerminalWorkerRequest) (auditLockTerminalWorkerResult, error)

type auditLockTerminalizationMode uint8

const (
	auditLockTerminalizationBounded auditLockTerminalizationMode = iota + 1
	auditLockTerminalizationDirectTest
)

type auditLockTerminalizationConfig struct {
	mode   auditLockTerminalizationMode
	runner auditLockTerminalRunner
}

type auditLockTerminalizationConfigError struct {
	mode   auditLockTerminalizationMode
	reason string
}

func (e *auditLockTerminalizationConfigError) Error() string {
	return fmt.Sprintf("invalid audit-lock terminalization config: mode=%d: %s", e.mode, e.reason)
}

func (c auditLockTerminalizationConfig) validate() error {
	switch c.mode {
	case auditLockTerminalizationBounded:
		if c.runner == nil {
			return &auditLockTerminalizationConfigError{mode: c.mode, reason: "bounded mode requires a runner"}
		}
	case auditLockTerminalizationDirectTest:
		if c.runner != nil {
			return &auditLockTerminalizationConfigError{mode: c.mode, reason: "direct-test mode forbids a runner"}
		}
	default:
		return &auditLockTerminalizationConfigError{mode: c.mode, reason: "unknown mode"}
	}
	return nil
}

func newAuditLockAdapter(events *Broadcaster) *auditLockAdapter {
	return newAuditLockAdapterWithTerminalizationBudget(events, auditLockStoreLockTimeout)
}

func newAuditLockAdapterWithTerminalizationBudget(events *Broadcaster, terminalizationBudget time.Duration) *auditLockAdapter {
	a := newUnclaimedAuditLockAdapterWithTerminalizationBudget(events, terminalizationBudget)
	if a.initErr == nil {
		_ = a.activateStore(context.Background())
	}
	return a
}

func newUnclaimedAuditLockAdapterWithTerminalizationBudget(events *Broadcaster, terminalizationBudget time.Duration) *auditLockAdapter {
	if terminalizationBudget <= 0 {
		terminalizationBudget = auditLockStoreLockTimeout
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return &auditLockAdapter{
			events:                 events,
			storeLockHealth:        newOccurrenceStoreLockHealth(nil, emitOccurrenceStoreLockHealthEvent),
			lockTimeout:            auditLockStoreLockTimeout,
			terminalizationBudget:  terminalizationBudget,
			writeStateFileLockHeld: api.WriteStateFileBytesLockHeld,
			terminalization: auditLockTerminalizationConfig{
				mode:   auditLockTerminalizationBounded,
				runner: runAuditLockTerminalWorker,
			},
			initErr: fmt.Errorf("resolve daemon state dir: %w", err),
		}
	}
	return newUnclaimedAuditLockAdapterInStateDirWithTerminalizationBudget(events, stateDir, terminalizationBudget, auditLockTerminalizationConfig{
		mode:   auditLockTerminalizationBounded,
		runner: runAuditLockTerminalWorker,
	})
}

func newAuditLockAdapterInStateDirWithTerminalizationBudget(events *Broadcaster, stateDir string, terminalizationBudget time.Duration, terminalization auditLockTerminalizationConfig) *auditLockAdapter {
	a := newUnclaimedAuditLockAdapterInStateDirWithTerminalizationBudget(events, stateDir, terminalizationBudget, terminalization)
	if a.initErr == nil {
		_ = a.activateStore(context.Background())
	}
	return a
}

func newUnclaimedAuditLockAdapterInStateDirWithTerminalizationBudget(events *Broadcaster, stateDir string, terminalizationBudget time.Duration, terminalization auditLockTerminalizationConfig) *auditLockAdapter {
	a := newUnclaimedAuditLockAdapterInStateDirWithStoreLockDeps(events, stateDir, nil, emitOccurrenceStoreLockHealthEvent, terminalization)
	if terminalizationBudget > 0 {
		a.terminalizationBudget = terminalizationBudget
	}
	return a
}

func newAuditLockAdapterInStateDirWithStoreLockDeps(
	events *Broadcaster,
	stateDir string,
	factory occurrenceStoreLockFactory,
	emit occurrenceStoreLockHealthEmitter,
	terminalization auditLockTerminalizationConfig,
) *auditLockAdapter {
	a := newUnclaimedAuditLockAdapterInStateDirWithStoreLockDeps(events, stateDir, factory, emit, terminalization)
	if a.initErr == nil {
		_ = a.activateStore(context.Background())
	}
	return a
}

func newUnclaimedAuditLockAdapterInStateDirWithStoreLockDeps(
	events *Broadcaster,
	stateDir string,
	factory occurrenceStoreLockFactory,
	emit occurrenceStoreLockHealthEmitter,
	terminalization auditLockTerminalizationConfig,
) *auditLockAdapter {
	a := &auditLockAdapter{
		events:                 events,
		storePath:              filepath.Join(stateDir, auditLockOccurrenceFileLeaf),
		logPath:                filepath.Join(stateDir, api.SupervisorEventLogFileLeaf),
		storeLockHealth:        newOccurrenceStoreLockHealth(factory, emit),
		lockTimeout:            auditLockStoreLockTimeout,
		terminalizationBudget:  auditLockStoreLockTimeout,
		writeStateFileLockHeld: api.WriteStateFileBytesLockHeld,
		terminalization:        terminalization,
	}
	if err := terminalization.validate(); err != nil {
		a.initErr = err
		return a
	}
	instance, err := newAuditLockCorrelationID()
	if err != nil {
		a.initErr = fmt.Errorf("generate audit-lock server instance: %w", err)
		return a
	}
	a.serverInstance = instance
	return a
}

func (a *auditLockAdapter) activateStore(ctx context.Context) error {
	if a == nil {
		return errors.New("nil audit-lock adapter")
	}
	a.activationMu.Lock()
	defer a.activationMu.Unlock()
	if a.initErr != nil {
		return a.initErr
	}
	if a.storeClaimed {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	claimCtx, cancel := context.WithTimeout(ctx, a.lockTimeout)
	defer cancel()
	if err := a.claimStore(claimCtx); err != nil {
		a.initErr = fmt.Errorf("claim daemon recovery occurrence store: %w", err)
		return a.initErr
	}
	a.storeClaimed = true
	return nil
}

func emitOccurrenceStoreLockHealthEvent(event occurrenceStoreLockHealthEvent) {
	_ = api.LogHubMcpEvent("warn", occurrenceStoreLockStrandedEvent, map[string]any{
		"code":                    string(daemonRecoverErrorOccurrenceStoreLockStranded),
		"operation":               event.Operation,
		"data_outcome":            event.DataOutcome,
		"occurrence_store_health": event.Snapshot.State,
		"revision":                event.Snapshot.Revision,
		"restart_required":        event.Snapshot.RestartRequired,
	})
}

func newAuditLockCorrelationID() (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	hexValue := make([]byte, 32)
	hex.Encode(hexValue, raw[:])
	return string(hexValue[:8]) + "-" +
		string(hexValue[8:12]) + "-" +
		string(hexValue[12:16]) + "-" +
		string(hexValue[16:20]) + "-" +
		string(hexValue[20:]), nil
}

func validateAuditLockCorrelationValue(field, value string) *auditLockRouteError {
	if len(value) != 36 {
		return &auditLockRouteError{status: 400, code: string(daemonRecoverErrorCorrelationInvalid)}
	}
	for i := 0; i < len(value); i++ {
		switch i {
		case 8, 13, 18, 23:
			if value[i] != '-' {
				return &auditLockRouteError{status: 400, code: string(daemonRecoverErrorCorrelationInvalid)}
			}
		default:
			if !((value[i] >= '0' && value[i] <= '9') || (value[i] >= 'a' && value[i] <= 'f')) {
				return &auditLockRouteError{status: 400, code: string(daemonRecoverErrorCorrelationInvalid)}
			}
		}
	}
	if value[14] != '4' || (value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b') {
		return &auditLockRouteError{status: 400, code: string(daemonRecoverErrorCorrelationInvalid)}
	}
	_ = field // field is intentionally not echoed to callers or logs.
	return nil
}

func validateAuditLockCorrelation(c auditLockCorrelation) *auditLockRouteError {
	for field, value := range map[string]string{
		"attempt_id":      c.AttemptID,
		"occurrence_id":   c.OccurrenceID,
		"server_instance": c.ServerInstance,
	} {
		if err := validateAuditLockCorrelationValue(field, value); err != nil {
			return err
		}
	}
	return nil
}

func decodeUniqueJSONObject(raw []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := tok.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New("expected JSON object")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("expected JSON object key")
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if tok, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected trailing token %v", tok)
		}
		return nil, err
	}
	return fields, nil
}

func decodeAuditLockCorrelationObject(raw json.RawMessage) (auditLockCorrelation, *auditLockRouteError) {
	fields, err := decodeUniqueJSONObject(raw)
	if err != nil || len(fields) != 3 {
		return auditLockCorrelation{}, &auditLockRouteError{status: 400, code: string(daemonRecoverErrorCorrelationInvalid)}
	}
	var c auditLockCorrelation
	targets := map[string]*string{
		"attempt_id":      &c.AttemptID,
		"occurrence_id":   &c.OccurrenceID,
		"server_instance": &c.ServerInstance,
	}
	for field, target := range targets {
		value, ok := fields[field]
		if !ok || json.Unmarshal(value, target) != nil {
			return auditLockCorrelation{}, &auditLockRouteError{status: 400, code: string(daemonRecoverErrorCorrelationInvalid)}
		}
	}
	for field := range fields {
		if targets[field] == nil {
			return auditLockCorrelation{}, &auditLockRouteError{status: 400, code: string(daemonRecoverErrorCorrelationInvalid)}
		}
	}
	if validationErr := validateAuditLockCorrelation(c); validationErr != nil {
		return auditLockCorrelation{}, validationErr
	}
	return c, nil
}

func cloneAuditLockTerminal(in *auditLockTerminalEvidence) *auditLockTerminalEvidence {
	if in == nil {
		return nil
	}
	out := *in
	if in.Success != nil {
		success := *in.Success
		out.Success = &success
	}
	return &out
}

func auditLockRecordBinding(record auditLockOccurrenceRecord) auditLockOccurrenceBinding {
	return auditLockOccurrenceBinding{
		serverInstance: record.OriginServerInstance,
		taskName:       record.TaskName,
		confirm:        record.Confirm,
	}
}

func auditLockTerminationCommitState(record auditLockOccurrenceRecord) string {
	switch record.Status {
	case auditLockOccurrenceCommittedError:
		return auditLockTerminationStateCommitted
	case auditLockOccurrenceNotCommitted:
		return auditLockTerminationStateNotCommitted
	case auditLockOccurrenceCommittedSuccess:
		if record.Success != nil && record.Success.TerminationCommitted {
			return auditLockTerminationStateCommitted
		}
		return auditLockTerminationStateNotCommitted
	default:
		return auditLockTerminationStateUnknown
	}
}

func auditLockReceiptFromRecord(record auditLockOccurrenceRecord) auditLockReceiptDTO {
	return auditLockReceiptDTO{
		AttemptID:              record.AttemptID,
		OccurrenceID:           record.OccurrenceID,
		ServerInstance:         record.OriginServerInstance,
		TaskName:               record.TaskName,
		Status:                 record.Status,
		LockAuthorization:      record.LockAuthorization,
		TerminationCommitState: auditLockTerminationCommitState(record),
	}
}

func auditLockTerminalFromRecord(record auditLockOccurrenceRecord) *auditLockTerminalEvidence {
	switch record.Status {
	case auditLockOccurrenceCommittedSuccess:
		return &auditLockTerminalEvidence{
			HTTPStatus:           record.HTTPStatus,
			TerminationCommitted: record.Success != nil && record.Success.TerminationCommitted,
			Success:              cloneDaemonRecoverSuccess(record.Success),
		}
	case auditLockOccurrenceCommittedError:
		return &auditLockTerminalEvidence{
			HTTPStatus:           record.HTTPStatus,
			ErrorCode:            record.ErrorCode,
			TerminationCommitted: true,
		}
	case auditLockOccurrenceNotCommitted:
		return &auditLockTerminalEvidence{
			HTTPStatus:           record.HTTPStatus,
			ErrorCode:            record.ErrorCode,
			TerminationCommitted: false,
		}
	default:
		return nil
	}
}

func auditLockSettlementKeyFor(generation uint64, record auditLockOccurrenceRecord) auditLockSettlementKey {
	return auditLockSettlementKey{
		Generation:     generation,
		ServerInstance: record.OriginServerInstance,
		AttemptID:      record.AttemptID,
		OccurrenceID:   record.OccurrenceID,
	}
}

func auditLockSettlementKeyForReservation(reservation auditLockReservation) auditLockSettlementKey {
	return auditLockSettlementKey{
		Generation:     reservation.MutationEpoch.Generation,
		ServerInstance: reservation.Receipt.ServerInstance,
		AttemptID:      reservation.Receipt.AttemptID,
		OccurrenceID:   reservation.Receipt.OccurrenceID,
	}
}

func (a *auditLockAdapter) settlementCell(generation uint64, record auditLockOccurrenceRecord) *auditLockSettlementCell {
	value, ok := a.settlements.Load(auditLockSettlementKeyFor(generation, record))
	if !ok {
		return nil
	}
	cell, _ := value.(*auditLockSettlementCell)
	return cell
}

// effectiveOccurrence is the single projection owner for durable occurrence
// state. Durable terminal, uncertain, and consumed records are authoritative;
// only an indexed uncertain cell may project durable in_flight as uncertain.
func (a *auditLockAdapter) effectiveOccurrence(generation uint64, durable auditLockOccurrenceRecord) auditLockOccurrenceRecord {
	if durable.Status != auditLockOccurrenceInFlight {
		return durable
	}
	cell := a.settlementCell(generation, durable)
	settlement := cell.load()
	if settlement != nil && settlement.phase == auditLockSettlementUncertain {
		durable.Status = settlement.receipt.Status
		durable.HTTPStatus = 0
		durable.ErrorCode = ""
		durable.LockAuthorization = settlement.receipt.LockAuthorization
		durable.Success = nil
	}
	return durable
}

func auditLockUncertainReceipt(reservation auditLockReservation) auditLockReceiptDTO {
	receipt := reservation.Receipt
	receipt.Status = auditLockOccurrenceUncertain
	receipt.LockAuthorization = auditLockAuthorizationUncertain
	receipt.TerminationCommitState = auditLockTerminationStateUnknown
	return receipt
}

func (a *auditLockAdapter) publishUncertainSettlement(reservation auditLockReservation) *auditLockSettlementSnapshot {
	return reservation.Settlement.publish(auditLockSettlementUncertain, auditLockUncertainReceipt(reservation))
}

func (a *auditLockAdapter) publishDurableSettlement(reservation auditLockReservation, receipt auditLockReceiptDTO) {
	reservation.Settlement.publish(auditLockSettlementDurableTerminal, receipt)
	a.settlements.CompareAndDelete(auditLockSettlementKeyForReservation(reservation), reservation.Settlement)
}

func (a *auditLockAdapter) clearSettlement(generation uint64, record auditLockOccurrenceRecord) {
	a.settlements.Delete(auditLockSettlementKeyFor(generation, record))
}

func cloneDaemonRecoverSuccess(in *daemonRecoverSuccessEvidence) *daemonRecoverSuccessEvidence {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func decodeRequiredObject(raw []byte, required ...string) (map[string]json.RawMessage, error) {
	fields, err := decodeUniqueJSONObject(raw)
	if err != nil {
		return nil, err
	}
	if len(fields) != len(required) {
		return nil, fmt.Errorf("JSON object has %d fields, want %d", len(fields), len(required))
	}
	for _, field := range required {
		if _, ok := fields[field]; !ok {
			return nil, fmt.Errorf("JSON object missing field %q", field)
		}
	}
	return fields, nil
}

func decodeAuditLockSuccess(raw json.RawMessage) (*daemonRecoverSuccessEvidence, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	fields, err := decodeRequiredObject(raw,
		"task_name",
		"reaped",
		"port_owner_check",
		"port_wait_outcome",
		"audit_handoff",
		"termination_committed",
	)
	if err != nil {
		return nil, err
	}
	success := &daemonRecoverSuccessEvidence{}
	targets := map[string]any{
		"task_name":             &success.TaskName,
		"reaped":                &success.Reaped,
		"port_owner_check":      &success.PortOwnerCheck,
		"port_wait_outcome":     &success.PortWaitOutcome,
		"audit_handoff":         &success.AuditHandoff,
		"termination_committed": &success.TerminationCommitted,
	}
	for field, target := range targets {
		if err := json.Unmarshal(fields[field], target); err != nil {
			return nil, fmt.Errorf("decode success field %s: %w", field, err)
		}
	}
	return success, nil
}

func decodeAuditLockRecord(raw json.RawMessage) (auditLockOccurrenceRecord, error) {
	fields, err := decodeRequiredObject(raw,
		"origin_server_instance",
		"attempt_id",
		"occurrence_id",
		"task_name",
		"confirm",
		"status",
		"http_status",
		"error_code",
		"lock_authorization",
		"success",
	)
	if err != nil {
		return auditLockOccurrenceRecord{}, err
	}
	var record auditLockOccurrenceRecord
	targets := map[string]any{
		"origin_server_instance": &record.OriginServerInstance,
		"attempt_id":             &record.AttemptID,
		"occurrence_id":          &record.OccurrenceID,
		"task_name":              &record.TaskName,
		"confirm":                &record.Confirm,
		"status":                 &record.Status,
		"http_status":            &record.HTTPStatus,
		"error_code":             &record.ErrorCode,
		"lock_authorization":     &record.LockAuthorization,
	}
	for field, target := range targets {
		if err := json.Unmarshal(fields[field], target); err != nil {
			return auditLockOccurrenceRecord{}, fmt.Errorf("decode occurrence field %s: %w", field, err)
		}
	}
	record.Success, err = decodeAuditLockSuccess(fields["success"])
	if err != nil {
		return auditLockOccurrenceRecord{}, fmt.Errorf("decode occurrence success: %w", err)
	}
	return record, nil
}

func decodeAuditLockStore(raw []byte) (auditLockOccurrenceStore, error) {
	if len(raw) > auditLockOccurrenceStoreMaxBytes {
		return auditLockOccurrenceStore{}, errors.New("occurrence store exceeds one-mebibyte cap")
	}
	fields, err := decodeRequiredObject(raw,
		"version",
		"generation",
		"active_server_instance",
		"records",
	)
	if err != nil {
		return auditLockOccurrenceStore{}, err
	}
	var store auditLockOccurrenceStore
	if err := json.Unmarshal(fields["version"], &store.Version); err != nil {
		return auditLockOccurrenceStore{}, fmt.Errorf("decode version: %w", err)
	}
	if err := json.Unmarshal(fields["generation"], &store.Generation); err != nil {
		return auditLockOccurrenceStore{}, fmt.Errorf("decode generation: %w", err)
	}
	if err := json.Unmarshal(fields["active_server_instance"], &store.ActiveServerInstance); err != nil {
		return auditLockOccurrenceStore{}, fmt.Errorf("decode active_server_instance: %w", err)
	}
	var records []json.RawMessage
	if err := json.Unmarshal(fields["records"], &records); err != nil {
		return auditLockOccurrenceStore{}, fmt.Errorf("decode records: %w", err)
	}
	store.Records = make([]auditLockOccurrenceRecord, 0, len(records))
	for index, recordRaw := range records {
		record, recordErr := decodeAuditLockRecord(recordRaw)
		if recordErr != nil {
			return auditLockOccurrenceStore{}, fmt.Errorf("decode record %d: %w", index, recordErr)
		}
		store.Records = append(store.Records, record)
	}
	if err := validateAuditLockStore(store); err != nil {
		return auditLockOccurrenceStore{}, err
	}
	return store, nil
}

func validateOpaqueTaskName(taskName string) error {
	if taskName == "" || len(taskName) > auditLockTaskNameMaxBytes || !utf8.ValidString(taskName) {
		return errors.New("task_name is empty, oversized, or invalid UTF-8")
	}
	if strings.TrimSpace(taskName) != taskName {
		return errors.New("task_name contains leading or trailing whitespace")
	}
	for _, r := range taskName {
		if r < 0x20 || r == 0x7f {
			return errors.New("task_name contains a control character")
		}
	}
	return nil
}

func validateAuditLockStore(store auditLockOccurrenceStore) error {
	if store.Version != auditLockOccurrenceStoreVersion {
		return fmt.Errorf("unsupported daemon recovery occurrence version %d", store.Version)
	}
	if store.Generation == 0 {
		return errors.New("daemon recovery occurrence generation is zero")
	}
	if err := validateAuditLockCorrelationValue("active_server_instance", store.ActiveServerInstance); err != nil {
		return errors.New("active_server_instance is not a canonical UUIDv4")
	}
	if len(store.Records) > auditLockOccurrenceCapacity {
		return fmt.Errorf("daemon recovery occurrence count %d exceeds cap %d", len(store.Records), auditLockOccurrenceCapacity)
	}
	attemptIDs := map[string]struct{}{}
	occurrenceIDs := map[string]struct{}{}
	unresolvedTasks := map[string]struct{}{}
	for index, record := range store.Records {
		if err := validateAuditLockCorrelation(auditLockCorrelation{
			AttemptID:      record.AttemptID,
			OccurrenceID:   record.OccurrenceID,
			ServerInstance: record.OriginServerInstance,
		}); err != nil {
			return fmt.Errorf("record %d has invalid correlation", index)
		}
		if _, duplicate := attemptIDs[record.AttemptID]; duplicate {
			return fmt.Errorf("record %d duplicates attempt_id", index)
		}
		if _, duplicate := occurrenceIDs[record.OccurrenceID]; duplicate {
			return fmt.Errorf("record %d duplicates occurrence_id", index)
		}
		attemptIDs[record.AttemptID] = struct{}{}
		occurrenceIDs[record.OccurrenceID] = struct{}{}
		if err := validateOpaqueTaskName(record.TaskName); err != nil {
			return fmt.Errorf("record %d: %w", index, err)
		}
		if !record.Confirm {
			return fmt.Errorf("record %d is not explicitly confirmed", index)
		}
		if !auditLockKnownValue(auditLockAuthorizations[:], record.LockAuthorization) {
			return fmt.Errorf("record %d contains invalid lock authorization", index)
		}
		if !auditLockKnownValue(auditLockOccurrenceStatuses[:], record.Status) {
			return fmt.Errorf("record %d has invalid status %q", index, record.Status)
		}
		if record.Status != auditLockOccurrenceConsumed {
			if _, duplicate := unresolvedTasks[record.TaskName]; duplicate {
				return fmt.Errorf("record %d duplicates unresolved task binding", index)
			}
			unresolvedTasks[record.TaskName] = struct{}{}
		}
		switch record.Status {
		case auditLockOccurrenceInFlight:
			if record.HTTPStatus != 0 || record.ErrorCode != "" || record.Success != nil || record.LockAuthorization != auditLockAuthorizationNone {
				return fmt.Errorf("record %d has invalid in-flight evidence", index)
			}
		case auditLockOccurrenceCommittedSuccess:
			if record.HTTPStatus != 200 || record.ErrorCode != "" || record.Success == nil {
				return fmt.Errorf("record %d has invalid committed-success evidence", index)
			}
			if record.Success.TaskName != record.TaskName ||
				!daemonrecovery.PortOwnerCheck(record.Success.PortOwnerCheck).Valid() ||
				!daemonrecovery.PortWaitOutcome(record.Success.PortWaitOutcome).Valid() ||
				!daemonrecovery.AuditHandoff(record.Success.AuditHandoff).Valid() ||
				auditLockAuthorization(
					daemonrecovery.AuditHandoff(record.Success.AuditHandoff),
					record.Success.TerminationCommitted,
				) != record.LockAuthorization {
				return fmt.Errorf("record %d has invalid success evidence", index)
			}
		case auditLockOccurrenceCommittedError, auditLockOccurrenceNotCommitted:
			if record.HTTPStatus < 400 || record.HTTPStatus > 599 ||
				!daemonRecoverErrorCode(record.ErrorCode).Valid() || record.Success != nil {
				return fmt.Errorf("record %d has invalid error evidence", index)
			}
			if record.Status == auditLockOccurrenceNotCommitted && record.LockAuthorization != auditLockAuthorizationNone {
				return fmt.Errorf("record %d authorizes a noncommitted result", index)
			}
		case auditLockOccurrenceUncertain:
			if record.HTTPStatus != 0 || record.ErrorCode != "" || record.Success != nil || record.LockAuthorization != auditLockAuthorizationUncertain {
				return fmt.Errorf("record %d has invalid uncertain evidence", index)
			}
		case auditLockOccurrenceConsumed:
			if record.HTTPStatus != 0 || record.ErrorCode != "" || record.Success != nil || record.LockAuthorization != auditLockAuthorizationNone {
				return fmt.Errorf("record %d has invalid consumed tombstone", index)
			}
		default:
			return fmt.Errorf("record %d has invalid status %q", index, record.Status)
		}
	}
	return nil
}

func (a *auditLockAdapter) withStoreLock(ctx context.Context, operation string, fn func(*auditLockStoreOperation) error) error {
	return a.withStoreLockFailure(ctx, operation, fn, nil)
}

// withStoreLockFailure runs the failure finalizer on every failure path. Before
// storeMu acquisition the finalizer must be lock-free; after acquisition it may
// additionally use the inode-anchored proof available under the physical lock.
func (a *auditLockAdapter) withStoreLockFailure(
	ctx context.Context,
	operation string,
	fn func(*auditLockStoreOperation) error,
	onFailure func(error),
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	healthLease, healthErr := a.storeLockHealth.begin(operation)
	if healthErr != nil {
		if onFailure != nil {
			onFailure(healthErr)
		}
		return healthErr
	}
	finishWithoutPhysicalLock := func(operationErr error, outcome occurrenceStoreDataOutcome) error {
		strandedErr := healthLease.finish(nil, outcome)
		var existingStranded *occurrenceStoreLockStrandedError
		if strandedErr != nil && !errors.As(operationErr, &existingStranded) {
			operationErr = errors.Join(operationErr, strandedErr)
		}
		if onFailure != nil {
			onFailure(operationErr)
		}
		return operationErr
	}
	lockCtx, cancel := context.WithTimeout(ctx, a.lockTimeout)
	defer cancel()
	for {
		if strandedErr := healthLease.stranded(occurrenceStoreDataNotEntered); strandedErr != nil {
			return finishWithoutPhysicalLock(strandedErr, occurrenceStoreDataNotEntered)
		}
		if a.storeMu.TryLock() {
			// Close failure is published before the releasing operation unlocks
			// storeMu. Re-check after acquisition so a waiter that was descheduled
			// between the pre-check and TryLock cannot enter the physical store.
			if strandedErr := healthLease.stranded(occurrenceStoreDataNotEntered); strandedErr != nil {
				a.storeMu.Unlock()
				return finishWithoutPhysicalLock(strandedErr, occurrenceStoreDataNotEntered)
			}
			break
		}
		select {
		case <-lockCtx.Done():
			operationErr := fmt.Errorf("%s: acquire in-process occurrence lock: %w", operation, lockCtx.Err())
			return finishWithoutPhysicalLock(operationErr, occurrenceStoreDataNotEntered)
		case <-time.After(auditLockStoreLockRetry):
		}
	}
	if err := os.MkdirAll(filepath.Dir(a.storePath), 0o700); err != nil {
		operationErr := fmt.Errorf("%s: create state directory: %w", operation, err)
		a.storeMu.Unlock()
		return finishWithoutPhysicalLock(operationErr, occurrenceStoreDataNotEntered)
	}
	lock := a.storeLockHealth.newLock(a.storePath + ".lock")
	locked, err := lock.TryLockContext(lockCtx, auditLockStoreLockRetry)
	if err != nil {
		operationErr := fmt.Errorf("%s: acquire occurrence lock: %w", operation, err)
		if closeErr := lock.Close(); closeErr != nil {
			operationErr = errors.Join(operationErr, fmt.Errorf("%s: close unacquired occurrence lock: %w", operation, closeErr))
		}
		a.storeMu.Unlock()
		return finishWithoutPhysicalLock(operationErr, occurrenceStoreDataNotEntered)
	}
	if !locked {
		operationErr := fmt.Errorf("%s: occurrence lock unavailable", operation)
		if closeErr := lock.Close(); closeErr != nil {
			operationErr = errors.Join(operationErr, fmt.Errorf("%s: close unacquired occurrence lock: %w", operation, closeErr))
		}
		a.storeMu.Unlock()
		return finishWithoutPhysicalLock(operationErr, occurrenceStoreDataNotEntered)
	}
	storeOperation := &auditLockStoreOperation{dataOutcome: occurrenceStoreDataUnproven}
	opErr := fn(storeOperation)
	if opErr != nil && onFailure != nil {
		onFailure(opErr)
	}
	closeErr := lock.Close()
	var strandedEvent *occurrenceStoreLockHealthEvent
	if closeErr != nil {
		healthErr, strandedEvent = healthLease.poison(closeErr, storeOperation.dataOutcome)
	}
	a.storeMu.Unlock()
	finishedHealthErr := healthLease.finish(lock, storeOperation.dataOutcome)
	if healthErr == nil {
		healthErr = finishedHealthErr
	}
	a.storeLockHealth.emitEvent(strandedEvent)
	if opErr != nil {
		if healthErr != nil {
			return errors.Join(opErr, healthErr)
		}
		return opErr
	}
	if healthErr != nil {
		return healthErr
	}
	return nil
}

func (a *auditLockAdapter) readStoreLockHeld(allowMissing bool) (auditLockOccurrenceStore, error) {
	raw, err := api.ReadStateFileInodeAnchored(a.storePath)
	if os.IsNotExist(err) && allowMissing {
		return auditLockOccurrenceStore{Version: auditLockOccurrenceStoreVersion}, nil
	}
	if err != nil {
		return auditLockOccurrenceStore{}, fmt.Errorf("read occurrence store: %w", err)
	}
	store, err := decodeAuditLockStore(raw)
	if err != nil {
		return auditLockOccurrenceStore{}, fmt.Errorf("decode occurrence store: %w", err)
	}
	return store, nil
}

func (a *auditLockAdapter) writeStoreLockHeld(store auditLockOccurrenceStore) error {
	if err := validateAuditLockStore(store); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal occurrence store: %w", err)
	}
	raw = append(raw, '\n')
	if len(raw) > auditLockOccurrenceStoreMaxBytes {
		return errors.New("occurrence store exceeds one-mebibyte cap")
	}
	writer := a.writeStateFileLockHeld
	if writer == nil {
		writer = api.WriteStateFileBytesLockHeld
	}
	if err := writer(a.storePath, raw); err != nil {
		return fmt.Errorf("write occurrence store: %w", err)
	}
	return nil
}

type auditLockStoreWriteOutcome uint8

const (
	auditLockStoreWriteDurable auditLockStoreWriteOutcome = iota + 1
	auditLockStoreWriteNotPublished
	auditLockStoreWriteUncertain
)

type auditLockStoreWriteResult struct {
	outcome           auditLockStoreWriteOutcome
	cause             error
	durableAfterError bool
}

func cloneAuditLockOccurrenceStore(store auditLockOccurrenceStore) auditLockOccurrenceStore {
	clone := store
	clone.Records = make([]auditLockOccurrenceRecord, len(store.Records))
	for index, record := range store.Records {
		clone.Records[index] = record
		clone.Records[index].Success = cloneDaemonRecoverSuccess(record.Success)
	}
	return clone
}

// writeStoreWithLockedReconciliation owns the one write attempt and, only
// after a writer error, one exact inode-anchored reread while both occurrence
// locks are still held. Callers supply independent before/intended values so
// slice aliasing cannot make either equality proof vacuous.
func (a *auditLockAdapter) writeStoreWithLockedReconciliation(
	operation *auditLockStoreOperation,
	before auditLockOccurrenceStore,
	intended auditLockOccurrenceStore,
) auditLockStoreWriteResult {
	before = cloneAuditLockOccurrenceStore(before)
	intended = cloneAuditLockOccurrenceStore(intended)
	writeErr := a.writeStoreLockHeld(intended)
	if writeErr == nil {
		operation.proveDurable()
		return auditLockStoreWriteResult{outcome: auditLockStoreWriteDurable}
	}
	actual, readErr := a.readStoreLockHeld(false)
	if readErr != nil {
		return auditLockStoreWriteResult{
			outcome: auditLockStoreWriteUncertain,
			cause:   errors.Join(writeErr, fmt.Errorf("reread occurrence store after writer error: %w", readErr)),
		}
	}
	switch {
	case reflect.DeepEqual(actual, intended):
		operation.proveDurable()
		return auditLockStoreWriteResult{outcome: auditLockStoreWriteDurable, cause: writeErr, durableAfterError: true}
	case reflect.DeepEqual(actual, before):
		return auditLockStoreWriteResult{outcome: auditLockStoreWriteNotPublished, cause: writeErr}
	default:
		return auditLockStoreWriteResult{
			outcome: auditLockStoreWriteUncertain,
			cause:   errors.Join(writeErr, errors.New("occurrence store matched neither pre-write nor intended state")),
		}
	}
}

func (a *auditLockAdapter) publishStoreWriteReconciled(operation string) {
	if a == nil || a.events == nil {
		return
	}
	a.events.Publish(Event{Type: "daemon-recovery-occurrence-store-write-reconciled", Body: map[string]any{
		"operation":    operation,
		"failure_id":   "post_rename_exact_reread",
		"data_outcome": "durable_proven",
	}})
}

func (a *auditLockAdapter) claimStore(ctx context.Context) error {
	var claimed auditLockOccurrenceStore
	reconciledAfterError := false
	err := a.withStoreLock(ctx, "claim occurrence store", func(operation *auditLockStoreOperation) error {
		store, err := a.readStoreLockHeld(true)
		if err != nil {
			return err
		}
		if store.Generation == math.MaxUint64 {
			return errors.New("occurrence generation overflow")
		}
		before := cloneAuditLockOccurrenceStore(store)
		intended := cloneAuditLockOccurrenceStore(store)
		records := intended.Records[:0]
		for _, record := range intended.Records {
			if record.Status == auditLockOccurrenceConsumed {
				continue
			}
			if record.Status == auditLockOccurrenceInFlight {
				record.Status = auditLockOccurrenceUncertain
				record.LockAuthorization = auditLockAuthorizationUncertain
				record.HTTPStatus = 0
				record.ErrorCode = ""
				record.Success = nil
			}
			records = append(records, record)
		}
		intended.Records = records
		intended.Version = auditLockOccurrenceStoreVersion
		intended.Generation++
		intended.ActiveServerInstance = a.serverInstance
		writeResult := a.writeStoreWithLockedReconciliation(operation, before, intended)
		switch writeResult.outcome {
		case auditLockStoreWriteDurable:
			reconciledAfterError = writeResult.durableAfterError
		case auditLockStoreWriteNotPublished, auditLockStoreWriteUncertain:
			return writeResult.cause
		default:
			return errors.New("unknown occurrence-store write outcome")
		}
		claimed = intended
		return nil
	})
	if reconciledAfterError {
		a.publishStoreWriteReconciled("claim occurrence store")
	}
	if err != nil {
		return err
	}
	a.generation = claimed.Generation
	return nil
}

func (a *auditLockAdapter) ensureReady() *auditLockRouteError {
	if a == nil {
		return &auditLockRouteError{status: 500, code: string(daemonRecoverErrorAuditLockAdapterInit)}
	}
	a.activationMu.Lock()
	initErr := a.initErr
	storeClaimed := a.storeClaimed
	a.activationMu.Unlock()
	if initErr != nil || !storeClaimed {
		if a != nil {
			if healthErr := a.storeLockHealth.strandedError("claim occurrence store", occurrenceStoreDataDurableProven); healthErr != nil {
				return auditLockRouteErrorFromStoreError(healthErr, 500, daemonRecoverErrorAuditLockAdapterInit)
			}
			if initErr == nil {
				initErr = errors.New("daemon recovery occurrence store is not activated")
			}
			return &auditLockRouteError{status: 500, code: string(daemonRecoverErrorAuditLockAdapterInit), cause: initErr}
		}
	}
	return nil
}

func auditLockRouteErrorFromStoreError(err error, fallbackStatus int, fallbackCode daemonRecoverErrorCode) *auditLockRouteError {
	var routeErr *auditLockRouteError
	errors.As(err, &routeErr)
	var healthErr *occurrenceStoreLockStrandedError
	if errors.As(err, &healthErr) {
		health := healthErr.Health
		promoted := &auditLockRouteError{
			status:                503,
			code:                  string(daemonRecoverErrorOccurrenceStoreLockStranded),
			cause:                 err,
			occurrenceStoreHealth: &health,
		}
		if routeErr != nil {
			promoted.auditLockStateProjection = routeErr.auditLockStateProjection
		}
		return promoted
	}
	if routeErr != nil {
		copy := *routeErr
		copy.cause = err
		return &copy
	}
	return &auditLockRouteError{status: fallbackStatus, code: string(fallbackCode), cause: err}
}

func (a *auditLockAdapter) currentMutationEpoch() auditLockMutationEpoch {
	a.mu.Lock()
	defer a.mu.Unlock()
	return auditLockMutationEpoch{
		ServerInstance: a.serverInstance,
		Generation:     a.generation,
	}
}

func (a *auditLockAdapter) currentIdentity() (string, uint64) {
	epoch := a.currentMutationEpoch()
	return epoch.ServerInstance, epoch.Generation
}

func (a *auditLockAdapter) withOccurrenceStoreHealth(dto auditLockStateDTO) auditLockStateDTO {
	health := a.storeLockHealth.snapshot()
	dto.OccurrenceStoreHealth = health.State
	dto.OccurrenceStoreHealthRevision = health.Revision
	dto.RestartRequired = health.RestartRequired
	return dto
}

func (a *auditLockAdapter) reserve(ctx context.Context, c auditLockCorrelation, binding auditLockOccurrenceBinding) (auditLockReservation, *auditLockRouteError) {
	if err := a.ensureReady(); err != nil {
		return auditLockReservation{}, err
	}
	if err := validateAuditLockCorrelation(c); err != nil || binding.serverInstance != c.ServerInstance ||
		binding.taskName == "" || !binding.confirm || validateOpaqueTaskName(binding.taskName) != nil {
		return auditLockReservation{}, &auditLockRouteError{status: 400, code: string(daemonRecoverErrorCorrelationInvalid)}
	}
	a.mu.Lock()
	closing := a.closing
	mutationEpoch := auditLockMutationEpoch{
		ServerInstance: a.serverInstance,
		Generation:     a.generation,
	}
	a.mu.Unlock()
	if closing {
		return auditLockReservation{}, &auditLockRouteError{status: 503, code: string(daemonRecoverErrorOccurrenceCapacity)}
	}

	var reservation auditLockReservation
	var reconciledAfterError bool
	err := a.withStoreLock(ctx, "reserve occurrence", func(operation *auditLockStoreOperation) error {
		store, err := a.readStoreLockHeld(false)
		if err != nil {
			return err
		}
		for _, record := range store.Records {
			if record.AttemptID != c.AttemptID && record.OccurrenceID != c.OccurrenceID {
				continue
			}
			exact := record.AttemptID == c.AttemptID &&
				record.OccurrenceID == c.OccurrenceID &&
				record.OriginServerInstance == c.ServerInstance &&
				auditLockRecordBinding(record) == binding
			if !exact {
				return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorAttemptConflict)}
			}
			effective := a.effectiveOccurrence(store.Generation, record)
			if effective.Status == auditLockOccurrenceConsumed {
				return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOccurrenceConsumed)}
			}
			reservation = auditLockReservation{
				Receipt:       auditLockReceiptFromRecord(effective),
				Terminal:      auditLockTerminalFromRecord(effective),
				Binding:       binding,
				MutationEpoch: mutationEpoch,
				Settlement:    a.settlementCell(store.Generation, record),
			}
			return nil
		}
		if c.ServerInstance != mutationEpoch.ServerInstance || !mutationEpoch.matchesStore(store) {
			return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorBaselineStale)}
		}
		for _, record := range store.Records {
			if record.Status != auditLockOccurrenceConsumed && record.TaskName == binding.taskName {
				return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorAttemptConflict)}
			}
		}
		if len(store.Records) >= auditLockOccurrenceCapacity {
			return &auditLockRouteError{status: 503, code: string(daemonRecoverErrorOccurrenceCapacity)}
		}
		record := auditLockOccurrenceRecord{
			OriginServerInstance: c.ServerInstance,
			AttemptID:            c.AttemptID,
			OccurrenceID:         c.OccurrenceID,
			TaskName:             binding.taskName,
			Confirm:              binding.confirm,
			Status:               auditLockOccurrenceInFlight,
			LockAuthorization:    auditLockAuthorizationNone,
		}
		before := cloneAuditLockOccurrenceStore(store)
		intended := cloneAuditLockOccurrenceStore(store)
		intended.Records = append(intended.Records, record)
		writeResult := a.writeStoreWithLockedReconciliation(operation, before, intended)
		switch writeResult.outcome {
		case auditLockStoreWriteDurable:
			reconciledAfterError = writeResult.durableAfterError
		case auditLockStoreWriteNotPublished:
			return writeResult.cause
		case auditLockStoreWriteUncertain:
			return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOutcomeUncertain), cause: writeResult.cause}
		default:
			return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOutcomeUncertain), cause: errors.New("unknown occurrence write reconciliation outcome")}
		}
		receipt := auditLockReceiptFromRecord(record)
		settlement := newAuditLockSettlementCell(receipt)
		a.settlements.Store(auditLockSettlementKeyFor(intended.Generation, record), settlement)
		reservation = auditLockReservation{
			Novel:         true,
			Receipt:       receipt,
			Binding:       binding,
			MutationEpoch: mutationEpoch,
			Settlement:    settlement,
		}
		return nil
	})
	if reconciledAfterError {
		a.publishStoreWriteReconciled("reserve occurrence")
	}
	if err != nil {
		routeErr := auditLockRouteErrorFromStoreError(err, 500, daemonRecoverErrorAuditLockAdapterInit)
		var healthErr *occurrenceStoreLockStrandedError
		if errors.As(err, &healthErr) && healthErr.DataOutcome == occurrenceStoreDataDurableProven && reservation.Novel {
			projection := a.snapshotProjection(&reservation.Receipt)
			routeErr.auditLockStateProjection = &projection
		}
		return reservation, routeErr
	}
	return reservation, nil
}

func (a *auditLockAdapter) terminalize(reservation auditLockReservation, receiptStatus, authorization string, terminal auditLockTerminalEvidence) (auditLockReceiptDTO, *auditLockRouteError) {
	// Unit tests exercise the transaction directly through terminalizeContext;
	// production crosses a process boundary so an uninterruptible secure write
	// cannot strand this process's mutex, request, or recovery lease.
	switch a.terminalization.mode {
	case auditLockTerminalizationDirectTest:
		return a.terminalizeContext(context.Background(), reservation, receiptStatus, authorization, terminal)
	case auditLockTerminalizationBounded:
		return a.terminalizeBounded(reservation, receiptStatus, authorization, terminal)
	default:
		return reservation.Receipt, &auditLockRouteError{status: 500, code: string(daemonRecoverErrorAuditLockAdapterInit), cause: a.terminalization.validate()}
	}
}

func (a *auditLockAdapter) terminalizeBounded(reservation auditLockReservation, receiptStatus, authorization string, terminal auditLockTerminalEvidence) (auditLockReceiptDTO, *auditLockRouteError) {
	if a.currentMutationEpoch() != reservation.MutationEpoch {
		return reservation.Receipt, &auditLockRouteError{status: 409, code: string(daemonRecoverErrorBaselineStale)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.terminalizationBudget)
	defer cancel()
	for !a.storeMu.TryLock() {
		select {
		case <-ctx.Done():
			a.publishUncertainSettlement(reservation)
			a.publishTerminalWorkerFailure(reservation, auditLockTerminalWorkerFailureTimeout, 0, false, 0, false)
			return auditLockUncertainReceipt(reservation), &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOutcomeUncertain), cause: ctx.Err()}
		case <-time.After(auditLockStoreLockRetry):
		}
	}
	defer a.storeMu.Unlock()
	remaining := time.Until(deadlineFromContext(ctx))
	allowanceMS := remaining.Milliseconds()
	if allowanceMS <= 0 {
		a.publishUncertainSettlement(reservation)
		a.publishTerminalWorkerFailure(reservation, auditLockTerminalWorkerFailureTimeout, 0, false, 0, false)
		return auditLockUncertainReceipt(reservation), &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOutcomeUncertain), cause: context.DeadlineExceeded}
	}
	result, err := a.terminalization.runner(ctx, auditLockTerminalWorkerRequest{
		Version:       auditLockTerminalWorkerProtocolVersion,
		Receipt:       reservation.Receipt,
		Generation:    reservation.MutationEpoch.Generation,
		Confirm:       reservation.Binding.confirm,
		Status:        receiptStatus,
		Authorization: authorization,
		Terminal:      terminal,
		AllowanceMS:   allowanceMS,
	})
	if err != nil {
		a.publishUncertainSettlement(reservation)
		failureID := auditLockTerminalWorkerFailureExecutionFailed
		stdoutBytes, stderrBytes := 0, 0
		stdoutTruncated, stderrTruncated := false, false
		var workerErr *auditLockTerminalWorkerRunError
		if errors.As(err, &workerErr) {
			failureID = workerErr.failure
			stdoutBytes, stdoutTruncated = workerErr.stdout.bytes, workerErr.stdout.truncated
			stderrBytes, stderrTruncated = workerErr.stderr.bytes, workerErr.stderr.truncated
		}
		var strictErr *process.StrictRunError
		if errors.As(err, &strictErr) {
			switch strictErr.Kind {
			case process.StrictRunTimeout:
				failureID = auditLockTerminalWorkerFailureTimeout
			case process.StrictRunContainmentFailed:
				failureID = auditLockTerminalWorkerFailureContainmentFailed
			}
		}
		a.publishTerminalWorkerFailure(reservation, failureID, stdoutBytes, stdoutTruncated, stderrBytes, stderrTruncated)
		return auditLockUncertainReceipt(reservation), &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOutcomeUncertain), cause: err}
	}
	if result.Version != auditLockTerminalWorkerProtocolVersion {
		a.publishUncertainSettlement(reservation)
		a.publishTerminalWorkerFailure(reservation, auditLockTerminalWorkerFailureProtocolInvalid, 0, false, 0, false)
		return auditLockUncertainReceipt(reservation), &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOutcomeUncertain), cause: errors.New("terminal worker version mismatch")}
	}
	switch result.Outcome {
	case "durable_terminal":
		if result.Failure != "" || result.Status != 0 || result.Code != "" || result.Receipt.AttemptID != reservation.Receipt.AttemptID ||
			result.Receipt.OccurrenceID != reservation.Receipt.OccurrenceID ||
			result.Receipt.ServerInstance != reservation.Receipt.ServerInstance ||
			result.Receipt.TaskName != reservation.Receipt.TaskName ||
			result.Receipt.Status != receiptStatus ||
			result.Receipt.LockAuthorization != authorization {
			a.publishUncertainSettlement(reservation)
			a.publishTerminalWorkerFailure(reservation, auditLockTerminalWorkerFailureProtocolInvalid, 0, false, 0, false)
			return auditLockUncertainReceipt(reservation), &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOutcomeUncertain), cause: errors.New("terminal worker durable receipt contradicts reservation")}
		}
		a.publishDurableSettlement(reservation, result.Receipt)
		return result.Receipt, nil
	case "baseline_stale":
		if result.Failure != "" || result.Status != 409 || result.Code != string(daemonRecoverErrorBaselineStale) {
			a.publishUncertainSettlement(reservation)
			a.publishTerminalWorkerFailure(reservation, auditLockTerminalWorkerFailureProtocolInvalid, 0, false, 0, false)
			return auditLockUncertainReceipt(reservation), &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOutcomeUncertain)}
		}
		return reservation.Receipt, &auditLockRouteError{status: 409, code: string(daemonRecoverErrorBaselineStale)}
	case "uncertain", "rejected":
		failureID, ok := auditLockTerminalWorkerFailureID(result.Failure)
		validUncertain := result.Outcome == "uncertain" && ((failureID == auditLockTerminalWorkerFailureStateDirFailed && result.Status == 0 && result.Code == "") ||
			(failureID == auditLockTerminalWorkerFailureUnproved && result.Status == 409 && result.Code == string(daemonRecoverErrorOutcomeUncertain)))
		if !ok || (result.Outcome == "rejected" && failureID != auditLockTerminalWorkerFailureProtocolInvalid) || (result.Outcome == "uncertain" && !validUncertain) {
			failureID = auditLockTerminalWorkerFailureProtocolInvalid
		}
		a.publishUncertainSettlement(reservation)
		a.publishTerminalWorkerFailure(reservation, failureID, 0, false, 0, false)
		return auditLockUncertainReceipt(reservation), &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOutcomeUncertain)}
	default:
		a.publishUncertainSettlement(reservation)
		a.publishTerminalWorkerFailure(reservation, auditLockTerminalWorkerFailureProtocolInvalid, 0, false, 0, false)
		return auditLockUncertainReceipt(reservation), &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOutcomeUncertain)}
	}
}

func deadlineFromContext(ctx context.Context) time.Time {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return time.Now()
}

func runAuditLockTerminalWorker(ctx context.Context, request auditLockTerminalWorkerRequest) (auditLockTerminalWorkerResult, error) {
	var result auditLockTerminalWorkerResult
	payload, err := json.Marshal(request)
	if err != nil || len(payload) > auditLockTerminalWorkerMessageMaxBytes {
		return result, newAuditLockTerminalWorkerRunError(auditLockTerminalWorkerFailureProtocolInvalid, fmt.Errorf("marshal bounded terminal worker request: %w", err), nil)
	}
	exe, err := os.Executable()
	if err != nil {
		return result, newAuditLockTerminalWorkerRunError(auditLockTerminalWorkerFailureExecutionFailed, fmt.Errorf("resolve current executable: %w", err), nil)
	}
	cmd := exec.Command(exe, "audit-lock-terminal-worker")
	run, runErr := process.RunStrictlyContained(ctx, process.StrictRunInvocation{
		Command: cmd, Input: payload, InputLimit: auditLockTerminalWorkerMessageMaxBytes,
		StdoutLimit: auditLockTerminalWorkerMessageMaxBytes, StderrLimit: auditLockTerminalWorkerStderrMaxBytes,
	})
	if runErr != nil {
		failure := auditLockTerminalWorkerFailureExecutionFailed
		var strictErr *process.StrictRunError
		if errors.As(runErr, &strictErr) {
			switch strictErr.Kind {
			case process.StrictRunTimeout:
				failure = auditLockTerminalWorkerFailureTimeout
			case process.StrictRunContainmentFailed:
				failure = auditLockTerminalWorkerFailureContainmentFailed
			}
		}
		return result, newAuditLockTerminalWorkerRunError(failure, runErr, &run)
	}
	if run.Stdout.Truncated {
		return result, newAuditLockTerminalWorkerRunError(auditLockTerminalWorkerFailureProtocolInvalid, errors.New("terminal worker result exceeds bound"), &run)
	}
	decoder := json.NewDecoder(bytes.NewReader(run.Stdout.Prefix))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, newAuditLockTerminalWorkerRunError(auditLockTerminalWorkerFailureProtocolInvalid, err, &run)
	}
	if err := ensureAuditLockTerminalWorkerEOF(decoder); err != nil {
		return result, newAuditLockTerminalWorkerRunError(auditLockTerminalWorkerFailureProtocolInvalid, err, &run)
	}
	return result, nil
}

type auditLockTerminalWorkerRunError struct {
	failure auditLockTerminalWorkerFailure
	cause   error
	stdout  auditLockWorkerCapture
	stderr  auditLockWorkerCapture
}

type auditLockWorkerCapture struct {
	bytes     int
	truncated bool
}

func newAuditLockTerminalWorkerRunError(failure auditLockTerminalWorkerFailure, cause error, run *process.StrictRunResult) *auditLockTerminalWorkerRunError {
	err := &auditLockTerminalWorkerRunError{failure: failure, cause: cause}
	if run != nil {
		err.stdout = auditLockWorkerCapture{bytes: run.Stdout.Bytes, truncated: run.Stdout.Truncated}
		err.stderr = auditLockWorkerCapture{bytes: run.Stderr.Bytes, truncated: run.Stderr.Truncated}
	}
	return err
}

func (e *auditLockTerminalWorkerRunError) Error() string {
	return "terminal worker " + string(e.failure)
}
func (e *auditLockTerminalWorkerRunError) Unwrap() error { return e.cause }

func (a *auditLockAdapter) publishTerminalWorkerFailure(reservation auditLockReservation, failure auditLockTerminalWorkerFailure, stdoutBytes int, stdoutTruncated bool, stderrBytes int, stderrTruncated bool) {
	if a == nil || a.events == nil {
		return
	}
	a.events.Publish(Event{Type: "daemon-recovery-terminal-worker-failure", Body: map[string]any{
		"failure_id":       string(failure),
		"attempt_id":       reservation.Receipt.AttemptID,
		"occurrence_id":    reservation.Receipt.OccurrenceID,
		"server_instance":  reservation.Receipt.ServerInstance,
		"stdout_bytes":     stdoutBytes,
		"stdout_truncated": stdoutTruncated,
		"stderr_bytes":     stderrBytes,
		"stderr_truncated": stderrTruncated,
	}})
}

func (a *auditLockAdapter) terminalizeContext(ctx context.Context, reservation auditLockReservation, receiptStatus, authorization string, terminal auditLockTerminalEvidence) (auditLockReceiptDTO, *auditLockRouteError) {
	if a.currentMutationEpoch() != reservation.MutationEpoch {
		return reservation.Receipt, &auditLockRouteError{status: 409, code: string(daemonRecoverErrorBaselineStale)}
	}
	var receipt auditLockReceiptDTO
	var requested auditLockOccurrenceRecord
	var mutationEntered bool
	var durableProven bool
	err := a.withStoreLockFailure(ctx, "terminalize occurrence", func(operation *auditLockStoreOperation) error {
		store, err := a.readStoreLockHeld(false)
		if err != nil {
			return err
		}
		if !reservation.MutationEpoch.matchesStore(store) {
			return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorBaselineStale)}
		}
		for index := range store.Records {
			record := &store.Records[index]
			if record.AttemptID != reservation.Receipt.AttemptID ||
				record.OccurrenceID != reservation.Receipt.OccurrenceID ||
				record.OriginServerInstance != reservation.Receipt.ServerInstance ||
				auditLockRecordBinding(*record) != reservation.Binding ||
				record.Status != auditLockOccurrenceInFlight {
				continue
			}
			switch receiptStatus {
			case auditLockOccurrenceCommittedSuccess, auditLockOccurrenceCommittedError, auditLockOccurrenceNotCommitted:
			default:
				return fmt.Errorf("invalid terminal receipt status %q", receiptStatus)
			}
			mutationEntered = true
			record.Status = receiptStatus
			record.HTTPStatus = terminal.HTTPStatus
			record.ErrorCode = terminal.ErrorCode
			record.LockAuthorization = authorization
			record.Success = cloneDaemonRecoverSuccess(terminal.Success)
			requested = *record
			if err := a.writeStoreLockHeld(store); err != nil {
				return err
			}
			operation.proveDurable()
			receipt = auditLockReceiptFromRecord(*record)
			a.publishDurableSettlement(reservation, receipt)
			durableProven = true
			return nil
		}
		return errors.New("in-flight occurrence changed before terminalization")
	}, func(failure error) {
		var routeErr *auditLockRouteError
		if errors.As(failure, &routeErr) && routeErr.code == string(daemonRecoverErrorBaselineStale) {
			return
		}
		// If the mutation closure reached the write, perform one inode-anchored
		// read while both locks are still held. A matching terminal record proves
		// the write despite its returned error; every other result is uncertain.
		if mutationEntered {
			if store, readErr := a.readStoreLockHeld(false); readErr == nil {
				for _, record := range store.Records {
					if record.AttemptID == requested.AttemptID &&
						record.OccurrenceID == requested.OccurrenceID &&
						record.OriginServerInstance == requested.OriginServerInstance &&
						auditLockRecordBinding(record) == reservation.Binding &&
						record.Status == requested.Status &&
						record.HTTPStatus == requested.HTTPStatus &&
						record.ErrorCode == requested.ErrorCode &&
						record.LockAuthorization == requested.LockAuthorization &&
						reflect.DeepEqual(record.Success, requested.Success) {
						receipt = auditLockReceiptFromRecord(record)
						a.publishDurableSettlement(reservation, receipt)
						durableProven = true
						return
					}
				}
			}
		}
		settlement := a.publishUncertainSettlement(reservation)
		if settlement != nil {
			receipt = settlement.receipt
			if settlement.phase == auditLockSettlementDurableTerminal {
				durableProven = true
			}
		}
	})
	if err == nil {
		return receipt, nil
	}
	var healthErr *occurrenceStoreLockStrandedError
	if errors.As(err, &healthErr) && !durableProven {
		if receipt.Status == "" {
			// A route error such as a stale baseline can fail before the
			// destructive mutation is entered. Preserve that in-flight data
			// outcome; only the failure callback may publish uncertainty when
			// the operation genuinely lacks a stronger route classification.
			receipt = reservation.Receipt
		}
		mapped := auditLockRouteErrorFromStoreError(err, 500, daemonRecoverErrorAuditLockAdapterInit)
		projection := a.snapshotProjection(&receipt)
		mapped.auditLockStateProjection = &projection
		return receipt, mapped
	}
	var routeErr *auditLockRouteError
	if errors.As(err, &routeErr) {
		mapped := auditLockRouteErrorFromStoreError(err, routeErr.status, daemonRecoverErrorAuditLockAdapterInit)
		if mapped.occurrenceStoreHealth != nil {
			projection := a.snapshotProjection(&reservation.Receipt)
			mapped.auditLockStateProjection = &projection
		}
		return reservation.Receipt, mapped
	}
	if durableProven {
		return receipt, nil
	}
	if receipt.Status != auditLockOccurrenceUncertain {
		settlement := a.publishUncertainSettlement(reservation)
		if settlement != nil && settlement.phase == auditLockSettlementDurableTerminal {
			return settlement.receipt, nil
		}
		if settlement != nil {
			receipt = settlement.receipt
		} else {
			receipt = auditLockUncertainReceipt(reservation)
		}
	}
	return receipt, &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOutcomeUncertain)}
}

func (a *auditLockAdapter) lookup(ctx context.Context, c auditLockCorrelation) (*auditLockReceiptDTO, *auditLockRouteError) {
	if err := a.ensureReady(); err != nil {
		return nil, err
	}
	var receipt *auditLockReceiptDTO
	err := a.withStoreLock(ctx, "lookup occurrence", func(_ *auditLockStoreOperation) error {
		store, err := a.readStoreLockHeld(false)
		if err != nil {
			return err
		}
		for _, record := range store.Records {
			if record.AttemptID == c.AttemptID &&
				record.OccurrenceID == c.OccurrenceID &&
				record.OriginServerInstance == c.ServerInstance {
				value := auditLockReceiptFromRecord(a.effectiveOccurrence(store.Generation, record))
				receipt = &value
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return nil, auditLockRouteErrorFromStoreError(err, 500, daemonRecoverErrorAuditLockAdapterInit)
	}
	return receipt, nil
}

func (a *auditLockAdapter) acknowledge(ctx context.Context, c auditLockCorrelation) *auditLockRouteError {
	return a.acknowledgeRequest(ctx, auditLockAcknowledgeRequest{Correlation: c})
}

func (a *auditLockAdapter) acknowledgeRequest(ctx context.Context, request auditLockAcknowledgeRequest) *auditLockRouteError {
	if err := a.ensureReady(); err != nil {
		return err
	}
	c := request.Correlation
	var rotatedInstance string
	var err error
	var nextGeneration uint64
	var acknowledgedGeneration uint64
	var acknowledgedRecord auditLockOccurrenceRecord
	mutationEpoch := a.currentMutationEpoch()
	rotationDurable := false
	reconciledAfterError := false
	err = a.withStoreLock(ctx, "acknowledge occurrence", func(operation *auditLockStoreOperation) error {
		store, err := a.readStoreLockHeld(false)
		if err != nil {
			return err
		}
		found := false
		var recordIndex int
		for index := range store.Records {
			record := &store.Records[index]
			if record.AttemptID != c.AttemptID ||
				record.OccurrenceID != c.OccurrenceID ||
				record.OriginServerInstance != c.ServerInstance {
				continue
			}
			found = true
			recordIndex = index
			acknowledgedGeneration = store.Generation
			acknowledgedRecord = *record
			effective := a.effectiveOccurrence(store.Generation, *record)
			if effective.Status == auditLockOccurrenceConsumed {
				return nil
			}
			if effective.Status == auditLockOccurrenceInFlight {
				return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorReceiptInFlight)}
			}
			break
		}
		if !found {
			return nil
		}
		if !mutationEpoch.matchesStore(store) {
			return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorBaselineStale)}
		}
		willRotate := true
		for index, candidate := range store.Records {
			if index != recordIndex && candidate.Status != auditLockOccurrenceConsumed {
				willRotate = false
				break
			}
		}
		if willRotate {
			rotatedInstance, err = newAuditLockCorrelationID()
			if err != nil {
				return err
			}
		}
		consumeAndWrite := func() error {
			before := cloneAuditLockOccurrenceStore(store)
			intended := cloneAuditLockOccurrenceStore(store)
			record := &intended.Records[recordIndex]
			record.Status = auditLockOccurrenceConsumed
			record.HTTPStatus = 0
			record.ErrorCode = ""
			record.LockAuthorization = auditLockAuthorizationNone
			record.Success = nil
			unresolved := false
			for _, candidate := range intended.Records {
				if candidate.Status != auditLockOccurrenceConsumed {
					unresolved = true
					break
				}
			}
			intendedRotated := false
			if !unresolved {
				if intended.Generation == math.MaxUint64 {
					return errors.New("occurrence generation overflow")
				}
				intended.Records = nil
				intended.Generation++
				intended.ActiveServerInstance = rotatedInstance
				intendedRotated = true
			}
			writeResult := a.writeStoreWithLockedReconciliation(operation, before, intended)
			switch writeResult.outcome {
			case auditLockStoreWriteDurable:
				reconciledAfterError = writeResult.durableAfterError
			case auditLockStoreWriteNotPublished:
				return writeResult.cause
			case auditLockStoreWriteUncertain:
				return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOutcomeUncertain), cause: writeResult.cause}
			default:
				return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOutcomeUncertain), cause: errors.New("unknown occurrence write reconciliation outcome")}
			}
			rotationDurable = intendedRotated
			if intendedRotated {
				nextGeneration = intended.Generation
			}
			a.clearSettlement(acknowledgedGeneration, acknowledgedRecord)
			return nil
		}
		effective := a.effectiveOccurrence(store.Generation, acknowledgedRecord)
		if effective.Status == auditLockOccurrenceCommittedSuccess {
			expected := request.ExpectedPhysical
			if expected == nil || expected.ServerInstance != mutationEpoch.ServerInstance || expected.State != api.SupervisorEventLockReleased {
				return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorAckPreconditionRequired)}
			}
			_, claimed := api.ClaimSupervisorEventLockSnapshot(a.logPath, api.SupervisorEventLockSnapshot{
				State: expected.State, Revision: expected.Revision,
			})
			if !claimed {
				return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorAckPhysicalStateChanged)}
			}
			if consumeErr := consumeAndWrite(); consumeErr != nil {
				return consumeErr
			}
		} else if effective.Status == auditLockOccurrenceCommittedError || effective.Status == auditLockOccurrenceUncertain || effective.Status == auditLockOccurrenceNotCommitted {
			if request.ExpectedPhysical != nil {
				return &auditLockRouteError{status: 400, code: string(daemonRecoverErrorCorrelationInvalid)}
			}
			if consumeErr := consumeAndWrite(); consumeErr != nil {
				return consumeErr
			}
		} else {
			return &auditLockRouteError{status: 500, code: string(daemonRecoverErrorAuditLockAdapterInit)}
		}
		return nil
	})
	if rotationDurable {
		a.mu.Lock()
		a.serverInstance = rotatedInstance
		a.generation = nextGeneration
		a.mu.Unlock()
	}
	if reconciledAfterError {
		a.publishStoreWriteReconciled("acknowledge occurrence")
	}
	if err != nil {
		return auditLockRouteErrorFromStoreError(err, 500, daemonRecoverErrorAuditLockAdapterInit)
	}
	return nil
}

func (a *auditLockAdapter) snapshot(ctx context.Context, receipt *auditLockReceiptDTO) (auditLockStateDTO, *auditLockRouteError) {
	if err := a.ensureReady(); err != nil {
		return auditLockStateDTO{}, err
	}
	var store auditLockOccurrenceStore
	var receipts []auditLockReceiptDTO
	err := a.withStoreLock(ctx, "snapshot occurrence store", func(_ *auditLockStoreOperation) error {
		var readErr error
		store, readErr = a.readStoreLockHeld(false)
		if readErr != nil {
			return readErr
		}
		receipts = make([]auditLockReceiptDTO, 0, len(store.Records))
		for _, record := range store.Records {
			effective := a.effectiveOccurrence(store.Generation, record)
			if effective.Status != auditLockOccurrenceConsumed {
				receipts = append(receipts, auditLockReceiptFromRecord(effective))
			}
		}
		return nil
	})
	if err != nil {
		return auditLockStateDTO{}, auditLockRouteErrorFromStoreError(err, 500, daemonRecoverErrorAuditLockAdapterInit)
	}
	serverInstance, generation := a.currentIdentity()
	if store.Generation != generation || store.ActiveServerInstance != serverInstance {
		return auditLockStateDTO{}, &auditLockRouteError{status: 409, code: string(daemonRecoverErrorBaselineStale)}
	}
	physical := api.SupervisorEventLockSnapshotForPath(a.logPath)
	return a.withOccurrenceStoreHealth(auditLockStateDTO{
		Scope:            auditLockScope,
		ServerInstance:   serverInstance,
		Revision:         physical.Revision,
		State:            physical.State,
		RecoveryReceipt:  receipt,
		RecoveryReceipts: receipts,
	}), nil
}

// snapshotAfterTerminal preserves the already-durable receipt when only the
// follow-up store projection fails. It is intentionally unavailable before
// reservation/terminalization; pre-mutation store failures still fail loud and
// prevent recovery.
func (a *auditLockAdapter) snapshotAfterTerminal(ctx context.Context, receipt *auditLockReceiptDTO) auditLockStateDTO {
	snapshot, routeErr := a.snapshot(ctx, receipt)
	if routeErr != nil {
		return a.snapshotProjection(receipt)
	}
	return snapshot
}

func (a *auditLockAdapter) snapshotProjection(receipt *auditLockReceiptDTO) auditLockStateDTO {
	serverInstance, _ := a.currentIdentity()
	physical := api.SupervisorEventLockSnapshotForPath(a.logPath)
	receipts := []auditLockReceiptDTO{}
	if receipt != nil {
		receipts = append(receipts, *receipt)
	}
	return a.withOccurrenceStoreHealth(auditLockStateDTO{
		Scope:            auditLockScope,
		ServerInstance:   serverInstance,
		Revision:         physical.Revision,
		State:            physical.State,
		RecoveryReceipt:  receipt,
		RecoveryReceipts: receipts,
	})
}

func (a *auditLockAdapter) armPendingSettlement() {
	if a == nil || a.initErr != nil {
		return
	}
	a.mu.Lock()
	if a.closing || a.watching {
		a.mu.Unlock()
		return
	}
	initial, subscription := api.SubscribeSupervisorEventLockState(a.logPath, a.observeSettlement)
	if initial.State != api.SupervisorEventLockOutstanding {
		a.mu.Unlock()
		subscription.Close()
		return
	}
	a.watching = true
	a.observedRevision = initial.Revision
	a.subscription = subscription
	a.mu.Unlock()
}

func (a *auditLockAdapter) observeSettlement(hint api.SupervisorEventLockSnapshot) {
	a.mu.Lock()
	if a.closing || !a.watching || hint.Revision <= a.observedRevision {
		a.mu.Unlock()
		return
	}
	a.observedRevision = hint.Revision
	if hint.State == api.SupervisorEventLockOutstanding {
		a.mu.Unlock()
		return
	}
	subscription := a.subscription
	a.mu.Unlock()
	if subscription == nil {
		return
	}
	authoritative, claimed := subscription.TryCloseAtTerminal(hint.Revision)
	if !claimed {
		a.mu.Lock()
		if authoritative.Revision > a.observedRevision {
			a.observedRevision = authoritative.Revision
		}
		a.mu.Unlock()
		return
	}

	a.mu.Lock()
	if a.subscription != subscription || !a.watching {
		a.mu.Unlock()
		return
	}
	a.watching = false
	a.subscription = nil
	a.observedRevision = authoritative.Revision
	events := a.events
	serverInstance := a.serverInstance
	a.mu.Unlock()
	if events != nil {
		dto := a.withOccurrenceStoreHealth(auditLockStateDTO{
			Scope:            auditLockScope,
			ServerInstance:   serverInstance,
			Revision:         authoritative.Revision,
			State:            authoritative.State,
			RecoveryReceipts: []auditLockReceiptDTO{},
		})
		events.Publish(Event{Type: "audit-lock-state", Body: dto.eventBody()})
	}
}

func (d auditLockStateDTO) eventBody() map[string]any {
	body := map[string]any{
		"scope":                            d.Scope,
		"server_instance":                  d.ServerInstance,
		"revision":                         d.Revision,
		"state":                            d.State,
		"occurrence_store_health":          d.OccurrenceStoreHealth,
		"occurrence_store_health_revision": d.OccurrenceStoreHealthRevision,
		"restart_required":                 d.RestartRequired,
		"recovery_receipt":                 nil,
		"recovery_receipts":                d.RecoveryReceipts,
	}
	if body["recovery_receipts"] == nil {
		body["recovery_receipts"] = []auditLockReceiptDTO{}
	}
	if d.RecoveryReceipt != nil {
		body["recovery_receipt"] = d.RecoveryReceipt
	}
	return body
}

func (a *auditLockAdapter) beginClose() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.closing = true
	a.mu.Unlock()
}

func (a *auditLockAdapter) close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closing = true
	a.closed = true
	subscription := a.subscription
	a.subscription = nil
	a.watching = false
	a.mu.Unlock()
	a.settlements.Clear()
	if subscription != nil {
		subscription.Close()
	}
}
