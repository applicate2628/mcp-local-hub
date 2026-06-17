package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newNeovateForTest(t *testing.T, initial string) *neovateClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &neovateClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "neovate",
		urlField:   "url",
	}}
}

// TestNeovate_AddEntry_WritesHTTPShape verifies AddEntry emits {type:"http",url}
// under the standard top-level `mcpServers` key and preserves Neovate's other
// (hierarchical-config) top-level fields.
func TestNeovate_AddEntry_WritesHTTPShape(t *testing.T) {
	n := newNeovateForTest(t, `{"model":"keep-me"}`)
	if err := n.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp", Headers: map[string]string{"X-Token": "abc"}}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(n.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed["model"] != "keep-me" {
		t.Error("unrelated top-level field dropped")
	}
	servers, ok := parsed["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing: %v", parsed)
	}
	entry, ok := servers["serena"].(map[string]any)
	if !ok {
		t.Fatalf("serena entry missing: %v", servers)
	}
	if entry["type"] != "http" || entry["url"] != "http://localhost:9121/mcp" {
		t.Errorf("entry shape = %v, want type=http url=loopback", entry)
	}
	hdrs, ok := entry["headers"].(map[string]any)
	if !ok || hdrs["X-Token"] != "abc" {
		t.Errorf("headers = %v, want {X-Token: abc}", entry["headers"])
	}
}

// TestNeovate_RoundTrip confirms GetEntry reads back what AddEntry wrote and
// RemoveEntry deletes it.
func TestNeovate_RoundTrip(t *testing.T) {
	n := newNeovateForTest(t, `{"mcpServers":{}}`)
	if err := n.AddEntry(MCPEntry{Name: "memory", URL: "http://localhost:9001/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	e, err := n.GetEntry("memory")
	if err != nil || e == nil {
		t.Fatalf("GetEntry: %v / %v", e, err)
	}
	if e.URL != "http://localhost:9001/mcp" {
		t.Errorf("URL = %q", e.URL)
	}
	if err := n.RemoveEntry("memory"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := n.GetEntry("memory"); e != nil {
		t.Errorf("entry still present after Remove: %v", e)
	}
}

// TestNeovate_NameConfigPathRelay pins identity + non-relay classification.
func TestNeovate_NameConfigPathRelay(t *testing.T) {
	c, err := NewNeovate()
	if err != nil {
		t.Fatalf("NewNeovate: %v", err)
	}
	if c.Name() != "neovate" {
		t.Errorf("Name() = %q", c.Name())
	}
	if c.IsRelayStdio() {
		t.Error("IsRelayStdio() = true, want false (URL-native)")
	}
	if filepath.Base(c.ConfigPath()) != "config.json" || filepath.Base(filepath.Dir(c.ConfigPath())) != ".neovate" {
		t.Errorf("ConfigPath() = %q, want ~/.neovate/config.json", c.ConfigPath())
	}
}
