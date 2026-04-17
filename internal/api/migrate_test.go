package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrateReplacesStdioWithHTTPForOneClient verifies that a single-
// client migration rewrites the expected config entry without touching
// other clients.
//
// HOME/USERPROFILE are overridden so that each client adapter's internal
// os.UserHomeDir() call resolves to the tempdir, matching where the test
// wrote the config files. This keeps the adapter contract unchanged
// (production adapters still resolve via UserHomeDir) while giving the
// test a hermetic filesystem layout.
func TestMigrateReplacesStdioWithHTTPForOneClient(t *testing.T) {
	tmp := t.TempDir()

	// Redirect UserHomeDir() to tmp for Claude/Codex/Gemini/Antigravity
	// adapter path resolution on both POSIX (HOME) and Windows (USERPROFILE).
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	claudePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(claudePath, []byte(`{"mcpServers":{"memory":{"command":"npx","args":["-y","@x/memory"]}}}`), 0600); err != nil {
		t.Fatalf("write claude config: %v", err)
	}

	// Codex adapter resolves to ~/.codex/config.toml, so create that subdir.
	codexDir := filepath.Join(tmp, ".codex")
	if err := os.MkdirAll(codexDir, 0755); err != nil {
		t.Fatalf("mkdir codex: %v", err)
	}
	codexPath := filepath.Join(codexDir, "config.toml")
	if err := os.WriteFile(codexPath, []byte(`[mcp_servers.memory]
command = "npx"
args = ["-y", "@x/memory"]
`), 0600); err != nil {
		t.Fatalf("write codex config: %v", err)
	}

	// Fake manifest so the migration can resolve the daemon port and URL path.
	manifestDir := filepath.Join(tmp, "servers")
	if err := os.MkdirAll(filepath.Join(manifestDir, "memory"), 0755); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "memory", "manifest.yaml"),
		[]byte(`name: memory
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: default
    port: 9123
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: codex-cli
    daemon: default
    url_path: /mcp
weekly_refresh: false
`), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	a := NewAPI()
	report, err := a.MigrateFrom(MigrateOpts{
		Servers:        []string{"memory"},
		ClientsInclude: []string{"claude-code"},
		ScanOpts: ScanOpts{
			ClaudeConfigPath: claudePath,
			CodexConfigPath:  codexPath,
			ManifestDir:      manifestDir,
		},
	})
	if err != nil {
		t.Fatalf("MigrateFrom: %v", err)
	}
	if report == nil {
		t.Fatal("MigrateFrom returned nil report")
	}

	// Claude is now http
	data, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("read claude: %v", err)
	}
	var claudeCfg struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &claudeCfg); err != nil {
		t.Fatalf("unmarshal claude: %v", err)
	}
	if got := claudeCfg.MCPServers["memory"]["type"]; got != "http" {
		t.Errorf("claude memory.type: want http, got %v", got)
	}

	// Codex is unchanged (still has command=npx)
	cod, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex: %v", err)
	}
	if !strings.Contains(string(cod), `command = "npx"`) {
		t.Errorf("codex was unexpectedly migrated: %s", cod)
	}
}
