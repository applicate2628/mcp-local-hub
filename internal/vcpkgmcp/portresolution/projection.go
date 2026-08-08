package portresolution

import "mcp-local-hub/internal/vcpkgmcp/publicresult"

// PublicResultProjection preserves the verdict and winning/blocking location
// while omitting unbounded candidate and shadow lists.
func (r Result) PublicResultProjection() any {
	const scalarAllowance = publicresult.MaxEncodedBytes / 16
	winner := r.Winner
	if winner != nil {
		copy := *winner
		copy.Directory = publicresult.AbbreviateEncoded(copy.Directory, scalarAllowance)
		copy.Source = publicresult.AbbreviateEncoded(copy.Source, scalarAllowance)
		winner = &copy
	}
	blocking := r.BlockingCandidate
	if blocking != nil {
		copy := *blocking
		copy.Directory = publicresult.AbbreviateEncoded(copy.Directory, scalarAllowance)
		copy.Source = publicresult.AbbreviateEncoded(copy.Source, scalarAllowance)
		copy.Reason = publicresult.AbbreviateEncoded(copy.Reason, scalarAllowance)
		blocking = &copy
	}
	return struct {
		Status            Status                  `json:"status"`
		Reason            Reason                  `json:"reason,omitempty"`
		Winner            *Winner                 `json:"winner,omitempty"`
		BlockingCandidate *CandidateLocation      `json:"blocking_candidate,omitempty"`
		InvalidRoot       string                  `json:"invalid_root,omitempty"`
		InvalidPort       string                  `json:"invalid_port,omitempty"`
		ResultProjection  publicresult.Projection `json:"result_projection"`
	}{r.Status, r.Reason, winner, blocking, publicresult.AbbreviateEncoded(r.InvalidRoot, scalarAllowance), publicresult.AbbreviateEncoded(r.InvalidPort, scalarAllowance), publicresult.MinimalProjection("all_candidates")}
}
