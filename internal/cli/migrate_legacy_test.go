package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrateLegacyCmd_WiredIntoRoot asserts the migrate-legacy command is
// attached to the root command tree.
func TestMigrateLegacyCmd_WiredIntoRoot(t *testing.T) {
	root := NewRootCmd()
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "migrate-legacy" {
			found = true
			break
		}
	}
	if !found {
		t.Error("migrate-legacy subcommand not wired into root")
	}
}

// TestMigrateLegacyCmd_FlagsPresent verifies the three documented flags
// are declared: --dry-run, --yes, --json.
func TestMigrateLegacyCmd_FlagsPresent(t *testing.T) {
	c := newMigrateLegacyCmdReal()
	for _, name := range []string{"dry-run", "yes", "json"} {
		if c.Flags().Lookup(name) == nil {
			t.Errorf("--%s flag missing", name)
		}
	}
}

// TestMigrateLegacyCmd_DryRunNoEntriesExits0 runs the command in dry-run
// mode against an empty $HOME (no configs). Result: zero detected entries,
// zero Applied, zero Failed — clean exit. Acts as a smoke test that the
// binding between detect -> migrate -> render compiles and runs.
func TestMigrateLegacyCmd_DryRunNoEntriesExits0(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	// Point the registry at a fresh empty dir to keep Register (if ever
	// reached) away from real state. Dry-run should not hit this path.
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	buf := &bytes.Buffer{}
	c := newMigrateLegacyCmdReal()
	c.SetOut(buf)
	c.SetErr(buf)
	c.SilenceUsage = true
	c.SetArgs([]string{"--dry-run"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Planned: 0") {
		t.Errorf("expected 'Planned: 0' in output, got:\n%s", out)
	}
	if !strings.Contains(out, "dry-run") {
		t.Errorf("expected dry-run marker in output, got:\n%s", out)
	}
}

// TestMigrateLegacyCmd_DryRunWithCodexFixture seeds a Codex TOML with a
// disabled mcp-language-server entry and confirms the dry-run output lists
// it in the plan without removing it from the config file.
func TestMigrateLegacyCmd_DryRunWithCodexFixture(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	_ = os.MkdirAll(filepath.Join(dir, ".codex"), 0755)
	fixture := `
[mcp_servers.py-lsp]
command = "mcp-language-server"
args = ["--workspace", "/tmp/ws", "--lsp", "pyright-langserver"]
enabled = false
`
	cfgPath := filepath.Join(dir, ".codex", "config.toml")
	if err := os.WriteFile(cfgPath, []byte(fixture), 0644); err != nil {
		t.Fatal(err)
	}

	buf := &bytes.Buffer{}
	c := newMigrateLegacyCmdReal()
	c.SetOut(buf)
	c.SetErr(buf)
	c.SilenceUsage = true
	c.SetArgs([]string{"--dry-run"})
	if err := c.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "py-lsp") {
		t.Errorf("expected py-lsp in plan output, got:\n%s", out)
	}
	if !strings.Contains(out, "Planned: 1") {
		t.Errorf("expected 'Planned: 1' in output, got:\n%s", out)
	}

	// Dry-run must not modify the config file.
	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "py-lsp") {
		t.Errorf("dry-run mutated the config file — py-lsp missing:\n%s", got)
	}
}
