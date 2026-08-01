package cmaketrace

import "mcp-local-hub/internal/vcpkgmcp/publicresult"

// PublicResultProjection retains the trace-verdict completeness signals while
// omitting large record and index collections.
func (r Result) PublicResultProjection() any {
	return struct {
		Status                 Status                  `json:"status"`
		Reason                 Reason                  `json:"reason,omitempty"`
		MalformedLineCount     int                     `json:"malformed_line_count"`
		InputIncomplete        bool                    `json:"input_incomplete"`
		InputIncompleteReasons []Reason                `json:"input_incomplete_reasons,omitempty"`
		VersionHeaderPresent   bool                    `json:"version_header_present"`
		Truncated              bool                    `json:"truncated"`
		ResultProjection       publicresult.Projection `json:"result_projection"`
	}{r.Status, r.Reason, r.MalformedLineCount, r.InputIncomplete, r.InputIncompleteReasons,
		r.VersionHeaderPresent, r.Truncated, publicresult.MinimalProjection("records")}
}
