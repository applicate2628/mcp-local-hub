package cmakewrap

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/cmakegraph"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

func TestR17ProjectionRetainsBoundedCausalCoverageAndEnumeratesOmissions(t *testing.T) {
	result := Result{
		Status:    evidence.StatusOK,
		Edges:     []Edge{{Kind: "include"}},
		Files:     []string{"/src/CMakeLists.txt"},
		Histogram: cmakegraph.Histogram{Resolved: 1},
		UnscannedFiles: []cmakegraph.UnscannedFile{
			{Path: "/src/missing.cmake", Reason: cmakegraph.CoverageEnumerateFailed, Detail: "denied"},
			{Path: "/src/second.cmake", Reason: cmakegraph.CoverageEnumerateFailed, Detail: "denied"},
		},
		Evidence: evidence.Evidence{Paths: []string{"/src/CMakeLists.txt"}, Commands: []string{"read /src/CMakeLists.txt"}},
	}
	body, err := json.Marshal(result.PublicResultProjection())
	if err != nil {
		t.Fatal(err)
	}
	var projected struct {
		Histogram  cmakegraph.Histogram       `json:"histogram"`
		Unscanned  []cmakegraph.UnscannedFile `json:"unscanned_files"`
		Evidence   evidence.Evidence          `json:"evidence"`
		Projection publicresult.Projection    `json:"result_projection"`
	}
	if err := json.Unmarshal(body, &projected); err != nil {
		t.Fatal(err)
	}
	if projected.Histogram.Resolved != 1 || len(projected.Unscanned) == 0 || len(projected.Evidence.Paths) != 1 {
		t.Fatalf("projection lost causal identity: %+v", projected)
	}
	omissions := map[string]bool{}
	for _, omission := range projected.Projection.Omissions {
		omissions[omission.Field] = true
	}
	for _, field := range []string{"edges", "files", "evidence.commands"} {
		if !omissions[field] {
			t.Fatalf("omissions=%+v, want %s", projected.Projection.Omissions, field)
		}
	}
}

func TestR17RunForwardsCoverageAggregateAccounting(t *testing.T) {
	file := filepath.Join(t.TempDir(), "CMakeLists.txt")
	walk := func(context.Context, string, string, cmakegraph.Options) (*cmakegraph.Result, error) {
		return &cmakegraph.Result{
			Root: file, WorkspaceRoot: filepath.Dir(file),
			CoverageCapTruncated: true, DroppedCoverageHoles: 7, RetainedCoverageBytes: 4096,
		}, nil
	}
	result := run(context.Background(), Args{File: file}, walk, nil)
	if !result.CoverageCapTruncated || result.DroppedCoverageHoles != 7 || result.RetainedCoverageBytes != 4096 {
		t.Fatalf("coverage accounting not forwarded: %+v", result)
	}
}
