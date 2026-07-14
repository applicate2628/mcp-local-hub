package api

import (
	"path/filepath"
	"testing"
)

func TestStateFileReadCapResolverByKind(t *testing.T) {
	stateDir := filepath.Join("tmp", "state")
	cases := []struct {
		name string
		path string
		want int64
	}{
		{
			name: "hub endpoint",
			path: filepath.Join(stateDir, hubMcpEndpointFileLeaf),
			want: 1 << 20,
		},
		{
			name: "hub tokens",
			path: filepath.Join(stateDir, hubMcpTokensFileLeaf),
			want: 1 << 20,
		},
		{
			name: "daemon intent",
			path: filepath.Join(stateDir, intentFileLeaf),
			want: 16 << 20,
		},
		{
			name: "supervisor intent",
			path: filepath.Join(stateDir, supervisorIntentFileLeaf),
			want: 16 << 20,
		},
		{
			name: "supervisor intent backup source",
			path: filepath.Join(stateDir, "pre-collapse-backup-2026-06-20T12-00-00.000000000Z", supervisorIntentFileLeaf),
			want: 16 << 20,
		},
		{
			name: "adopt provenance snapshot",
			path: filepath.Join(stateDir, "adopt-provenance", "mymanifest", "claude-code.snapshot"),
			want: maxIntentFileBytes,
		},
		{
			name: "age identity",
			path: filepath.Join(stateDir, ".age-key"),
			want: 64 << 10,
		},
		{
			name: "encrypted vault",
			path: filepath.Join(stateDir, "secrets.age"),
			want: 16 << 20,
		},
		{
			name: "workspace registry",
			path: filepath.Join(stateDir, "workspaces.yaml"),
			want: maxWorkspaceRegistryFileBytes,
		},
		{
			name: "ordinary state",
			path: filepath.Join(stateDir, "supervisor-state.json"),
			want: 1 << 20,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stateFileReadCapBytes(tc.path); got != tc.want {
				t.Fatalf("stateFileReadCapBytes(%q) = %d, want %d", tc.path, got, tc.want)
			}
		})
	}
}
