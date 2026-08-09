package portresolution

import "mcp-local-hub/internal/vcpkgmcp/publicresult"

// PublicResultRequiresProjection pre-admits repeated candidate, shadow, and
// evidence rows separately from the scalar result envelope.
func (r Result) PublicResultRequiresProjection(limit int) bool {
	envelope := r
	envelope.AllCandidates = nil
	envelope.Shadows = nil
	envelope.Evidence.Paths = nil
	envelope.Evidence.Commands = nil
	envelope.Evidence.Locations = nil
	admission := publicresult.NewProjectionAdmission(limit)
	if admission.AddJSON(envelope) {
		return true
	}
	for _, candidate := range r.AllCandidates {
		if admission.AddJSON(candidate) {
			return true
		}
	}
	for _, shadow := range r.Shadows {
		if admission.AddJSON(shadow) {
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
		Status                            Status                  `json:"status"`
		Reason                            Reason                  `json:"reason,omitempty"`
		Winner                            *Winner                 `json:"winner,omitempty"`
		BlockingCandidate                 *CandidateLocation      `json:"blocking_candidate,omitempty"`
		InvalidRoot                       string                  `json:"invalid_root,omitempty"`
		InvalidPort                       string                  `json:"invalid_port,omitempty"`
		OverlayToOverlayShadowingOccurred bool                    `json:"overlay_to_overlay_shadowing_occurred,omitempty"`
		ResultProjection                  publicresult.Projection `json:"result_projection"`
	}{
		Status:                            r.Status,
		Reason:                            r.Reason,
		Winner:                            winner,
		BlockingCandidate:                 blocking,
		InvalidRoot:                       publicresult.AbbreviateEncoded(r.InvalidRoot, scalarAllowance),
		InvalidPort:                       publicresult.AbbreviateEncoded(r.InvalidPort, scalarAllowance),
		OverlayToOverlayShadowingOccurred: r.OverlayToOverlayShadowingOccurred,
		ResultProjection: publicresult.Projection{Complete: false, Omissions: []publicresult.Omission{
			{Field: "all_candidates", Reason: publicresult.InternalProjectionLimit},
			{Field: "shadows", Reason: publicresult.InternalProjectionLimit},
			{Field: "evidence", Reason: publicresult.InternalProjectionLimit},
		}},
	}
}
