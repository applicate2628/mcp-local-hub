package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newDroidForTest(t *testing.T, initial string) *droidClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &droidClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "droid",
		urlField:   "url",
	}}
}

// TestDroid_AddEntry_WritesHTTPShape verifies AddEntry emits Droid's HTTP
// shape: type="http" + url + disabled:false under the top-level `mcpServers`
// key, optionally with headers, preserving unrelated top-level fields.
func TestDroid_AddEntry_WritesHTTPShape(t *testing.T) {
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
			d := newDroidForTest(t, `{"other":"keep-me"}`)
			if err := d.AddEntry(tc.entry); err != nil {
				t.Fatalf("AddEntry: %v", err)
			}
			raw, _ := os.ReadFile(d.path)
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
			if entry["type"] != "http" {
				t.Errorf("type = %v, want http", entry["type"])
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
				t.Errorf("headers should be absent for entry without headers: %v", entry)
			}
			if parsed["other"] != "keep-me" {
				t.Error("other top-level field dropped")
			}
		})
	}
}

// TestDroid_GetEntry_ReadsURLField confirms GetEntry reads `url` and
// round-trips headers.
func TestDroid_GetEntry_ReadsURLField(t *testing.T) {
	d := newDroidForTest(t, `{
  "mcpServers": {
    "serena": {
      "type": "http",
      "url": "http://localhost:9121/mcp",
      "disabled": false,
      "headers": {"X-Token": "abc"}
    }
  }
}`)
	e, err := d.GetEntry("serena")
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

// TestDroid_GetEntry_Missing returns nil for an absent entry.
func TestDroid_GetEntry_Missing(t *testing.T) {
	d := newDroidForTest(t, `{"mcpServers":{}}`)
	e, err := d.GetEntry("nope")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e != nil {
		t.Errorf("GetEntry = %v, want nil for absent entry", e)
	}
}

// TestDroid_RemoveEntry_Inherited confirms RemoveEntry works and leaves other
// entries.
func TestDroid_RemoveEntry_Inherited(t *testing.T) {
	d := newDroidForTest(t, `{"mcpServers":{"serena":{"type":"http","url":"http://x"},"other":{"type":"http","url":"http://y"}}}`)
	if err := d.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := d.GetEntry("serena"); e != nil {
		t.Errorf("serena still present after Remove: %v", e)
	}
	if e, _ := d.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
}

// TestDroid_RestoreEntryFromBackup_RestoresStdio restores a pre-hub stdio entry
// over a hub-HTTP live entry.
func TestDroid_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9001/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	d := &droidClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "droid", urlField: "url"}}
	if err := d.RestoreEntryFromBackup(backup, "memory"); err != nil {
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
		t.Errorf("hub url should be gone, got %v", entry)
	}
}

// TestDroid_RestoreEntryFromBackup_RefusesHubBackupEntry verifies the inherited
// url-keyed guard refuses an already-hub-managed backup entry.
func TestDroid_RestoreEntryFromBackup_RefusesHubBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	d := &droidClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "droid", urlField: "url"}}
	err := d.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

// TestDroid_RestoreEntryFromBackupForRollback_AllowsHubEntry verifies the
// rollback variant bypasses the guard.
func TestDroid_RestoreEntryFromBackupForRollback_AllowsHubEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"serena":{"type":"http","url":"http://localhost:9999/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"serena":{"type":"http","url":"http://localhost:9121/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	d := &droidClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "droid", urlField: "url"}}
	if err := d.RestoreEntryFromBackupForRollback(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackupForRollback: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	entry := m["mcpServers"].(map[string]any)["serena"].(map[string]any)
	if entry["url"] != "http://localhost:9121/mcp" {
		t.Errorf("url = %v, want restored http://localhost:9121/mcp", entry["url"])
	}
}

// TestDroid_BackupEntryIsHubManaged exercises the inherited url-keyed predicate.
func TestDroid_BackupEntryIsHubManaged(t *testing.T) {
	cases := []struct {
		name   string
		backup string
		entry  string
		want   bool
	}{
		{
			name:   "hub http+url shape",
			backup: `{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp","disabled":false}}}`,
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
			backup: `{"mcpServers":{"linear":{"type":"http","url":"https://mcp.linear.app/mcp"}}}`,
			entry:  "linear",
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
			path := filepath.Join(dir, "mcp.json")
			backup := path + ".bak-mcp-local-hub-20260101-000000"
			if err := os.WriteFile(backup, []byte(tc.backup), 0600); err != nil {
				t.Fatal(err)
			}
			d := &droidClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "droid", urlField: "url"}}
			got, err := d.BackupEntryIsHubManaged(backup, tc.entry)
			if err != nil {
				t.Fatalf("BackupEntryIsHubManaged: %v", err)
			}
			if got != tc.want {
				t.Errorf("BackupEntryIsHubManaged = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDroid_Exists_DirBased verifies the dir-based Exists override against the
// ~/.factory parent directory.
func TestDroid_Exists_DirBased(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".factory")
	path := filepath.Join(cfgDir, "mcp.json")
	d := &droidClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "droid", urlField: "url"}}

	if d.Exists() {
		t.Error("Exists() = true before parent dir created, want false")
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !d.Exists() {
		t.Error("Exists() = false after parent dir created, want true (dir-based probe)")
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !d.Exists() {
		t.Error("Exists() = false with config file present, want true")
	}
}

// TestDroid_BackupKeep_SeedsFreshInstall verifies BackupKeep seeds the parent
// dir + stub on a fresh install.
func TestDroid_BackupKeep_SeedsFreshInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".factory", "mcp.json")
	d := &droidClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "droid", urlField: "url"}}
	bak, err := d.BackupKeep(5)
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

// TestDroid_NameAndConfigPath confirms the identifier and the
// ~/.factory/mcp.json path.
func TestDroid_NameAndConfigPath(t *testing.T) {
	c, err := NewDroid()
	if err != nil {
		t.Fatalf("NewDroid: %v", err)
	}
	if c.Name() != "droid" {
		t.Errorf("Name() = %q, want droid", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false (URL-native HTTP client)")
	}
	got := c.ConfigPath()
	if filepath.Base(got) != "mcp.json" {
		t.Errorf("ConfigPath() base = %q, want mcp.json", filepath.Base(got))
	}
	if filepath.Base(filepath.Dir(got)) != ".factory" {
		t.Errorf("ConfigPath() parent = %q, want .factory (got %q)", filepath.Base(filepath.Dir(got)), got)
	}
}
