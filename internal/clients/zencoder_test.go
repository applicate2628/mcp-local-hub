package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newZencoderForTest(t *testing.T, initial string) *zencoderClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &zencoderClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "zencoder",
		serversKey: "zencoder.mcpServers",
		urlField:   "command",
	}}
}

// TestZencoder_AddEntry_WritesFlatDottedKey is the make-or-break test: the
// dotted key `zencoder.mcpServers` must be written as a FLAT top-level string
// key (VS Code semantics), NOT split into a nested {zencoder:{mcpServers:{}}}
// object. It also confirms the relay-stdio shape and that unrelated editor
// settings survive.
func TestZencoder_AddEntry_WritesFlatDottedKey(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "mcphub.exe")
	z := newZencoderForTest(t, `{"editor.fontSize":14,"workbench.colorTheme":"Dark"}`)
	err := z.AddEntry(MCPEntry{Name: "serena", RelayExePath: exe, RelayServer: "serena", RelayDaemon: "serena-default"})
	if err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(z.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// FLAT dotted key — must NOT be nested.
	if _, nested := parsed["zencoder"]; nested {
		t.Errorf("dotted key was nested into {zencoder:...}: %v", parsed["zencoder"])
	}
	servers, ok := parsed["zencoder.mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("flat `zencoder.mcpServers` key missing: %v", parsed)
	}
	entry, ok := servers["serena"].(map[string]any)
	if !ok {
		t.Fatalf("serena entry missing: %v", servers)
	}
	if entry["command"] != exe {
		t.Errorf("command = %v, want %q", entry["command"], exe)
	}
	// Unrelated editor settings preserved.
	if parsed["editor.fontSize"] != float64(14) || parsed["workbench.colorTheme"] != "Dark" {
		t.Errorf("unrelated editor settings dropped: %v", parsed)
	}
}

// TestZencoder_GetEntry_RoundTrip confirms GetEntry reads back the relay fields
// from the flat dotted key.
func TestZencoder_GetEntry_RoundTrip(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "mcphub.exe")
	z := newZencoderForTest(t, `{}`)
	if err := z.AddEntry(MCPEntry{Name: "memory", RelayExePath: exe, RelayServer: "memory", RelayDaemon: "memory-d"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	e, err := z.GetEntry("memory")
	if err != nil || e == nil {
		t.Fatalf("GetEntry: %v / %v", e, err)
	}
	if e.RelayServer != "memory" || e.RelayDaemon != "memory-d" || e.RelayExePath != exe {
		t.Errorf("GetEntry = %+v, want relay fields round-tripped", e)
	}
	if err := z.RemoveEntry("memory"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := z.GetEntry("memory"); e != nil {
		t.Errorf("entry still present after Remove: %v", e)
	}
}

// TestZencoder_NameConfigPathRelay pins identity + relay classification.
func TestZencoder_NameConfigPathRelay(t *testing.T) {
	c, err := NewZencoder()
	if err != nil {
		t.Fatalf("NewZencoder: %v", err)
	}
	if c.Name() != "zencoder" {
		t.Errorf("Name() = %q", c.Name())
	}
	if !c.IsRelayStdio() {
		t.Error("IsRelayStdio() = false, want true (stdio-only documented hand-edit form)")
	}
	if filepath.Base(c.ConfigPath()) != "settings.json" {
		t.Errorf("ConfigPath() base = %q, want settings.json", filepath.Base(c.ConfigPath()))
	}
}
