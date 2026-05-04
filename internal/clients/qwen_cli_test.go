package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newQwenForTest(t *testing.T, initial string) *qwenCLI {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &qwenCLI{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "qwen-cli",
		urlField:   "httpUrl",
	}}
}

func TestQwenCLI_AddEntry_WritesHTTPURLSchema(t *testing.T) {
	q := newQwenForTest(t, `{"other":"keep-me"}`)
	if err := q.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(q.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers := parsed["mcpServers"].(map[string]any)
	serena := servers["serena"].(map[string]any)
	if serena["httpUrl"] != "http://localhost:9121/mcp" {
		t.Errorf("httpUrl = %v, want hub URL", serena["httpUrl"])
	}
	if tm, _ := serena["timeout"].(float64); tm != float64(defaultQwenHTTPTimeoutMs) {
		t.Errorf("timeout = %v, want %d", serena["timeout"], defaultQwenHTTPTimeoutMs)
	}
	if parsed["other"] != "keep-me" {
		t.Error("other top-level field dropped")
	}
}
