package daemon_env_overlay

import "testing"

// TestNormalizeOverlayKey verifies the helper produces the canonical
// leading-backslash form used by SupervisorDaemon.TaskName
// (see internal/api/supervisor_intent.go:25).
func TestNormalizeOverlayKey(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bare form gets backslash prefix",
			in:   "mcp-local-hub-memory-default",
			want: "\\mcp-local-hub-memory-default",
		},
		{
			name: "already canonical is idempotent",
			in:   "\\mcp-local-hub-memory-default",
			want: "\\mcp-local-hub-memory-default",
		},
		{
			name: "empty string preserved as empty",
			in:   "",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeOverlayKey(tc.in)
			if got != tc.want {
				t.Fatalf("NormalizeOverlayKey(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
