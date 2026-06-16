package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newOnaForTest(t *testing.T, initial string) *onaClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp-config.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &onaClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "ona",
		urlField:   "url",
	}}
}

// TestOna_AddEntry_WritesBareURLSchema verifies AddEntry (inherited from the
// base) emits Ona's bare-`url` shape (url + disabled:false, NO `type`
// discriminator) under the top-level `mcpServers` key, optionally with
// headers, preserving unrelated top-level fields.
func TestOna_AddEntry_WritesBareURLSchema(t *testing.T) {
	o := newOnaForTest(t, `{"other":"keep-me"}`)
	if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(o.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers := parsed["mcpServers"].(map[string]any)
	serena := servers["serena"].(map[string]any)
	if serena["url"] != "http://localhost:9121/mcp" {
		t.Errorf("url = %v, want hub URL", serena["url"])
	}
	// Ona uses NO `type` discriminator — presence of url is the HTTP signal.
	if _, has := serena["type"]; has {
		t.Errorf("Ona entry must not carry a `type` field: %v", serena)
	}
	if serena["disabled"] != false {
		t.Errorf("disabled = %v, want false", serena["disabled"])
	}
	if parsed["other"] != "keep-me" {
		t.Error("other top-level field dropped")
	}
}

func TestOna_AddEntry_WithHeaders(t *testing.T) {
	o := newOnaForTest(t, `{"mcpServers":{}}`)
	hdrs := map[string]string{"Authorization": "Bearer xyz"}
	if err := o.AddEntry(MCPEntry{Name: "memory", URL: "http://localhost:9140/mcp", Headers: hdrs}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	got, err := o.GetEntry("memory")
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

func TestOna_GetEntry_Missing(t *testing.T) {
	o := newOnaForTest(t, `{"mcpServers":{}}`)
	e, err := o.GetEntry("nope")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e != nil {
		t.Errorf("GetEntry = %v, want nil for absent entry", e)
	}
}

func TestOna_RemoveEntry_Inherited(t *testing.T) {
	o := newOnaForTest(t, `{"mcpServers":{"serena":{"url":"http://x"},"other":{"url":"http://y"}}}`)
	if err := o.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := o.GetEntry("serena"); e != nil {
		t.Errorf("serena still present after Remove: %v", e)
	}
	if e, _ := o.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
}

func TestOna_RestoreEntryFromBackup_RefusesHubBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp-config.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"url":"http://localhost:9200/mcp","disabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"url":"http://localhost:9200/mcp","disabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &onaClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "ona", urlField: "url"}}
	err := o.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

// TestOna_Exists_DirBased verifies the dir-based Exists override.
func TestOna_Exists_DirBased(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".ona")
	path := filepath.Join(cfgDir, "mcp-config.json")
	o := &onaClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "ona", urlField: "url"}}
	if o.Exists() {
		t.Error("Exists() = true before parent dir created, want false")
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !o.Exists() {
		t.Error("Exists() = false after parent dir created, want true (dir-based probe)")
	}
}

// TestOna_BackupKeep_SeedsFreshInstall verifies BackupKeep creates the
// ~/.ona parent dir and seeds an empty stub on a fresh install.
func TestOna_BackupKeep_SeedsFreshInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".ona", "mcp-config.json")
	o := &onaClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "ona", urlField: "url"}}
	bak, err := o.BackupKeep(5)
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

func TestOna_NameAndConfigPath(t *testing.T) {
	c, err := NewOna()
	if err != nil {
		t.Fatalf("NewOna: %v", err)
	}
	if c.Name() != "ona" {
		t.Errorf("Name() = %q, want ona", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false (Ona is URL-native HTTP)")
	}
	got := c.ConfigPath()
	if filepath.Base(got) != "mcp-config.json" {
		t.Errorf("ConfigPath() base = %q, want mcp-config.json", filepath.Base(got))
	}
	if filepath.Base(filepath.Dir(got)) != ".ona" {
		t.Errorf("ConfigPath() parent = %q, want .ona (got %q)", filepath.Base(filepath.Dir(got)), got)
	}
}
