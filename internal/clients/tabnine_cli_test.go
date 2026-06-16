package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newTabnineCLIForTest(t *testing.T, initial string) *tabnineCLIClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_servers.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &tabnineCLIClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "tabnine-cli",
		urlField:   "url",
	}}
}

// TestTabnineCLI_AddEntry_WritesBareURLSchema verifies AddEntry emits
// Tabnine's minimal bare-`url` shape (transport auto-detected from `url`; NO
// `type` and NO `disabled` field) under the top-level `mcpServers` key,
// optionally with headers.
func TestTabnineCLI_AddEntry_WritesBareURLSchema(t *testing.T) {
	tc := newTabnineCLIForTest(t, `{"other":"keep-me"}`)
	if err := tc.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(tc.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers := parsed["mcpServers"].(map[string]any)
	serena := servers["serena"].(map[string]any)
	if serena["url"] != "http://localhost:9121/mcp" {
		t.Errorf("url = %v, want hub URL", serena["url"])
	}
	if _, has := serena["type"]; has {
		t.Errorf("Tabnine entry must not carry a `type` field: %v", serena)
	}
	if _, has := serena["disabled"]; has {
		t.Errorf("Tabnine entry must not carry a `disabled` field: %v", serena)
	}
	if parsed["other"] != "keep-me" {
		t.Error("other top-level field dropped")
	}
}

func TestTabnineCLI_AddEntry_WithHeaders(t *testing.T) {
	tc := newTabnineCLIForTest(t, `{"mcpServers":{}}`)
	if err := tc.AddEntry(MCPEntry{Name: "memory", URL: "http://localhost:9140/mcp", Headers: map[string]string{"Authorization": "Bearer xyz"}}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	got, err := tc.GetEntry("memory")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != "http://localhost:9140/mcp" {
		t.Fatalf("memory URL not set: %+v", got)
	}
	if got.Headers["Authorization"] != "Bearer xyz" {
		t.Errorf("headers not round-tripped: %+v", got.Headers)
	}
}

func TestTabnineCLI_RemoveEntry_Inherited(t *testing.T) {
	tc := newTabnineCLIForTest(t, `{"mcpServers":{"serena":{"url":"http://x"},"other":{"url":"http://y"}}}`)
	if err := tc.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := tc.GetEntry("serena"); e != nil {
		t.Errorf("serena still present: %v", e)
	}
	if e, _ := tc.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
}

func TestTabnineCLI_RestoreEntryFromBackup_RefusesHubBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_servers.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"url":"http://localhost:9200/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"url":"http://localhost:9200/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	tc := &tabnineCLIClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "tabnine-cli", urlField: "url"}}
	err := tc.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

func TestTabnineCLI_Exists_DirBased(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".tabnine")
	path := filepath.Join(cfgDir, "mcp_servers.json")
	tc := &tabnineCLIClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "tabnine-cli", urlField: "url"}}
	if tc.Exists() {
		t.Error("Exists() = true before parent dir created, want false")
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !tc.Exists() {
		t.Error("Exists() = false after parent dir created, want true")
	}
}

func TestTabnineCLI_BackupKeep_SeedsFreshInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tabnine", "mcp_servers.json")
	tc := &tabnineCLIClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "tabnine-cli", urlField: "url"}}
	bak, err := tc.BackupKeep(5)
	if err != nil {
		t.Fatalf("BackupKeep on fresh install: %v", err)
	}
	if bak == "" {
		t.Fatal("BackupKeep returned empty backup path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config not seeded: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("seeded config not valid JSON: %v", err)
	}
	if _, ok := m["mcpServers"].(map[string]any); !ok {
		t.Errorf("seeded config missing mcpServers map: %v", m)
	}
}

func TestTabnineCLI_NameAndConfigPath(t *testing.T) {
	c, err := NewTabnineCLI()
	if err != nil {
		t.Fatalf("NewTabnineCLI: %v", err)
	}
	if c.Name() != "tabnine-cli" {
		t.Errorf("Name() = %q, want tabnine-cli", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false")
	}
	got := c.ConfigPath()
	if filepath.Base(got) != "mcp_servers.json" {
		t.Errorf("ConfigPath() base = %q, want mcp_servers.json", filepath.Base(got))
	}
	if filepath.Base(filepath.Dir(got)) != ".tabnine" {
		t.Errorf("ConfigPath() parent = %q, want .tabnine (got %q)", filepath.Base(filepath.Dir(got)), got)
	}
}
