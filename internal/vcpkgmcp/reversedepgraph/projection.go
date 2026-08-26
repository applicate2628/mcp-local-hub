package reversedepgraph

import "mcp-local-hub/internal/vcpkgmcp/publicresult"

func (result Result) PublicResultRequiresProjection(limit int) bool {
	admission := publicresult.NewProjectionAdmission(limit)
	scalar := result
	scalar.Targets = nil
	scalar.Direct = nil
	scalar.Transitive = nil
	scalar.Edges = nil
	scalar.Cycles = nil
	scalar.Provenance = nil
	scalar.Diagnostics = nil
	scalar.Evidence.Paths = nil
	scalar.Evidence.Commands = nil
	scalar.Evidence.Locations = nil
	scalar.Projection = nil
	admission.AddJSON(scalar)
	for _, collection := range []any{result.Targets, result.Direct, result.Transitive, result.Edges, result.Cycles, result.Provenance, result.Diagnostics, result.Evidence} {
		if admission.AddJSON(collection) {
			return true
		}
	}
	return admission.RequiresProjection()
}

func (result Result) PublicResultProjection() any {
	omissions := []publicresult.Omission{}
	add := func(field string, total, retained int) {
		if total == 0 || total <= retained {
			return
		}
		if retained > total {
			retained = total
		}
		omitted := total - retained
		omissions = append(omissions, publicresult.Omission{Field: field, Reason: publicresult.InternalProjectionLimit, Retained: retained, Omitted: &omitted})
	}
	add("target_instances", len(result.Targets), 4)
	add("direct_dependents", len(result.Direct), 8)
	add("transitive_dependents", len(result.Transitive), 8)
	add("edges", len(result.Edges), 8)
	add("cycles", len(result.Cycles), 2)
	add("provenance", len(result.Provenance), 8)
	add("evidence", len(result.Evidence.Paths)+len(result.Evidence.Commands)+len(result.Evidence.Locations), 0)
	projected := result
	projected.Query.VcpkgRoot = publicresult.AbbreviateEncoded(result.Query.VcpkgRoot, 1024)
	projected.Query.ManifestRoot = publicresult.AbbreviateEncoded(result.Query.ManifestRoot, 1024)
	projected.Query.ScratchRoot = publicresult.AbbreviateEncoded(result.Query.ScratchRoot, 1024)
	projected.Query.OverlayPorts = abbreviateProjectionStrings(result.Query.OverlayPorts, 512)
	projected.Query.OverlayTriplets = abbreviateProjectionStrings(result.Query.OverlayTriplets, 512)
	projected.Executable.Path = publicresult.AbbreviateEncoded(result.Executable.Path, 1024)
	if projected.Failure != nil {
		failure := *projected.Failure
		failure.Detail = publicresult.AbbreviateEncoded(failure.Detail, 1024)
		projected.Failure = &failure
	}
	projected.Targets = projectNodes(result.Targets, 4)
	projected.Direct = projectDependents(result.Direct, 8)
	projected.Transitive = projectDependents(result.Transitive, 8)
	projected.Edges = projectEdges(result.Edges, 8)
	projected.Cycles = projectCycles(result.Cycles, 2)
	if len(result.Provenance) > 8 {
		projected.Provenance = append([]Provenance(nil), result.Provenance[:8]...)
	}
	for index := range projected.Provenance {
		projected.Provenance[index].Winner = publicresult.AbbreviateEncoded(projected.Provenance[index].Winner, 1024)
		projected.Provenance[index].WinnerSource = publicresult.AbbreviateEncoded(projected.Provenance[index].WinnerSource, 1024)
		projected.Provenance[index].Shadows = abbreviateProjectionStrings(projected.Provenance[index].Shadows, 512)
	}
	if len(result.Diagnostics) > 1 {
		projected.Diagnostics = append([]Diagnostic(nil), result.Diagnostics[:1]...)
	}
	projected.Evidence.Paths = nil
	projected.Evidence.Commands = nil
	projected.Evidence.Locations = nil
	projected.Projection = &publicresult.Projection{Complete: false, Omissions: omissions}
	return projected
}

func abbreviateProjectionStrings(values []string, allowance int) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = publicresult.AbbreviateEncoded(value, allowance)
	}
	return result
}

func projectNode(node Node) Node {
	node.Features = abbreviateProjectionStrings(node.Features, 128)
	if len(node.Features) > 32 {
		node.Features = append([]string(nil), node.Features[:32]...)
	}
	return node
}

func projectNodes(values []Node, limit int) []Node {
	if len(values) > limit {
		values = values[:limit]
	}
	result := make([]Node, len(values))
	for index, value := range values {
		result[index] = projectNode(value)
	}
	return result
}

func projectDependents(values []Dependent, limit int) []Dependent {
	if len(values) > limit {
		values = values[:limit]
	}
	result := make([]Dependent, len(values))
	for index, value := range values {
		result[index] = Dependent{Node: projectNode(value.Node), Distance: value.Distance}
	}
	return result
}

func projectEdges(values []Edge, limit int) []Edge {
	if len(values) > limit {
		values = values[:limit]
	}
	result := make([]Edge, len(values))
	for index, value := range values {
		result[index] = Edge{From: projectNode(value.From), To: projectNode(value.To)}
	}
	return result
}

func projectCycles(values []Cycle, limit int) []Cycle {
	if len(values) > limit {
		values = values[:limit]
	}
	result := make([]Cycle, len(values))
	for index, value := range values {
		result[index] = Cycle{Nodes: projectNodes(value.Nodes, 8), Edges: projectEdges(value.Edges, 8)}
	}
	return result
}
