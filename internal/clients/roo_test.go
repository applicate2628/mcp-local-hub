package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newRooForTest(t *testing.T, initial string) *rooClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cline_mcp_settings.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &rooClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "roo",
		urlField:   "url",
	}}
}

// TestRoo_AddEntry_WritesStreamableHTTPSchema verifies AddEntry emits Roo's
// Streamable HTTP shape: type="streamable-http" + url + disabled:false under
// the top-level `mcpServers` key, optionally with headers.
func TestRoo_AddEntry_WritesStreamableHTTPSchema(t *testing.T) {
	cases := []struct {
		name    string
		entry   MCPEntry
		wantURL string
		wantHdr bool
	}{
		{
			name:    "plain http entry",
			entry:   MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"},
			wantURL: "http://localhost:9121/mcp",
		},
		{
			name:    "entry with headers",
			entry:   MCPEntry{Name: "memory", URL: "http://localhost:9001/mcp", Headers: map[string]string{"X-Token": "abc"}},
			wantURL: "http://localhost:9001/mcp",
			wantHdr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRooForTest(t, `{"other":"keep-me"}`)
			if err := r.AddEntry(tc.entry); err != nil {
				t.Fatalf("AddEntry: %v", err)
			}
			raw, _ := os.ReadFile(r.path)
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("parse: %v", err)
			}
			servers := parsed["mcpServers"].(map[string]any)
			entry := servers[tc.entry.Name].(map[string]any)
			if entry["type"] != "streamable-http" {
				t.Errorf("type = %v, want streamable-http", entry["type"])
			}
			if entry["url"] != tc.wantURL {
				t.Errorf("url = %v, want %v", entry["url"], tc.wantURL)
			}
			if entry["disabled"] != false {
				t.Errorf("disabled = %v, want false", entry["disabled"])
			}
			if tc.wantHdr {
				hdrs, ok := entry["headers"].(map[string]any)
				if !ok || hdrs["X-Token"] != "abc" {
					t.Errorf("headers = %v, want {X-Token: abc}", entry["headers"])
				}
			} else if _, has := entry["headers"]; has {
				t.Errorf("headers should be absent: %v", entry)
			}
			if parsed["other"] != "keep-me" {
				t.Error("other top-level field dropped")
			}
		})
	}
}

func TestRoo_GetEntry_ReadsURLField(t *testing.T) {
	r := newRooForTest(t, `{"mcpServers":{"serena":{"type":"streamable-http","url":"http://localhost:9121/mcp","headers":{"X-Token":"abc"}}}}`)
	e, err := r.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e == nil || e.URL != "http://localhost:9121/mcp" {
		t.Fatalf("URL not read: %+v", e)
	}
	if e.Headers["X-Token"] != "abc" {
		t.Errorf("Headers = %v, want {X-Token: abc}", e.Headers)
	}
}

func TestRoo_RemoveEntry_Inherited(t *testing.T) {
	r := newRooForTest(t, `{"mcpServers":{"serena":{"type":"streamable-http","url":"http://x"},"other":{"type":"streamable-http","url":"http://y"}}}`)
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

func TestRoo_RestoreEntryFromBackup_RefusesHubBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cline_mcp_settings.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"streamable-http","url":"http://localhost:9200/mcp","disabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"streamable-http","url":"http://localhost:9200/mcp","disabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	r := &rooClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "roo", urlField: "url"}}
	err := r.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

func TestRoo_Exists_DirBased(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "globalStorage", "rooveterinaryinc.roo-cline", "settings")
	path := filepath.Join(cfgDir, "cline_mcp_settings.json")
	r := &rooClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "roo", urlField: "url"}}
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

func TestRoo_BackupKeep_SeedsFreshInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "globalStorage", "rooveterinaryinc.roo-cline", "settings", "cline_mcp_settings.json")
	r := &rooClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "roo", urlField: "url"}}
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

// TestRoo_NameAndConfigPath confirms the stable identifier and that the
// config path lands under
// …/globalStorage/rooveterinaryinc.roo-cline/settings/cline_mcp_settings.json.
func TestRoo_NameAndConfigPath(t *testing.T) {
	c, err := NewRoo()
	if err != nil {
		t.Fatalf("NewRoo: %v", err)
	}
	if c.Name() != "roo" {
		t.Errorf("Name() = %q, want roo", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false")
	}
	got := c.ConfigPath()
	if filepath.Base(got) != "cline_mcp_settings.json" {
		t.Errorf("ConfigPath() base = %q, want cline_mcp_settings.json", filepath.Base(got))
	}
	if filepath.Base(filepath.Dir(got)) != "settings" {
		t.Errorf("ConfigPath() parent = %q, want settings (got %q)", filepath.Base(filepath.Dir(got)), got)
	}
	if filepath.Base(filepath.Dir(filepath.Dir(got))) != "rooveterinaryinc.roo-cline" {
		t.Errorf("ConfigPath() grandparent = %q, want rooveterinaryinc.roo-cline (got %q)", filepath.Base(filepath.Dir(filepath.Dir(got))), got)
	}
	if filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(got)))) != "globalStorage" {
		t.Errorf("ConfigPath() great-grandparent = %q, want globalStorage (got %q)", filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(got)))), got)
	}
}
