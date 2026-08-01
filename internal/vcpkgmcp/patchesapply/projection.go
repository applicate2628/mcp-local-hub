package patchesapply

import "mcp-local-hub/internal/vcpkgmcp/publicresult"

// PublicResultProjection retains the request identity and verdict when the
// per-patch evidence collections exceed the common output budget.
func (r Result) PublicResultProjection() any {
	return struct {
		Status           Status                  `json:"status"`
		Reason           Reason                  `json:"reason,omitempty"`
		Triplet          string                  `json:"triplet,omitempty"`
		PortDir          string                  `json:"port_dir,omitempty"`
		TripletFile      string                  `json:"triplet_file,omitempty"`
		ResultProjection publicresult.Projection `json:"result_projection"`
	}{r.Status, r.Reason, r.Triplet, r.PortDir, r.TripletFile, publicresult.MinimalProjection("patch_results")}
}
