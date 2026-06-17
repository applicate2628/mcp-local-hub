package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newAmpForTest(t *testing.T, initial string) *ampClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &ampClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "amp",
		serversKey: "amp.mcpServers",
		urlField:   "url",
	}}
}

// TestAmp_AddEntry_WritesFlatDottedKey is the make-or-break test: `amp.mcpServers`
// must be a FLAT top-level key (VS Code semantics), not nested under {amp:{}}.
// It also confirms the minimal {url} shape (no disabled/type keys Amp's schema
// omits) and that unrelated editor settings survive.
func TestAmp_AddEntry_WritesFlatDottedKey(t *testing.T) {
	a := newAmpForTest(t, `{"editor.tabSize":2}`)
	if err := a.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp", Headers: map[string]string{"X-Token": "abc"}}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(a.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, nested := parsed["amp"]; nested {
		t.Errorf("dotted key was nested into {amp:...}: %v", parsed["amp"])
	}
	servers, ok := parsed["amp.mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("flat `amp.mcpServers` key missing: %v", parsed)
	}
	entry, ok := servers["serena"].(map[string]any)
	if !ok {
		t.Fatalf("serena entry missing: %v", servers)
	}
	if entry["url"] != "http://localhost:9121/mcp" {
		t.Errorf("url = %v", entry["url"])
	}
	// Amp's minimal shape: no `disabled`, no `type`.
	if _, has := entry["disabled"]; has {
		t.Errorf("entry should not carry `disabled` (Amp schema omits it): %v", entry)
	}
	hdrs, ok := entry["headers"].(map[string]any)
	if !ok || hdrs["X-Token"] != "abc" {
		t.Errorf("headers = %v, want {X-Token: abc}", entry["headers"])
	}
	if parsed["editor.tabSize"] != float64(2) {
		t.Error("unrelated editor setting dropped")
	}
}

// TestAmp_RoundTrip confirms GetEntry/RemoveEntry operate on the flat dotted key.
func TestAmp_RoundTrip(t *testing.T) {
	a := newAmpForTest(t, `{}`)
	if err := a.AddEntry(MCPEntry{Name: "memory", URL: "http://localhost:9001/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	e, err := a.GetEntry("memory")
	if err != nil || e == nil {
		t.Fatalf("GetEntry: %v / %v", e, err)
	}
	if e.URL != "http://localhost:9001/mcp" {
		t.Errorf("URL = %q", e.URL)
	}
	if err := a.RemoveEntry("memory"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := a.GetEntry("memory"); e != nil {
		t.Errorf("entry still present after Remove: %v", e)
	}
}

// TestAmp_NameConfigPathRelay pins identity + non-relay classification + the
// settings.json target.
func TestAmp_NameConfigPathRelay(t *testing.T) {
	c, err := NewAmp()
	if err != nil {
		t.Fatalf("NewAmp: %v", err)
	}
	if c.Name() != "amp" {
		t.Errorf("Name() = %q", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false (URL-native)")
	}
	if filepath.Base(c.ConfigPath()) != "settings.json" {
		t.Errorf("ConfigPath() base = %q, want settings.json", filepath.Base(c.ConfigPath()))
	}
}
