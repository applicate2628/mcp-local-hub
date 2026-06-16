package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newJunieForTest(t *testing.T, initial string) *junieClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &junieClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "junie",
		urlField:   "url",
	}}
}

// TestJunie_AddEntry_WritesURLShape verifies AddEntry emits Junie's minimal
// documented shape: just `url` (+ optional headers) under the top-level
// `mcpServers` key, with NO `type` or `disabled` field, preserving unrelated
// top-level fields.
func TestJunie_AddEntry_WritesURLShape(t *testing.T) {
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
			j := newJunieForTest(t, `{"other":"keep-me"}`)
			if err := j.AddEntry(tc.entry); err != nil {
				t.Fatalf("AddEntry: %v", err)
			}
			raw, _ := os.ReadFile(j.path)
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
			if _, has := entry["type"]; has {
				t.Errorf("type should be absent (Junie has no type discriminator): %v", entry)
			}
			if _, has := entry["disabled"]; has {
				t.Errorf("disabled should be absent (not in Junie docs shape): %v", entry)
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

// TestJunie_GetEntry_ReadsURLField confirms GetEntry (inherited from the base)
// reads the `url` field and round-trips headers.
func TestJunie_GetEntry_ReadsURLField(t *testing.T) {
	j := newJunieForTest(t, `{
  "mcpServers": {
    "serena": {
      "url": "http://localhost:9121/mcp",
      "headers": {"X-Token": "abc"}
    }
  }
}`)
	e, err := j.GetEntry("serena")
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

// TestJunie_GetEntry_Missing returns nil for an absent entry.
func TestJunie_GetEntry_Missing(t *testing.T) {
	j := newJunieForTest(t, `{"mcpServers":{}}`)
	e, err := j.GetEntry("nope")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e != nil {
		t.Errorf("GetEntry = %v, want nil for absent entry", e)
	}
}

// TestJunie_RemoveEntry_Inherited confirms RemoveEntry (promoted from
// jsonMCPClient) works through the embedded struct and leaves other entries.
func TestJunie_RemoveEntry_Inherited(t *testing.T) {
	j := newJunieForTest(t, `{"mcpServers":{"serena":{"url":"http://x"},"other":{"url":"http://y"}}}`)
	if err := j.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := j.GetEntry("serena"); e != nil {
		t.Errorf("serena still present after Remove: %v", e)
	}
	if e, _ := j.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
}

// TestJunie_RestoreEntryFromBackup_RestoresStdio restores a pre-hub stdio entry
// over a hub-HTTP live entry (inherited base body, url-keyed guard).
func TestJunie_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"url":"http://localhost:9001/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	j := &junieClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "junie", urlField: "url"}}
	if err := j.RestoreEntryFromBackup(backup, "memory"); err != nil {
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

// TestJunie_RestoreEntryFromBackup_RefusesHubBackupEntry verifies the inherited
// url-keyed guard refuses a backup whose entry is already hub-managed.
func TestJunie_RestoreEntryFromBackup_RefusesHubBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	j := &junieClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "junie", urlField: "url"}}
	err := j.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

// TestJunie_RestoreEntryFromBackupForRollback_AllowsHubEntry verifies the
// rollback variant bypasses the hub-managed guard.
func TestJunie_RestoreEntryFromBackupForRollback_AllowsHubEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"serena":{"url":"http://localhost:9999/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"serena":{"url":"http://localhost:9121/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	j := &junieClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "junie", urlField: "url"}}
	if err := j.RestoreEntryFromBackupForRollback(backup, "serena"); err != nil {
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

// TestJunie_BackupEntryIsHubManaged exercises the inherited url-keyed predicate.
func TestJunie_BackupEntryIsHubManaged(t *testing.T) {
	cases := []struct {
		name   string
		backup string
		entry  string
		want   bool
	}{
		{
			name:   "hub url shape",
			backup: `{"mcpServers":{"memory":{"url":"http://localhost:9200/mcp"}}}`,
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
			backup: `{"mcpServers":{"ctx7":{"url":"https://api.example.com/mcp"}}}`,
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
			path := filepath.Join(dir, "mcp.json")
			backup := path + ".bak-mcp-local-hub-20260101-000000"
			if err := os.WriteFile(backup, []byte(tc.backup), 0600); err != nil {
				t.Fatal(err)
			}
			j := &junieClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "junie", urlField: "url"}}
			got, err := j.BackupEntryIsHubManaged(backup, tc.entry)
			if err != nil {
				t.Fatalf("BackupEntryIsHubManaged: %v", err)
			}
			if got != tc.want {
				t.Errorf("BackupEntryIsHubManaged = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestJunie_Exists_DirBased verifies the dir-based Exists override against the
// nested ~/.junie/mcp parent directory.
func TestJunie_Exists_DirBased(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".junie", "mcp")
	path := filepath.Join(cfgDir, "mcp.json")
	j := &junieClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "junie", urlField: "url"}}

	if j.Exists() {
		t.Error("Exists() = true before parent dir created, want false")
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !j.Exists() {
		t.Error("Exists() = false after parent dir created, want true (dir-based probe)")
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !j.Exists() {
		t.Error("Exists() = false with config file present, want true")
	}
}

// TestJunie_BackupKeep_SeedsFreshInstall verifies BackupKeep creates the
// nested parent dir and seeds an empty stub on a fresh install.
func TestJunie_BackupKeep_SeedsFreshInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".junie", "mcp", "mcp.json")
	j := &junieClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "junie", urlField: "url"}}
	bak, err := j.BackupKeep(5)
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

// TestJunie_NameAndConfigPath confirms the identifier and the
// ~/.junie/mcp/mcp.json path layout.
func TestJunie_NameAndConfigPath(t *testing.T) {
	c, err := NewJunie()
	if err != nil {
		t.Fatalf("NewJunie: %v", err)
	}
	if c.Name() != "junie" {
		t.Errorf("Name() = %q, want junie", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false (URL-native HTTP client)")
	}
	got := c.ConfigPath()
	if filepath.Base(got) != "mcp.json" {
		t.Errorf("ConfigPath() base = %q, want mcp.json", filepath.Base(got))
	}
	if filepath.Base(filepath.Dir(got)) != "mcp" {
		t.Errorf("ConfigPath() parent = %q, want mcp (got %q)", filepath.Base(filepath.Dir(got)), got)
	}
	if filepath.Base(filepath.Dir(filepath.Dir(got))) != ".junie" {
		t.Errorf("ConfigPath() grandparent = %q, want .junie (got %q)", filepath.Base(filepath.Dir(filepath.Dir(got))), got)
	}
}
