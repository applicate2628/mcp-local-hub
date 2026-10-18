package gui

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"mcp-local-hub/internal/api"
)

type SupervisorRestartOutcome string

const (
	SupervisorRestartRestarted SupervisorRestartOutcome = "restarted"
	SupervisorRestartPending   SupervisorRestartOutcome = "pending"
	SupervisorRestartRefused   SupervisorRestartOutcome = "refused"
	SupervisorRestartFailed    SupervisorRestartOutcome = "failed"
)

// SupervisorRestartResult carries actual lifecycle facts. Creation and transfer
// commitment alone never establish restart completion. Generations are DATA,
// not authority to control the named process.
type SupervisorRestartResult struct {
	KilledPID                int                      `json:"killed_pid"`
	Killed                   bool                     `json:"killed"`
	SpawnedPID               int                      `json:"spawned_pid"`
	Spawned                  bool                     `json:"spawned"`
	PerStepError             map[string]string        `json:"per_step_error,omitempty"`
	Outcome                  SupervisorRestartOutcome `json:"outcome"`
	Ready                    bool                     `json:"ready"`
	OriginalGeneration       *api.SupervisorLockOwner `json:"original_generation,omitempty"`
	SelectedGeneration       *api.SupervisorLockOwner `json:"selected_generation,omitempty"`
	OriginalSettled          bool                     `json:"original_settled"`
	Phase                    string                   `json:"phase"`
	TransferCommitted        bool                     `json:"transfer_committed"`
	Transferred              bool                     `json:"transferred"`
	OriginalChildDisposition string                   `json:"original_child_disposition"`
	NamespaceDisposition     string                   `json:"namespace_disposition"`
	Code                     string                   `json:"code,omitempty"`
}

// OriginalChildDisposition is none, settled, retained, transferred, pending or
// unknown; it describes this operation's actual created actor/closers, never a
// foreign namespace winner. NamespaceDisposition is cleared, occupied or unknown.
// Phase is admission, stop, settlement, namespace, spawn, readiness, commit,
// transfer, complete or unknown. Code/PerStepError are safe classifications;
// the separate returned error retains the original diagnostic causes.
type SupervisorRestartHandler func(context.Context) (SupervisorRestartResult, error)

// SetSupervisorRestartHandler is composed by the original CLI GUI lifetime
// before serving. A standby or uncomposed server has no destructive fallback.
func (s *Server) SetSupervisorRestartHandler(handler SupervisorRestartHandler) {
	s.supervisorRestartHandler = handler
}

func registerSupervisorRestartRoutes(s *Server) {
	s.mux.HandleFunc("/api/supervisor/restart", s.requireSameOrigin(s.supervisorRestartHTTPHandler))
}

func (s *Server) supervisorRestartHTTPHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// This route has no caller-selected target or capability input. The
	// callback selects and retains its original target under its own owners.
	if s.supervisorRestartHandler == nil {
		writeSupervisorRestartResult(w, SupervisorRestartResult{
			Outcome: SupervisorRestartRefused, Phase: "admission",
			OriginalChildDisposition: "none", NamespaceDisposition: "unknown",
			Code: "RESTART_UNAVAILABLE",
		}, nil)
		return
	}

	result, ownerErr := s.supervisorRestartHandler(r.Context())
	response := projectSupervisorRestartResult(result, ownerErr)
	fields := map[string]any{
		"killed_pid": response.KilledPID, "killed": response.Killed,
		"spawned_pid": response.SpawnedPID, "spawned": response.Spawned,
		"outcome": response.Outcome, "ready": response.Ready,
		"original_generation": response.OriginalGeneration,
		"selected_generation": response.SelectedGeneration,
		"original_settled":    response.OriginalSettled, "phase": response.Phase,
		"transfer_committed": response.TransferCommitted, "transferred": response.Transferred,
		"original_child_disposition": response.OriginalChildDisposition,
		"namespace_disposition":      response.NamespaceDisposition,
		"step_errors":                result.PerStepError, "code": response.Code,
	}
	if ownerErr != nil {
		fields["owner_error"] = ownerErr.Error()
		log.Printf("/api/supervisor/restart owner: %v", ownerErr)
	}
	deliveryErr := api.LogHubMcpEventRequired("info", "supervisor-restart-via-gui", fields)
	if deliveryErr != nil {
		log.Printf("/api/supervisor/restart delivery: %v", errors.Join(ownerErr, deliveryErr))
		if response.Outcome == SupervisorRestartRestarted {
			response.Outcome = SupervisorRestartFailed
		}
		if response.PerStepError == nil {
			response.PerStepError = map[string]string{}
		}
		response.PerStepError["diagnostic"] = "restart diagnostic delivery unconfirmed"
		if response.Code == "" {
			response.Code = "RESTART_DIAGNOSTIC_UNCONFIRMED"
		}
	}
	if response.Outcome == SupervisorRestartRestarted {
		s.events.PublishOperatorAction("supervisor-restart", api.CurrentOSUser(), map[string]any{
			"killed_pid": response.KilledPID, "killed": response.Killed,
			"spawned_pid":         response.SpawnedPID,
			"selected_generation": response.SelectedGeneration, "ready": response.Ready,
		})
	}
	writeSupervisorRestartResult(w, response, errors.Join(ownerErr, deliveryErr))
}

func writeSupervisorRestartResult(w http.ResponseWriter, response SupervisorRestartResult, acquiredErr error) {
	w.Header().Set("Content-Type", "application/json")
	// Composite results preserve the existing HTTP200 operational contract.
	// Outcome and all actual effect facts remain in the response.
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("/api/supervisor/restart response: %v", errors.Join(acquiredErr, err))
	}
}

// projectSupervisorRestartResult enforces only the typed transport contract,
// not native identity/control policy. Raw callback errors/step messages and
// unknown classification strings never enter the public response.
func projectSupervisorRestartResult(result SupervisorRestartResult, ownerErr error) SupervisorRestartResult {
	response := result
	if result.OriginalGeneration != nil {
		original := *result.OriginalGeneration
		response.OriginalGeneration = &original
	}
	if result.SelectedGeneration != nil {
		selected := *result.SelectedGeneration
		response.SelectedGeneration = &selected
	}
	response.PerStepError = nil
	for step := range result.PerStepError {
		switch step {
		case "read_lock", "kill", "spawn", "admission", "settlement", "readiness", "commit", "transfer", "namespace", "diagnostic":
		default:
			step = "restart"
		}
		if response.PerStepError == nil {
			response.PerStepError = map[string]string{}
		}
		response.PerStepError[step] = "restart step incomplete"
	}
	switch response.Phase {
	case "admission", "stop", "settlement", "namespace", "spawn", "readiness", "commit", "transfer", "complete":
	default:
		response.Phase = "unknown"
	}
	switch response.OriginalChildDisposition {
	case "none", "settled", "retained", "transferred", "pending":
	default:
		response.OriginalChildDisposition = "unknown"
	}
	switch response.NamespaceDisposition {
	case "cleared", "occupied":
	default:
		response.NamespaceDisposition = "unknown"
	}
	validGeneration := func(owner *api.SupervisorLockOwner) bool {
		if owner == nil || owner.PID <= 0 {
			return false
		}
		startedAt, err := time.Parse(time.RFC3339Nano, owner.StartedAt)
		return err == nil && !startedAt.IsZero()
	}
	if response.OriginalGeneration != nil && !validGeneration(response.OriginalGeneration) {
		response.OriginalGeneration = nil
	}
	if response.SelectedGeneration != nil && !validGeneration(response.SelectedGeneration) {
		response.SelectedGeneration = nil
	}
	switch response.Code {
	case "", "RESTART_UNAVAILABLE", "RESTART_NOT_READY", "RESTART_NAMESPACE_CONFLICT",
		"RESTART_NAMESPACE_UNCONFIRMED", "RESTART_GENERATION_CONFLICT",
		"RESTART_IDENTITY_UNCONFIRMED", "RESTART_STOPPED", "RESTART_CANCELED",
		"RESTART_FAILED", "RESTART_PENDING", "RESTART_RESULT_UNCONFIRMED",
		"RESTART_DIAGNOSTIC_UNCONFIRMED":
	default:
		response.Code = "RESTART_RESULT_UNCONFIRMED"
	}
	switch response.Outcome {
	case SupervisorRestartRestarted:
		custody := response.OriginalChildDisposition == "retained" ||
			(response.OriginalChildDisposition == "transferred" && response.TransferCommitted && response.Transferred)
		if ownerErr != nil || len(response.PerStepError) != 0 || response.Code != "" ||
			!response.Ready || !response.OriginalSettled || response.SelectedGeneration == nil ||
			!response.Spawned || response.SpawnedPID <= 0 || (response.SelectedGeneration != nil && response.SelectedGeneration.PID != response.SpawnedPID) ||
			!custody || response.NamespaceDisposition != "occupied" {
			response.Outcome = SupervisorRestartFailed
			if response.Code == "" {
				response.Code = "RESTART_RESULT_UNCONFIRMED"
			}
		}
	case SupervisorRestartPending, SupervisorRestartRefused, SupervisorRestartFailed:
	default:
		response.Outcome = SupervisorRestartFailed
		response.Code = "RESTART_RESULT_UNCONFIRMED"
	}
	if ownerErr != nil || response.Outcome != SupervisorRestartRestarted {
		if response.PerStepError == nil {
			response.PerStepError = map[string]string{}
		}
		if len(response.PerStepError) == 0 {
			response.PerStepError["restart"] = "restart did not complete"
		}
	}
	return response
}
