package gui

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"

	"mcp-local-hub/internal/api"
)

const (
	auditLockScope              = "supervisor_events_log"
	auditLockOccurrenceCapacity = 64
)

type auditLockReceiptDTO struct {
	AttemptID         string `json:"attempt_id"`
	OccurrenceID      string `json:"occurrence_id"`
	Status            string `json:"status"`
	LockAuthorization string `json:"lock_authorization"`
}

type auditLockStateDTO struct {
	Scope           string                       `json:"scope"`
	ServerInstance  string                       `json:"server_instance"`
	Revision        uint64                       `json:"revision"`
	State           api.SupervisorEventLockState `json:"state"`
	RecoveryReceipt *auditLockReceiptDTO         `json:"recovery_receipt"`
}

type auditLockCorrelation struct {
	AttemptID      string
	OccurrenceID   string
	ServerInstance string
}

type auditLockOccurrenceKey struct {
	attemptID    string
	occurrenceID string
}

type auditLockOccurrenceBinding struct {
	serverInstance string
	taskName       string
	confirm        bool
}

type auditLockOccurrencePhase uint8

const (
	auditLockOccurrenceInFlight auditLockOccurrencePhase = iota
	auditLockOccurrenceTerminal
	auditLockOccurrenceConsumed
)

type daemonRecoverSuccessEvidence struct {
	TaskName        string
	Reaped          bool
	PortOwnerCheck  string
	PortWaitOutcome string
	AuditHandoff    string
}

type auditLockTerminalEvidence struct {
	HTTPStatus           int
	ErrorCode            string
	TerminationCommitted bool
	Success              *daemonRecoverSuccessEvidence
}

type auditLockOccurrence struct {
	binding  auditLockOccurrenceBinding
	phase    auditLockOccurrencePhase
	receipt  auditLockReceiptDTO
	terminal *auditLockTerminalEvidence
}

type auditLockReservation struct {
	Novel    bool
	Receipt  auditLockReceiptDTO
	Terminal *auditLockTerminalEvidence
}

type auditLockRouteError struct {
	status int
	code   string
}

func (e *auditLockRouteError) Error() string { return e.code }

type auditLockAdapter struct {
	mu             sync.Mutex
	serverInstance string
	logPath        string
	initErr        error
	closing        bool
	closed         bool
	occurrences    map[auditLockOccurrenceKey]*auditLockOccurrence
	watching       bool
	unsubscribe    func()
	events         *Broadcaster
}

func newAuditLockAdapter(events *Broadcaster) *auditLockAdapter {
	a := &auditLockAdapter{
		occurrences: map[auditLockOccurrenceKey]*auditLockOccurrence{},
		events:      events,
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		a.initErr = fmt.Errorf("resolve daemon state dir: %w", err)
		return a
	}
	instance, err := newAuditLockCorrelationID()
	if err != nil {
		a.initErr = fmt.Errorf("generate audit-lock server instance: %w", err)
		return a
	}
	a.serverInstance = instance
	a.logPath = filepath.Join(stateDir, api.SupervisorEventLogFileLeaf)
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
		return &auditLockRouteError{status: 400, code: "RECOVER_CORRELATION_INVALID"}
	}
	for i := 0; i < len(value); i++ {
		switch i {
		case 8, 13, 18, 23:
			if value[i] != '-' {
				return &auditLockRouteError{status: 400, code: "RECOVER_CORRELATION_INVALID"}
			}
		default:
			if !((value[i] >= '0' && value[i] <= '9') || (value[i] >= 'a' && value[i] <= 'f')) {
				return &auditLockRouteError{status: 400, code: "RECOVER_CORRELATION_INVALID"}
			}
		}
	}
	if value[14] != '4' || (value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b') {
		return &auditLockRouteError{status: 400, code: "RECOVER_CORRELATION_INVALID"}
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
		return auditLockCorrelation{}, &auditLockRouteError{status: 400, code: "RECOVER_CORRELATION_INVALID"}
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
			return auditLockCorrelation{}, &auditLockRouteError{status: 400, code: "RECOVER_CORRELATION_INVALID"}
		}
	}
	for field := range fields {
		if targets[field] == nil {
			return auditLockCorrelation{}, &auditLockRouteError{status: 400, code: "RECOVER_CORRELATION_INVALID"}
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

func (a *auditLockAdapter) ensureReady() *auditLockRouteError {
	if a == nil || a.initErr != nil {
		return &auditLockRouteError{status: 500, code: "AUDIT_LOCK_ADAPTER_INIT_FAILED"}
	}
	return nil
}

func (a *auditLockAdapter) validateInstance(serverInstance string) *auditLockRouteError {
	if err := a.ensureReady(); err != nil {
		return err
	}
	if serverInstance != a.serverInstance {
		return &auditLockRouteError{status: 409, code: "RECOVER_BASELINE_STALE"}
	}
	return nil
}

func (a *auditLockAdapter) reserve(c auditLockCorrelation, binding auditLockOccurrenceBinding) (auditLockReservation, *auditLockRouteError) {
	if err := a.validateInstance(c.ServerInstance); err != nil {
		return auditLockReservation{}, err
	}
	key := auditLockOccurrenceKey{attemptID: c.AttemptID, occurrenceID: c.OccurrenceID}
	a.mu.Lock()
	defer a.mu.Unlock()
	if existing := a.occurrences[key]; existing != nil {
		if existing.binding != binding {
			return auditLockReservation{}, &auditLockRouteError{status: 409, code: "RECOVER_ATTEMPT_CONFLICT"}
		}
		if existing.phase == auditLockOccurrenceConsumed {
			return auditLockReservation{}, &auditLockRouteError{status: 409, code: "RECOVER_OCCURRENCE_CONSUMED"}
		}
		return auditLockReservation{
			Receipt:  existing.receipt,
			Terminal: cloneAuditLockTerminal(existing.terminal),
		}, nil
	}
	if a.closing || len(a.occurrences) >= auditLockOccurrenceCapacity {
		return auditLockReservation{}, &auditLockRouteError{status: 503, code: "RECOVER_OCCURRENCE_CAPACITY_EXCEEDED"}
	}
	receipt := auditLockReceiptDTO{
		AttemptID:         c.AttemptID,
		OccurrenceID:      c.OccurrenceID,
		Status:            "in_flight",
		LockAuthorization: "none",
	}
	a.occurrences[key] = &auditLockOccurrence{
		binding: binding,
		phase:   auditLockOccurrenceInFlight,
		receipt: receipt,
	}
	return auditLockReservation{Novel: true, Receipt: receipt}, nil
}

func (a *auditLockAdapter) terminalize(c auditLockCorrelation, receiptStatus, authorization string, terminal auditLockTerminalEvidence) auditLockReceiptDTO {
	key := auditLockOccurrenceKey{attemptID: c.AttemptID, occurrenceID: c.OccurrenceID}
	a.mu.Lock()
	defer a.mu.Unlock()
	entry := a.occurrences[key]
	if entry == nil || entry.phase != auditLockOccurrenceInFlight {
		return auditLockReceiptDTO{}
	}
	entry.phase = auditLockOccurrenceTerminal
	entry.receipt.Status = receiptStatus
	entry.receipt.LockAuthorization = authorization
	entry.terminal = cloneAuditLockTerminal(&terminal)
	return entry.receipt
}

func (a *auditLockAdapter) lookup(c auditLockCorrelation) (*auditLockReceiptDTO, *auditLockRouteError) {
	if err := a.validateInstance(c.ServerInstance); err != nil {
		return nil, err
	}
	key := auditLockOccurrenceKey{attemptID: c.AttemptID, occurrenceID: c.OccurrenceID}
	a.mu.Lock()
	defer a.mu.Unlock()
	entry := a.occurrences[key]
	if entry == nil {
		return nil, nil
	}
	receipt := entry.receipt
	return &receipt, nil
}

func (a *auditLockAdapter) acknowledge(c auditLockCorrelation) *auditLockRouteError {
	if err := a.validateInstance(c.ServerInstance); err != nil {
		return err
	}
	key := auditLockOccurrenceKey{attemptID: c.AttemptID, occurrenceID: c.OccurrenceID}
	a.mu.Lock()
	defer a.mu.Unlock()
	entry := a.occurrences[key]
	if entry == nil || entry.phase == auditLockOccurrenceConsumed {
		return nil
	}
	if entry.phase == auditLockOccurrenceInFlight {
		return &auditLockRouteError{status: 409, code: "RECOVER_RECEIPT_IN_FLIGHT"}
	}
	entry.phase = auditLockOccurrenceConsumed
	entry.terminal = nil
	entry.receipt.Status = "consumed"
	entry.receipt.LockAuthorization = "none"
	return nil
}

func (a *auditLockAdapter) snapshot(receipt *auditLockReceiptDTO) (auditLockStateDTO, *auditLockRouteError) {
	if err := a.ensureReady(); err != nil {
		return auditLockStateDTO{}, err
	}
	physical := api.SupervisorEventLockSnapshotForPath(a.logPath)
	return auditLockStateDTO{
		Scope:           auditLockScope,
		ServerInstance:  a.serverInstance,
		Revision:        physical.Revision,
		State:           physical.State,
		RecoveryReceipt: receipt,
	}, nil
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
	initial, unsubscribe := api.SubscribeSupervisorEventLockState(a.logPath, a.observeSettlement)
	if initial.State != api.SupervisorEventLockOutstanding {
		a.mu.Unlock()
		unsubscribe()
		return
	}
	a.watching = true
	a.unsubscribe = unsubscribe
	a.mu.Unlock()
}

func (a *auditLockAdapter) observeSettlement(snapshot api.SupervisorEventLockSnapshot) {
	if snapshot.State != api.SupervisorEventLockReleased && snapshot.State != api.SupervisorEventLockStranded {
		return
	}
	a.mu.Lock()
	if a.closing || !a.watching {
		a.mu.Unlock()
		return
	}
	a.watching = false
	unsubscribe := a.unsubscribe
	a.unsubscribe = nil
	events := a.events
	dto := auditLockStateDTO{
		Scope:          auditLockScope,
		ServerInstance: a.serverInstance,
		Revision:       snapshot.Revision,
		State:          snapshot.State,
	}
	a.mu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
	if events != nil {
		events.Publish(Event{Type: "audit-lock-state", Body: dto.eventBody()})
	}
}

func (d auditLockStateDTO) eventBody() map[string]any {
	body := map[string]any{
		"scope":            d.Scope,
		"server_instance":  d.ServerInstance,
		"revision":         d.Revision,
		"state":            d.State,
		"recovery_receipt": nil,
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
	unsubscribe := a.unsubscribe
	a.unsubscribe = nil
	a.watching = false
	clear(a.occurrences)
	a.mu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
}
