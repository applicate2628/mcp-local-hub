package cmakewrap

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/cmakegraph"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR27GraphEvidenceIsLinearUniqueAndCancellationAware(t *testing.T) {
	root := t.TempDir()
	workspace := t.TempDir()
	files := make([]string, 20_000)
	for index := range files {
		files[index] = filepath.Join(workspace, fmt.Sprintf("file-%05d.cmake", index))
	}
	files[len(files)-1] = workspace
	ev, err := graphEvidence(context.Background(), root, workspace, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Paths) != len(files)+1 {
		t.Fatalf("paths=%d, want %d unique root/workspace/files", len(ev.Paths), len(files)+1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := run(ctx, Args{Root: root}, cmakegraph.Walk, func(context.Context, string, string, []string, cmakegraph.Options) (*cmakegraph.Result, error) {
		return &cmakegraph.Result{Root: root, WorkspaceRoot: workspace, Files: files}, nil
	})
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonCanceled {
		t.Fatalf("status=%s reason=%s, want unknown(canceled)", result.Status, result.Reason)
	}
}
