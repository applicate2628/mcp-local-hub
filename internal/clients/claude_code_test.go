package clients

import (
	"encoding/json"
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
