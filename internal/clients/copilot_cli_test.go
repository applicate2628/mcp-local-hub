package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newCopilotCLIForTest(t *testing.T, initial string) *copilotCLIClient {
	t.Helper()
	dir := t.TempDir()
	// Mirror Copilot CLI's real layout (~/.copilot/mcp-config.json) so the
	// parent-dir Exists heuristic and MkdirAll path are exercised against the
	// same shape production uses.
	copilotDir := filepath.Join(dir, ".copilot")
	if err := os.MkdirAll(copilotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(copilotDir, "mcp-config.json")
	if initial != "" {
		if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return &copilotCLIClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "copilot-cli",
		urlField:   "url",
	}}
}

func TestCopilotCLI_Name(t *testing.T) {
	c := newCopilotCLIForTest(t, `{"mcpServers":{}}`)
	if got := c.Name(); got != "copilot-cli" {
		t.Errorf("Name() = %q, want copilot-cli", got)
	}
}

// TestCopilotCLI_IsRelayStdio pins the HTTP-direct classification: Copilot CLI
// speaks HTTP MCP natively, so it is NOT a relay-stdio client.
func TestCopilotCLI_IsRelayStdio(t *testing.T) {
	c := newCopilotCLIForTest(t, `{"mcpServers":{}}`)
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false (copilot-cli is URL-native HTTP)")
	}
}

// TestCopilotCLI_AddEntry_WritesHTTPShape verifies the hub writes Copilot CLI's
// documented http-transport shape: `type:"http"`, a `url` key, and a
// `tools:["*"]` filter (plus `headers` when provided). Top-level
// non-mcpServers fields and sibling entries must survive the merge.
func TestCopilotCLI_AddEntry_WritesHTTPShape(t *testing.T) {
	tests := []struct {
		name    string
		initial string
		entry   MCPEntry
	}{
		{
			name:    "into existing mcpServers preserves siblings",
			initial: `{"other":"keep-me","mcpServers":{"existing":{"type":"http","url":"http://localhost:1/mcp","tools":["*"]}}}`,
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
			c := newCopilotCLIForTest(t, tc.initial)
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
			if got["type"] != "http" {
				t.Errorf("type = %v, want http", got["type"])
			}
			if got["url"] != tc.entry.URL {
				t.Errorf("url = %v, want %q", got["url"], tc.entry.URL)
			}
			tools, ok := got["tools"].([]any)
			if !ok || len(tools) != 1 || tools[0] != "*" {
				t.Errorf("tools = %v, want [\"*\"]", got["tools"])
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

func TestCopilotCLI_TopLevelFieldsPreserved(t *testing.T) {
	c := newCopilotCLIForTest(t, `{"keep":"me","mcpServers":{}}`)
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

func TestCopilotCLI_GetEntry_RoundTrip(t *testing.T) {
	c := newCopilotCLIForTest(t, `{"mcpServers":{}}`)
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

func TestCopilotCLI_RemoveEntry_Idempotent(t *testing.T) {
	c := newCopilotCLIForTest(t, `{"mcpServers":{"serena":{"type":"http","url":"http://localhost:9121/mcp","tools":["*"]}}}`)
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

func TestCopilotCLI_InitEmpty(t *testing.T) {
	// Build a client whose config file does not yet exist (but parent does).
	c := newCopilotCLIForTest(t, "")
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

func TestCopilotCLI_Exists_DirHeuristic(t *testing.T) {
	// Parent dir present, file absent -> Exists() true (installed-but-no-config).
	c := newCopilotCLIForTest(t, "")
	if !c.Exists() {
		t.Error("Exists() = false when ~/.copilot dir present, want true")
	}
}

// TestCopilotCLI_BackupKeep_FreshHost verifies BackupKeep creates the parent
// dir and seeds a stub on a clean host where neither the config nor the
// .copilot directory exist yet.
func TestCopilotCLI_BackupKeep_FreshHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".copilot", "mcp-config.json")
	c := &copilotCLIClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "copilot-cli",
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

// TestCopilotCLI_BackupEntryIsHubManaged confirms the inherited demigrate
// predicate classifies a hub loopback url entry (urlField "url", no command)
// as hub-managed even with the extra `type`/`tools` fields present, and a
// user-configured remote entry as not hub-managed.
func TestCopilotCLI_BackupEntryIsHubManaged(t *testing.T) {
	dir := t.TempDir()
	hubBackup := filepath.Join(dir, "hub.json")
	if err := os.WriteFile(hubBackup, []byte(`{"mcpServers":{"serena":{"type":"http","url":"http://localhost:9121/mcp","tools":["*"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	userBackup := filepath.Join(dir, "user.json")
	if err := os.WriteFile(userBackup, []byte(`{"mcpServers":{"serena":{"type":"http","url":"https://remote.example.com/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := newCopilotCLIForTest(t, `{"mcpServers":{}}`)

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

// TestCopilotCLI_DefaultConfigPath_HonorsCopilotHome verifies path resolution:
// COPILOT_HOME, when set, replaces the ~/.copilot directory; when unset the
// path falls back to ~/.copilot/mcp-config.json under the home dir.
func TestCopilotCLI_DefaultConfigPath_HonorsCopilotHome(t *testing.T) {
	t.Run("COPILOT_HOME set", func(t *testing.T) {
		custom := t.TempDir()
		t.Setenv("COPILOT_HOME", custom)
		got := defaultCopilotCLIConfigPath()
		want := filepath.Join(custom, "mcp-config.json")
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	})
	t.Run("COPILOT_HOME unset falls back to home/.copilot", func(t *testing.T) {
		t.Setenv("COPILOT_HOME", "")
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("no home dir on host: %v", err)
		}
		got := defaultCopilotCLIConfigPath()
		want := filepath.Join(home, ".copilot", "mcp-config.json")
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	})
}

// TestCopilotCLI_NewCopilotCLI_HonorsCopilotHome verifies the public
// constructor threads COPILOT_HOME through to the live adapter's ConfigPath.
func TestCopilotCLI_NewCopilotCLI_HonorsCopilotHome(t *testing.T) {
	custom := t.TempDir()
	t.Setenv("COPILOT_HOME", custom)
	c, err := NewCopilotCLI()
	if err != nil {
		t.Fatalf("NewCopilotCLI: %v", err)
	}
	want := filepath.Join(custom, "mcp-config.json")
	if got := c.ConfigPath(); got != want {
		t.Errorf("ConfigPath() = %q, want %q", got, want)
	}
	if c.Name() != "copilot-cli" {
		t.Errorf("Name() = %q, want copilot-cli", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false")
	}
}
