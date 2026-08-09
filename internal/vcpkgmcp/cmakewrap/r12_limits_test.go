package cmakewrap

import (
	"context"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/cmakegraph"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestOptionsRejectEveryHardMaximumPlusOneBeforeWork_Wrapper(t *testing.T) {
	tests := []struct {
		name string
		args Args
	}{
		{"depth", Args{MaxDepth: cmakegraph.MaxDepthLimit + 1}},
		{"nodes", Args{MaxNodes: cmakegraph.MaxNodesLimit + 1}},
		{"file-bytes", Args{MaxFileBytes: cmakegraph.MaxFileBytesLimit + 1}},
		{"roots", Args{MaxRoots: cmakegraph.MaxRootsLimit + 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.args.File = filepath.Join(t.TempDir(), "must-not-open.cmake")
			calls := 0
			result := run(context.Background(), tc.args,
				func(ctx context.Context, file, workspace string, opts cmakegraph.Options) (*cmakegraph.Result, error) {
					calls++
					_, err := cmakegraph.Walk(ctx, file, workspace, opts)
					return nil, err
				},
				func(context.Context, string, string, []string, cmakegraph.Options) (*cmakegraph.Result, error) {
					t.Fatal("WalkTree called in file mode")
					return nil, nil
				})
			if calls != 1 || result.Status != evidence.StatusUnknown || result.Reason != ReasonArgsInvalid {
				t.Fatalf("calls=%d result=%+v, want one owner call and unknown/%s", calls, result, ReasonArgsInvalid)
			}
		})
	}
}
