package cmakewrap

import (
	"context"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/cmakegraph"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR31RunPreservesWhitespaceInFilesystemPaths(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), " workspace ")
	root := filepath.Join(workspace, " root ")
	var gotRoot, gotWorkspace string
	result := run(context.Background(), Args{Root: root, WorkspaceRoot: workspace}, nil,
		func(_ context.Context, actualRoot, actualWorkspace string, _ []string, _ cmakegraph.Options) (*cmakegraph.Result, error) {
			gotRoot, gotWorkspace = actualRoot, actualWorkspace
			return &cmakegraph.Result{Root: actualRoot, WorkspaceRoot: actualWorkspace}, nil
		})
	if result.Status != evidence.StatusOK {
		t.Fatalf("result=%+v, want ok", result)
	}
	if gotRoot != root || gotWorkspace != workspace {
		t.Fatalf("walkTree paths=(%q,%q), want exact (%q,%q)", gotRoot, gotWorkspace, root, workspace)
	}

	file := filepath.Join(workspace, " CMakeLists.txt ")
	var gotFile string
	result = run(context.Background(), Args{File: file, WorkspaceRoot: workspace},
		func(_ context.Context, actualFile, actualWorkspace string, _ cmakegraph.Options) (*cmakegraph.Result, error) {
			gotFile, gotWorkspace = actualFile, actualWorkspace
			return &cmakegraph.Result{Root: actualFile, WorkspaceRoot: actualWorkspace}, nil
		}, nil)
	if result.Status != evidence.StatusOK {
		t.Fatalf("file result=%+v, want ok", result)
	}
	if gotFile != file || gotWorkspace != workspace {
		t.Fatalf("walk paths=(%q,%q), want exact (%q,%q)", gotFile, gotWorkspace, file, workspace)
	}
}

func TestR31RunRejectsNegativeGraphLimitsBeforeTraversal(t *testing.T) {
	tests := []struct {
		name string
		args Args
	}{
		{"depth", Args{MaxDepth: -1}},
		{"nodes", Args{MaxNodes: -1}},
		{"file-bytes", Args{MaxFileBytes: -1}},
		{"roots", Args{MaxRoots: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.args.File = filepath.Join(t.TempDir(), "CMakeLists.txt")
			called := false
			result := run(context.Background(), tc.args,
				func(context.Context, string, string, cmakegraph.Options) (*cmakegraph.Result, error) {
					called = true
					return nil, nil
				}, nil)
			if called || result.Status != evidence.StatusUnknown || result.Reason != ReasonArgsInvalid {
				t.Fatalf("called=%v result=%+v, want no traversal and unknown/%s", called, result, ReasonArgsInvalid)
			}
		})
	}
}
