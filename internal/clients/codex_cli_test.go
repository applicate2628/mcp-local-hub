package clients

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupCodexConfig(t *testing.T, initial string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexCLI_AddEntry_ReplaceStdioBlock(t *testing.T) {
	initial := `[mcp_servers.serena]
command = "uvx"
args = ["--from", "git+...", "serena", "start-mcp-server"]
startup_timeout_sec = 30.0

[mcp_servers.other]
command = "echo"
args = ["hi"]
`
	path := setupCodexConfig(t, initial)
	c := &codexCLI{path: path}

	err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9122/mcp"})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(path)
	s := string(raw)
	// TOML accepts both "basic" (double-quoted) and "literal" (single-quoted) strings;
	// go-toml/v2 emits literal strings for simple ASCII. Accept either.
	if !strings.Contains(s, `url = "http://localhost:9122/mcp"`) && !strings.Contains(s, `url = 'http://localhost:9122/mcp'`) {
		t.Errorf("URL not set: %s", s)
	}
	// Verify old `command` field was removed from the serena block (wholesale replace).
	// Quote-agnostic check: just look for the key name inside the block.
	serenaStart := strings.Index(s, "[mcp_servers.serena]")
	otherStart := strings.Index(s, "[mcp_servers.other]")
	if serenaStart >= 0 && otherStart > serenaStart {
		if strings.Contains(s[serenaStart:otherStart], "command") {
			t.Errorf("old command field not removed from serena block:\n%s", s[serenaStart:otherStart])
		}
	}
	// Other section preserved
	if !strings.Contains(s, "[mcp_servers.other]") {
		t.Error("other section dropped")
	}
}

func TestCodexCLI_RemoveEntry(t *testing.T) {
	initial := `[mcp_servers.serena]
url = "http://localhost:9122/mcp"

[mcp_servers.memory]
url = "http://localhost:9140/mcp"
`
	path := setupCodexConfig(t, initial)
	c := &codexCLI{path: path}
	if err := c.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "serena") {
		t.Errorf("serena not removed: %s", raw)
	}
	if !strings.Contains(string(raw), "memory") {
		t.Error("memory also removed (should be preserved)")
	}
}

func TestCodexCLI_LatestBackupPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	c := &codexCLI{path: path}
	got, ok, err := c.LatestBackupPath()
	if err != nil || !ok || got != backup {
		t.Errorf("LatestBackupPath = %q ok=%v err=%v", got, ok, err)
	}
}

func TestCodexCLI_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	live := `[mcp_servers.memory]
url = "http://localhost:9123/mcp"
startup_timeout_sec = 10.0
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	backupBody := `[mcp_servers.memory]
command = "npx"
args = ["-y", "mem"]
`
	if err := os.WriteFile(backup, []byte(backupBody), 0600); err != nil {
		t.Fatal(err)
	}
	c := &codexCLI{path: path}
	if err := c.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	// go-toml/v2 emits literal strings (single-quoted) for simple ASCII;
	// accept either quoting style, matching TestCodexCLI_AddEntry_ReplaceStdioBlock.
	if !strings.Contains(s, `command = "npx"`) && !strings.Contains(s, `command = 'npx'`) {
		t.Errorf("expected restored stdio command, got:\n%s", s)
	}
	if strings.Contains(s, `url = "http`) || strings.Contains(s, `url = 'http`) {
		t.Errorf("hub-HTTP url should be gone after restore, got:\n%s", s)
	}
}

func TestCodexCLI_RestoreEntryFromBackup_RemovesOnAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	live := `[mcp_servers.newserver]
url = "http://localhost:9999/mcp"
`
	if err := os.WriteFile(path, []byte(live), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(``), 0600); err != nil {
		t.Fatal(err)
	}
	c := &codexCLI{path: path}
	if err := c.RestoreEntryFromBackup(backup, "newserver"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "newserver") {
		t.Errorf("newserver should have been removed, got:\n%s", string(data))
	}
}

func TestCodexCLI_RestoreEntryFromBackup_RefusesHubHTTPBackupEntry(t *testing.T) {
	// Backup was taken AFTER an earlier migrate already rewrote this
	// entry to hub-HTTP form. Defensive refuse.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`[mcp_servers.memory]
url = "http://localhost:9200/mcp"
startup_timeout_sec = 10.0
`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`[mcp_servers.memory]
url = "http://localhost:9200/mcp"
startup_timeout_sec = 10.0
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &codexCLI{path: path}
	err := c.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}
