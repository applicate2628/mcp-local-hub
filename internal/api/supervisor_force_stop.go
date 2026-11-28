package api

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrOriginalProcessOwnerRequired = errors.New("original process creator authority required")

// OriginalProcessOwnerRequiredError is a refusal, never an already-gone
// classification. A PID alone cannot recover the at-create process/Job owner.
type OriginalProcessOwnerRequiredError struct {
	PID int
}

func (e *OriginalProcessOwnerRequiredError) Error() string {
	return fmt.Sprintf("%s: PID %d is an observation; cleanup is incomplete", ErrOriginalProcessOwnerRequired, e.PID)
}

func (e *OriginalProcessOwnerRequiredError) Unwrap() error { return ErrOriginalProcessOwnerRequired }

const (
	ForceStopClosed            = "closed"
	ForceStopRefused           = "refused"
	ForceStopIncomplete        = "incomplete"
	ForceStopOwnerUnavailable  = "FORCE_STOP_OWNER_UNAVAILABLE"
	ForceStopUnsupported       = "FORCE_STOP_UNSUPPORTED"
	ForceStopGenerationChanged = "FORCE_STOP_GENERATION_CHANGED"
	ForceStopNotRunning        = "FORCE_STOP_NOT_RUNNING"
	ForceStopIntentStopped     = "FORCE_STOP_INTENT_STOPPED"
	ForceStopClosureIncomplete = "FORCE_STOP_CLOSURE_INCOMPLETE"
	ForceStopControllerBusy    = "FORCE_STOP_CONTROLLER_BUSY"
	ForceStopInvalidRequest    = "FORCE_STOP_INVALID_REQUEST"
	ForceStopRetained          = "retained"
	ForceStopRetainRemoved     = "retain_removed"
	ForceStopCompleteRemoved   = "complete_removed"
)

// ForceStopTargetV1 is an observation used to select one original creator in
// the receiving Supervisor. Neither these numbers nor a status response grant
// the client process termination authority. ExpectedPort is only observed after
// closure; it is never used to select a process to kill.
type ForceStopTargetV1 struct {
	TaskName      string `json:"task_name"`
	PID           int    `json:"pid"`
	StartedAt     string `json:"started_at"`
	PIDGeneration int    `json:"pid_generation"`
	ExpectedPort  int    `json:"expected_port"`
}

type ForceStopCommandV1 struct {
	ProtocolVersion int               `json:"protocol_version"`
	Target          ForceStopTargetV1 `json:"target"`
	// RemovalPhase binds prepare/complete to one authenticated connection.
	// Empty preserves the explicit force-stop contract and running intent.
	RemovalPhase string `json:"removal_phase,omitempty"`
}

// ForceStopResultV1 describes the original generation, not its successor.
// RootExited is the original wait result. TreeClosed and HandlesClosed require
// the retained creator's native terminal receipt; root exit alone is insufficient.
// PortState is a separate current observation: not_observed, unspecified,
// free, occupied, or unknown. A closed tree may already have a successor using
// the port. Unspecified means no positive port was supplied, not a free-port
// proof. Desired intent and ordinary crash/backoff/restart policy are unchanged.
type ForceStopResultV1 struct {
	ProtocolVersion int               `json:"protocol_version"`
	Target          ForceStopTargetV1 `json:"target"`
	State           string            `json:"state"`
	Reason          string            `json:"reason"`
	RootExited      bool              `json:"root_exited"`
	TreeClosed      bool              `json:"tree_closed"`
	HandlesClosed   bool              `json:"handles_closed"`
	Action          string            `json:"action"`
	PortState       string            `json:"port_state"`
	PortOwnerPID    int               `json:"port_owner_pid,omitempty"`
	PortError       string            `json:"port_error,omitempty"`
	Error           string            `json:"error,omitempty"`
}

func ValidateForceStopCommandV1(command ForceStopCommandV1) error {
	if command.RemovalPhase != "" && command.RemovalPhase != ForceStopRetainRemoved && command.RemovalPhase != ForceStopCompleteRemoved {
		return &IPCErr{Code: ForceStopInvalidRequest, Message: "unsupported force_stop removal phase"}
	}
	target := command.Target
	started, err := time.Parse(time.RFC3339Nano, target.StartedAt)
	if command.ProtocolVersion != 1 || target.TaskName == "" || target.TaskName != canonicalIntentTaskKey(target.TaskName) ||
		strings.TrimSpace(target.TaskName) != target.TaskName || target.PID <= 0 || target.PIDGeneration <= 0 || err != nil || started.IsZero() ||
		target.ExpectedPort < 0 || target.ExpectedPort > 65535 {
		return &IPCErr{Code: ForceStopInvalidRequest, Message: "force_stop v1 requires an exact task generation and valid port"}
	}
	return nil
}

func validateForceStopResult(command ForceStopCommandV1, result ForceStopResultV1) error {
	if result.ProtocolVersion != command.ProtocolVersion || result.Target != command.Target {
		return fmt.Errorf("force_stop response does not echo the requested generation")
	}
	switch result.State {
	case ForceStopRetained:
		if command.RemovalPhase != ForceStopRetainRemoved || result.Reason != ForceStopRetained || result.Error != "" ||
			result.RootExited || result.TreeClosed || result.HandlesClosed || result.Action != "not-requested" || result.PortState != "not_observed" {
			return fmt.Errorf("force_stop retained response does not prove a connection-bound capture")
		}
	case ForceStopClosed:
		if command.RemovalPhase == ForceStopRetainRemoved {
			return fmt.Errorf("force_stop preparation unexpectedly performed termination")
		}
		if !result.RootExited || !result.TreeClosed || !result.HandlesClosed || result.Error != "" || result.Reason != ForceStopClosed {
			return fmt.Errorf("force_stop closed response lacks the original creator's complete terminal receipt")
		}
	case ForceStopRefused, ForceStopIncomplete:
		if result.Reason == "" || result.Error == "" {
			return fmt.Errorf("force_stop refusal/incomplete response lacks its cause")
		}
	default:
		return fmt.Errorf("force_stop response has unknown state %q", result.State)
	}
	switch result.PortState {
	case "not_observed", "unspecified", "free", "occupied", "unknown":
	default:
		return fmt.Errorf("force_stop response has unknown port state %q", result.PortState)
	}
	return nil
}
