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
	TaskName string `json:"task_name"`
	Confirm  bool   `json:"confirm"`
}

func registerDaemonRecoverRoutes(s *Server) {
	// Authorization: this local operator endpoint is available only through the
	// same-origin guard used by the GUI's other mutating routes. It has no remote
	// user identity model by design.
	s.mux.HandleFunc("/api/daemon/recover", s.requireSameOrigin(s.daemonRecoverHandler))
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

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := s.daemonRecover.Recover(ctx, taskName, req.Confirm)
	if err != nil {
		status, code := daemonRecoverHTTPFailure(err)
		terminationCommitted := result.PortOwnerCheck == daemonrecovery.PortOwnerReaped ||
			result.PortOwnerCheck == daemonrecovery.PortOwnerTerminationUnconfirmed
		writeAPIErrorRedactedFields(w, err, status, code, "/api/daemon/recover", map[string]bool{
			"termination_committed": terminationCommitted,
		})
		return
	}
	if !result.PortOwnerCheck.Valid() {
		writeAPIErrorRedacted(w, fmt.Errorf("invalid port owner check %q", result.PortOwnerCheck),
			http.StatusInternalServerError, "RECOVER_STATE_READ_FAILED", "/api/daemon/recover response")
		return
	}
	if !result.PortWaitOutcome.Valid() {
		writeAPIErrorRedacted(w, fmt.Errorf("invalid port wait outcome %q", result.PortWaitOutcome),
			http.StatusInternalServerError, "RECOVER_STATE_READ_FAILED", "/api/daemon/recover response")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"task_name":         taskName,
		"state":             "respawn_accepted",
		"reaped":            result.Reaped,
		"port_owner_check":  result.PortOwnerCheck,
		"port_wait_outcome": result.PortWaitOutcome,
	})
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
	default:
		return http.StatusInternalServerError, "RECOVER_UNCLASSIFIED_FAILURE"
	}
}
