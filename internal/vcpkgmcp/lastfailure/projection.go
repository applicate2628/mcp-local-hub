package lastfailure

import (
	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

// PublicResultProjection retains the causal failure core and resource
// completeness when ranked diagnostics and path collections exceed the common
// encoded-result budget.
func (r Result) PublicResultProjection() any {
	return struct {
		Status                  Status                  `json:"status"`
		Reason                  Reason                  `json:"reason,omitempty"`
		Phase                   Phase                   `json:"phase,omitempty"`
		FailedTarget            string                  `json:"failed_target,omitempty"`
		ExactCommand            string                  `json:"exact_command,omitempty"`
		BuildCommand            string                  `json:"build_command,omitempty"`
		DiagnosticLog           string                  `json:"diagnostic_log,omitempty"`
		FirstError              *Diagnostic             `json:"first_error,omitempty"`
		DiagnosticsDropped      int                     `json:"diagnostics_dropped,omitempty"`
		DiagnosticsDroppedExact bool                    `json:"diagnostics_dropped_exact"`
		ExitCode                *int                    `json:"exit_code,omitempty"`
		Resources               ResourceReport          `json:"resources"`
		ResultProjection        publicresult.Projection `json:"result_projection"`
	}{r.Status, r.Reason, r.Phase, r.FailedTarget, r.ExactCommand, r.BuildCommand,
		r.DiagnosticLog, r.FirstError, r.DiagnosticsDropped, r.DiagnosticsDroppedExact,
		r.ExitCode, r.Resources, lastFailureProjection(r)}
}

func lastFailureProjection(r Result) publicresult.Projection {
	omissions := make([]publicresult.Omission, 0, 6)
	add := func(field string, count int) {
		if count == 0 {
			return
		}
		omitted := count
		omissions = append(omissions, publicresult.Omission{
			Field: field, Reason: publicresult.InternalProjectionLimit, Omitted: &omitted,
		})
	}
	add("diagnostics", len(r.Diagnostics))
	add("log_paths", len(r.LogPaths))
	add("overlay_chain", len(r.OverlayChain))
	add("context_source", len(r.ContextSource))
	add("notes", len(r.Notes))
	add("evidence", evidenceItemCount(r.Evidence))
	return publicresult.Projection{Complete: false, Omissions: omissions}
}

func evidenceItemCount(ev evidence.Evidence) int {
	return len(ev.Paths) + len(ev.Commands) + len(ev.Locations)
}
