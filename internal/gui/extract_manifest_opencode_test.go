package gui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

func TestRealExtractor_OpenCodeConfigPath(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	xdg := filepath.Join(tmp, "xdg")
	localAppData := filepath.Join(tmp, "localappdata")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("LOCALAPPDATA", localAppData)

	opencodeDir := filepath.Join(xdg, "opencode")
	if err := os.MkdirAll(opencodeDir, 0755); err != nil {
		t.Fatal(err)
	}
	opencodePath := filepath.Join(opencodeDir, "opencode.json")
	cfg := `{"mcp":{"fetch":{"type":"local","command":["uvx","mcp-server-fetch"],"environment":{"FETCH_TOKEN":"secret-x"},"enabled":true}}}`
	if err := os.WriteFile(opencodePath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}

	yaml, err := (realExtractor{}).ExtractManifestFromClient("opencode", "fetch", api.ScanOpts{})
	if err != nil {
		t.Fatalf("realExtractor ExtractManifestFromClient(opencode): %v", err)
	}
	m, err := config.ParseManifest(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("ParseManifest: %v\n%s", err, yaml)
	}
	if m.Command != "uvx" {
		t.Errorf("Command = %q, want uvx", m.Command)
	}
	if len(m.BaseArgs) != 1 || m.BaseArgs[0] != "mcp-server-fetch" {
		t.Errorf("BaseArgs = %v, want [mcp-server-fetch]", m.BaseArgs)
	}
	if m.Env["FETCH_TOKEN"] != "secret-x" {
		t.Errorf("env not translated from OpenCode environment: %#v", m.Env)
	}
}
