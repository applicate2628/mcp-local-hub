package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestScanClassifiesEntries verifies the three key classifications:
// via-hub (HTTP entry pointing at our daemon), can-migrate (stdio entry
// matching one of our manifest names), unknown (stdio entry with no
// manifest), per-session (well-known per-session server like gdb).
func TestScanClassifiesEntries(t *testing.T) {
	tmp := t.TempDir()

	// Fake Claude Code config with 4 entries.
	claudeCfg := map[string]any{
		"mcpServers": map[string]any{
			"memory":     map[string]any{"type": "http", "url": "http://localhost:9123/mcp"},
			"filesystem": map[string]any{"command": "npx", "args": []string{"-y", "@x/filesystem"}},
			"my-thing":   map[string]any{"command": "python", "args": []string{"my.py"}},
			"gdb":        map[string]any{"command": "uv", "args": []string{"run", "server.py"}},
		},
	}
	claudePath := filepath.Join(tmp, ".claude.json")
	b, _ := json.Marshal(claudeCfg)
	_ = os.WriteFile(claudePath, b, 0600)

	// Manifest dir with memory + filesystem.
	manifestDir := filepath.Join(tmp, "servers")
	_ = os.MkdirAll(filepath.Join(manifestDir, "memory"), 0755)
	_ = os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
		[]byte("name: memory\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9123\n"), 0644)
	_ = os.MkdirAll(filepath.Join(manifestDir, "filesystem"), 0755)
	_ = os.WriteFile(filepath.Join(manifestDir, "filesystem", "manifest.yaml"),
		[]byte("name: filesystem\nkind: global\ntransport: stdio-bridge\ncommand: npx\ndaemons:\n  - name: default\n    port: 9130\n"), 0644)

	a := NewAPI()
	result, err := a.ScanFrom(ScanOpts{
		ClaudeConfigPath:      claudePath,
		CodexConfigPath:       "",
		GeminiConfigPath:      "",
		AntigravityConfigPath: "",
		ManifestDir:           manifestDir,
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	byName := map[string]ScanEntry{}
	for _, e := range result.Entries {
		byName[e.Name] = e
	}

	if got := byName["memory"].Status; got != "via-hub" {
		t.Errorf("memory.Status: got %q, want via-hub", got)
	}
	if got := byName["filesystem"].Status; got != "can-migrate" {
		t.Errorf("filesystem.Status: got %q, want can-migrate", got)
	}
	if got := byName["my-thing"].Status; got != "unknown" {
		t.Errorf("my-thing.Status: got %q, want unknown", got)
	}
	if got := byName["gdb"].Status; got != "per-session" {
		t.Errorf("gdb.Status: got %q, want per-session", got)
	}
}
