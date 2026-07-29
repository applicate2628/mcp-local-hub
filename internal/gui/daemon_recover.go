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
	Recover(ctx context.Context, taskName string, confirmed bool) (daemonrecovery.Result, error)
}

type realDaemonRecoverer struct{}

func (realDaemonRecoverer) Recover(ctx context.Context, taskName string, confirmed bool) (daemonrecovery.Result, error) {
	return daemonrecovery.Execute(ctx, taskName, daemonrecovery.Options{Confirmed: confirmed})
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
	TaskName        string            `json:"task_name"`
	State           string            `json:"state"`
	Reaped          bool              `json:"reaped"`
	PortOwnerCheck  string            `json:"port_owner_check"`
	PortWaitOutcome string            `json:"port_wait_outcome"`
	AuditHandoff    string            `json:"audit_handoff"`
	AuditLock       auditLockStateDTO `json:"audit_lock"`
}

type daemonRecoverOccurrenceErrorResponse struct {
	Error                string            `json:"error"`
	Code                 string            `json:"code"`
	TerminationCommitted bool              `json:"termination_committed"`
	AuditLock            auditLockStateDTO `json:"audit_lock"`
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
		writeAPIError(w, errors.New("task_name is required"), http.StatusBadRequest, "INVALID_ARGS")
		return
	}
	if api.IsMaintenanceTaskName(taskName) {
		writeAPIError(w, fmt.Errorf("task_name %q is a maintenance task, not a daemon", req.TaskName),
			http.StatusBadRequest, "INVALID_ARGS")
		return
	}
	if !req.Confirm {
		writeAPIError(w, errors.New("explicit recovery confirmation is required"),
			http.StatusPreconditionFailed, "RECOVER_CONFIRMATION_REQUIRED")
		return
	}

	if readyErr := s.auditLock.ensureReady(); readyErr != nil {
		writeAuditLockRouteError(w, readyErr)
		return
	}

	var correlation *auditLockCorrelation
	if req.AuditLockAttempt != nil {
		parsed, correlationErr := decodeAuditLockCorrelationObject(req.AuditLockAttempt)
		if correlationErr != nil {
			writeAuditLockRouteError(w, correlationErr)
			return
		}
		correlation = &parsed
		reservation, reserveErr := s.auditLock.reserve(parsed, auditLockOccurrenceBinding{
			serverInstance: parsed.ServerInstance,
			taskName:       taskName,
			confirm:        req.Confirm,
		})
		if reserveErr != nil {
			writeAuditLockRouteError(w, reserveErr)
			return
		}
		if !reservation.Novel {
			s.writeDaemonRecoverReplay(w, reservation)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := s.daemonRecover.Recover(ctx, taskName, req.Confirm)
	if err != nil {
		status, code := daemonRecoverHTTPFailure(err)
		terminationCommitted := result.PortOwnerCheck == daemonrecovery.PortOwnerReaped ||
			result.PortOwnerCheck == daemonrecovery.PortOwnerTerminationUnconfirmed
		if correlation != nil {
			authorization := auditLockAuthorization(result.AuditHandoff, terminationCommitted)
			receipt := s.auditLock.terminalize(*correlation, auditLockReceiptStatus(false, terminationCommitted), authorization, auditLockTerminalEvidence{
				HTTPStatus:           status,
				ErrorCode:            code,
				TerminationCommitted: terminationCommitted,
			})
			if authorization == "current_truth" && result.AuditHandoff == daemonrecovery.AuditHandoffReleasePending {
				s.auditLock.armPendingSettlement()
			}
			s.writeDaemonRecoverOccurrenceError(w, status, code, terminationCommitted, receipt)
			return
		}
		writeAPIErrorRedactedFields(w, err, status, code, "/api/daemon/recover", map[string]bool{
			"termination_committed": terminationCommitted,
		})
		return
	}
	if !result.PortOwnerCheck.Valid() {
		if correlation != nil {
			receipt := s.auditLock.terminalize(*correlation, "committed_error", "uncertain", auditLockTerminalEvidence{
				HTTPStatus:           http.StatusInternalServerError,
				ErrorCode:            "RECOVER_STATE_READ_FAILED",
				TerminationCommitted: true,
			})
			s.writeDaemonRecoverOccurrenceError(w, http.StatusInternalServerError, "RECOVER_STATE_READ_FAILED", true, receipt)
			return
		}
		writeAPIErrorRedacted(w, fmt.Errorf("invalid port owner check %q", result.PortOwnerCheck),
			http.StatusInternalServerError, "RECOVER_STATE_READ_FAILED", "/api/daemon/recover response")
		return
	}
	if !result.PortWaitOutcome.Valid() {
		if correlation != nil {
			receipt := s.auditLock.terminalize(*correlation, "committed_error", "uncertain", auditLockTerminalEvidence{
				HTTPStatus:           http.StatusInternalServerError,
				ErrorCode:            "RECOVER_STATE_READ_FAILED",
				TerminationCommitted: true,
			})
			s.writeDaemonRecoverOccurrenceError(w, http.StatusInternalServerError, "RECOVER_STATE_READ_FAILED", true, receipt)
			return
		}
		writeAPIErrorRedacted(w, fmt.Errorf("invalid port wait outcome %q", result.PortWaitOutcome),
			http.StatusInternalServerError, "RECOVER_STATE_READ_FAILED", "/api/daemon/recover response")
		return
	}
	if !result.AuditHandoff.Valid() {
		if correlation != nil {
			receipt := s.auditLock.terminalize(*correlation, "committed_error", "uncertain", auditLockTerminalEvidence{
				HTTPStatus:           http.StatusInternalServerError,
				ErrorCode:            "RECOVER_STATE_READ_FAILED",
				TerminationCommitted: true,
			})
			s.writeDaemonRecoverOccurrenceError(w, http.StatusInternalServerError, "RECOVER_STATE_READ_FAILED", true, receipt)
			return
		}
		writeAPIErrorRedacted(w, fmt.Errorf("invalid audit handoff %q", result.AuditHandoff),
			http.StatusInternalServerError, "RECOVER_STATE_READ_FAILED", "/api/daemon/recover response")
		return
	}

	var receipt *auditLockReceiptDTO
	if correlation != nil {
		authorization := auditLockAuthorization(result.AuditHandoff, true)
		terminalReceipt := s.auditLock.terminalize(*correlation, "committed_success", authorization, auditLockTerminalEvidence{
			HTTPStatus: http.StatusOK,
			Success: &daemonRecoverSuccessEvidence{
				TaskName:        taskName,
				Reaped:          result.Reaped,
				PortOwnerCheck:  string(result.PortOwnerCheck),
				PortWaitOutcome: string(result.PortWaitOutcome),
				AuditHandoff:    string(result.AuditHandoff),
			},
		})
		receipt = &terminalReceipt
		if result.AuditHandoff == daemonrecovery.AuditHandoffReleasePending {
			s.auditLock.armPendingSettlement()
		}
	}
	lockSnapshot, snapshotErr := s.auditLock.snapshot(receipt)
	if snapshotErr != nil {
		writeAuditLockRouteError(w, snapshotErr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// audit_handoff is a WARNING field on a SUCCESS response: "release_unconfirmed"
	// means this GUI process may still hold the supervisor-events.log flock, which
	// blocks the supervisor and the install CLI until the GUI exits. The recovery
	// itself still succeeded, so it must not be retried.
	_ = json.NewEncoder(w).Encode(daemonRecoverResponse{
		TaskName:        taskName,
		State:           "respawn_accepted",
		Reaped:          result.Reaped,
		PortOwnerCheck:  string(result.PortOwnerCheck),
		PortWaitOutcome: string(result.PortWaitOutcome),
		AuditHandoff:    string(result.AuditHandoff),
		AuditLock:       lockSnapshot,
	})
}

func auditLockReceiptStatus(success, terminationCommitted bool) string {
	if success {
		return "committed_success"
	}
	if terminationCommitted {
		return "committed_error"
	}
	return "not_committed"
}

func auditLockAuthorization(handoff daemonrecovery.AuditHandoff, committed bool) string {
	if !committed {
		return "none"
	}
	switch handoff {
	case daemonrecovery.AuditHandoffReleasePending, daemonrecovery.AuditHandoffReleaseUnconfirmed:
		return "current_truth"
	case daemonrecovery.AuditHandoffNotRequired, daemonrecovery.AuditHandoffDurable:
		return "none"
	default:
		return "uncertain"
	}
}

func writeAuditLockRouteError(w http.ResponseWriter, routeErr *auditLockRouteError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(routeErr.status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": "internal error",
		"code":  routeErr.code,
	})
}

func (s *Server) writeDaemonRecoverOccurrenceError(w http.ResponseWriter, status int, code string, terminationCommitted bool, receipt auditLockReceiptDTO) {
	snapshot, routeErr := s.auditLock.snapshot(&receipt)
	if routeErr != nil {
		writeAuditLockRouteError(w, routeErr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(daemonRecoverOccurrenceErrorResponse{
		Error:                "internal error",
		Code:                 code,
		TerminationCommitted: terminationCommitted,
		AuditLock:            snapshot,
	})
}

func (s *Server) writeDaemonRecoverReplay(w http.ResponseWriter, reservation auditLockReservation) {
	receipt := reservation.Receipt
	snapshot, routeErr := s.auditLock.snapshot(&receipt)
	if routeErr != nil {
		writeAuditLockRouteError(w, routeErr)
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
		TaskName:        terminal.Success.TaskName,
		State:           "respawn_accepted",
		Reaped:          terminal.Success.Reaped,
		PortOwnerCheck:  terminal.Success.PortOwnerCheck,
		PortWaitOutcome: terminal.Success.PortWaitOutcome,
		AuditHandoff:    terminal.Success.AuditHandoff,
		AuditLock:       snapshot,
	})
}

func decodeAuditLockCorrelationQuery(r *http.Request) (auditLockCorrelation, bool, *auditLockRouteError) {
	query := r.URL.Query()
	if len(query) == 0 {
		return auditLockCorrelation{}, false, nil
	}
	if len(query) != 3 {
		return auditLockCorrelation{}, false, &auditLockRouteError{status: http.StatusBadRequest, code: "RECOVER_CORRELATION_INVALID"}
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
			return auditLockCorrelation{}, false, &auditLockRouteError{status: http.StatusBadRequest, code: "RECOVER_CORRELATION_INVALID"}
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
		receipt, lookupErr = s.auditLock.lookup(correlation)
		if lookupErr != nil {
			writeAuditLockRouteError(w, lookupErr)
			return
		}
	}
	snapshot, snapshotErr := s.auditLock.snapshot(receipt)
	if snapshotErr != nil {
		writeAuditLockRouteError(w, snapshotErr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshot)
}

func decodeAuditLockAcknowledge(raw json.RawMessage) (auditLockCorrelation, *auditLockRouteError) {
	fields, err := decodeUniqueJSONObject(raw)
	if err != nil || len(fields) != 4 {
		return auditLockCorrelation{}, &auditLockRouteError{status: http.StatusBadRequest, code: "RECOVER_CORRELATION_INVALID"}
	}
	correlationRaw, err := json.Marshal(map[string]json.RawMessage{
		"attempt_id":      fields["attempt_id"],
		"occurrence_id":   fields["occurrence_id"],
		"server_instance": fields["server_instance"],
	})
	if err != nil {
		return auditLockCorrelation{}, &auditLockRouteError{status: http.StatusBadRequest, code: "RECOVER_CORRELATION_INVALID"}
	}
	correlation, correlationErr := decodeAuditLockCorrelationObject(correlationRaw)
	if correlationErr != nil {
		return auditLockCorrelation{}, correlationErr
	}
	var acknowledge bool
	if value, ok := fields["acknowledge"]; !ok || json.Unmarshal(value, &acknowledge) != nil || !acknowledge {
		return auditLockCorrelation{}, &auditLockRouteError{status: http.StatusBadRequest, code: "RECOVER_CORRELATION_INVALID"}
	}
	for field := range fields {
		switch field {
		case "attempt_id", "occurrence_id", "server_instance", "acknowledge":
		default:
			return auditLockCorrelation{}, &auditLockRouteError{status: http.StatusBadRequest, code: "RECOVER_CORRELATION_INVALID"}
		}
	}
	return correlation, nil
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
	correlation, correlationErr := decodeAuditLockAcknowledge(raw)
	if correlationErr != nil {
		writeAuditLockRouteError(w, correlationErr)
		return
	}
	if acknowledgeErr := s.auditLock.acknowledge(correlation); acknowledgeErr != nil {
		writeAuditLockRouteError(w, acknowledgeErr)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func daemonRecoverHTTPFailure(err error) (int, string) {
	var operationErr *daemonrecovery.OperationError
	if !errors.As(err, &operationErr) {
		return http.StatusInternalServerError, "RECOVER_UNCLASSIFIED_FAILURE"
	}
	switch operationErr.Kind {
	case daemonrecovery.FailureInvalidArgs:
		return http.StatusBadRequest, "INVALID_ARGS"
	case daemonrecovery.FailureConfirmationRequired:
		return http.StatusPreconditionFailed, "RECOVER_CONFIRMATION_REQUIRED"
	case daemonrecovery.FailureUnknownTask:
		return http.StatusBadRequest, "RECOVER_UNKNOWN_TASK"
	case daemonrecovery.FailureRefusedPortOwner:
		return http.StatusConflict, "RECOVER_REFUSED_PORT_OWNER"
	case daemonrecovery.FailureRespawnFailed:
		return http.StatusInternalServerError, "RECOVER_RESPAWN_FAILED"
	case daemonrecovery.FailureSupervisorUnavailable:
		return http.StatusServiceUnavailable, "RECOVER_SUPERVISOR_UNAVAILABLE"
	case daemonrecovery.FailureRequestCanceled:
		return http.StatusRequestTimeout, "RECOVER_REQUEST_CANCELED"
	case daemonrecovery.FailureBoundaryProbeTimeout:
		return http.StatusGatewayTimeout, "RECOVER_BOUNDARY_PROBE_TIMEOUT"
	case daemonrecovery.FailureRespawnBudgetInsufficient:
		return http.StatusServiceUnavailable, "RECOVER_RESPAWN_BUDGET_INSUFFICIENT"
	case daemonrecovery.FailureStateRead:
		return http.StatusInternalServerError, "RECOVER_STATE_READ_FAILED"
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
		return http.StatusInternalServerError, "RECOVER_AUDIT_DURABILITY_FAILED"
	default:
		return http.StatusInternalServerError, "RECOVER_UNCLASSIFIED_FAILURE"
	}
}
