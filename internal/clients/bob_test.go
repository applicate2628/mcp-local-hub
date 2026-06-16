package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newBobForTest(t *testing.T, initial string) *bobClient {
	t.Helper()
	dir := t.TempDir()
	bobDir := filepath.Join(dir, ".bob")
	if err := os.MkdirAll(bobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bobDir, "mcp_settings.json")
	if initial != "" {
		if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return &bobClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "bob",
		urlField:   "url",
	}}
}

func TestBob_Name(t *testing.T) {
	c := newBobForTest(t, `{"mcpServers":{}}`)
	if got := c.Name(); got != "bob" {
		t.Errorf("Name() = %q, want bob", got)
	}
}

// TestBob_IsRelayStdio pins the HTTP-direct classification: Bob speaks HTTP
// MCP natively, so it is NOT a relay-stdio client.
func TestBob_IsRelayStdio(t *testing.T) {
	c := newBobForTest(t, `{"mcpServers":{}}`)
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false (bob is URL-native HTTP)")
	}
}

// TestBob_AddEntry_WritesHTTPShape verifies the hub writes Bob's documented
// remote shape: a `url` key + `disabled:false`, NO `type`/`transport` field
// (transport is inferred from `url`), plus `headers` when provided. Sibling
// entries and unrelated top-level fields survive the merge.
func TestBob_AddEntry_WritesHTTPShape(t *testing.T) {
	tests := []struct {
		name    string
		initial string
		entry   MCPEntry
	}{
		{
			name:    "into existing mcpServers preserves siblings",
			initial: `{"other":"keep-me","mcpServers":{"existing":{"url":"http://localhost:1/mcp","disabled":false}}}`,
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
			c := newBobForTest(t, tc.initial)
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
			// Bob infers transport from `url`; no type/transport field is written.
			if _, has := got["type"]; has {
				t.Errorf("unexpected 'type' field for Bob (transport inferred from url): %v", got)
			}
			if _, has := got["transport"]; has {
				t.Errorf("unexpected 'transport' field for Bob: %v", got)
			}
			// Must NOT write a stdio `command` for an HTTP-direct client.
			if _, has := got["command"]; has {
				t.Errorf("unexpected stdio 'command' field in HTTP entry: %v", got)
			}
			if len(tc.entry.Headers) > 0 {
				hdrs, ok := got["headers"].(map[string]any)
				if !ok || hdrs["X-Auth"] != "tok" {
					t.Errorf("headers = %v, want X-Auth:tok", got["headers"])
				}
			} else if _, hasHdrs := got["headers"]; hasHdrs {
				t.Errorf("headers present with no Headers supplied: %v", got["headers"])
			}
		})
	}
}

func TestBob_TopLevelFieldsPreserved(t *testing.T) {
	c := newBobForTest(t, `{"keep":"me","mcpServers":{}}`)
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

func TestBob_GetEntry_RoundTrip(t *testing.T) {
	c := newBobForTest(t, `{"mcpServers":{}}`)
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

func TestBob_RemoveEntry_Idempotent(t *testing.T) {
	c := newBobForTest(t, `{"mcpServers":{"serena":{"url":"http://localhost:9121/mcp","disabled":false}}}`)
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
	if err := c.RemoveEntry("serena"); err != nil {
		t.Errorf("second RemoveEntry not idempotent: %v", err)
	}
}

func TestBob_InitEmpty(t *testing.T) {
	c := newBobForTest(t, "")
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
	created2, err := c.InitEmpty()
	if err != nil {
		t.Fatalf("second InitEmpty: %v", err)
	}
	if created2 {
		t.Error("second InitEmpty created=true, want false (idempotent)")
	}
}

func TestBob_Exists_DirHeuristic(t *testing.T) {
	c := newBobForTest(t, "")
	if !c.Exists() {
		t.Error("Exists() = false when ~/.bob dir present, want true")
	}
}

// TestBob_BackupKeep_FreshHost verifies BackupKeep creates the parent dir and
// seeds a stub on a clean host where neither the config nor the .bob directory
// exist yet.
func TestBob_BackupKeep_FreshHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".bob", "mcp_settings.json")
	c := &bobClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "bob",
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

// TestBob_BackupEntryIsHubManaged confirms the inherited demigrate predicate
// classifies a hub loopback url entry (urlField "url", no command) as
// hub-managed, and a user-configured remote entry as not hub-managed.
func TestBob_BackupEntryIsHubManaged(t *testing.T) {
	dir := t.TempDir()
	hubBackup := filepath.Join(dir, "hub.json")
	if err := os.WriteFile(hubBackup, []byte(`{"mcpServers":{"serena":{"url":"http://localhost:9121/mcp","disabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	userBackup := filepath.Join(dir, "user.json")
	if err := os.WriteFile(userBackup, []byte(`{"mcpServers":{"serena":{"url":"https://remote.example.com/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := newBobForTest(t, `{"mcpServers":{}}`)

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

// TestBob_DefaultConfigPath asserts ~/.bob/mcp_settings.json resolution and
// that the public constructor threads it through to the live adapter.
func TestBob_DefaultConfigPath(t *testing.T) {
	got := defaultBobConfigPath(filepath.Join("home", "u"))
	want := filepath.Join("home", "u", ".bob", "mcp_settings.json")
	if got != want {
		t.Errorf("defaultBobConfigPath = %q, want %q", got, want)
	}

	c, err := NewBob()
	if err != nil {
		t.Fatalf("NewBob: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir on host: %v", err)
	}
	if cp := c.ConfigPath(); cp != filepath.Join(home, ".bob", "mcp_settings.json") {
		t.Errorf("ConfigPath() = %q, want ~/.bob/mcp_settings.json", cp)
	}
	if c.Name() != "bob" {
		t.Errorf("Name() = %q, want bob", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false")
	}
}
