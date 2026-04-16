package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newJSONClientForTest(t *testing.T, initial string) *jsonMCPClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &jsonMCPClient{path: path, clientName: "test", urlField: "httpUrl"}
}

func TestJSONMCP_AddReplacesStdio(t *testing.T) {
	j := newJSONClientForTest(t, `{
  "mcpServers": {
    "serena": {
      "command": "uvx",
      "args": ["--from", "git+...", "serena", "start-mcp-server"],
      "disabled": false
    }
  }
}`)
	if err := j.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9123/mcp"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(j.path)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	servers := parsed["mcpServers"].(map[string]any)
	serena := servers["serena"].(map[string]any)
	if serena["httpUrl"] != "http://localhost:9123/mcp" {
		t.Errorf("httpUrl = %v, want http://localhost:9123/mcp", serena["httpUrl"])
	}
	if _, ok := serena["command"]; ok {
		t.Error("old command field not removed")
	}
}

func TestJSONMCP_RemoveEntry_Idempotent(t *testing.T) {
	j := newJSONClientForTest(t, `{"mcpServers":{}}`)
	if err := j.RemoveEntry("nonexistent"); err != nil {
		t.Errorf("remove of nonexistent should be nil, got %v", err)
	}
}
