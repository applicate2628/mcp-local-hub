package api

import (
	"fmt"

	"mcp-local-hub/internal/mcpcompat/readinesswire"
)

const (
	MCPReadinessNotChecked    = "not_checked"
	MCPReadinessReady         = "ready"
	MCPReadinessUnready       = "unready"
	MCPReadinessNotApplicable = "not_applicable"

	MCPReadinessStageNone           = "none"
	MCPReadinessStageProfile        = "profile"
	MCPReadinessStageIdentity       = "identity"
	MCPReadinessStageInitialize     = "initialize"
	MCPReadinessStageInitialized    = "initialized"
	MCPReadinessStageToolsList      = "tools_list"
	MCPReadinessStageSessionCleanup = "session_cleanup"

	MCPReadinessFailureSessionCleanup = "MCP_HEALTH_SESSION_CLEANUP_FAILED"
)

// MCPReadinessResult is the one additive API projection shared by install,
// status, health, capability, command-line, and GUI consumers.
type MCPReadinessResult struct {
	SchemaVersion      string `json:"schema_version"`
	State              string `json:"state"`
	Stage              string `json:"stage"`
	FailureID          string `json:"failure_id"`
	SecondaryStage     string `json:"secondary_stage,omitempty"`
	SecondaryFailureID string `json:"secondary_failure_id,omitempty"`
	HTTPStatus         int    `json:"http_status"`
	RequestedProtocol  string `json:"requested_protocol"`
	NegotiatedProtocol string `json:"negotiated_protocol"`
	SupportedFloor     string `json:"supported_floor"`
	ProfileID          string `json:"profile_id"`
	ToolCount          int    `json:"tool_count"`
	Detail             string `json:"detail"`
}

// MCPReadinessError keeps the typed projection intact through call paths that
// retain an error return for compatibility.
type MCPReadinessError struct {
	Result MCPReadinessResult
}

func (e *MCPReadinessError) Error() string {
	if e == nil {
		return ""
	}
	return e.Result.Detail
}

func NewMCPReadinessNotChecked() MCPReadinessResult {
	return MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessNotChecked, Stage: MCPReadinessStageNone}
}

func NewMCPReadinessNotApplicable() MCPReadinessResult {
	return MCPReadinessResult{SchemaVersion: "mcp-readiness-v1", State: MCPReadinessNotApplicable, Stage: MCPReadinessStageNone}
}

func readinessStageFromWire(stage string) string {
	switch stage {
	case readinesswire.StageProfile:
		return MCPReadinessStageProfile
	case readinesswire.StageIdentity:
		return MCPReadinessStageIdentity
	case readinesswire.StageInitialize:
		return MCPReadinessStageInitialize
	case readinesswire.StageInitialized:
		return MCPReadinessStageInitialized
	case readinesswire.StageToolsList:
		return MCPReadinessStageToolsList
	case readinesswire.StageToolCall, readinesswire.StageResponse:
		return MCPReadinessStageInitialized
	default:
		return MCPReadinessStageNone
	}
}

func closedReadinessDetail(result MCPReadinessResult) string {
	if result.FailureID == "" {
		return ""
	}
	detail := fmt.Sprintf("%s at %s", result.FailureID, result.Stage)
	if result.HTTPStatus != 0 {
		detail += fmt.Sprintf(" (HTTP %d", result.HTTPStatus)
		if result.RequestedProtocol != "" {
			detail += "; requested " + result.RequestedProtocol
		}
		if result.NegotiatedProtocol != "" {
			detail += "; negotiated " + result.NegotiatedProtocol
		}
		detail += ")"
	}
	return detail
}

func readinessFromFailure(f readinesswire.Failure) MCPReadinessResult {
	result := MCPReadinessResult{
		SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready,
		Stage: readinessStageFromWire(f.Stage), FailureID: f.FailureID,
		HTTPStatus: f.HTTPStatus, RequestedProtocol: f.RequestedProtocol,
		NegotiatedProtocol: f.NegotiatedProtocol, SupportedFloor: f.SupportedFloor,
		ProfileID: f.ProfileID,
	}
	result.Detail = closedReadinessDetail(result)
	return result
}

func healthProbeFromReadiness(result MCPReadinessResult) *HealthProbe {
	return &HealthProbe{
		OK: result.State == MCPReadinessReady, ToolCount: result.ToolCount,
		Err: result.Detail, Readiness: result,
	}
}

// withSecondaryReadinessFailure keeps the first failed readiness stage as the
// primary result. Cleanup happens after the forward probe, so its failure is
// still observable but must not erase the cause that stopped forward progress.
func withSecondaryReadinessFailure(primary, secondary MCPReadinessResult) MCPReadinessResult {
	if primary.State != MCPReadinessUnready || primary.FailureID == "" {
		return secondary
	}
	primary.SecondaryStage = secondary.Stage
	primary.SecondaryFailureID = secondary.FailureID
	return primary
}

func normalizeDaemonReadiness(rows []DaemonStatus) {
	for i := range rows {
		if rows[i].Health != nil && rows[i].Health.Readiness.SchemaVersion == "mcp-readiness-v1" {
			rows[i].MCPReadiness = rows[i].Health.Readiness
			continue
		}
		if rows[i].MCPReadiness.SchemaVersion == "mcp-readiness-v1" {
			continue
		}
		if rows[i].IsMaintenance {
			rows[i].MCPReadiness = NewMCPReadinessNotApplicable()
			continue
		}
		rows[i].MCPReadiness = NewMCPReadinessNotChecked()
	}
}

// ProbeMCPReadiness returns the canonical typed projection while the retained
// HealthProbe compatibility fields are derived by singleHealthProbe.
func ProbeMCPReadiness(port int) MCPReadinessResult {
	probe := singleHealthProbe(port)
	if probe == nil || probe.Readiness.SchemaVersion != "mcp-readiness-v1" {
		result := MCPReadinessResult{
			SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready,
			Stage:     MCPReadinessStageNone,
			FailureID: readinesswire.FailureReadinessHostUnavailable,
		}
		result.Detail = closedReadinessDetail(result)
		return result
	}
	return probe.Readiness
}
