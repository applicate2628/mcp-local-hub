package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newKiloCodeForTest(t *testing.T, initial string) *kiloCodeClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_settings.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &kiloCodeClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "kilocode",
		urlField:   "url",
	}}
}

// TestKiloCode_AddEntry_WritesStreamableHTTPSchema verifies AddEntry emits
// Kilo Code's Streamable HTTP shape: type="streamable-http" + url +
// disabled:false under the top-level `mcpServers` key, optionally with
// headers, while preserving unrelated top-level fields.
func TestKiloCode_AddEntry_WritesStreamableHTTPSchema(t *testing.T) {
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
			k := newKiloCodeForTest(t, `{"other":"keep-me"}`)
			if err := k.AddEntry(tc.entry); err != nil {
				t.Fatalf("AddEntry: %v", err)
			}
			raw, _ := os.ReadFile(k.path)
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
				t.Errorf("headers should be absent for entry without headers: %v", entry)
			}
			if parsed["other"] != "keep-me" {
				t.Error("other top-level field dropped")
			}
		})
	}
}

// TestKiloCode_GetEntry_ReadsURLField confirms GetEntry (inherited from the
// base) reads the `url` field and round-trips headers.
func TestKiloCode_GetEntry_ReadsURLField(t *testing.T) {
	k := newKiloCodeForTest(t, `{
  "mcpServers": {
    "serena": {
      "type": "streamable-http",
      "url": "http://localhost:9121/mcp",
      "disabled": false,
      "headers": {"X-Token": "abc"}
    }
  }
}`)
	e, err := k.GetEntry("serena")
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

// TestKiloCode_GetEntry_Missing returns nil for an absent entry.
func TestKiloCode_GetEntry_Missing(t *testing.T) {
	k := newKiloCodeForTest(t, `{"mcpServers":{}}`)
	e, err := k.GetEntry("nope")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e != nil {
		t.Errorf("GetEntry = %v, want nil for absent entry", e)
	}
}

// TestKiloCode_RemoveEntry_Inherited confirms RemoveEntry (promoted from
// jsonMCPClient) works through the embedded struct and leaves other entries.
func TestKiloCode_RemoveEntry_Inherited(t *testing.T) {
	k := newKiloCodeForTest(t, `{"mcpServers":{"serena":{"type":"streamable-http","url":"http://x"},"other":{"type":"streamable-http","url":"http://y"}}}`)
	if err := k.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := k.GetEntry("serena"); e != nil {
		t.Errorf("serena still present after Remove: %v", e)
	}
	if e, _ := k.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
}

// TestKiloCode_RestoreEntryFromBackup_RestoresStdio restores a pre-hub stdio
// entry over a hub-HTTP live entry (inherited base body, url-keyed guard).
func TestKiloCode_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_settings.json")
	// Live is post-migrate hub Streamable-HTTP shape.
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"streamable-http","url":"http://localhost:9001/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	k := &kiloCodeClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "kilocode", urlField: "url"}}
	if err := k.RestoreEntryFromBackup(backup, "memory"); err != nil {
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

// TestKiloCode_RestoreEntryFromBackup_RefusesHubBackupEntry verifies the
// inherited url-keyed guard refuses a backup whose entry is already in
// hub-managed Streamable-HTTP shape with ErrBackupEntryAlreadyMigrated.
func TestKiloCode_RestoreEntryFromBackup_RefusesHubBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_settings.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"streamable-http","url":"http://localhost:9200/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"streamable-http","url":"http://localhost:9200/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	k := &kiloCodeClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "kilocode", urlField: "url"}}
	err := k.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

// TestKiloCode_RestoreEntryFromBackupForRollback_AllowsHubEntry verifies the
// rollback variant bypasses the hub-managed guard and writes the backup's
// hub-shaped entry verbatim (inherited from the base).
func TestKiloCode_RestoreEntryFromBackupForRollback_AllowsHubEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_settings.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"serena":{"type":"streamable-http","url":"http://localhost:9999/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"serena":{"type":"streamable-http","url":"http://localhost:9121/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	k := &kiloCodeClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "kilocode", urlField: "url"}}
	if err := k.RestoreEntryFromBackupForRollback(backup, "serena"); err != nil {
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

// TestKiloCode_BackupEntryIsHubManaged exercises the inherited url-keyed
// predicate across hub-shaped, pre-hub stdio, remote non-loopback, and
// absent entries.
func TestKiloCode_BackupEntryIsHubManaged(t *testing.T) {
	cases := []struct {
		name   string
		backup string
		entry  string
		want   bool
	}{
		{
			name:   "hub streamable-http shape",
			backup: `{"mcpServers":{"memory":{"type":"streamable-http","url":"http://localhost:9200/mcp","disabled":false}}}`,
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
			backup: `{"mcpServers":{"ctx7":{"type":"streamable-http","url":"https://api.example.com/mcp"}}}`,
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
			path := filepath.Join(dir, "mcp_settings.json")
			backup := path + ".bak-mcp-local-hub-20260101-000000"
			if err := os.WriteFile(backup, []byte(tc.backup), 0600); err != nil {
				t.Fatal(err)
			}
			k := &kiloCodeClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "kilocode", urlField: "url"}}
			got, err := k.BackupEntryIsHubManaged(backup, tc.entry)
			if err != nil {
				t.Fatalf("BackupEntryIsHubManaged: %v", err)
			}
			if got != tc.want {
				t.Errorf("BackupEntryIsHubManaged = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestKiloCode_Exists_DirBased verifies the dir-based Exists override: the
// client counts as installed when the parent (…/settings) dir exists even
// before the config file is written, but not when neither is present.
func TestKiloCode_Exists_DirBased(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "globalStorage", "kilo-code.kilo-code", "settings")
	path := filepath.Join(cfgDir, "mcp_settings.json")
	k := &kiloCodeClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "kilocode", urlField: "url"}}

	if k.Exists() {
		t.Error("Exists() = true before parent dir created, want false")
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !k.Exists() {
		t.Error("Exists() = false after parent dir created, want true (dir-based probe)")
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !k.Exists() {
		t.Error("Exists() = false with config file present, want true")
	}
}

// TestKiloCode_BackupKeep_SeedsFreshInstall verifies BackupKeep creates the
// deeply-nested parent dir and seeds an empty stub when run against a fresh
// install with no config file, instead of failing with ErrClientNotInstalled.
func TestKiloCode_BackupKeep_SeedsFreshInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "globalStorage", "kilo-code.kilo-code", "settings", "mcp_settings.json")
	k := &kiloCodeClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "kilocode", urlField: "url"}}
	bak, err := k.BackupKeep(5)
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

// TestKiloCode_NameAndConfigPath confirms the stable identifier and that the
// config path lands under …/globalStorage/kilo-code.kilo-code/settings/mcp_settings.json.
func TestKiloCode_NameAndConfigPath(t *testing.T) {
	c, err := NewKiloCode()
	if err != nil {
		t.Fatalf("NewKiloCode: %v", err)
	}
	if c.Name() != "kilocode" {
		t.Errorf("Name() = %q, want kilocode", c.Name())
	}
	got := c.ConfigPath()
	if filepath.Base(got) != "mcp_settings.json" {
		t.Errorf("ConfigPath() base = %q, want mcp_settings.json", filepath.Base(got))
	}
	if filepath.Base(filepath.Dir(got)) != "settings" {
		t.Errorf("ConfigPath() parent = %q, want settings (got %q)", filepath.Base(filepath.Dir(got)), got)
	}
	if filepath.Base(filepath.Dir(filepath.Dir(got))) != "kilo-code.kilo-code" {
		t.Errorf("ConfigPath() grandparent = %q, want kilo-code.kilo-code (got %q)", filepath.Base(filepath.Dir(filepath.Dir(got))), got)
	}
	if filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(got)))) != "globalStorage" {
		t.Errorf("ConfigPath() great-grandparent = %q, want globalStorage (got %q)", filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(got)))), got)
	}
}
