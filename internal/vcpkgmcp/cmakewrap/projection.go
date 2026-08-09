package cmakewrap

import (
	"mcp-local-hub/internal/cmakegraph"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

const (
	graphProjectionIdentityEntries = 4
	graphProjectionValueBytes      = publicresult.MaxEncodedBytes / 32
)

// PublicResultProjection keeps bounded causal graph identity and explicitly
// enumerates every collection reduced by the public-result budget.
func (r Result) PublicResultProjection() any {
	unscanned, unscannedOmissions := projectUnscanned(r.UnscannedFiles)
	projectedEvidence, evidenceOmissions := projectGraphEvidence(r.Evidence)
	omissions := []publicresult.Omission{
		wholeGraphOmission("edges", len(r.Edges)),
		wholeGraphOmission("files", len(r.Files)),
	}
	omissions = append(omissions, unscannedOmissions...)
	omissions = append(omissions, evidenceOmissions...)
	return struct {
		Status                Status                     `json:"status"`
		Reason                Reason                     `json:"reason,omitempty"`
		Root                  string                     `json:"root,omitempty"`
		WorkspaceRoot         string                     `json:"workspace_root,omitempty"`
		Histogram             cmakegraph.Histogram       `json:"histogram"`
		NodeCapTruncated      bool                       `json:"node_cap_truncated"`
		RootsSkippedByNodeCap int                        `json:"roots_skipped_by_node_cap,omitempty"`
		RootEnumerationCapped bool                       `json:"root_enumeration_capped"`
		EdgeCapTruncated      bool                       `json:"edge_cap_truncated"`
		RootsSkippedByEdgeCap int                        `json:"roots_skipped_by_edge_cap,omitempty"`
		RetainedEdgeBytes     int64                      `json:"retained_edge_bytes,omitempty"`
		CoverageCapTruncated  bool                       `json:"coverage_cap_truncated"`
		DroppedCoverageHoles  int                        `json:"dropped_coverage_holes,omitempty"`
		RetainedCoverageBytes int64                      `json:"retained_coverage_bytes,omitempty"`
		UnscannedFiles        []cmakegraph.UnscannedFile `json:"unscanned_files,omitempty"`
		Evidence              evidence.Evidence          `json:"evidence"`
		ResultProjection      publicresult.Projection    `json:"result_projection"`
	}{
		r.Status, r.Reason, r.Root, r.WorkspaceRoot, r.Histogram,
		r.NodeCapTruncated, r.RootsSkippedByNodeCap, r.RootEnumerationCapped,
		r.EdgeCapTruncated, r.RootsSkippedByEdgeCap, r.RetainedEdgeBytes,
		r.CoverageCapTruncated, r.DroppedCoverageHoles, r.RetainedCoverageBytes,
		unscanned, projectedEvidence,
		publicresult.Projection{Complete: false, Omissions: omissions},
	}
}

func wholeGraphOmission(field string, total int) publicresult.Omission {
	omitted := total
	return publicresult.Omission{Field: field, Reason: publicresult.InternalProjectionLimit, Retained: 0, Omitted: &omitted}
}

func projectUnscanned(source []cmakegraph.UnscannedFile) ([]cmakegraph.UnscannedFile, []publicresult.Omission) {
	retained := len(source)
	if retained > graphProjectionIdentityEntries {
		retained = graphProjectionIdentityEntries
	}
	out := make([]cmakegraph.UnscannedFile, 0, retained)
	for _, hole := range source[:retained] {
		hole.Path = publicresult.AbbreviateEncoded(hole.Path, graphProjectionValueBytes)
		hole.Detail = publicresult.AbbreviateEncoded(hole.Detail, graphProjectionValueBytes)
		out = append(out, hole)
	}
	if retained == len(source) {
		return out, nil
	}
	omitted := len(source) - retained
	return out, []publicresult.Omission{{Field: "unscanned_files", Reason: publicresult.InternalProjectionLimit, Retained: retained, Omitted: &omitted}}
}

func projectGraphEvidence(source evidence.Evidence) (evidence.Evidence, []publicresult.Omission) {
	var projected evidence.Evidence
	var omissions []publicresult.Omission
	retainedPaths := min(len(source.Paths), graphProjectionIdentityEntries)
	for _, path := range source.Paths[:retainedPaths] {
		projected.Paths = append(projected.Paths, publicresult.AbbreviateEncoded(path, graphProjectionValueBytes))
	}
	if retainedPaths < len(source.Paths) {
		omitted := len(source.Paths) - retainedPaths
		omissions = append(omissions, publicresult.Omission{Field: "evidence.paths", Reason: publicresult.InternalProjectionLimit, Retained: retainedPaths, Omitted: &omitted})
	}
	if len(source.Commands) != 0 {
		omissions = append(omissions, wholeGraphOmission("evidence.commands", len(source.Commands)))
	}
	if len(source.Locations) != 0 {
		omissions = append(omissions, wholeGraphOmission("evidence.locations", len(source.Locations)))
	}
	return projected, omissions
}
