package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newCommandCodeForTest(t *testing.T, initial string) *commandCodeClient {
	t.Helper()
	dir := t.TempDir()
	ccDir := filepath.Join(dir, ".commandcode")
	if err := os.MkdirAll(ccDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ccDir, "mcp.json")
	if initial != "" {
		if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return &commandCodeClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "command-code",
		urlField:   "url",
	}}
}

func TestCommandCode_Name(t *testing.T) {
	c := newCommandCodeForTest(t, `{"mcpServers":{}}`)
	if got := c.Name(); got != "command-code" {
		t.Errorf("Name() = %q, want command-code", got)
	}
}

func TestCommandCode_IsRelayStdio(t *testing.T) {
	c := newCommandCodeForTest(t, `{"mcpServers":{}}`)
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false (command-code is URL-native HTTP)")
	}
}

// TestCommandCode_AddEntry_WritesHTTPShape verifies the documented
// `{"type":"http","url":...}` shape (matching `cmd mcp add-json`), no
// `disabled` flag, plus `headers` when provided. Siblings/top-level fields
// survive.
func TestCommandCode_AddEntry_WritesHTTPShape(t *testing.T) {
	tests := []struct {
		name    string
		initial string
		entry   MCPEntry
	}{
		{
			name:    "into existing mcpServers preserves siblings",
			initial: `{"other":"keep-me","mcpServers":{"existing":{"type":"http","url":"http://localhost:1/mcp"}}}`,
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
			c := newCommandCodeForTest(t, tc.initial)
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
			if _, has := got["disabled"]; has {
				t.Errorf("unexpected 'disabled' flag for Command Code http entry: %v", got)
			}
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

func TestCommandCode_GetEntry_RoundTrip(t *testing.T) {
	c := newCommandCodeForTest(t, `{"mcpServers":{}}`)
	want := MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp", Headers: map[string]string{"X-Auth": "tok"}}
	if err := c.AddEntry(want); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	got, err := c.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != want.URL {
		t.Fatalf("GetEntry = %v, want URL %q", got, want.URL)
	}
	if got.Headers["X-Auth"] != "tok" {
		t.Errorf("Headers = %v, want X-Auth:tok", got.Headers)
	}
	if missing, err := c.GetEntry("nope"); err != nil || missing != nil {
		t.Errorf("GetEntry(missing) = %v, %v; want nil, nil", missing, err)
	}
}

func TestCommandCode_RemoveEntry_Idempotent(t *testing.T) {
	c := newCommandCodeForTest(t, `{"mcpServers":{"serena":{"type":"http","url":"http://localhost:9121/mcp"}}}`)
	if err := c.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if got, _ := c.GetEntry("serena"); got != nil {
		t.Errorf("entry still present after RemoveEntry: %v", got)
	}
	if err := c.RemoveEntry("serena"); err != nil {
		t.Errorf("second RemoveEntry not idempotent: %v", err)
	}
}

func TestCommandCode_InitEmpty(t *testing.T) {
	c := newCommandCodeForTest(t, "")
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
	if created2, err := c.InitEmpty(); err != nil || created2 {
		t.Errorf("second InitEmpty = %v, %v; want false, nil (idempotent)", created2, err)
	}
}

func TestCommandCode_Exists_DirHeuristic(t *testing.T) {
	c := newCommandCodeForTest(t, "")
	if !c.Exists() {
		t.Error("Exists() = false when ~/.commandcode dir present, want true")
	}
}

func TestCommandCode_BackupKeep_FreshHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".commandcode", "mcp.json")
	c := &commandCodeClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "command-code",
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

func TestCommandCode_DefaultConfigPath(t *testing.T) {
	got := defaultCommandCodeConfigPath(filepath.Join("home", "u"))
	want := filepath.Join("home", "u", ".commandcode", "mcp.json")
	if got != want {
		t.Errorf("defaultCommandCodeConfigPath = %q, want %q", got, want)
	}
	c, err := NewCommandCode()
	if err != nil {
		t.Fatalf("NewCommandCode: %v", err)
	}
	if c.Name() != "command-code" {
		t.Errorf("Name() = %q, want command-code", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false")
	}
}
