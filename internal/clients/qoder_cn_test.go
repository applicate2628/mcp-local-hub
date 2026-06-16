package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newQoderCNForTest(t *testing.T, initial string) *qoderCNClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".qoder-cn.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &qoderCNClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "qoder-cn",
		urlField:   "url",
	}}
}

func TestQoderCN_AddEntry_WritesStreamableHTTPSchema(t *testing.T) {
	q := newQoderCNForTest(t, `{"other":"keep-me"}`)
	if err := q.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(q.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers := parsed["mcpServers"].(map[string]any)
	serena := servers["serena"].(map[string]any)
	if serena["type"] != "streamable-http" {
		t.Errorf("type = %v, want streamable-http", serena["type"])
	}
	if serena["url"] != "http://localhost:9121/mcp" {
		t.Errorf("url = %v, want hub URL", serena["url"])
	}
	if serena["disabled"] != false {
		t.Errorf("disabled = %v, want false", serena["disabled"])
	}
	if parsed["other"] != "keep-me" {
		t.Error("other top-level field dropped")
	}
}

func TestQoderCN_AddEntry_WithHeaders(t *testing.T) {
	q := newQoderCNForTest(t, `{"mcpServers":{}}`)
	if err := q.AddEntry(MCPEntry{Name: "memory", URL: "http://localhost:9140/mcp", Headers: map[string]string{"X-Token": "abc"}}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	got, err := q.GetEntry("memory")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != "http://localhost:9140/mcp" {
		t.Fatalf("memory URL not set: %+v", got)
	}
	if got.Headers["X-Token"] != "abc" {
		t.Errorf("headers not round-tripped: %+v", got.Headers)
	}
}

func TestQoderCN_RemoveEntry_Inherited(t *testing.T) {
	q := newQoderCNForTest(t, `{"mcpServers":{"serena":{"type":"streamable-http","url":"http://x"},"other":{"type":"streamable-http","url":"http://y"}}}`)
	if err := q.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := q.GetEntry("serena"); e != nil {
		t.Errorf("serena still present: %v", e)
	}
	if e, _ := q.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
}

func TestQoderCN_RestoreEntryFromBackup_RefusesHubBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".qoder-cn.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"streamable-http","url":"http://localhost:9200/mcp","disabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"streamable-http","url":"http://localhost:9200/mcp","disabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	q := &qoderCNClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "qoder-cn", urlField: "url"}}
	err := q.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

// TestQoderCN_BackupKeep_SeedsFreshInstall verifies BackupKeep seeds an empty
// stub on a fresh install (the parent is the home dir; no MkdirAll needed).
func TestQoderCN_BackupKeep_SeedsFreshInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".qoder-cn.json")
	q := &qoderCNClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "qoder-cn", urlField: "url"}}
	bak, err := q.BackupKeep(5)
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

func TestQoderCN_NameAndConfigPath(t *testing.T) {
	c, err := NewQoderCN()
	if err != nil {
		t.Fatalf("NewQoderCN: %v", err)
	}
	if c.Name() != "qoder-cn" {
		t.Errorf("Name() = %q, want qoder-cn", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false")
	}
	got := c.ConfigPath()
	if !strings.HasSuffix(got, ".qoder-cn.json") {
		t.Errorf("ConfigPath() = %q, want suffix .qoder-cn.json", got)
	}
}
