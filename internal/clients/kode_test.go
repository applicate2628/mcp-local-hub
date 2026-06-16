package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newKodeForTest(t *testing.T, initial string) *kodeClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".kode.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &kodeClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "kode",
		urlField:   "url",
	}}
}

// TestKode_AddEntry_WritesHTTPShape verifies AddEntry emits Kode's
// discriminated-union HTTP shape: type="http" + url (+ optional headers) under
// the top-level `mcpServers` key, with NO `disabled` field, preserving
// unrelated top-level fields.
func TestKode_AddEntry_WritesHTTPShape(t *testing.T) {
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
			k := newKodeForTest(t, `{"other":"keep-me"}`)
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
			if entry["type"] != "http" {
				t.Errorf("type = %v, want http", entry["type"])
			}
			if entry["url"] != tc.wantURL {
				t.Errorf("url = %v, want %v", entry["url"], tc.wantURL)
			}
			if _, has := entry["disabled"]; has {
				t.Errorf("disabled should be absent (not in Kode union HTTP variant): %v", entry)
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

// TestKode_GetEntry_ReadsURLField confirms GetEntry reads `url` and round-trips
// headers.
func TestKode_GetEntry_ReadsURLField(t *testing.T) {
	k := newKodeForTest(t, `{
  "mcpServers": {
    "serena": {
      "type": "http",
      "url": "http://localhost:9121/mcp",
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

// TestKode_GetEntry_Missing returns nil for an absent entry.
func TestKode_GetEntry_Missing(t *testing.T) {
	k := newKodeForTest(t, `{"mcpServers":{}}`)
	e, err := k.GetEntry("nope")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e != nil {
		t.Errorf("GetEntry = %v, want nil for absent entry", e)
	}
}

// TestKode_RemoveEntry_Inherited confirms RemoveEntry works and leaves other
// entries.
func TestKode_RemoveEntry_Inherited(t *testing.T) {
	k := newKodeForTest(t, `{"mcpServers":{"serena":{"type":"http","url":"http://x"},"other":{"type":"http","url":"http://y"}}}`)
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

// TestKode_RestoreEntryFromBackup_RestoresStdio restores a pre-hub stdio entry
// over a hub-HTTP live entry.
func TestKode_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".kode.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9001/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	k := &kodeClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "kode", urlField: "url"}}
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

// TestKode_RestoreEntryFromBackup_RefusesHubBackupEntry verifies the inherited
// url-keyed guard refuses an already-hub-managed backup entry.
func TestKode_RestoreEntryFromBackup_RefusesHubBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".kode.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	k := &kodeClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "kode", urlField: "url"}}
	err := k.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

// TestKode_RestoreEntryFromBackupForRollback_AllowsHubEntry verifies the
// rollback variant bypasses the guard.
func TestKode_RestoreEntryFromBackupForRollback_AllowsHubEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".kode.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"serena":{"type":"http","url":"http://localhost:9999/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"serena":{"type":"http","url":"http://localhost:9121/mcp"}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	k := &kodeClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "kode", urlField: "url"}}
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
		t.Errorf("url = %v, want restored http://localhost:9121/mcp", entry["url"])
	}
}

// TestKode_BackupEntryIsHubManaged exercises the inherited url-keyed predicate.
func TestKode_BackupEntryIsHubManaged(t *testing.T) {
	cases := []struct {
		name   string
		backup string
		entry  string
		want   bool
	}{
		{
			name:   "hub http+url shape",
			backup: `{"mcpServers":{"memory":{"type":"http","url":"http://localhost:9200/mcp"}}}`,
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
			backup: `{"mcpServers":{"ctx7":{"type":"http","url":"https://api.example.com/mcp"}}}`,
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
			path := filepath.Join(dir, ".kode.json")
			backup := path + ".bak-mcp-local-hub-20260101-000000"
			if err := os.WriteFile(backup, []byte(tc.backup), 0600); err != nil {
				t.Fatal(err)
			}
			k := &kodeClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "kode", urlField: "url"}}
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

// TestKode_Exists_FileBased verifies the inherited (base) file-existence
// Exists: the dotfile-under-home config has no per-tool subdir, so presence is
// keyed on the file itself, not a parent directory.
func TestKode_Exists_FileBased(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".kode.json")
	k := &kodeClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "kode", urlField: "url"}}

	if k.Exists() {
		t.Error("Exists() = true before config file created, want false")
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !k.Exists() {
		t.Error("Exists() = false with config file present, want true")
	}
}

// TestKode_BackupKeep_SeedsFreshInstall verifies BackupKeep seeds an empty stub
// on a fresh install (no ~/.kode.json yet) without failing.
func TestKode_BackupKeep_SeedsFreshInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".kode.json")
	k := &kodeClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "kode", urlField: "url"}}
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

// TestKode_NameAndConfigPath confirms the identifier and the ~/.kode.json path.
func TestKode_NameAndConfigPath(t *testing.T) {
	c, err := NewKode()
	if err != nil {
		t.Fatalf("NewKode: %v", err)
	}
	if c.Name() != "kode" {
		t.Errorf("Name() = %q, want kode", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false (URL-native HTTP client)")
	}
	got := c.ConfigPath()
	if filepath.Base(got) != ".kode.json" {
		t.Errorf("ConfigPath() base = %q, want .kode.json", filepath.Base(got))
	}
}
