package discovery

import "mcp-local-hub/internal/vcpkgmcp/publicresult"

// PublicResultRequiresProjection pre-admits the scalar envelope and each
// retained evidence row independently, avoiding one complete aggregate JSON
// allocation at the public boundary.
func (r Result) PublicResultRequiresProjection(limit int) bool {
	envelope := r
	envelope.Candidates = nil
	envelope.Evidence.Paths = nil
	envelope.Evidence.Commands = nil
	envelope.Evidence.Locations = nil
	admission := publicresult.NewProjectionAdmission(limit)
	if admission.AddJSON(envelope) {
		return true
	}
	for _, candidate := range r.Candidates {
		if admission.AddJSON(candidate) {
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

// PublicResultProjection keeps the resolved root/rule causal core when the
// complete candidate evidence cannot fit in the universal result budget.
func (r Result) PublicResultProjection() any {
	return struct {
		Status           Status                  `json:"status"`
		Reason           Reason                  `json:"reason,omitempty"`
		RuleFired        Rule                    `json:"rule_fired,omitempty"`
		Root             string                  `json:"root,omitempty"`
		ResultProjection publicresult.Projection `json:"result_projection"`
	}{r.Status, r.Reason, r.RuleFired, r.Root, publicresult.MinimalProjection("candidates")}
}
