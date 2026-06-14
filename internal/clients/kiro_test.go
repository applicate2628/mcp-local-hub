package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newKiroForTest(t *testing.T, initial string) *kiroClient {
	t.Helper()
	dir := t.TempDir()
	// Mirror Kiro's real nested layout (~/.kiro/settings/mcp.json) so
	// the parent-dir Exists heuristic and MkdirAll path are exercised
	// against the same shape production uses.
	settings := filepath.Join(dir, ".kiro", "settings")
	if err := os.MkdirAll(settings, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(settings, "mcp.json")
	if initial != "" {
		if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return &kiroClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "kiro",
		urlField:   "url",
	}}
}

func TestKiro_Name(t *testing.T) {
	c := newKiroForTest(t, `{"mcpServers":{}}`)
	if got := c.Name(); got != "kiro" {
		t.Errorf("Name() = %q, want kiro", got)
	}
}

// TestKiro_AddEntry_WritesURLNoType verifies the hub writes Kiro's
// documented remote-server shape: a `url` key, `disabled:false`, and
// crucially NO `type` field (Kiro distinguishes remote-vs-local by the
// presence of url vs command, unlike Cursor/Gemini which write
// type:"http"). Top-level non-mcpServers fields must survive the merge.
func TestKiro_AddEntry_WritesURLNoType(t *testing.T) {
	tests := []struct {
		name    string
		initial string
		entry   MCPEntry
	}{
		{
			name:    "into existing mcpServers preserves siblings",
			initial: `{"other":"keep-me","mcpServers":{"existing":{"url":"http://localhost:1/mcp"}}}`,
			entry:   MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"},
		},
		{
			name:    "into empty stub",
			initial: `{"mcpServers":{}}`,
			entry:   MCPEntry{Name: "memory", URL: "http://127.0.0.1:9128/mcp"},
		},
		{
			name:    "with headers",
			initial: `{"mcpServers":{}}`,
			entry:   MCPEntry{Name: "time", URL: "http://localhost:9130/mcp", Headers: map[string]string{"X-Auth": "tok"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newKiroForTest(t, tc.initial)
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
			got, ok := servers[tc.entry.Name].(map[string]any)
			if !ok {
				t.Fatalf("entry %q missing: %v", tc.entry.Name, servers)
			}
			if got["url"] != tc.entry.URL {
				t.Errorf("url = %v, want %q", got["url"], tc.entry.URL)
			}
			if _, hasType := got["type"]; hasType {
				t.Errorf("entry has a `type` field (%v); Kiro remote entries must NOT carry one", got["type"])
			}
			if got["disabled"] != false {
				t.Errorf("disabled = %v, want false", got["disabled"])
			}
			if len(tc.entry.Headers) > 0 {
				hdrs, ok := got["headers"].(map[string]any)
				if !ok || hdrs["X-Auth"] != "tok" {
					t.Errorf("headers = %v, want X-Auth:tok", got["headers"])
				}
			}
		})
	}
}

func TestKiro_TopLevelFieldsPreserved(t *testing.T) {
	c := newKiroForTest(t, `{"keep":"me","mcpServers":{}}`)
	if err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(c.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed["keep"] != "me" {
		t.Errorf("top-level field dropped: %v", parsed["keep"])
	}
}

func TestKiro_GetEntry_RoundTrip(t *testing.T) {
	c := newKiroForTest(t, `{"mcpServers":{}}`)
	want := MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp", Headers: map[string]string{"X-Auth": "tok"}}
	if err := c.AddEntry(want); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	got, err := c.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil {
		t.Fatal("GetEntry returned nil for present entry")
	}
	if got.URL != want.URL {
		t.Errorf("URL = %q, want %q", got.URL, want.URL)
	}
	if got.Headers["X-Auth"] != "tok" {
		t.Errorf("Headers = %v, want X-Auth:tok", got.Headers)
	}

	missing, err := c.GetEntry("nope")
	if err != nil {
		t.Fatalf("GetEntry(missing): %v", err)
	}
	if missing != nil {
		t.Errorf("GetEntry(missing) = %v, want nil", missing)
	}
}

func TestKiro_RemoveEntry_Idempotent(t *testing.T) {
	c := newKiroForTest(t, `{"mcpServers":{"serena":{"url":"http://localhost:9121/mcp","disabled":false}}}`)
	if err := c.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	got, err := c.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry after remove: %v", err)
	}
	if got != nil {
		t.Errorf("entry still present after RemoveEntry: %v", got)
	}
	// Second remove is a no-op.
	if err := c.RemoveEntry("serena"); err != nil {
		t.Errorf("second RemoveEntry not idempotent: %v", err)
	}
}

func TestKiro_InitEmpty(t *testing.T) {
	// Build a client whose config file does not yet exist (but parent does).
	c := newKiroForTest(t, "")
	if _, err := os.Stat(c.path); !os.IsNotExist(err) {
		t.Fatalf("precondition: config file should be absent, stat err=%v", err)
	}
	created, err := c.InitEmpty()
	if err != nil {
		t.Fatalf("InitEmpty: %v", err)
	}
	if !created {
		t.Error("InitEmpty created=false on first call, want true")
	}
	raw, _ := os.ReadFile(c.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse stub: %v", err)
	}
	if _, ok := parsed["mcpServers"].(map[string]any); !ok {
		t.Errorf("stub missing mcpServers object: %v", parsed)
	}
	// Second call is idempotent (file already exists).
	created2, err := c.InitEmpty()
	if err != nil {
		t.Fatalf("second InitEmpty: %v", err)
	}
	if created2 {
		t.Error("second InitEmpty created=true, want false (idempotent)")
	}
}

func TestKiro_Exists_DirHeuristic(t *testing.T) {
	// Parent dir present, file absent -> Exists() true (installed-but-no-config).
	c := newKiroForTest(t, "")
	if !c.Exists() {
		t.Error("Exists() = false when ~/.kiro/settings dir present, want true")
	}
}

// TestKiro_BackupKeep_FreshHost verifies BackupKeep creates the nested
// parent dir and seeds a stub on a clean host where neither the config
// nor the .kiro/settings directory exist yet.
func TestKiro_BackupKeep_FreshHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".kiro", "settings", "mcp.json")
	c := &kiroClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "kiro",
		urlField:   "url",
	}}
	bak, err := c.BackupKeep(3)
	if err != nil {
		t.Fatalf("BackupKeep on fresh host: %v", err)
	}
	if _, err := os.Stat(bak); err != nil {
		t.Errorf("backup file not written: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config stub not seeded: %v", err)
	}
}

// TestKiro_BackupEntryIsHubManaged confirms the inherited demigrate
// predicate classifies a hub loopback url entry (urlField "url", no
// command, no type) as hub-managed, and a user-configured remote entry
// as not hub-managed.
func TestKiro_BackupEntryIsHubManaged(t *testing.T) {
	dir := t.TempDir()
	hubBackup := filepath.Join(dir, "hub.json")
	if err := os.WriteFile(hubBackup, []byte(`{"mcpServers":{"serena":{"url":"http://localhost:9121/mcp","disabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	userBackup := filepath.Join(dir, "user.json")
	if err := os.WriteFile(userBackup, []byte(`{"mcpServers":{"serena":{"url":"https://remote.example.com/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := newKiroForTest(t, `{"mcpServers":{}}`)

	managed, err := c.BackupEntryIsHubManaged(hubBackup, "serena")
	if err != nil {
		t.Fatalf("BackupEntryIsHubManaged(hub): %v", err)
	}
	if !managed {
		t.Error("hub loopback entry classified as NOT hub-managed, want hub-managed")
	}

	managed2, err := c.BackupEntryIsHubManaged(userBackup, "serena")
	if err != nil {
		t.Fatalf("BackupEntryIsHubManaged(user): %v", err)
	}
	if managed2 {
		t.Error("user remote entry classified as hub-managed, want NOT hub-managed")
	}
}
