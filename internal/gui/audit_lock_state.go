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
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemonrecovery"
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
	Scope            string                       `json:"scope"`
	ServerInstance   string                       `json:"server_instance"`
	Revision         uint64                       `json:"revision"`
	State            api.SupervisorEventLockState `json:"state"`
	RecoveryReceipt  *auditLockReceiptDTO         `json:"recovery_receipt"`
	RecoveryReceipts []auditLockReceiptDTO        `json:"recovery_receipts"`
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

type auditLockReservation struct {
	Novel      bool
	Receipt    auditLockReceiptDTO
	Terminal   *auditLockTerminalEvidence
	Binding    auditLockOccurrenceBinding
	Generation uint64
}

type auditLockUncertaintyKey struct {
	Generation     uint64
	ServerInstance string
	AttemptID      string
	OccurrenceID   string
}

type auditLockStoreLockReleaseError struct {
	operation string
	cause     error
}

func (e *auditLockStoreLockReleaseError) Error() string {
	return fmt.Sprintf("%s: release occurrence lock: %v", e.operation, e.cause)
}

func (e *auditLockStoreLockReleaseError) Unwrap() error { return e.cause }

type auditLockRouteError struct {
	status int
	code   string
}

func (e *auditLockRouteError) Error() string { return e.code }

type auditLockAdapter struct {
	mu               sync.Mutex
	storeMu          sync.Mutex
	serverInstance   string
	generation       uint64
	storePath        string
	logPath          string
	initErr          error
	closing          bool
	closed           bool
	watching         bool
	observedRevision uint64
	uncertain        map[auditLockUncertaintyKey]auditLockReceiptDTO
	subscription     *api.SupervisorEventLockSubscription
	events           *Broadcaster
	lockTimeout      time.Duration
}

func newAuditLockAdapter(events *Broadcaster) *auditLockAdapter {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return &auditLockAdapter{
			events:      events,
			lockTimeout: auditLockStoreLockTimeout,
			initErr:     fmt.Errorf("resolve daemon state dir: %w", err),
		}
	}
	return newAuditLockAdapterInStateDir(events, stateDir)
}

func newAuditLockAdapterInStateDir(events *Broadcaster, stateDir string) *auditLockAdapter {
	a := &auditLockAdapter{
		events:      events,
		storePath:   filepath.Join(stateDir, auditLockOccurrenceFileLeaf),
		logPath:     filepath.Join(stateDir, api.SupervisorEventLogFileLeaf),
		lockTimeout: auditLockStoreLockTimeout,
		uncertain:   make(map[auditLockUncertaintyKey]auditLockReceiptDTO),
	}
	instance, err := newAuditLockCorrelationID()
	if err != nil {
		a.initErr = fmt.Errorf("generate audit-lock server instance: %w", err)
		return a
	}
	a.serverInstance = instance
	claimCtx, cancel := context.WithTimeout(context.Background(), a.lockTimeout)
	defer cancel()
	if err := a.claimStore(claimCtx); err != nil {
		a.initErr = fmt.Errorf("claim daemon recovery occurrence store: %w", err)
	}
	return a
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

func auditLockUncertaintyKeyFor(generation uint64, record auditLockOccurrenceRecord) auditLockUncertaintyKey {
	return auditLockUncertaintyKey{
		Generation:     generation,
		ServerInstance: record.OriginServerInstance,
		AttemptID:      record.AttemptID,
		OccurrenceID:   record.OccurrenceID,
	}
}

// effectiveOccurrence is the single projection owner for durable occurrence
// state. Callers must hold storeMu: the generation-scoped overlay exists only
// to keep a terminal result that could not be proven durable from regressing
// to in_flight inside the same process.
func (a *auditLockAdapter) effectiveOccurrence(generation uint64, durable auditLockOccurrenceRecord) auditLockOccurrenceRecord {
	if durable.Status != auditLockOccurrenceInFlight {
		return durable
	}
	if receipt, ok := a.uncertain[auditLockUncertaintyKeyFor(generation, durable)]; ok {
		durable.Status = receipt.Status
		durable.HTTPStatus = 0
		durable.ErrorCode = ""
		durable.LockAuthorization = receipt.LockAuthorization
		durable.Success = nil
	}
	return durable
}

func (a *auditLockAdapter) installUncertaintyLockHeld(reservation auditLockReservation) auditLockReceiptDTO {
	receipt := reservation.Receipt
	receipt.Status = auditLockOccurrenceUncertain
	receipt.LockAuthorization = auditLockAuthorizationUncertain
	receipt.TerminationCommitState = auditLockTerminationStateUnknown
	key := auditLockUncertaintyKey{
		Generation:     reservation.Generation,
		ServerInstance: receipt.ServerInstance,
		AttemptID:      receipt.AttemptID,
		OccurrenceID:   receipt.OccurrenceID,
	}
	if a.uncertain == nil {
		a.uncertain = make(map[auditLockUncertaintyKey]auditLockReceiptDTO)
	}
	a.uncertain[key] = receipt
	return receipt
}

func (a *auditLockAdapter) clearUncertaintyLockHeld(generation uint64, record auditLockOccurrenceRecord) {
	delete(a.uncertain, auditLockUncertaintyKeyFor(generation, record))
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

func (a *auditLockAdapter) withStoreLock(ctx context.Context, operation string, fn func() error) error {
	return a.withStoreLockFailure(ctx, operation, fn, nil)
}

// withStoreLockFailure keeps the failure finalizer under storeMu and, when
// acquired, the file lock. Terminalization uses this seam to publish its
// generation-scoped uncertainty projection before any same-process reader can
// observe the old durable in_flight record.
func (a *auditLockAdapter) withStoreLockFailure(
	ctx context.Context,
	operation string,
	fn func() error,
	onFailure func(error),
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	lockCtx, cancel := context.WithTimeout(ctx, a.lockTimeout)
	defer cancel()
	for !a.storeMu.TryLock() {
		select {
		case <-lockCtx.Done():
			return fmt.Errorf("%s: acquire in-process occurrence lock: %w", operation, lockCtx.Err())
		case <-time.After(auditLockStoreLockRetry):
		}
	}
	defer a.storeMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(a.storePath), 0o700); err != nil {
		operationErr := fmt.Errorf("%s: create state directory: %w", operation, err)
		if onFailure != nil {
			onFailure(operationErr)
		}
		return operationErr
	}
	lock := flock.New(a.storePath + ".lock")
	locked, err := lock.TryLockContext(lockCtx, auditLockStoreLockRetry)
	if err != nil {
		_ = lock.Close()
		operationErr := fmt.Errorf("%s: acquire occurrence lock: %w", operation, err)
		if onFailure != nil {
			onFailure(operationErr)
		}
		return operationErr
	}
	if !locked {
		_ = lock.Close()
		operationErr := fmt.Errorf("%s: occurrence lock unavailable", operation)
		if onFailure != nil {
			onFailure(operationErr)
		}
		return operationErr
	}
	opErr := fn()
	if opErr != nil && onFailure != nil {
		onFailure(opErr)
	}
	closeErr := lock.Close()
	if opErr != nil {
		return opErr
	}
	if closeErr != nil {
		return &auditLockStoreLockReleaseError{operation: operation, cause: closeErr}
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
	if err := api.WriteStateFileBytesLockHeld(a.storePath, raw); err != nil {
		return fmt.Errorf("write occurrence store: %w", err)
	}
	return nil
}

func (a *auditLockAdapter) claimStore(ctx context.Context) error {
	var claimed auditLockOccurrenceStore
	err := a.withStoreLock(ctx, "claim occurrence store", func() error {
		store, err := a.readStoreLockHeld(true)
		if err != nil {
			return err
		}
		if store.Generation == math.MaxUint64 {
			return errors.New("occurrence generation overflow")
		}
		records := store.Records[:0]
		for _, record := range store.Records {
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
		store.Records = records
		store.Version = auditLockOccurrenceStoreVersion
		store.Generation++
		store.ActiveServerInstance = a.serverInstance
		if err := a.writeStoreLockHeld(store); err != nil {
			return err
		}
		claimed = store
		return nil
	})
	if err != nil {
		return err
	}
	a.generation = claimed.Generation
	return nil
}

func (a *auditLockAdapter) ensureReady() *auditLockRouteError {
	if a == nil || a.initErr != nil {
		return &auditLockRouteError{status: 500, code: string(daemonRecoverErrorAuditLockAdapterInit)}
	}
	return nil
}

func (a *auditLockAdapter) currentIdentity() (string, uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.serverInstance, a.generation
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
	a.mu.Unlock()
	if closing {
		return auditLockReservation{}, &auditLockRouteError{status: 503, code: string(daemonRecoverErrorOccurrenceCapacity)}
	}

	var reservation auditLockReservation
	err := a.withStoreLock(ctx, "reserve occurrence", func() error {
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
				Receipt:    auditLockReceiptFromRecord(effective),
				Terminal:   auditLockTerminalFromRecord(effective),
				Binding:    binding,
				Generation: store.Generation,
			}
			return nil
		}
		serverInstance, generation := a.currentIdentity()
		if c.ServerInstance != serverInstance ||
			store.ActiveServerInstance != serverInstance ||
			store.Generation != generation {
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
		store.Records = append(store.Records, record)
		if err := a.writeStoreLockHeld(store); err != nil {
			return err
		}
		reservation = auditLockReservation{
			Novel:      true,
			Receipt:    auditLockReceiptFromRecord(record),
			Binding:    binding,
			Generation: store.Generation,
		}
		return nil
	})
	if err != nil {
		var routeErr *auditLockRouteError
		if errors.As(err, &routeErr) {
			return auditLockReservation{}, routeErr
		}
		return auditLockReservation{}, &auditLockRouteError{status: 500, code: string(daemonRecoverErrorAuditLockAdapterInit)}
	}
	return reservation, nil
}

func (a *auditLockAdapter) terminalize(reservation auditLockReservation, receiptStatus, authorization string, terminal auditLockTerminalEvidence) (auditLockReceiptDTO, *auditLockRouteError) {
	ctx, cancel := context.WithTimeout(context.Background(), a.lockTimeout)
	defer cancel()
	var receipt auditLockReceiptDTO
	var requested auditLockOccurrenceRecord
	var mutationEntered bool
	var durableProven bool
	err := a.withStoreLockFailure(ctx, "terminalize occurrence", func() error {
		store, err := a.readStoreLockHeld(false)
		if err != nil {
			return err
		}
		serverInstance, generation := a.currentIdentity()
		if generation != reservation.Generation ||
			store.Generation != reservation.Generation ||
			store.ActiveServerInstance != serverInstance ||
			serverInstance != reservation.Binding.serverInstance {
			return errors.New("occurrence generation changed before terminalization")
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
			a.clearUncertaintyLockHeld(store.Generation, *record)
			receipt = auditLockReceiptFromRecord(*record)
			durableProven = true
			return nil
		}
		return errors.New("in-flight occurrence changed before terminalization")
	}, func(_ error) {
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
						a.clearUncertaintyLockHeld(store.Generation, record)
						receipt = auditLockReceiptFromRecord(record)
						durableProven = true
						return
					}
				}
			}
		}
		receipt = a.installUncertaintyLockHeld(reservation)
	})
	if err == nil {
		return receipt, nil
	}
	if durableProven {
		var releaseErr *auditLockStoreLockReleaseError
		if errors.As(err, &releaseErr) {
			_ = api.LogHubMcpEvent("warn", "daemon-recovery-occurrence-store-lock-release-failed", map[string]any{
				"operation": "terminalize occurrence",
			})
		}
		return receipt, nil
	}
	if receipt.Status != auditLockOccurrenceUncertain {
		// A failure before the finalizer could run is not expected, but remains
		// fail-closed and never exposes a retryable terminal outcome.
		receipt = reservation.Receipt
		receipt.Status = auditLockOccurrenceUncertain
		receipt.LockAuthorization = auditLockAuthorizationUncertain
		receipt.TerminationCommitState = auditLockTerminationStateUnknown
	}
	return receipt, &auditLockRouteError{status: 409, code: string(daemonRecoverErrorOutcomeUncertain)}
}

func (a *auditLockAdapter) lookup(ctx context.Context, c auditLockCorrelation) (*auditLockReceiptDTO, *auditLockRouteError) {
	if err := a.ensureReady(); err != nil {
		return nil, err
	}
	var receipt *auditLockReceiptDTO
	err := a.withStoreLock(ctx, "lookup occurrence", func() error {
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
		return nil, &auditLockRouteError{status: 500, code: string(daemonRecoverErrorAuditLockAdapterInit)}
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
	var rotated bool
	var nextGeneration uint64
	var acknowledgedGeneration uint64
	var acknowledgedRecord auditLockOccurrenceRecord
	err = a.withStoreLock(ctx, "acknowledge occurrence", func() error {
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
		serverInstance, generation := a.currentIdentity()
		if store.Generation != generation || store.ActiveServerInstance != serverInstance || c.ServerInstance != serverInstance {
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
			record := &store.Records[recordIndex]
			record.Status = auditLockOccurrenceConsumed
			record.HTTPStatus = 0
			record.ErrorCode = ""
			record.LockAuthorization = auditLockAuthorizationNone
			record.Success = nil
			unresolved := false
			for _, candidate := range store.Records {
				if candidate.Status != auditLockOccurrenceConsumed {
					unresolved = true
					break
				}
			}
			if !unresolved {
				if store.Generation == math.MaxUint64 {
					return errors.New("occurrence generation overflow")
				}
				store.Records = nil
				store.Generation++
				store.ActiveServerInstance = rotatedInstance
				rotated = true
				nextGeneration = store.Generation
			}
			if err := a.writeStoreLockHeld(store); err != nil {
				return err
			}
			a.clearUncertaintyLockHeld(acknowledgedGeneration, acknowledgedRecord)
			return nil
		}
		effective := a.effectiveOccurrence(store.Generation, acknowledgedRecord)
		if effective.Status == auditLockOccurrenceCommittedSuccess {
			expected := request.ExpectedPhysical
			if expected == nil || expected.ServerInstance != c.ServerInstance || expected.State != api.SupervisorEventLockReleased {
				return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorAckPreconditionRequired)}
			}
			_, committed, commitErr := api.CommitIfSupervisorEventLockSnapshot(a.logPath, api.SupervisorEventLockSnapshot{
				State: expected.State, Revision: expected.Revision,
			}, consumeAndWrite)
			if commitErr != nil {
				return commitErr
			}
			if !committed {
				return &auditLockRouteError{status: 409, code: string(daemonRecoverErrorAckPhysicalStateChanged)}
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
	if err != nil {
		var routeErr *auditLockRouteError
		if errors.As(err, &routeErr) {
			return routeErr
		}
		return &auditLockRouteError{status: 500, code: string(daemonRecoverErrorAuditLockAdapterInit)}
	}
	if rotated {
		a.mu.Lock()
		a.serverInstance = rotatedInstance
		a.generation = nextGeneration
		a.mu.Unlock()
	}
	return nil
}

func (a *auditLockAdapter) snapshot(ctx context.Context, receipt *auditLockReceiptDTO) (auditLockStateDTO, *auditLockRouteError) {
	if err := a.ensureReady(); err != nil {
		return auditLockStateDTO{}, err
	}
	var store auditLockOccurrenceStore
	var receipts []auditLockReceiptDTO
	err := a.withStoreLock(ctx, "snapshot occurrence store", func() error {
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
		return auditLockStateDTO{}, &auditLockRouteError{status: 500, code: string(daemonRecoverErrorAuditLockAdapterInit)}
	}
	serverInstance, generation := a.currentIdentity()
	if store.Generation != generation || store.ActiveServerInstance != serverInstance {
		return auditLockStateDTO{}, &auditLockRouteError{status: 409, code: string(daemonRecoverErrorBaselineStale)}
	}
	physical := api.SupervisorEventLockSnapshotForPath(a.logPath)
	return auditLockStateDTO{
		Scope:            auditLockScope,
		ServerInstance:   serverInstance,
		Revision:         physical.Revision,
		State:            physical.State,
		RecoveryReceipt:  receipt,
		RecoveryReceipts: receipts,
	}, nil
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
	return auditLockStateDTO{
		Scope:            auditLockScope,
		ServerInstance:   serverInstance,
		Revision:         physical.Revision,
		State:            physical.State,
		RecoveryReceipt:  receipt,
		RecoveryReceipts: receipts,
	}
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
		dto := auditLockStateDTO{
			Scope:            auditLockScope,
			ServerInstance:   serverInstance,
			Revision:         authoritative.Revision,
			State:            authoritative.State,
			RecoveryReceipts: []auditLockReceiptDTO{},
		}
		events.Publish(Event{Type: "audit-lock-state", Body: dto.eventBody()})
	}
}

func (d auditLockStateDTO) eventBody() map[string]any {
	body := map[string]any{
		"scope":             d.Scope,
		"server_instance":   d.ServerInstance,
		"revision":          d.Revision,
		"state":             d.State,
		"recovery_receipt":  nil,
		"recovery_receipts": d.RecoveryReceipts,
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
	if subscription != nil {
		subscription.Close()
	}
}
