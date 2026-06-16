package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newPiForTest(t *testing.T, initial string) *piClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &piClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "pi",
		urlField:   "command",
	}}
}

// TestPi_IsRelayStdio asserts Pi is a relay-stdio adapter (Pi documents
// stdio command/args entries only, so the hub bridges via `mcphub relay`).
func TestPi_IsRelayStdio(t *testing.T) {
	c, err := NewPi()
	if err != nil {
		t.Fatalf("NewPi: %v", err)
	}
	if !c.IsRelayStdio() {
		t.Error("IsRelayStdio() = false, want true (Pi is stdio-only → relay)")
	}
}

// TestPi_AddEntry_WritesStdioRelayShape verifies Pi entries are written as
// stdio invocations of the local mcphub.exe relay subcommand (manifest-lookup
// form).
func TestPi_AddEntry_WritesStdioRelayShape(t *testing.T) {
	p := newPiForTest(t, `{"mcpServers":{"keep":{"command":"x"}}}`)
	exePath := filepath.Join(t.TempDir(), "mcphub.exe")
	err := p.AddEntry(MCPEntry{
		Name:         "serena",
		URL:          "http://localhost:9121/mcp", // ignored by adapter; relay args take over
		RelayServer:  "serena",
		RelayDaemon:  "claude",
		RelayExePath: exePath,
	})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(p.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers := parsed["mcpServers"].(map[string]any)
	serena, ok := servers["serena"].(map[string]any)
	if !ok {
		t.Fatalf("serena entry missing: %v", servers)
	}
	if cmd, _ := serena["command"].(string); cmd != exePath {
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
	// Must NOT write any HTTP shape fields — Pi only accepts stdio entries.
	for _, bad := range []string{"url", "serverUrl", "httpUrl", "type", "transport"} {
		if _, has := serena[bad]; has {
			t.Errorf("unexpected HTTP-shape field %q present in stdio-relay entry: %v", bad, serena)
		}
	}
	if _, ok := servers["keep"]; !ok {
		t.Error("unrelated 'keep' entry dropped")
	}
}

// TestPi_AddEntry_WritesURLRelayShape verifies the direct --url relay form
// (used by the serena dynamic-pool client-reconcile).
func TestPi_AddEntry_WritesURLRelayShape(t *testing.T) {
	p := newPiForTest(t, `{"mcpServers":{}}`)
	exePath := filepath.Join(t.TempDir(), "mcphub.exe")
	err := p.AddEntry(MCPEntry{
		Name:         "serena",
		RelayExePath: exePath,
		RelayURL:     "http://localhost:9121/serena/mcp",
	})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	e, err := p.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e == nil || e.RelayURL != "http://localhost:9121/serena/mcp" {
		t.Fatalf("RelayURL not round-tripped: %+v", e)
	}
}

// TestPi_AddEntry_RejectsMissingRelayFields ensures the adapter fails loudly
// when relay context is incomplete (a silent fallback to URL would produce
// entries Pi ignores).
func TestPi_AddEntry_RejectsMissingRelayFields(t *testing.T) {
	p := newPiForTest(t, `{"mcpServers":{}}`)
	cases := []struct {
		name string
		e    MCPEntry
	}{
		{"no relay server", MCPEntry{Name: "x", URL: "http://x", RelayDaemon: "d", RelayExePath: "/abs/path"}},
		{"no relay daemon", MCPEntry{Name: "x", URL: "http://x", RelayServer: "s", RelayExePath: "/abs/path"}},
		{"no exe path", MCPEntry{Name: "x", URL: "http://x", RelayServer: "s", RelayDaemon: "d"}},
		{"relative exe path", MCPEntry{Name: "x", URL: "http://x", RelayServer: "s", RelayDaemon: "d", RelayExePath: "mcphub"}},
	}
	for _, c := range cases {
		err := p.AddEntry(c.e)
		if err == nil {
			t.Errorf("case %q: expected error, got nil", c.name)
			continue
		}
		if !strings.Contains(err.Error(), "pi adapter requires") {
			t.Errorf("case %q: error should reference required fields: %v", c.name, err)
		}
	}
}

// TestPi_GetEntry_ReconstructsRelayArgs reads back a relay entry and exposes
// RelayServer/RelayDaemon/RelayExePath for diagnostics.
func TestPi_GetEntry_ReconstructsRelayArgs(t *testing.T) {
	p := newPiForTest(t, `{
  "mcpServers": {
    "serena": {
      "command": "/abs/mcphub",
      "args": ["relay", "--server", "serena", "--daemon", "claude"],
      "disabled": false
    }
  }
}`)
	e, err := p.GetEntry("serena")
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

func TestPi_RemoveEntry_Inherited(t *testing.T) {
	p := newPiForTest(t, `{"mcpServers":{"serena":{"command":"x","args":["relay"]},"other":{"command":"y"}}}`)
	if err := p.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := p.GetEntry("serena"); e != nil {
		t.Errorf("serena still present: %v", e)
	}
	if e, _ := p.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
}

func TestPi_Exists_DirBased(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".pi", "agent")
	path := filepath.Join(cfgDir, "mcp.json")
	p := &piClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "pi", urlField: "command"}}
	if p.Exists() {
		t.Error("Exists() = true before parent dir created, want false")
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !p.Exists() {
		t.Error("Exists() = false after parent dir created, want true")
	}
}

func TestPi_BackupKeep_SeedsFreshInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".pi", "agent", "mcp.json")
	p := &piClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "pi", urlField: "command"}}
	bak, err := p.BackupKeep(5)
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

func TestPi_NameAndConfigPath(t *testing.T) {
	c, err := NewPi()
	if err != nil {
		t.Fatalf("NewPi: %v", err)
	}
	if c.Name() != "pi" {
		t.Errorf("Name() = %q, want pi", c.Name())
	}
	got := c.ConfigPath()
	if filepath.Base(got) != "mcp.json" {
		t.Errorf("ConfigPath() base = %q, want mcp.json", filepath.Base(got))
	}
	if filepath.Base(filepath.Dir(got)) != "agent" {
		t.Errorf("ConfigPath() parent = %q, want agent (got %q)", filepath.Base(filepath.Dir(got)), got)
	}
	if filepath.Base(filepath.Dir(filepath.Dir(got))) != ".pi" {
		t.Errorf("ConfigPath() grandparent = %q, want .pi (got %q)", filepath.Base(filepath.Dir(filepath.Dir(got))), got)
	}
}
