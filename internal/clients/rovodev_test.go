package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newRovoDevForTest(t *testing.T, initial string) *rovoDevClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &rovoDevClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "rovodev",
		urlField:   "url",
	}}
}

// TestRovoDev_AddEntry_WritesTransportHTTPSchema verifies AddEntry emits Rovo
// Dev's HTTP shape: url + transport:"http" (NOT a `type` discriminator),
// optionally with headers, under the top-level `mcpServers` key.
func TestRovoDev_AddEntry_WritesTransportHTTPSchema(t *testing.T) {
	r := newRovoDevForTest(t, `{"other":"keep-me"}`)
	if err := r.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(r.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers := parsed["mcpServers"].(map[string]any)
	serena := servers["serena"].(map[string]any)
	if serena["url"] != "http://localhost:9121/mcp" {
		t.Errorf("url = %v, want hub URL", serena["url"])
	}
	if serena["transport"] != "http" {
		t.Errorf("transport = %v, want http", serena["transport"])
	}
	// Rovo Dev uses `transport`, not the Roo-family `type` key.
	if _, has := serena["type"]; has {
		t.Errorf("Rovo Dev entry must not carry a `type` field (uses `transport`): %v", serena)
	}
	if parsed["other"] != "keep-me" {
		t.Error("other top-level field dropped")
	}
}

func TestRovoDev_AddEntry_WithHeaders(t *testing.T) {
	r := newRovoDevForTest(t, `{"mcpServers":{}}`)
	if err := r.AddEntry(MCPEntry{Name: "memory", URL: "http://localhost:9140/mcp", Headers: map[string]string{"Authorization": "Bearer xyz"}}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	got, err := r.GetEntry("memory")
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

func TestRovoDev_RemoveEntry_Inherited(t *testing.T) {
	r := newRovoDevForTest(t, `{"mcpServers":{"serena":{"url":"http://x","transport":"http"},"other":{"url":"http://y","transport":"http"}}}`)
	if err := r.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := r.GetEntry("serena"); e != nil {
		t.Errorf("serena still present: %v", e)
	}
	if e, _ := r.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
}

func TestRovoDev_RestoreEntryFromBackup_RefusesHubBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"url":"http://localhost:9200/mcp","transport":"http"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"url":"http://localhost:9200/mcp","transport":"http"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	r := &rovoDevClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "rovodev", urlField: "url"}}
	err := r.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

func TestRovoDev_Exists_DirBased(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".rovodev")
	path := filepath.Join(cfgDir, "mcp.json")
	r := &rovoDevClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "rovodev", urlField: "url"}}
	if r.Exists() {
		t.Error("Exists() = true before parent dir created, want false")
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !r.Exists() {
		t.Error("Exists() = false after parent dir created, want true")
	}
}

func TestRovoDev_BackupKeep_SeedsFreshInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".rovodev", "mcp.json")
	r := &rovoDevClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "rovodev", urlField: "url"}}
	bak, err := r.BackupKeep(5)
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

func TestRovoDev_NameAndConfigPath(t *testing.T) {
	c, err := NewRovoDev()
	if err != nil {
		t.Fatalf("NewRovoDev: %v", err)
	}
	if c.Name() != "rovodev" {
		t.Errorf("Name() = %q, want rovodev", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false")
	}
	got := c.ConfigPath()
	if filepath.Base(got) != "mcp.json" {
		t.Errorf("ConfigPath() base = %q, want mcp.json", filepath.Base(got))
	}
	if filepath.Base(filepath.Dir(got)) != ".rovodev" {
		t.Errorf("ConfigPath() parent = %q, want .rovodev (got %q)", filepath.Base(filepath.Dir(got)), got)
	}
}
