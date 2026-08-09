package cmakewrap

import (
	"context"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/cmakegraph"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR15RunForwardsAggregateEdgeCoverage(t *testing.T) {
	file := filepath.Join(t.TempDir(), "CMakeLists.txt")
	walk := func(context.Context, string, string, cmakegraph.Options) (*cmakegraph.Result, error) {
		return &cmakegraph.Result{
			Root:                  file,
			WorkspaceRoot:         filepath.Dir(file),
			EdgeCapTruncated:      true,
			RootsSkippedByEdgeCap: 3,
			RetainedEdgeBytes:     1234,
			UnscannedFiles: []cmakegraph.UnscannedFile{{
				Path: file, Reason: cmakegraph.CoverageEdgeCapExceeded,
			}},
		}, nil
	}
	result := run(context.Background(), Args{File: file}, walk, nil)
	if result.Status != evidence.StatusOK || !result.EdgeCapTruncated ||
		result.RootsSkippedByEdgeCap != 3 || result.RetainedEdgeBytes != 1234 {
		t.Fatalf("result = %+v, want exact aggregate edge coverage fields", result)
	}
	if len(result.UnscannedFiles) != 1 || result.UnscannedFiles[0].Reason != cmakegraph.CoverageEdgeCapExceeded {
		t.Fatalf("unscanned_files = %+v, want edge_cap_exceeded", result.UnscannedFiles)
	}
}
