package cmakewrap

import "mcp-local-hub/internal/vcpkgmcp/publicresult"

// PublicResultProjection keeps the graph roots and coverage flags while
// omitting potentially large graph collections.
func (r Result) PublicResultProjection() any {
	return struct {
		Status                Status                  `json:"status"`
		Reason                Reason                  `json:"reason,omitempty"`
		Root                  string                  `json:"root,omitempty"`
		WorkspaceRoot         string                  `json:"workspace_root,omitempty"`
		NodeCapTruncated      bool                    `json:"node_cap_truncated"`
		RootsSkippedByNodeCap int                     `json:"roots_skipped_by_node_cap,omitempty"`
		RootEnumerationCapped bool                    `json:"root_enumeration_capped"`
		ResultProjection      publicresult.Projection `json:"result_projection"`
	}{r.Status, r.Reason, r.Root, r.WorkspaceRoot, r.NodeCapTruncated,
		r.RootsSkippedByNodeCap, r.RootEnumerationCapped, publicresult.MinimalProjection("edges")}
}
