package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newCursorForTest(t *testing.T, initial string) *cursorClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &cursorClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "cursor",
		urlField:   "url",
	}}
}

func TestCursor_AddEntry_WritesHTTPURLSchema(t *testing.T) {
	c := newCursorForTest(t, `{"other":"keep-me"}`)
	if err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(c.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers := parsed["mcpServers"].(map[string]any)
	serena := servers["serena"].(map[string]any)
	if serena["url"] != "http://localhost:9121/mcp" {
		t.Errorf("url = %v, want hub URL", serena["url"])
	}
	if serena["type"] != "http" {
		t.Errorf("type = %v, want http", serena["type"])
	}
	if parsed["other"] != "keep-me" {
		t.Error("other top-level field dropped")
	}
}
