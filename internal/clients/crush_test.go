package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newCrushForTest(t *testing.T, initial string) *crushClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "crush.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &crushClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "crush",
		serversKey: "mcp",
		urlField:   "url",
	}}
}

// TestCrush_AddEntry_UsesMcpKey verifies AddEntry writes {type:"http",url} under
// the NON-standard top-level `mcp` key (NOT mcpServers) and preserves unrelated
// Crush config keys.
func TestCrush_AddEntry_UsesMcpKey(t *testing.T) {
	c := newCrushForTest(t, `{"options":{"keep":true}}`)
	if err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(c.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, hasStd := parsed["mcpServers"]; hasStd {
		t.Error("wrote standard mcpServers key — should use `mcp`")
	}
	servers, ok := parsed["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("`mcp` key missing: %v", parsed)
	}
	entry, ok := servers["serena"].(map[string]any)
	if !ok {
		t.Fatalf("serena entry missing: %v", servers)
	}
	if entry["type"] != "http" || entry["url"] != "http://localhost:9121/mcp" {
		t.Errorf("entry = %v, want type=http url=loopback", entry)
	}
	if opts, _ := parsed["options"].(map[string]any); opts["keep"] != true {
		t.Error("unrelated `options` key dropped")
	}
}

// TestCrush_RoundTrip confirms GetEntry/RemoveEntry operate on the `mcp` key.
func TestCrush_RoundTrip(t *testing.T) {
	c := newCrushForTest(t, `{"mcp":{}}`)
	if err := c.AddEntry(MCPEntry{Name: "memory", URL: "http://localhost:9001/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	e, err := c.GetEntry("memory")
	if err != nil || e == nil {
		t.Fatalf("GetEntry: %v / %v", e, err)
	}
	if e.URL != "http://localhost:9001/mcp" {
		t.Errorf("URL = %q", e.URL)
	}
	if err := c.RemoveEntry("memory"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := c.GetEntry("memory"); e != nil {
		t.Errorf("entry still present after Remove: %v", e)
	}
}

// TestCrush_InitEmpty_SeedsMcpKey verifies the parameterized stub uses `mcp`.
func TestCrush_InitEmpty_SeedsMcpKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crush.json")
	c := &crushClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "crush", serversKey: "mcp", urlField: "url"}}
	if _, err := c.InitEmpty(); err != nil {
		t.Fatalf("InitEmpty: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("seeded not valid JSON: %v", err)
	}
	if _, ok := m["mcp"].(map[string]any); !ok {
		t.Errorf("seeded stub missing `mcp` map: %v", m)
	}
}

// TestCrush_NameConfigPathRelay pins identity + non-relay classification.
func TestCrush_NameConfigPathRelay(t *testing.T) {
	c, err := NewCrush()
	if err != nil {
		t.Fatalf("NewCrush: %v", err)
	}
	if c.Name() != "crush" {
		t.Errorf("Name() = %q", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false (URL-native)")
	}
	if filepath.Base(c.ConfigPath()) != "crush.json" {
		t.Errorf("ConfigPath() base = %q, want crush.json", filepath.Base(c.ConfigPath()))
	}
}
