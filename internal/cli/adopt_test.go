package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdoptCmdDryRunByDefaultMutatesNothingAndRedactsSecrets(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	manifestDir := filepath.Join(root, "manifests")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)

	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatalf("mkdir codex config parent: %v", err)
	}
	initial := `[mcp_servers.mui-dry-cli]
command = "go"
args = ["version"]

[mcp_servers.mui-dry-cli.env]
API_KEY = "cli-secret-value"
`
	if err := os.WriteFile(codexPath, []byte(initial), 0o600); err != nil {
		t.Fatalf("seed codex config: %v", err)
	}

	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"adopt", "mui-dry-cli", "--client", "codex-cli", "--port", "9313"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("adopt dry-run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Fatalf("dry-run output missing dry-run marker:\n%s", out.String())
	}
	if strings.Contains(out.String(), "cli-secret-value") {
		t.Fatalf("dry-run output leaked secret value:\n%s", out.String())
	}
	after, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex config after dry-run: %v", err)
	}
	if string(after) != initial {
		t.Fatalf("codex config mutated during dry-run\nbefore:\n%s\nafter:\n%s", initial, after)
	}
	if entries, err := os.ReadDir(manifestDir); err == nil && len(entries) > 0 {
		t.Fatalf("dry-run wrote manifest entries under override dir: %v", entries)
	}
}

func TestAdoptCmdRequiresNameToMatchEntry(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))

	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatalf("mkdir codex config parent: %v", err)
	}
	if err := os.WriteFile(codexPath, []byte(`[mcp_servers.mui-name-cli]
command = "go"
args = ["version"]
`), 0o600); err != nil {
		t.Fatalf("seed codex config: %v", err)
	}

	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"adopt", "mui-name-cli", "--client", "codex-cli", "--name", "mui-name-custom", "--port", "9314"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("adopt with mismatched --name succeeded")
	}
	if !strings.Contains(err.Error(), "--name") {
		t.Fatalf("error = %v, want --name context", err)
	}
}
