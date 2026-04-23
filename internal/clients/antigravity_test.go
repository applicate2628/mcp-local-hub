package clients

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newAntigravityForTest(t *testing.T, initial string) *antigravityClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &antigravityClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "antigravity",
		urlField:   "command",
	}}
}

// TestAntigravity_AddEntry_WritesStdioRelayShape verifies that Antigravity
// entries are written as stdio invocations of the local mcphub.exe relay
// subcommand (Cascade silently drops loopback-HTTP entries regardless of
// schema — stdio relay is the only working path as of v0.x).
func TestAntigravity_AddEntry_WritesStdioRelayShape(t *testing.T) {
	a := newAntigravityForTest(t, `{"mcpServers":{"keep":{"command":"x"}}}`)
	err := a.AddEntry(MCPEntry{
		Name:         "serena",
		URL:          "http://localhost:9121/mcp", // ignored by adapter; relay args take over
		RelayServer:  "serena",
		RelayDaemon:  "claude",
		RelayExePath: `D:\dev\mcp-local-hub\mcphub.exe`,
	})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(a.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers := parsed["mcpServers"].(map[string]any)
	serena, ok := servers["serena"].(map[string]any)
	if !ok {
		t.Fatalf("serena entry missing: %v", servers)
	}
	if cmd, _ := serena["command"].(string); cmd != `D:\dev\mcp-local-hub\mcphub.exe` {
		t.Errorf("command = %q, want absolute mcphub.exe path", cmd)
	}
	argsAny, ok := serena["args"].([]any)
	if !ok || len(argsAny) != 5 {
		t.Fatalf("args must be 5-element array [relay, --server, <s>, --daemon, <d>], got %v", serena["args"])
	}
	want := []string{"relay", "--server", "serena", "--daemon", "claude"}
	for i, v := range want {
		got, _ := argsAny[i].(string)
		if got != v {
			t.Errorf("args[%d] = %q, want %q", i, got, v)
		}
	}
	if d, _ := serena["disabled"].(bool); d != false {
		t.Errorf("disabled = %v, want false", d)
	}
	// Must NOT write any HTTP shape fields — Cascade drops those.
	for _, bad := range []string{"url", "serverUrl", "httpUrl", "type", "timeout"} {
		if _, has := serena[bad]; has {
			t.Errorf("unexpected HTTP-shape field %q present in stdio-relay entry: %v", bad, serena)
		}
	}
	// Unrelated entry preserved.
	if _, ok := servers["keep"]; !ok {
		t.Error("unrelated 'keep' entry dropped")
	}
}

// TestAntigravity_AddEntry_RejectsMissingRelayFields ensures the adapter
// fails loudly when install.go forgets to populate the relay identifiers.
// A silent fallback to URL would produce entries Cascade ignores.
func TestAntigravity_AddEntry_RejectsMissingRelayFields(t *testing.T) {
	a := newAntigravityForTest(t, `{"mcpServers":{}}`)
	cases := []struct {
		name string
		e    MCPEntry
	}{
		{"no relay server", MCPEntry{Name: "x", URL: "http://x", RelayDaemon: "d", RelayExePath: "path"}},
		{"no relay daemon", MCPEntry{Name: "x", URL: "http://x", RelayServer: "s", RelayExePath: "path"}},
		{"no exe path", MCPEntry{Name: "x", URL: "http://x", RelayServer: "s", RelayDaemon: "d"}},
	}
	for _, c := range cases {
		err := a.AddEntry(c.e)
		if err == nil {
			t.Errorf("case %q: expected error, got nil", c.name)
			continue
		}
		if !strings.Contains(err.Error(), "antigravity adapter requires") {
			t.Errorf("case %q: error should reference required fields: %v", c.name, err)
		}
	}
}

// TestAntigravity_GetEntry_ReconstructsRelayArgs reads back an entry
// the adapter itself wrote and exposes RelayServer/RelayDaemon/RelayExePath
// for diagnostics (e.g. verifying install idempotency).
func TestAntigravity_GetEntry_ReconstructsRelayArgs(t *testing.T) {
	a := newAntigravityForTest(t, `{
  "mcpServers": {
    "serena": {
      "command": "D:\\dev\\mcp-local-hub\\mcphub.exe",
      "args": ["relay", "--server", "serena", "--daemon", "claude"],
      "disabled": false
    }
  }
}`)
	e, err := a.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e == nil {
		t.Fatal("GetEntry returned nil")
	}
	if e.RelayServer != "serena" {
		t.Errorf("RelayServer = %q", e.RelayServer)
	}
	if e.RelayDaemon != "claude" {
		t.Errorf("RelayDaemon = %q", e.RelayDaemon)
	}
	if e.RelayExePath == "" {
		t.Error("RelayExePath should be populated from 'command' field")
	}
}

// TestAntigravity_RemoveEntry_Inherited confirms the inherited RemoveEntry
// still works even with the new stdio-relay entry shape.
func TestAntigravity_RemoveEntry_Inherited(t *testing.T) {
	a := newAntigravityForTest(t, `{"mcpServers":{"serena":{"command":"x","args":["relay"]},"other":{"command":"y"}}}`)
	if err := a.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := a.GetEntry("serena"); e != nil {
		t.Errorf("serena still present: %v", e)
	}
	if e, _ := a.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
}

func TestAntigravity_RestoreEntryFromBackup_RestoresOrRemovesPerBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	// Live has a relay-stdio entry written by mcphub migrate.
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"serena":{"command":"C:/mcphub.exe","args":["relay","--server","serena","--daemon","claude"],"disabled":false}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	// Backup predates the install — no serena entry.
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := &antigravityClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "antigravity", urlField: "command"}}
	if err := a.RestoreEntryFromBackup(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackup: %v", err)
	}
	live, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(live, &m); err != nil {
		t.Fatal(err)
	}
	servers := m["mcpServers"].(map[string]any)
	if _, present := servers["serena"]; present {
		t.Error("serena should have been removed")
	}
}

func TestAntigravity_LatestBackupPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := &antigravityClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "antigravity", urlField: "command"}}
	got, ok, err := a.LatestBackupPath()
	if err != nil || !ok || got != backup {
		t.Errorf("LatestBackupPath = %q ok=%v err=%v", got, ok, err)
	}
}

func TestAntigravity_RestoreEntryFromBackup_RefusesHubRelayBackupEntry(t *testing.T) {
	// Antigravity's hub-managed form is a RELAY entry: command points
	// at the mcphub binary and args[0] == "relay". Refuse restoring
	// from a backup that already contains a relay-shaped entry.
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	if err := os.WriteFile(path, []byte(
		`{"mcpServers":{"serena":{"command":"C:/mcphub.exe","args":["relay","--server","serena","--daemon","claude"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(
		`{"mcpServers":{"serena":{"command":"C:/mcphub.exe","args":["relay","--server","serena","--daemon","claude"]}}}`),
		0600); err != nil {
		t.Fatal(err)
	}
	a := &antigravityClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "antigravity", urlField: "command"}}
	err := a.RestoreEntryFromBackup(backup, "serena")
	if !errors.Is(err, ErrBackupEntryAlreadyMigrated) {
		t.Fatalf("expected ErrBackupEntryAlreadyMigrated, got %v", err)
	}
}
