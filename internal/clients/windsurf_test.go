package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newWindsurfForTest(t *testing.T, initial string) *windsurfClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &windsurfClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "windsurf",
		urlField:   windsurfURLField,
	}}
}

// TestWindsurf_AddEntry_WritesServerURLSchema verifies that AddEntry emits the
// Windsurf HTTP schema (serverUrl + disabled:false), NOT the Cursor/Gemini
// `url`/`type` form — Windsurf is the only major MCP client that names the
// endpoint field `serverUrl`.
func TestWindsurf_AddEntry_WritesServerURLSchema(t *testing.T) {
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
			w := newWindsurfForTest(t, `{"other":"keep-me"}`)
			if err := w.AddEntry(tc.entry); err != nil {
				t.Fatalf("AddEntry: %v", err)
			}
			raw, _ := os.ReadFile(w.path)
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
			if entry["serverUrl"] != tc.wantURL {
				t.Errorf("serverUrl = %v, want %v", entry["serverUrl"], tc.wantURL)
			}
			if entry["disabled"] != false {
				t.Errorf("disabled = %v, want false", entry["disabled"])
			}
			// Must NOT write the Cursor/Gemini `url` or `type` fields.
			if _, hasURL := entry["url"]; hasURL {
				t.Errorf("Cursor/Gemini `url` field should NOT be present: %v", entry)
			}
			if _, hasType := entry["type"]; hasType {
				t.Errorf("`type` field should NOT be present (Windsurf infers from serverUrl): %v", entry)
			}
			if tc.wantHdr {
				hdrs, ok := entry["headers"].(map[string]any)
				if !ok || hdrs["X-Token"] != "abc" {
					t.Errorf("headers = %v, want {X-Token: abc}", entry["headers"])
				}
			}
			// Unrelated top-level field must survive round-trip.
			if parsed["other"] != "keep-me" {
				t.Error("other top-level field dropped")
			}
		})
	}
}

// TestWindsurf_GetEntry_ReadsServerURLField confirms GetEntry reads the
// `serverUrl` field and round-trips headers.
func TestWindsurf_GetEntry_ReadsServerURLField(t *testing.T) {
	w := newWindsurfForTest(t, `{
  "mcpServers": {
    "serena": {
      "serverUrl": "http://localhost:9121/mcp",
      "disabled": false,
      "headers": {"X-Token": "abc"}
    }
  }
}`)
	e, err := w.GetEntry("serena")
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

// TestWindsurf_GetEntry_Missing returns nil for an absent entry.
func TestWindsurf_GetEntry_Missing(t *testing.T) {
	w := newWindsurfForTest(t, `{"mcpServers":{}}`)
	e, err := w.GetEntry("nope")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e != nil {
		t.Errorf("GetEntry = %v, want nil for absent entry", e)
	}
}

// TestWindsurf_RemoveEntry_Inherited confirms RemoveEntry (promoted from
// jsonMCPClient) works through the embedded struct and leaves other entries.
func TestWindsurf_RemoveEntry_Inherited(t *testing.T) {
	w := newWindsurfForTest(t, `{"mcpServers":{"serena":{"serverUrl":"http://x"},"other":{"serverUrl":"http://y"}}}`)
	if err := w.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := w.GetEntry("serena"); e != nil {
		t.Errorf("serena still present after Remove: %v", e)
	}
	if e, _ := w.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
}

// TestWindsurf_RestoreEntryFromBackup_RestoresStdio restores a pre-hub stdio
// entry over a hub-HTTP live entry.
func TestWindsurf_RestoreEntryFromBackup_RestoresStdio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	// Live is post-migrate hub-HTTP (serverUrl shape).
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"serverUrl":"http://localhost:9001/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"command":"npx","args":["-y","mem"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	w := &windsurfClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "windsurf", urlField: windsurfURLField}}
	if err := w.RestoreEntryFromBackup(backup, "memory"); err != nil {
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
	if _, hasURL := entry["serverUrl"]; hasURL {
		t.Errorf("hub-http serverUrl should be gone, got %v", entry)
	}
}

// TestWindsurf_RestoreEntryFromBackup_RemovesOnAbsent removes the live entry
// when the backup did not contain it (migrate added it from scratch).
func TestWindsurf_RestoreEntryFromBackup_RemovesOnAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"newserver":{"serverUrl":"http://localhost:9009/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	w := &windsurfClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "windsurf", urlField: windsurfURLField}}
	if err := w.RestoreEntryFromBackup(backup, "newserver"); err != nil {
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

// TestWindsurf_RestoreEntryFromBackup_RefusesHubServerURLBackupEntry verifies
// the serverUrl-aware override: a backup whose entry is ALREADY in hub-managed
// serverUrl-HTTP shape must be refused with ErrBackupEntryAlreadyMigrated. The
// base routing would have mis-classified this as relay-shape (no command) and
// silently restored it, so this exercises the override specifically.
func TestWindsurf_RestoreEntryFromBackup_RefusesHubServerURLBackupEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"memory":{"serverUrl":"http://localhost:9200/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"memory":{"serverUrl":"http://localhost:9200/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	w := &windsurfClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "windsurf", urlField: windsurfURLField}}
	err := w.RestoreEntryFromBackup(backup, "memory")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}

// TestWindsurf_RestoreEntryFromBackupForRollback_AllowsHubEntry verifies the
// rollback variant bypasses the hub-managed guard and writes the backup's
// hub-shaped serverUrl entry verbatim.
func TestWindsurf_RestoreEntryFromBackupForRollback_AllowsHubEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"serena":{"serverUrl":"http://localhost:9999/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"serena":{"serverUrl":"http://localhost:9121/mcp","disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	w := &windsurfClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "windsurf", urlField: windsurfURLField}}
	if err := w.RestoreEntryFromBackupForRollback(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackupForRollback: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	entry := m["mcpServers"].(map[string]any)["serena"].(map[string]any)
	if entry["serverUrl"] != "http://localhost:9121/mcp" {
		t.Errorf("serverUrl = %v, want restored pre-reconcile http://localhost:9121/mcp", entry["serverUrl"])
	}
}

// TestWindsurf_BackupEntryIsHubManaged exercises the serverUrl-aware predicate
// override across hub-shaped, pre-hub stdio, and absent entries.
func TestWindsurf_BackupEntryIsHubManaged(t *testing.T) {
	cases := []struct {
		name   string
		backup string
		entry  string
		want   bool
	}{
		{
			name:   "hub serverUrl shape",
			backup: `{"mcpServers":{"memory":{"serverUrl":"http://localhost:9200/mcp","disabled":false}}}`,
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
			name:   "remote non-loopback serverUrl is NOT hub-managed",
			backup: `{"mcpServers":{"ctx7":{"serverUrl":"https://api.example.com/mcp"}}}`,
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
			path := filepath.Join(dir, "mcp_config.json")
			backup := path + ".bak-mcp-local-hub-20260101-000000"
			if err := os.WriteFile(backup, []byte(tc.backup), 0600); err != nil {
				t.Fatal(err)
			}
			w := &windsurfClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "windsurf", urlField: windsurfURLField}}
			got, err := w.BackupEntryIsHubManaged(backup, tc.entry)
			if err != nil {
				t.Fatalf("BackupEntryIsHubManaged: %v", err)
			}
			if got != tc.want {
				t.Errorf("BackupEntryIsHubManaged = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWindsurf_Exists_DirBased verifies the dir-based Exists override: the
// client counts as installed when the parent dir exists even before the
// config file is written, but not when neither is present.
func TestWindsurf_Exists_DirBased(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".codeium", "windsurf")
	path := filepath.Join(cfgDir, "mcp_config.json")
	w := &windsurfClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "windsurf", urlField: windsurfURLField}}

	if w.Exists() {
		t.Error("Exists() = true before parent dir created, want false")
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !w.Exists() {
		t.Error("Exists() = false after parent dir created, want true (dir-based probe)")
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !w.Exists() {
		t.Error("Exists() = false with config file present, want true")
	}
}

// TestWindsurf_BackupKeep_SeedsFreshInstall verifies BackupKeep creates the
// parent dir and seeds an empty stub when run against a fresh install with no
// config file, instead of failing with ErrClientNotInstalled.
func TestWindsurf_BackupKeep_SeedsFreshInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".codeium", "windsurf", "mcp_config.json")
	w := &windsurfClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "windsurf", urlField: windsurfURLField}}
	bak, err := w.BackupKeep(5)
	if err != nil {
		t.Fatalf("BackupKeep on fresh install: %v", err)
	}
	if bak == "" {
		t.Fatal("BackupKeep returned empty backup path")
	}
	// The stub must now exist and be a valid empty mcpServers map.
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

// TestWindsurf_NameAndConfigPath confirms the stable identifier and that the
// config path lands under ~/.codeium/windsurf/mcp_config.json.
func TestWindsurf_NameAndConfigPath(t *testing.T) {
	c, err := NewWindsurf()
	if err != nil {
		t.Fatalf("NewWindsurf: %v", err)
	}
	if c.Name() != "windsurf" {
		t.Errorf("Name() = %q, want windsurf", c.Name())
	}
	got := c.ConfigPath()
	wantSuffix := filepath.Join(".codeium", "windsurf", "mcp_config.json")
	if filepath.Base(got) != "mcp_config.json" ||
		filepath.Base(filepath.Dir(got)) != "windsurf" ||
		filepath.Base(filepath.Dir(filepath.Dir(got))) != ".codeium" {
		t.Errorf("ConfigPath() = %q, want path ending in %q", got, wantSuffix)
	}
}
