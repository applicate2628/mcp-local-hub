package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/daemon_env_overlay"
	"mcp-local-hub/internal/daemonrecovery"
)

type daemonRecoverer interface {
	Recover(ctx context.Context, taskName string, confirmed bool, onTerminationCommitted func()) (daemonrecovery.Result, error)
}

type realDaemonRecoverer struct{}

func (realDaemonRecoverer) Recover(ctx context.Context, taskName string, confirmed bool, onTerminationCommitted func()) (daemonrecovery.Result, error) {
	return daemonrecovery.Execute(ctx, taskName, daemonrecovery.Options{
		Confirmed:              confirmed,
		OnTerminationCommitted: onTerminationCommitted,
	})
}

type daemonRecoverRequest struct {
	TaskName         string          `json:"task_name"`
	Confirm          bool            `json:"confirm"`
	AuditLockAttempt json.RawMessage `json:"audit_lock_attempt,omitempty"`
}

func (r *daemonRecoverRequest) UnmarshalJSON(raw []byte) error {
	fields, err := decodeUniqueJSONObject(raw)
	if err != nil {
		return err
	}
	for field := range fields {
		switch field {
		case "task_name", "confirm", "audit_lock_attempt":
		default:
			return fmt.Errorf("unknown field %q", field)
		}
	}
	if value := fields["task_name"]; value != nil {
		if err := json.Unmarshal(value, &r.TaskName); err != nil {
			return err
		}
	}
	if value := fields["confirm"]; value != nil {
		if err := json.Unmarshal(value, &r.Confirm); err != nil {
			return err
		}
	}
	if value, ok := fields["audit_lock_attempt"]; ok {
		r.AuditLockAttempt = append(r.AuditLockAttempt[:0], value...)
	}
	return nil
}

type daemonRecoverResponse struct {
	TaskName             string            `json:"task_name"`
	State                string            `json:"state"`
	Reaped               bool              `json:"reaped"`
	PortOwnerCheck       string            `json:"port_owner_check"`
	PortWaitOutcome      string            `json:"port_wait_outcome"`
	AuditHandoff         string            `json:"audit_handoff"`
	TerminationCommitted bool              `json:"termination_committed"`
	AuditLock            auditLockStateDTO `json:"audit_lock"`
}

type daemonRecoverOccurrenceErrorResponse struct {
	Error                  string            `json:"error"`
	Code                   string            `json:"code"`
	TerminationCommitted   bool              `json:"termination_committed"`
	TerminationCommitState string            `json:"termination_commit_state,omitempty"`
	AuditLock              auditLockStateDTO `json:"audit_lock"`
}

// daemonRecoverErrorCode owns the finite set that may be persisted in a
// terminal recovery receipt. Route-only audit-lock errors are deliberately
// excluded because they never cross the durable terminalization boundary.
type daemonRecoverErrorCode string

const (
	daemonRecoverErrorInvalidArgs                 daemonRecoverErrorCode = "INVALID_ARGS"
	daemonRecoverErrorConfirmationRequired        daemonRecoverErrorCode = "RECOVER_CONFIRMATION_REQUIRED"
	daemonRecoverErrorUnknownTask                 daemonRecoverErrorCode = "RECOVER_UNKNOWN_TASK"
	daemonRecoverErrorRefusedPortOwner            daemonRecoverErrorCode = "RECOVER_REFUSED_PORT_OWNER"
	daemonRecoverErrorRespawnFailed               daemonRecoverErrorCode = "RECOVER_RESPAWN_FAILED"
	daemonRecoverErrorSupervisorUnavailable       daemonRecoverErrorCode = "RECOVER_SUPERVISOR_UNAVAILABLE"
	daemonRecoverErrorRequestCanceled             daemonRecoverErrorCode = "RECOVER_REQUEST_CANCELED"
	daemonRecoverErrorBoundaryProbeTimeout        daemonRecoverErrorCode = "RECOVER_BOUNDARY_PROBE_TIMEOUT"
	daemonRecoverErrorRespawnBudgetInsufficient   daemonRecoverErrorCode = "RECOVER_RESPAWN_BUDGET_INSUFFICIENT"
	daemonRecoverErrorStateRead                   daemonRecoverErrorCode = "RECOVER_STATE_READ_FAILED"
	daemonRecoverErrorAuditDurability             daemonRecoverErrorCode = "RECOVER_AUDIT_DURABILITY_FAILED"
	daemonRecoverErrorUnclassifiedFailure         daemonRecoverErrorCode = "RECOVER_UNCLASSIFIED_FAILURE"
	daemonRecoverErrorAuditLockAdapterInit        daemonRecoverErrorCode = "AUDIT_LOCK_ADAPTER_INIT_FAILED"
	daemonRecoverErrorCorrelationInvalid          daemonRecoverErrorCode = "RECOVER_CORRELATION_INVALID"
	daemonRecoverErrorBaselineStale               daemonRecoverErrorCode = "RECOVER_BASELINE_STALE"
	daemonRecoverErrorAttemptConflict             daemonRecoverErrorCode = "RECOVER_ATTEMPT_CONFLICT"
	daemonRecoverErrorOccurrenceConsumed          daemonRecoverErrorCode = "RECOVER_OCCURRENCE_CONSUMED"
	daemonRecoverErrorOccurrenceCapacity          daemonRecoverErrorCode = "RECOVER_OCCURRENCE_CAPACITY_EXCEEDED"
	daemonRecoverErrorReceiptInFlight             daemonRecoverErrorCode = "RECOVER_RECEIPT_IN_FLIGHT"
	daemonRecoverErrorOutcomeUncertain            daemonRecoverErrorCode = "RECOVER_OUTCOME_UNCERTAIN"
	daemonRecoverErrorAckPreconditionRequired     daemonRecoverErrorCode = "RECOVER_ACK_PRECONDITION_REQUIRED"
	daemonRecoverErrorAckPhysicalStateChanged     daemonRecoverErrorCode = "RECOVER_ACK_PHYSICAL_STATE_CHANGED"
	daemonRecoverErrorOccurrenceStoreLockStranded daemonRecoverErrorCode = "RECOVERY_OCCURRENCE_STORE_LOCK_STRANDED"
)

type daemonRecoverErrorCatalogEntry struct {
	code        daemonRecoverErrorCode
	persistable bool
}

var daemonRecoverErrorCatalog = [...]daemonRecoverErrorCatalogEntry{
	{daemonRecoverErrorInvalidArgs, true},
	{daemonRecoverErrorConfirmationRequired, true},
	{daemonRecoverErrorUnknownTask, true},
	{daemonRecoverErrorRefusedPortOwner, true},
	{daemonRecoverErrorRespawnFailed, true},
	{daemonRecoverErrorSupervisorUnavailable, true},
	{daemonRecoverErrorRequestCanceled, true},
	{daemonRecoverErrorBoundaryProbeTimeout, true},
	{daemonRecoverErrorRespawnBudgetInsufficient, true},
	{daemonRecoverErrorStateRead, true},
	{daemonRecoverErrorAuditDurability, true},
	{daemonRecoverErrorUnclassifiedFailure, true},
	{daemonRecoverErrorAuditLockAdapterInit, false},
	{daemonRecoverErrorCorrelationInvalid, false},
	{daemonRecoverErrorBaselineStale, false},
	{daemonRecoverErrorAttemptConflict, false},
	{daemonRecoverErrorOccurrenceConsumed, false},
	{daemonRecoverErrorOccurrenceCapacity, false},
	{daemonRecoverErrorReceiptInFlight, false},
	{daemonRecoverErrorOutcomeUncertain, false},
	{daemonRecoverErrorAckPreconditionRequired, false},
	{daemonRecoverErrorAckPhysicalStateChanged, false},
	{daemonRecoverErrorOccurrenceStoreLockStranded, false},
}

func (c daemonRecoverErrorCode) Valid() bool {
	for _, entry := range daemonRecoverErrorCatalog {
		if c == entry.code {
			return entry.persistable
		}
	}
	return false
}

func daemonRecoverErrorCodes() []string {
	codes := make([]string, 0, len(daemonRecoverErrorCatalog))
	for _, entry := range daemonRecoverErrorCatalog {
		codes = append(codes, string(entry.code))
	}
	return codes
}

func registerDaemonRecoverRoutes(s *Server) {
	// Authorization: this local operator endpoint is available only through the
	// same-origin guard used by the GUI's other mutating routes. It has no remote
	// user identity model by design.
	s.mux.HandleFunc("/api/daemon/recover", s.requireSameOrigin(s.daemonRecoverHandler))
	s.mux.HandleFunc("/api/daemon/recover/audit-lock-state", s.requireSameOrigin(s.daemonRecoverAuditLockStateHandler))
	s.mux.HandleFunc("/api/daemon/recover/audit-lock-receipt", s.requireSameOrigin(s.daemonRecoverAuditLockReceiptHandler))
}

func (s *Server) daemonRecoverHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, fmt.Errorf("method %s not allowed", r.Method), http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}
	defer r.Body.Close()

	var req daemonRecoverRequest
	if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "BAD_REQUEST")
		return
	}
	taskName := daemon_env_overlay.NormalizeOverlayKey(strings.TrimSpace(req.TaskName))
	if taskName == "" {
		writeAPIError(w, errors.New("task_name is required"), http.StatusBadRequest, string(daemonRecoverErrorInvalidArgs))
		return
	}
	if api.IsMaintenanceTaskName(taskName) {
		writeAPIError(w, fmt.Errorf("task_name %q is a maintenance task, not a daemon", req.TaskName),
			http.StatusBadRequest, string(daemonRecoverErrorInvalidArgs))
		return
	}
	if !req.Confirm {
		writeAPIError(w, errors.New("explicit recovery confirmation is required"),
			http.StatusPreconditionFailed, string(daemonRecoverErrorConfirmationRequired))
		return
	}

	if readyErr := s.auditLock.ensureReady(); readyErr != nil {
		writeAuditLockRouteError(w, readyErr)
		return
	}

	if req.AuditLockAttempt == nil {
		writeAuditLockRouteError(w, &auditLockRouteError{status: http.StatusBadRequest, code: string(daemonRecoverErrorCorrelationInvalid)})
		return
	}
	correlation, correlationErr := decodeAuditLockCorrelationObject(req.AuditLockAttempt)
	if correlationErr != nil {
		writeAuditLockRouteError(w, correlationErr)
		return
	}
	lease, admitted := s.recoverySettlements.admit(taskName, correlation)
	if !admitted {
		// Admission closes before the occurrence adapter begins closing, so a
		// recovery can never durably reserve work that process shutdown does not
		// own. Reuse the existing 503 no-admission route contract.
		writeAuditLockRouteError(w, &auditLockRouteError{status: http.StatusServiceUnavailable, code: string(daemonRecoverErrorOccurrenceCapacity)})
		return
	}
	leaseCompleted := false
	completeLease := func() {
		if leaseCompleted {
			return
		}
		lease.complete()
		leaseCompleted = true
	}
	defer func() {
		// A panic after the destructive boundary must remain unsettled so the
		// process-level drain fails loud rather than reporting a false clean
		// settlement. Ordinary pre-commit exits have no destructive work and can
		// release their admission immediately.
		if !leaseCompleted && !lease.committed() {
			completeLease()
		}
	}()
	reservation, reserveErr := s.auditLock.reserve(r.Context(), correlation, auditLockOccurrenceBinding{
		serverInstance: correlation.ServerInstance,
		taskName:       taskName,
		confirm:        req.Confirm,
	})
	if reserveErr != nil {
		writeAuditLockRouteError(w, reserveErr)
		return
	}
	if !reservation.Novel {
		completeLease()
		s.writeDaemonRecoverReplay(r.Context(), w, reservation)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := s.daemonRecover.Recover(ctx, taskName, req.Confirm, lease.markCommitted)
	if err != nil {
		status, code := daemonRecoverHTTPFailure(err)
		terminationCommitted := result.TerminationCommitted
		authorization := auditLockAuthorization(result.AuditHandoff, terminationCommitted)
		receipt, terminalErr := s.auditLock.terminalize(reservation, auditLockReceiptStatus(false, terminationCommitted), authorization, auditLockTerminalEvidence{
			HTTPStatus:           status,
			ErrorCode:            code,
			TerminationCommitted: terminationCommitted,
		})
		completeLease()
		if terminalErr != nil {
			s.writeDaemonRecoverTerminalError(w, receipt, terminalErr)
			return
		}
		if authorization == "current_truth" && result.AuditHandoff == daemonrecovery.AuditHandoffReleasePending {
			s.auditLock.armPendingSettlement()
		}
		s.writeDaemonRecoverOccurrenceError(r.Context(), w, status, code, terminationCommitted, receipt)
		return
	}
	if !result.PortOwnerCheck.Valid() {
		receipt, terminalErr := s.auditLock.terminalize(reservation, auditLockReceiptStatus(false, result.TerminationCommitted), auditLockAuthorization(result.AuditHandoff, result.TerminationCommitted), auditLockTerminalEvidence{
			HTTPStatus:           http.StatusInternalServerError,
			ErrorCode:            string(daemonRecoverErrorStateRead),
			TerminationCommitted: result.TerminationCommitted,
		})
		completeLease()
		if terminalErr != nil {
			s.writeDaemonRecoverTerminalError(w, receipt, terminalErr)
			return
		}
		s.writeDaemonRecoverOccurrenceError(r.Context(), w, http.StatusInternalServerError, string(daemonRecoverErrorStateRead), result.TerminationCommitted, receipt)
		return
	}
	if !result.PortWaitOutcome.Valid() {
		receipt, terminalErr := s.auditLock.terminalize(reservation, auditLockReceiptStatus(false, result.TerminationCommitted), auditLockAuthorization(result.AuditHandoff, result.TerminationCommitted), auditLockTerminalEvidence{
			HTTPStatus:           http.StatusInternalServerError,
			ErrorCode:            string(daemonRecoverErrorStateRead),
			TerminationCommitted: result.TerminationCommitted,
		})
		completeLease()
		if terminalErr != nil {
			s.writeDaemonRecoverTerminalError(w, receipt, terminalErr)
			return
		}
		s.writeDaemonRecoverOccurrenceError(r.Context(), w, http.StatusInternalServerError, string(daemonRecoverErrorStateRead), result.TerminationCommitted, receipt)
		return
	}
	if !result.AuditHandoff.Valid() {
		receipt, terminalErr := s.auditLock.terminalize(reservation, auditLockReceiptStatus(false, result.TerminationCommitted), auditLockAuthorization(result.AuditHandoff, result.TerminationCommitted), auditLockTerminalEvidence{
			HTTPStatus:           http.StatusInternalServerError,
			ErrorCode:            string(daemonRecoverErrorStateRead),
			TerminationCommitted: result.TerminationCommitted,
		})
		completeLease()
		if terminalErr != nil {
			s.writeDaemonRecoverTerminalError(w, receipt, terminalErr)
			return
		}
		s.writeDaemonRecoverOccurrenceError(r.Context(), w, http.StatusInternalServerError, string(daemonRecoverErrorStateRead), result.TerminationCommitted, receipt)
		return
	}

	authorization := auditLockAuthorization(result.AuditHandoff, result.TerminationCommitted)
	terminalReceipt, terminalErr := s.auditLock.terminalize(reservation, auditLockOccurrenceCommittedSuccess, authorization, auditLockTerminalEvidence{
		HTTPStatus: http.StatusOK,
		Success: &daemonRecoverSuccessEvidence{
			TaskName:             taskName,
			Reaped:               result.Reaped,
			PortOwnerCheck:       string(result.PortOwnerCheck),
			PortWaitOutcome:      string(result.PortWaitOutcome),
			AuditHandoff:         string(result.AuditHandoff),
			TerminationCommitted: result.TerminationCommitted,
		},
	})
	completeLease()
	if terminalErr != nil {
		s.writeDaemonRecoverTerminalError(w, terminalReceipt, terminalErr)
		return
	}
	if result.AuditHandoff == daemonrecovery.AuditHandoffReleasePending {
		s.auditLock.armPendingSettlement()
	}
	lockSnapshot := s.auditLock.snapshotAfterTerminal(r.Context(), &terminalReceipt)
	w.Header().Set("Content-Type", "application/json")
	// audit_handoff is a WARNING field on a SUCCESS response: "release_unconfirmed"
	// means this GUI process may still hold the supervisor-events.log flock, which
	// blocks the supervisor and the install CLI until the GUI exits. The recovery
	// itself still succeeded, so it must not be retried.
	_ = json.NewEncoder(w).Encode(daemonRecoverResponse{
		TaskName:             taskName,
		State:                "respawn_accepted",
		Reaped:               result.Reaped,
		PortOwnerCheck:       string(result.PortOwnerCheck),
		PortWaitOutcome:      string(result.PortWaitOutcome),
		AuditHandoff:         string(result.AuditHandoff),
		TerminationCommitted: result.TerminationCommitted,
		AuditLock:            lockSnapshot,
	})
}

func auditLockReceiptStatus(success, terminationCommitted bool) string {
	if success {
		return auditLockOccurrenceCommittedSuccess
	}
	if terminationCommitted {
		return auditLockOccurrenceCommittedError
	}
	return auditLockOccurrenceNotCommitted
}

func auditLockAuthorization(handoff daemonrecovery.AuditHandoff, committed bool) string {
	if !committed {
		return auditLockAuthorizationNone
	}
	switch handoff {
	case daemonrecovery.AuditHandoffReleasePending, daemonrecovery.AuditHandoffReleaseUnconfirmed:
		return auditLockAuthorizationCurrentTruth
	case daemonrecovery.AuditHandoffNotRequired, daemonrecovery.AuditHandoffDurable:
		return auditLockAuthorizationNone
	default:
		return auditLockAuthorizationUncertain
	}
}

func writeAuditLockRouteError(w http.ResponseWriter, routeErr *auditLockRouteError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(routeErr.status)
	body := map[string]any{
		"error": "internal error",
		"code":  routeErr.code,
	}
	if health := routeErr.occurrenceStoreHealth; health != nil {
		body["occurrence_store_health"] = health.State
		body["occurrence_store_health_revision"] = health.Revision
		body["restart_required"] = health.RestartRequired
	}
	if routeErr.auditLockStateProjection != nil {
		body["audit_lock"] = routeErr.auditLockStateProjection
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) writeDaemonRecoverOccurrenceError(ctx context.Context, w http.ResponseWriter, status int, code string, terminationCommitted bool, receipt auditLockReceiptDTO) {
	snapshot := s.auditLock.snapshotAfterTerminal(ctx, &receipt)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(daemonRecoverOccurrenceErrorResponse{
		Error:                "internal error",
		Code:                 code,
		TerminationCommitted: terminationCommitted,
		AuditLock:            snapshot,
	})
}

func (s *Server) writeDaemonRecoverUncertain(w http.ResponseWriter, receipt auditLockReceiptDTO) {
	snapshot := s.auditLock.snapshotProjection(&receipt)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(daemonRecoverOccurrenceErrorResponse{
		Error:                  "internal error",
		Code:                   string(daemonRecoverErrorOutcomeUncertain),
		TerminationCommitted:   true,
		TerminationCommitState: auditLockTerminationStateUnknown,
		AuditLock:              snapshot,
	})
}

// writeDaemonRecoverTerminalError is the single wire owner for every
// terminalization failure exit. A process-stranded occurrence-store lock is
// authoritative and uses the stable typed 503 route; genuine non-health
// uncertainty retains the existing 409 response contract.
func (s *Server) writeDaemonRecoverTerminalError(w http.ResponseWriter, receipt auditLockReceiptDTO, routeErr *auditLockRouteError) {
	if routeErr != nil && routeErr.occurrenceStoreHealth != nil {
		if routeErr.auditLockStateProjection == nil {
			copy := *routeErr
			projection := s.auditLock.snapshotProjection(&receipt)
			copy.auditLockStateProjection = &projection
			routeErr = &copy
		}
		writeAuditLockRouteError(w, routeErr)
		return
	}
	s.writeDaemonRecoverUncertain(w, receipt)
}

func (s *Server) writeDaemonRecoverReplay(ctx context.Context, w http.ResponseWriter, reservation auditLockReservation) {
	receipt := reservation.Receipt
	snapshot := s.auditLock.snapshotAfterTerminal(ctx, &receipt)
	if receipt.Status == auditLockOccurrenceUncertain {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(daemonRecoverOccurrenceErrorResponse{
			Error:                  "internal error",
			Code:                   string(daemonRecoverErrorOutcomeUncertain),
			TerminationCommitted:   true,
			TerminationCommitState: auditLockTerminationStateUnknown,
			AuditLock:              snapshot,
		})
		return
	}
	if reservation.Terminal == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state":      "recovery_in_flight",
			"audit_lock": snapshot,
		})
		return
	}
	terminal := reservation.Terminal
	if terminal.Success == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(terminal.HTTPStatus)
		_ = json.NewEncoder(w).Encode(daemonRecoverOccurrenceErrorResponse{
			Error:                "internal error",
			Code:                 terminal.ErrorCode,
			TerminationCommitted: terminal.TerminationCommitted,
			AuditLock:            snapshot,
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(daemonRecoverResponse{
		TaskName:             terminal.Success.TaskName,
		State:                "respawn_accepted",
		Reaped:               terminal.Success.Reaped,
		PortOwnerCheck:       terminal.Success.PortOwnerCheck,
		PortWaitOutcome:      terminal.Success.PortWaitOutcome,
		AuditHandoff:         terminal.Success.AuditHandoff,
		TerminationCommitted: terminal.Success.TerminationCommitted,
		AuditLock:            snapshot,
	})
}

func decodeAuditLockCorrelationQuery(r *http.Request) (auditLockCorrelation, bool, *auditLockRouteError) {
	query := r.URL.Query()
	if len(query) == 0 {
		return auditLockCorrelation{}, false, nil
	}
	if len(query) != 3 {
		return auditLockCorrelation{}, false, &auditLockRouteError{status: http.StatusBadRequest, code: string(daemonRecoverErrorCorrelationInvalid)}
	}
	correlation := auditLockCorrelation{}
	targets := map[string]*string{
		"attempt_id":      &correlation.AttemptID,
		"occurrence_id":   &correlation.OccurrenceID,
		"server_instance": &correlation.ServerInstance,
	}
	for field, target := range targets {
		values, ok := query[field]
		if !ok || len(values) != 1 {
			return auditLockCorrelation{}, false, &auditLockRouteError{status: http.StatusBadRequest, code: string(daemonRecoverErrorCorrelationInvalid)}
		}
		*target = values[0]
	}
	if validationErr := validateAuditLockCorrelation(correlation); validationErr != nil {
		return auditLockCorrelation{}, false, validationErr
	}
	return correlation, true, nil
}

func (s *Server) daemonRecoverAuditLockStateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, fmt.Errorf("method %s not allowed", r.Method), http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}
	correlation, lookup, correlationErr := decodeAuditLockCorrelationQuery(r)
	if correlationErr != nil {
		writeAuditLockRouteError(w, correlationErr)
		return
	}
	var receipt *auditLockReceiptDTO
	if lookup {
		var lookupErr *auditLockRouteError
		receipt, lookupErr = s.auditLock.lookup(r.Context(), correlation)
		if lookupErr != nil {
			writeAuditLockRouteError(w, lookupErr)
			return
		}
	}
	snapshot, snapshotErr := s.auditLock.snapshot(r.Context(), receipt)
	if snapshotErr != nil {
		writeAuditLockRouteError(w, snapshotErr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshot)
}

func decodeAuditLockExpectedPhysical(raw json.RawMessage) (auditLockExpectedPhysical, *auditLockRouteError) {
	fields, err := decodeUniqueJSONObject(raw)
	if err != nil || len(fields) != 3 {
		return auditLockExpectedPhysical{}, &auditLockRouteError{status: http.StatusBadRequest, code: string(daemonRecoverErrorCorrelationInvalid)}
	}
	var expected auditLockExpectedPhysical
	if value, ok := fields["server_instance"]; !ok || json.Unmarshal(value, &expected.ServerInstance) != nil ||
		validateAuditLockCorrelationValue("server_instance", expected.ServerInstance) != nil {
		return auditLockExpectedPhysical{}, &auditLockRouteError{status: http.StatusBadRequest, code: string(daemonRecoverErrorCorrelationInvalid)}
	}
	if value, ok := fields["revision"]; !ok || json.Unmarshal(value, &expected.Revision) != nil {
		return auditLockExpectedPhysical{}, &auditLockRouteError{status: http.StatusBadRequest, code: string(daemonRecoverErrorCorrelationInvalid)}
	}
	var state string
	if value, ok := fields["state"]; !ok || json.Unmarshal(value, &state) != nil || state != string(api.SupervisorEventLockReleased) {
		return auditLockExpectedPhysical{}, &auditLockRouteError{status: http.StatusBadRequest, code: string(daemonRecoverErrorCorrelationInvalid)}
	}
	expected.State = api.SupervisorEventLockReleased
	for field := range fields {
		switch field {
		case "server_instance", "revision", "state":
		default:
			return auditLockExpectedPhysical{}, &auditLockRouteError{status: http.StatusBadRequest, code: string(daemonRecoverErrorCorrelationInvalid)}
		}
	}
	return expected, nil
}

func decodeAuditLockAcknowledge(raw json.RawMessage) (auditLockAcknowledgeRequest, *auditLockRouteError) {
	fields, err := decodeUniqueJSONObject(raw)
	if err != nil || (len(fields) != 4 && len(fields) != 5) {
		return auditLockAcknowledgeRequest{}, &auditLockRouteError{status: http.StatusBadRequest, code: string(daemonRecoverErrorCorrelationInvalid)}
	}
	correlationRaw, err := json.Marshal(map[string]json.RawMessage{
		"attempt_id":      fields["attempt_id"],
		"occurrence_id":   fields["occurrence_id"],
		"server_instance": fields["server_instance"],
	})
	if err != nil {
		return auditLockAcknowledgeRequest{}, &auditLockRouteError{status: http.StatusBadRequest, code: string(daemonRecoverErrorCorrelationInvalid)}
	}
	correlation, correlationErr := decodeAuditLockCorrelationObject(correlationRaw)
	if correlationErr != nil {
		return auditLockAcknowledgeRequest{}, correlationErr
	}
	var acknowledge bool
	if value, ok := fields["acknowledge"]; !ok || json.Unmarshal(value, &acknowledge) != nil || !acknowledge {
		return auditLockAcknowledgeRequest{}, &auditLockRouteError{status: http.StatusBadRequest, code: string(daemonRecoverErrorCorrelationInvalid)}
	}
	request := auditLockAcknowledgeRequest{Correlation: correlation}
	if rawExpected, ok := fields["expected_physical"]; ok {
		expected, expectedErr := decodeAuditLockExpectedPhysical(rawExpected)
		if expectedErr != nil {
			return auditLockAcknowledgeRequest{}, expectedErr
		}
		request.ExpectedPhysical = &expected
	}
	for field := range fields {
		switch field {
		case "attempt_id", "occurrence_id", "server_instance", "acknowledge", "expected_physical":
		default:
			return auditLockAcknowledgeRequest{}, &auditLockRouteError{status: http.StatusBadRequest, code: string(daemonRecoverErrorCorrelationInvalid)}
		}
	}
	return request, nil
}

func (s *Server) daemonRecoverAuditLockReceiptHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeAPIError(w, fmt.Errorf("method %s not allowed", r.Method), http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}
	defer r.Body.Close()
	var raw json.RawMessage
	if err := decodeJSONBodyLimited(w, r, &raw, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "BAD_REQUEST")
		return
	}
	acknowledgement, correlationErr := decodeAuditLockAcknowledge(raw)
	if correlationErr != nil {
		writeAuditLockRouteError(w, correlationErr)
		return
	}
	if acknowledgeErr := s.auditLock.acknowledgeRequest(r.Context(), acknowledgement); acknowledgeErr != nil {
		writeAuditLockRouteError(w, acknowledgeErr)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func daemonRecoverHTTPFailure(err error) (int, string) {
	var operationErr *daemonrecovery.OperationError
	if !errors.As(err, &operationErr) {
		return http.StatusInternalServerError, string(daemonRecoverErrorUnclassifiedFailure)
	}
	switch operationErr.Kind {
	case daemonrecovery.FailureInvalidArgs:
		return http.StatusBadRequest, string(daemonRecoverErrorInvalidArgs)
	case daemonrecovery.FailureConfirmationRequired:
		return http.StatusPreconditionFailed, string(daemonRecoverErrorConfirmationRequired)
	case daemonrecovery.FailureUnknownTask:
		return http.StatusBadRequest, string(daemonRecoverErrorUnknownTask)
	case daemonrecovery.FailureRefusedPortOwner:
		return http.StatusConflict, string(daemonRecoverErrorRefusedPortOwner)
	case daemonrecovery.FailureRespawnFailed:
		return http.StatusInternalServerError, string(daemonRecoverErrorRespawnFailed)
	case daemonrecovery.FailureSupervisorUnavailable:
		return http.StatusServiceUnavailable, string(daemonRecoverErrorSupervisorUnavailable)
	case daemonrecovery.FailureRequestCanceled:
		return http.StatusRequestTimeout, string(daemonRecoverErrorRequestCanceled)
	case daemonrecovery.FailureBoundaryProbeTimeout:
		return http.StatusGatewayTimeout, string(daemonRecoverErrorBoundaryProbeTimeout)
	case daemonrecovery.FailureRespawnBudgetInsufficient:
		return http.StatusServiceUnavailable, string(daemonRecoverErrorRespawnBudgetInsufficient)
	case daemonrecovery.FailureStateRead:
		return http.StatusInternalServerError, string(daemonRecoverErrorStateRead)
	case daemonrecovery.FailureAuditDurability:
		// Mirrors the CLI's dedicated exit-7 semantic (internal/cli/daemon_recover.go):
		// the termination WAS committed and the respawn was attempted; only the audit
		// record or its durable handoff could not be preserved. It gets its own code
		// rather than reusing a neighbour because RECOVER_RESPAWN_FAILED would assert
		// a failed respawn (it may well have been accepted) and RECOVER_STATE_READ_FAILED
		// would assert a pre-mutation read failure — both would tell an operator that
		// nothing was destroyed, and invite a retry of a destructive recovery that
		// already completed. The redacted body's termination_committed field carries
		// the "do not retry" signal.
		return http.StatusInternalServerError, string(daemonRecoverErrorAuditDurability)
	default:
		return http.StatusInternalServerError, string(daemonRecoverErrorUnclassifiedFailure)
	}
}
