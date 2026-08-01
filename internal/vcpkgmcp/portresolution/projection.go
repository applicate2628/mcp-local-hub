package portresolution

import "mcp-local-hub/internal/vcpkgmcp/publicresult"

// PublicResultProjection preserves the verdict and winning/blocking location
// while omitting unbounded candidate and shadow lists.
func (r Result) PublicResultProjection() any {
	return struct {
		Status            Status                  `json:"status"`
		Reason            Reason                  `json:"reason,omitempty"`
		Winner            *Winner                 `json:"winner,omitempty"`
		BlockingCandidate *CandidateLocation      `json:"blocking_candidate,omitempty"`
		InvalidRoot       string                  `json:"invalid_root,omitempty"`
		InvalidPort       string                  `json:"invalid_port,omitempty"`
		ResultProjection  publicresult.Projection `json:"result_projection"`
	}{r.Status, r.Reason, r.Winner, r.BlockingCandidate, r.InvalidRoot, r.InvalidPort, publicresult.MinimalProjection("all_candidates")}
}
