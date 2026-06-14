package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newClineForTest(t *testing.T, initial string) *clineClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cline_mcp_settings.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &clineClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "cline",
		urlField:   "url",
	}}
}

// TestCline_AddEntry_WritesStreamableHTTPSchema verifies that AddEntry emits
// Cline's Streamable HTTP schema (type:"streamableHttp" + url), NOT the
// Cursor/VS Code `type:"http"` form. Cline's schema only accepts
// "stdio"/"sse"/"streamableHttp" and defaults an entry with no `type` to SSE,
// so the explicit "streamableHttp" discriminator is load-bearing.
func TestCline_AddEntry_WritesStreamableHTTPSchema(t *testing.T) {
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
			c := newClineForTest(t, `{"other":"keep-me"}`)
			if err := c.AddEntry(tc.entry); err != nil {
				t.Fatalf("AddEntry: %v", err)
			}
			raw, _ := os.ReadFile(c.path)
			var parsed map[string]any
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("parse: %v", err)
			}
			servers, ok := parsed["mcpServers"].(map[string]any)
			if !ok {
				t.Fatalf("mcpServers missing or wrong type: %v", parsed["mcpServers"])
			}
			entry, ok := servers[tc.entry.Name].(map[string]any)
			if !ok {
				t.Fatalf("%s entry missing: %v", tc.entry.Name, servers)
			}
			if entry["url"] != tc.wantURL {
				t.Errorf("url = %v, want %v", entry["url"], tc.wantURL)
			}
			// The Streamable HTTP discriminator MUST be exactly "streamableHttp";
			// "http" fails Cline's schema and an absent type is parsed as SSE.
			if entry["type"] != "streamableHttp" {
				t.Errorf("type = %v, want streamableHttp", entry["type"])
			}
			if tc.wantHdr {
				hdrs, ok := entry["headers"].(map[string]any)
				if !ok || hdrs["X-Token"] != "abc" {
					t.Errorf("headers = %v, want {X-Token: abc}", entry["headers"])
				}
			} else if _, hasHdr := entry["headers"]; hasHdr {
				t.Errorf("headers should be absent for entry with no headers: %v", entry)
			}
			// Unrelated top-level field must survive round-trip.
			if parsed["other"] != "keep-me" {
				t.Error("other top-level field dropped")
			}
		})
	}
}

// TestCline_GetEntry_ReadsURLField confirms the inherited GetEntry reads the
// standard `url` field and round-trips headers.
func TestCline_GetEntry_ReadsURLField(t *testing.T) {
	c := newClineForTest(t, `{
  "mcpServers": {
    "serena": {
      "type": "streamableHttp",
      "url": "http://localhost:9121/mcp",
      "headers": {"X-Token": "abc"}
    }
  }
}`)
	e, err := c.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e == nil {
		t.Fatal("GetEntry returned nil")
	}
	if e.URL != "http://localhost:9121/mcp" {
		t.Errorf("URL = %q, want http://localhost:9121/mcp", e.URL)
	}
	if e.Headers["X-Token"] != "abc" {
		t.Errorf("Headers = %v, want {X-Token: abc}", e.Headers)
	}
}

// TestCline_GetEntry_Missing returns nil for an absent entry.
func TestCline_GetEntry_Missing(t *testing.T) {
	c := newClineForTest(t, `{"mcpServers":{}}`)
	e, err := c.GetEntry("nope")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e != nil {
		t.Errorf("GetEntry = %v, want nil for absent entry", e)
	}
}

// TestCline_RemoveEntry_Inherited confirms RemoveEntry (promoted from
// jsonMCPClient) works through the embedded struct and leaves other entries.
func TestCline_RemoveEntry_Inherited(t *testing.T) {
	c := newClineForTest(t, `{"mcpServers":{"serena":{"type":"streamableHttp","url":"http://x"},"other":{"type":"streamableHttp","url":"http://y"}}}`)
	if err := c.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := c.GetEntry("serena"); e != nil {
		t.Errorf("serena still present after Remove: %v", e)
	}
	if e, _ := c.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
}

// TestCline_RestoreEntryFromBackup_RestoresStdio restores a pre-hub stdio entry
// over a hub-HTTP live entry. Exercises the inherited base restore body, which
// keys hub-shape detection off urlField "url".
func TestCline_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cline_mcp_settings.json")
	// Live is post-migrate hub-HTTP.
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"streamableHttp","url":"http://localhost:9001/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &clineClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "cline", urlField: "url"}}
	if err := c.RestoreEntryFromBackup(backup, "memory"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	entry := m["mcpServers"].(map[string]any)["memory"].(map[string]any)
	if entry["command"] != "npx" {
		t.Errorf("command=%v, want npx", entry["command"])
	}
	if _, hasURL := entry["url"]; hasURL {
		t.Errorf("hub-http url should be gone, got %v", entry)
	}
}

// TestCline_RestoreEntryFromBackup_RemovesOnAbsent removes the live entry when
// the backup did not contain it (migrate added it from scratch).
func TestCline_RestoreEntryFromBackup_RemovesOnAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cline_mcp_settings.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"newserver":{"type":"streamableHttp","url":"http://localhost:9009/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &clineClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "cline", urlField: "url"}}
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
		t.Error("newserver should have been removed")
	}
}

// TestCline_RestoreEntryFromBackup_RefusesHubBackupEntry verifies a backup whose
// entry is ALREADY in hub-managed loopback-url shape is refused with
// ErrBackupEntryAlreadyMigrated (demigrate guard). The base body routes
// url-shape detection because urlField is "url".
func TestCline_RestoreEntryFromBackup_RefusesHubBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cline_mcp_settings.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"streamableHttp","url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"streamableHttp","url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &clineClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "cline", urlField: "url"}}
	err := c.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

// TestCline_RestoreEntryFromBackupForRollback_AllowsHubEntry verifies the
// rollback variant bypasses the hub-managed guard and writes the backup's
// hub-shaped url entry verbatim.
func TestCline_RestoreEntryFromBackupForRollback_AllowsHubEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cline_mcp_settings.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"serena":{"type":"streamableHttp","url":"http://localhost:9999/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"serena":{"type":"streamableHttp","url":"http://localhost:9121/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	c := &clineClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "cline", urlField: "url"}}
	if err := c.RestoreEntryFromBackupForRollback(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackupForRollback: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	entry := m["mcpServers"].(map[string]any)["serena"].(map[string]any)
	if entry["url"] != "http://localhost:9121/mcp" {
		t.Errorf("url = %v, want restored pre-reconcile http://localhost:9121/mcp", entry["url"])
	}
}

// TestCline_BackupEntryIsHubManaged exercises the inherited url-keyed predicate
// across hub-shaped, pre-hub stdio, remote non-loopback, and absent entries.
func TestCline_BackupEntryIsHubManaged(t *testing.T) {
	cases := []struct {
		name   string
		backup string
		entry  string
		want   bool
	}{
		{
			name:   "hub loopback url shape",
			backup: `{"mcpServers":{"memory":{"type":"streamableHttp","url":"http://localhost:9200/mcp"}}}`,
			entry:  "memory",
			want:   true,
		},
		{
			name:   "pre-hub stdio shape",
			backup: `{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"]}}}`,
			entry:  "memory",
			want:   false,
		},
		{
			name:   "remote non-loopback url is NOT hub-managed",
			backup: `{"mcpServers":{"ctx7":{"type":"streamableHttp","url":"https://api.example.com/mcp"}}}`,
			entry:  "ctx7",
			want:   false,
		},
		{
			name:   "absent entry",
			backup: `{"mcpServers":{}}`,
			entry:  "memory",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "cline_mcp_settings.json")
			backup := path + ".bak-mcp-local-hub-20260101-000000"
			if err := os.WriteFile(backup, []byte(tc.backup), 0600); err != nil {
				t.Fatal(err)
			}
			c := &clineClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "cline", urlField: "url"}}
			got, err := c.BackupEntryIsHubManaged(backup, tc.entry)
			if err != nil {
				t.Fatalf("BackupEntryIsHubManaged: %v", err)
			}
			if got != tc.want {
				t.Errorf("BackupEntryIsHubManaged = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCline_Exists_DirBased verifies the dir-based Exists override: the client
// counts as installed when the parent (settings) dir exists even before the
// config file is written, but not when neither is present.
func TestCline_Exists_DirBased(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "globalStorage", "saoudrizwan.claude-dev", "settings")
	path := filepath.Join(cfgDir, "cline_mcp_settings.json")
	c := &clineClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "cline", urlField: "url"}}

	if c.Exists() {
		t.Error("Exists() = true before parent dir created, want false")
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !c.Exists() {
		t.Error("Exists() = false after parent dir created, want true (dir-based probe)")
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !c.Exists() {
		t.Error("Exists() = false with config file present, want true")
	}
}

// TestCline_BackupKeep_SeedsFreshInstall verifies BackupKeep creates the parent
// dir and seeds an empty stub when run against a fresh install with no config
// file, instead of failing with ErrClientNotInstalled.
func TestCline_BackupKeep_SeedsFreshInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")
	c := &clineClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "cline", urlField: "url"}}
	bak, err := c.BackupKeep(5)
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

// TestCline_NameAndConfigPath confirms the stable identifier and that the config
// path lands under the VS Code globalStorage saoudrizwan.claude-dev/settings
// directory ending in cline_mcp_settings.json.
func TestCline_NameAndConfigPath(t *testing.T) {
	c, err := NewCline()
	if err != nil {
		t.Fatalf("NewCline: %v", err)
	}
	if c.Name() != "cline" {
		t.Errorf("Name() = %q, want cline", c.Name())
	}
	got := c.ConfigPath()
	if filepath.Base(got) != "cline_mcp_settings.json" ||
		filepath.Base(filepath.Dir(got)) != "settings" ||
		filepath.Base(filepath.Dir(filepath.Dir(got))) != "saoudrizwan.claude-dev" {
		t.Errorf("ConfigPath() = %q, want path ending in saoudrizwan.claude-dev/settings/cline_mcp_settings.json", got)
	}
}
