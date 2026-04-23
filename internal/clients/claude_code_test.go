package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func setupClaudeConfig(t *testing.T, initial string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeCode_AddEntry_CreatesField(t *testing.T) {
	path := setupClaudeConfig(t, `{"other":"field"}`)
	c := &claudeCode{path: path}

	err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers, ok := parsed["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or wrong type: %v", parsed["mcpServers"])
	}
	serena, ok := servers["serena"].(map[string]any)
	if !ok {
		t.Fatalf("serena entry missing: %v", servers)
	}
	if serena["url"] != "http://localhost:9121/mcp" {
		t.Errorf("url = %v, want http://localhost:9121/mcp", serena["url"])
	}
	if serena["type"] != "http" {
		t.Errorf("type = %v, want http (required by Claude Code schema for HTTP transport)", serena["type"])
	}
	// Original field preserved
	if parsed["other"] != "field" {
		t.Error("original field dropped")
	}
}

func TestClaudeCode_AddEntry_Replaces(t *testing.T) {
	path := setupClaudeConfig(t, `{"mcpServers":{"serena":{"url":"http://old"}}}`)
	c := &claudeCode{path: path}
	_ = c.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"})

	entry, _ := c.GetEntry("serena")
	if entry == nil || entry.URL != "http://localhost:9121/mcp" {
		t.Errorf("entry not replaced: %v", entry)
	}
}

func TestClaudeCode_RemoveEntry(t *testing.T) {
	path := setupClaudeConfig(t, `{"mcpServers":{"serena":{"url":"http://x"},"other":{"url":"http://y"}}}`)
	c := &claudeCode{path: path}
	_ = c.RemoveEntry("serena")

	entry, _ := c.GetEntry("serena")
	if entry != nil {
		t.Errorf("serena still present: %v", entry)
	}
	other, _ := c.GetEntry("other")
	if other == nil {
		t.Error("other entry should still be present")
	}
}

func TestClaudeCode_BackupRestore(t *testing.T) {
	original := `{"mcpServers":{"serena":{"url":"http://old"}}}`
	path := setupClaudeConfig(t, original)
	c := &claudeCode{path: path}

	bak, err := c.Backup()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// Mutate live file
	_ = c.AddEntry(MCPEntry{Name: "serena", URL: "http://new"})
	// Restore
	if err := c.Restore(bak); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Errorf("after restore = %q, want %q", got, original)
	}
}

func TestClaudeCode_LatestBackupPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	got, ok, err := c.LatestBackupPath()
	if err != nil || !ok || got != backup {
		t.Errorf("LatestBackupPath = %q ok=%v err=%v", got, ok, err)
	}
}

func TestClaudeCode_RestoreEntryFromBackup_RestoresStdioShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	// Live config is in post-migrate hub-HTTP state.
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9123/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	// Backup has pre-migrate stdio.
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	if err := c.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	entry := m["mcpServers"].(map[string]any)["memory"].(map[string]any)
	if entry["type"] != "stdio" {
		t.Errorf("type=%v, want stdio", entry["type"])
	}
	if entry["command"] != "npx" {
		t.Errorf("command=%v, want npx", entry["command"])
	}
}

func TestClaudeCode_RestoreEntryFromBackup_RemovesEntryIfBackupLacksIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	// Live has two entries; only `memory` exists in backup.
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"newserver":{"type":"http","url":"x"},"memory":{"type":"http","url":"y"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	if err := c.RestoreEntryFromBackup(backup, "newserver"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	servers := m["mcpServers"].(map[string]any)
	if _, present := servers["newserver"]; present {
		t.Error("newserver should have been removed — backup predates it")
	}
	// memory must survive because the call targeted only `newserver`.
	if _, present := servers["memory"]; !present {
		t.Error("memory was touched but should be untouched — call targeted newserver")
	}
}

func TestClaudeCode_RestoreEntryFromBackup_PreservesUnrelatedEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	// Live has `memory` (migrated) + `other` (user added since migrate).
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"y"},"other":{"type":"stdio","command":"echo"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	// Backup predates `other`, has stdio `memory`.
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"stdio","command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	if err := c.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	servers := m["mcpServers"].(map[string]any)
	// memory is rolled back to stdio.
	memEntry := servers["memory"].(map[string]any)
	if memEntry["type"] != "stdio" {
		t.Errorf("memory.type=%v, want stdio", memEntry["type"])
	}
	// `other` entry (added after migrate, not in backup) is preserved.
	if _, present := servers["other"]; !present {
		t.Error("unrelated 'other' entry lost — per-entry rollback must preserve it")
	}
}

func TestClaudeCode_RestoreEntryFromBackup_AcceptsRemoteHTTPBackup(t *testing.T) {
	// User has a legit remote HTTP MCP server (non-loopback URL). The
	// defensive "already migrated" check must ONLY fire on loopback
	// urls (hub-managed shape); remote urls pass through to the
	// normal restore path.
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"remote":{"type":"http","url":"https://api.example.com/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	if err := c.RestoreEntryFromBackup(backup, "remote"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v (remote HTTP url must not be rejected as hub-managed)", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	entry := m["mcpServers"].(map[string]any)["remote"].(map[string]any)
	if entry["url"] != "https://api.example.com/mcp" {
		t.Errorf("url=%v, want https://api.example.com/mcp", entry["url"])
	}
}

func TestClaudeCode_RestoreEntryFromBackup_RefusesHubHTTPBackupEntry(t *testing.T) {
	// Backup was taken AFTER an earlier migrate already rewrote this
	// entry to hub-HTTP form (typical when two servers are migrated
	// sequentially from the same client). Restoring from this backup
	// would silently re-write the hub-HTTP entry. Defensive refuse.
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	// Backup has memory ALREADY migrated — this is the bug we must catch.
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	err := c.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}
