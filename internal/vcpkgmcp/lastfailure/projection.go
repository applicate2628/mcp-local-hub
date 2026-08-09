package lastfailure

import (
	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

// PublicResultRequiresProjection pre-admits every repeated diagnostic and
// evidence value independently from the scalar causal envelope.
func (r Result) PublicResultRequiresProjection(limit int) bool {
	envelope := r
	envelope.Diagnostics = nil
	envelope.LogPaths = nil
	envelope.OverlayChain = nil
	envelope.ContextSource = nil
	envelope.Notes = nil
	envelope.Evidence.Paths = nil
	envelope.Evidence.Commands = nil
	envelope.Evidence.Locations = nil
	admission := publicresult.NewProjectionAdmission(limit)
	if admission.AddJSON(envelope) {
		return true
	}
	for _, diagnostic := range r.Diagnostics {
		if admission.AddJSON(diagnostic) {
			return true
		}
	}
	for _, path := range r.LogPaths {
		if admission.AddJSON(path) {
			return true
		}
	}
	for _, overlay := range r.OverlayChain {
		if admission.AddJSON(overlay) {
			return true
		}
	}
	for _, source := range r.ContextSource {
		if admission.AddJSON(source) {
			return true
		}
	}
	for _, note := range r.Notes {
		if admission.AddJSON(note) {
			return true
		}
	}
	for _, path := range r.Evidence.Paths {
		if admission.AddJSON(path) {
			return true
		}
	}
	for _, command := range r.Evidence.Commands {
		if admission.AddJSON(command) {
			return true
		}
	}
	for _, location := range r.Evidence.Locations {
		if admission.AddJSON(location) {
			return true
		}
	}
	return false
}

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
