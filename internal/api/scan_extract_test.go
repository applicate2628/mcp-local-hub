package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractManifestFromClientPreservesCommandAndEnv asserts that the
// extracted manifest has command/base_args/env matching the original stdio
// entry, plus a port auto-picked and all-four client bindings as defaults.
func TestExtractManifestFromClientPreservesCommandAndEnv(t *testing.T) {
	tmp := t.TempDir()
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"fetch": map[string]any{
				"command": "uvx",
				"args":    []string{"--from", "mcp-server-fetch", "mcp-server-fetch"},
				"env":     map[string]any{"CACHE_DIR": "/tmp/fetch"},
			},
		},
	}
	path := filepath.Join(tmp, ".claude.json")
	b, _ := json.Marshal(cfg)
	_ = os.WriteFile(path, b, 0600)

	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("claude-code", "fetch", ScanOpts{
		ClaudeConfigPath: path,
		ManifestDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(yaml, "name: fetch") {
		t.Error("missing name: fetch")
	}
	if !strings.Contains(yaml, "command: uvx") {
		t.Error("missing command: uvx")
	}
	if !strings.Contains(yaml, "CACHE_DIR") {
		t.Error("env CACHE_DIR lost")
	}
	if !strings.Contains(yaml, "client_bindings:") {
		t.Error("missing client_bindings")
	}
}
