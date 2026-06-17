package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newPochiForTest(t *testing.T, initial string) *pochiClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &pochiClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "pochi",
		serversKey: "mcp",
		urlField:   "command",
	}}
}

// TestPochi_AddEntry_WritesRelayStdioUnderMcpKey verifies AddEntry emits the
// relay-stdio shape (command=exe, args=[relay,--server,--daemon]) under the
// `mcp` key and preserves unrelated keys.
func TestPochi_AddEntry_WritesRelayStdioUnderMcpKey(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "mcphub.exe") // guaranteed-absolute path
	p := newPochiForTest(t, `{"other":"keep","mcp":{}}`)
	err := p.AddEntry(MCPEntry{Name: "serena", RelayExePath: exe, RelayServer: "serena", RelayDaemon: "serena-default"})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(p.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed["other"] != "keep" {
		t.Error("unrelated key dropped")
	}
	if _, hasStd := parsed["mcpServers"]; hasStd {
		t.Error("wrote standard mcpServers — should use `mcp`")
	}
	servers, ok := parsed["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("`mcp` key missing: %v", parsed)
	}
	entry, ok := servers["serena"].(map[string]any)
	if !ok {
		t.Fatalf("serena entry missing: %v", servers)
	}
	if entry["command"] != exe {
		t.Errorf("command = %v, want %q", entry["command"], exe)
	}
	args, ok := entry["args"].([]any)
	if !ok || len(args) != 5 || args[0] != "relay" || args[1] != "--server" || args[3] != "--daemon" {
		t.Errorf("args = %v, want [relay --server serena --daemon serena-default]", entry["args"])
	}
}

func TestPochi_ExistsRequiresMCPSection(t *testing.T) {
	p := newPochiForTest(t, `{"owner":"other-app","settings":{}}`)
	if p.Exists() {
		t.Fatal("Exists() = true for unrelated ~/config.json without top-level mcp object")
	}

	p = newPochiForTest(t, `{"mcp":{}}`)
	if !p.Exists() {
		t.Fatal("Exists() = false for config with top-level mcp object")
	}
}

func TestPochi_RefusesToMutateUnrelatedConfig(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "mcphub.exe")
	p := newPochiForTest(t, `{"owner":"other-app","settings":{}}`)
	original, err := os.ReadFile(p.path)
	if err != nil {
		t.Fatal(err)
	}

	if err := p.AddEntry(MCPEntry{Name: "serena", RelayExePath: exe, RelayServer: "serena", RelayDaemon: "serena-default"}); err == nil {
		t.Fatal("AddEntry succeeded for unrelated config without top-level mcp object")
	}
	if _, err := p.BackupKeep(0); err == nil {
		t.Fatal("BackupKeep succeeded for unrelated config without top-level mcp object")
	}
	after, err := os.ReadFile(p.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("unrelated config mutated: got %s, want %s", after, original)
	}
}

// TestPochi_AddEntry_RejectsURLOnly confirms a URL-only entry (no relay context)
// is refused — Pochi cannot consume a loopback-HTTP entry.
func TestPochi_AddEntry_RejectsURLOnly(t *testing.T) {
	p := newPochiForTest(t, `{"mcp":{}}`)
	if err := p.AddEntry(MCPEntry{Name: "x", URL: "http://localhost:9001/mcp"}); err == nil {
		t.Error("AddEntry(url-only) succeeded, want error (relay-stdio adapter requires RelayExePath)")
	}
}

// TestPochi_GetEntry_RoundTrip confirms GetEntry reconstructs the relay fields.
func TestPochi_GetEntry_RoundTrip(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "mcphub.exe")
	p := newPochiForTest(t, `{"mcp":{}}`)
	if err := p.AddEntry(MCPEntry{Name: "memory", RelayExePath: exe, RelayServer: "memory", RelayDaemon: "memory-d"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	e, err := p.GetEntry("memory")
	if err != nil || e == nil {
		t.Fatalf("GetEntry: %v / %v", e, err)
	}
	if e.RelayExePath != exe || e.RelayServer != "memory" || e.RelayDaemon != "memory-d" {
		t.Errorf("GetEntry = %+v, want exe/server/daemon round-tripped", e)
	}
	if err := p.RemoveEntry("memory"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := p.GetEntry("memory"); e != nil {
		t.Errorf("entry still present after Remove: %v", e)
	}
}

// TestPochi_NameConfigPathRelay pins identity + relay classification.
func TestPochi_NameConfigPathRelay(t *testing.T) {
	c, err := NewPochi()
	if err != nil {
		t.Fatalf("NewPochi: %v", err)
	}
	if c.Name() != "pochi" {
		t.Errorf("Name() = %q", c.Name())
	}
	if !c.IsRelayStdio() {
		t.Error("IsRelayStdio() = false, want true (stdio-only documented form)")
	}
	if filepath.Base(c.ConfigPath()) != "config.json" {
		t.Errorf("ConfigPath() base = %q, want config.json", filepath.Base(c.ConfigPath()))
	}
}
