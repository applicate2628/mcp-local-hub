// Tests for `mcphub language-server cleanup` (PR #189).
//
// The CLI orchestrates over real client adapters constructed via
// AllClients(), which in turn call os.UserHomeDir(). To keep tests
// hermetic and avoid touching the developer's real config:
//
//   - On Windows, t.Setenv("USERPROFILE", tempHome) redirects every
//     adapter to a fresh tmp dir; HOME does not affect os.UserHomeDir
//     on Windows.
//   - On POSIX, t.Setenv("HOME", tempHome) redirects similarly.
//
// Each test seeds the per-client config files under tempHome and
// runs the command via cobra's Execute() so flags, stdin, and stdout
// are all wired the way they would be in production.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withHermeticHome redirects all home-dir-resolving adapter
// constructors to a fresh tmp dir for the duration of the test.
// Returns the tmp dir so callers can seed config files at known
// locations.
func withHermeticHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	// State-dir env vars too, in case any path resolves through them.
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return tmp
}

// writeFile is a t.Helper wrapper that creates parent dirs and
// writes the file with 0600 perms.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// seedClaudeLS writes a .claude.json containing a stdio
// mcp-language-server entry named "clangd" and a hub-managed HTTP
// entry. Returns the absolute path so tests can re-read it after
// cleanup.
func seedClaudeLS(t *testing.T, home string) string {
	t.Helper()
	path := filepath.Join(home, ".claude.json")
	content := `{
  "mcpServers": {
    "clangd": {
      "type": "stdio",
      "command": "mcp-language-server",
      "args": ["--lsp", "clangd"]
    },
    "mcp-language-server-clangd": {
      "type": "http",
      "url": "http://localhost:9200/mcp"
    },
    "user-stdio": {
      "command": "node",
      "args": ["server.js"]
    }
  }
}`
	writeFile(t, path, content)
	return path
}

// seedCodexLS writes a ~/.codex/config.toml containing a stdio
// mcp-language-server entry. Returns the absolute path.
func seedCodexLS(t *testing.T, home string) string {
	t.Helper()
	path := filepath.Join(home, ".codex", "config.toml")
	content := `[mcp_servers.clangd]
command = "mcp-language-server"
args = ["--lsp", "clangd"]

[mcp_servers.gdb]
url = "http://localhost:9129/mcp"
`
	writeFile(t, path, content)
	return path
}

func runCleanup(t *testing.T, args []string, stdin string) (string, error) {
	t.Helper()
	c := newLanguageServerCmdReal()
	buf := &bytes.Buffer{}
	c.SetOut(buf)
	c.SetErr(buf)
	c.SetIn(strings.NewReader(stdin))
	c.SilenceUsage = true
	c.SetArgs(append([]string{"cleanup"}, args...))
	err := c.Execute()
	return buf.String(), err
}

func TestLanguageServerCleanup_NoEntries(t *testing.T) {
	withHermeticHome(t)
	out, err := runCleanup(t, []string{"--yes"}, "")
	if err != nil {
		t.Fatalf("Execute: %v\n%s", err, out)
	}
	if !strings.Contains(out, "No stdio mcp-language-server entries found.") {
		t.Errorf("output missing zero-entry message:\n%s", out)
	}
}

func TestLanguageServerCleanup_DryRunDoesNotWrite(t *testing.T) {
	home := withHermeticHome(t)
	claudePath := seedClaudeLS(t, home)
	originalContent, _ := os.ReadFile(claudePath)

	out, err := runCleanup(t, []string{"--dry-run"}, "")
	if err != nil {
		t.Fatalf("Execute: %v\n%s", err, out)
	}
	if !strings.Contains(out, "clangd") {
		t.Errorf("output missing matched entry name:\n%s", out)
	}
	if !strings.Contains(out, "--dry-run: no changes written.") {
		t.Errorf("output missing dry-run footer:\n%s", out)
	}

	after, _ := os.ReadFile(claudePath)
	if string(after) != string(originalContent) {
		t.Errorf("--dry-run modified the file; before vs after diff:\nbefore: %s\nafter:  %s",
			originalContent, after)
	}
}

func TestLanguageServerCleanup_YesRemovesAndBacksUp(t *testing.T) {
	home := withHermeticHome(t)
	claudePath := seedClaudeLS(t, home)

	out, err := runCleanup(t, []string{"--yes"}, "")
	if err != nil {
		t.Fatalf("Execute: %v\n%s", err, out)
	}
	if !strings.Contains(out, "removed clangd") {
		t.Errorf("output missing removal line:\n%s", out)
	}
	if !strings.Contains(out, "backed up") {
		t.Errorf("output missing backup line:\n%s", out)
	}

	// Verify backup file exists and live config no longer contains
	// the stdio entry.
	dir := filepath.Dir(claudePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var sawBackup bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".claude.json.bak-mcp-local-hub-") {
			sawBackup = true
			break
		}
	}
	if !sawBackup {
		t.Error("backup file not created next to .claude.json")
	}

	// Live config should have lost the "clangd" entry but kept the
	// hub-managed HTTP entry and the unrelated user-stdio one.
	raw, _ := os.ReadFile(claudePath)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("post-cleanup parse: %v", err)
	}
	servers, _ := parsed["mcpServers"].(map[string]any)
	if _, present := servers["clangd"]; present {
		t.Error("clangd entry still present after cleanup")
	}
	if _, present := servers["mcp-language-server-clangd"]; !present {
		t.Error("hub-managed entry removed (regression)")
	}
	if _, present := servers["user-stdio"]; !present {
		t.Error("unrelated user-stdio entry removed (regression)")
	}
}

func TestLanguageServerCleanup_ClientFilter(t *testing.T) {
	home := withHermeticHome(t)
	claudePath := seedClaudeLS(t, home)
	codexPath := seedCodexLS(t, home)

	out, err := runCleanup(t, []string{"--client", "codex-cli", "--yes"}, "")
	if err != nil {
		t.Fatalf("Execute: %v\n%s", err, out)
	}
	if !strings.Contains(out, "codex-cli") {
		t.Errorf("codex-cli not mentioned in output:\n%s", out)
	}
	if strings.Contains(out, "removed clangd") && strings.Contains(out, "claude-code") {
		t.Errorf("claude-code processed despite --client codex-cli filter:\n%s", out)
	}

	// Claude config must be untouched.
	claudeRaw, _ := os.ReadFile(claudePath)
	var claudeParsed map[string]any
	_ = json.Unmarshal(claudeRaw, &claudeParsed)
	claudeServers, _ := claudeParsed["mcpServers"].(map[string]any)
	if _, present := claudeServers["clangd"]; !present {
		t.Error("claude clangd removed despite --client codex-cli filter")
	}

	// Codex config must have lost clangd.
	codexRaw, _ := os.ReadFile(codexPath)
	if strings.Contains(string(codexRaw), `[mcp_servers.clangd]`) {
		t.Errorf("codex clangd block still present after cleanup:\n%s", codexRaw)
	}
}

func TestLanguageServerCleanup_UnknownClientErrors(t *testing.T) {
	withHermeticHome(t)
	_, err := runCleanup(t, []string{"--client", "does-not-exist", "--yes"}, "")
	if err == nil {
		t.Fatal("expected error for unknown --client value")
	}
	if !strings.Contains(err.Error(), "unknown client") {
		t.Errorf("error = %v, want 'unknown client ...'", err)
	}
}

func TestLanguageServerCleanup_LanguageFilter(t *testing.T) {
	home := withHermeticHome(t)
	claudePath := filepath.Join(home, ".claude.json")
	writeFile(t, claudePath, `{
  "mcpServers": {
    "clangd": {
      "command": "mcp-language-server",
      "args": ["--lsp", "clangd"]
    },
    "pythonls": {
      "command": "mcp-language-server",
      "args": ["--lsp", "pylsp"]
    }
  }
}`)
	out, err := runCleanup(t, []string{"--language", "clangd", "--yes"}, "")
	if err != nil {
		t.Fatalf("Execute: %v\n%s", err, out)
	}
	if !strings.Contains(out, "removed clangd") {
		t.Errorf("clangd not removed despite --language clangd:\n%s", out)
	}
	if strings.Contains(out, "removed pythonls") {
		t.Errorf("pythonls removed despite --language clangd:\n%s", out)
	}

	raw, _ := os.ReadFile(claudePath)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	servers, _ := parsed["mcpServers"].(map[string]any)
	if _, present := servers["clangd"]; present {
		t.Error("clangd not removed")
	}
	if _, present := servers["pythonls"]; !present {
		t.Error("pythonls removed (regression)")
	}
}

func TestLanguageServerCleanup_PromptDeclined(t *testing.T) {
	home := withHermeticHome(t)
	claudePath := seedClaudeLS(t, home)
	original, _ := os.ReadFile(claudePath)
	out, err := runCleanup(t, nil, "n\n")
	if err != nil {
		t.Fatalf("Execute: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Aborted.") {
		t.Errorf("output missing abort message:\n%s", out)
	}
	after, _ := os.ReadFile(claudePath)
	if string(after) != string(original) {
		t.Error("file modified after prompt decline")
	}
}

func TestLanguageServerCleanup_PromptBareEnterDeclines(t *testing.T) {
	home := withHermeticHome(t)
	claudePath := seedClaudeLS(t, home)
	original, _ := os.ReadFile(claudePath)
	// Bare Enter — Fscanln reports "unexpected newline" which the
	// confirm helper treats as a no.
	out, err := runCleanup(t, nil, "\n")
	if err != nil {
		t.Fatalf("Execute: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Aborted.") {
		t.Errorf("output missing abort message:\n%s", out)
	}
	after, _ := os.ReadFile(claudePath)
	if string(after) != string(original) {
		t.Error("file modified after bare-Enter decline")
	}
}
