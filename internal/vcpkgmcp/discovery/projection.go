package discovery

import "mcp-local-hub/internal/vcpkgmcp/publicresult"

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
