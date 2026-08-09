package patchesapply

import (
	"encoding/json"

	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

// projectionIdentityByteLimit bounds each retained identity before it enters a
// projected result. The divisor leaves room for JSON escaping, the other two
// identity fields, and field-specific omission metadata.
const projectionIdentityByteLimit = publicresult.MaxEncodedBytes / 32

// PublicResultRequiresProjection bounds every owner-retained collection before
// the common marshaler constructs the complete indented JSON value. Each
// aggregate item is admitted separately and traversal stops at saturation.
func (r Result) PublicResultRequiresProjection(limit int) bool {
	admission := publicresult.NewProjectionAdmission(limit)
	scalars := struct {
		Status              Status
		Reason              Reason
		Triplet             string
		PortDir             string
		TripletFile         string
		OrphanScanStopCause OrphanScanStopCause
	}{r.Status, r.Reason, r.Triplet, r.PortDir, r.TripletFile, r.OrphanScanStopCause}
	if admission.AddJSON(scalars) {
		return true
	}
	for _, item := range r.Applied {
		if admission.AddJSON(item) {
			return true
		}
	}
	for _, item := range r.ConditionalNotApplied {
		if admission.AddJSON(item) {
			return true
		}
	}
	for _, item := range r.Undecidable {
		if admission.AddJSON(item) {
			return true
		}
	}
	for _, item := range r.Orphaned {
		if admission.AddJSON(item) {
			return true
		}
	}
	for _, item := range r.Missing {
		if admission.AddJSON(item) {
			return true
		}
	}
	for _, item := range r.Unreadable {
		if admission.AddJSON(item) {
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
	return admission.RequiresProjection()
}

// PublicResultProjection retains the request identity and verdict when the
// per-patch evidence collections exceed the common output budget.
func (r Result) PublicResultProjection() any {
	projected := projectedResult{
		Status: r.Status,
		Reason: r.Reason,
		ResultProjection: publicresult.Projection{Complete: false, Omissions: []publicresult.Omission{
			publicresult.MinimalProjection("patch_results").Omissions[0],
		}},
	}
	projected.admitIdentity("triplet", r.Triplet)
	projected.admitIdentity("port_dir", r.PortDir)
	projected.admitIdentity("triplet_file", r.TripletFile)
	return projected
}

type projectedResult struct {
	Status           Status                  `json:"status"`
	Reason           Reason                  `json:"reason,omitempty"`
	Triplet          string                  `json:"triplet,omitempty"`
	PortDir          string                  `json:"port_dir,omitempty"`
	TripletFile      string                  `json:"triplet_file,omitempty"`
	ResultProjection publicresult.Projection `json:"result_projection"`
}

func (r *projectedResult) admitIdentity(field, value string) {
	if value == "" {
		return
	}
	if len(value) > projectionIdentityByteLimit {
		r.omitIdentity(field)
		return
	}

	candidate := *r
	switch field {
	case "triplet":
		candidate.Triplet = value
	case "port_dir":
		candidate.PortDir = value
	case "triplet_file":
		candidate.TripletFile = value
	}
	encoded, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil || len(encoded) > publicresult.MaxEncodedBytes {
		r.omitIdentity(field)
		return
	}
	*r = candidate
}

func (r *projectedResult) omitIdentity(field string) {
	omitted := 1
	r.ResultProjection.Omissions = append(r.ResultProjection.Omissions, publicresult.Omission{
		Field: field, Reason: publicresult.InternalProjectionLimit, Omitted: &omitted,
	})
}
