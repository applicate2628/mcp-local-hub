package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newVSCodeForTest(t *testing.T, initial string) *vscodeClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &vscodeClient{path: path}
}

func TestVSCode_AddEntry_WritesServersHTTPURLSchema(t *testing.T) {
	v := newVSCodeForTest(t, `{"inputs":[],"other":"keep-me"}`)
	if err := v.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(v.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers := parsed["servers"].(map[string]any)
	serena := servers["serena"].(map[string]any)
	if serena["url"] != "http://localhost:9121/mcp" {
		t.Errorf("url = %v, want hub URL", serena["url"])
	}
	if serena["type"] != "http" {
		t.Errorf("type = %v, want http", serena["type"])
	}
	if _, ok := parsed["inputs"].([]any); !ok {
		t.Error("inputs array was dropped")
	}
	if parsed["other"] != "keep-me" {
		t.Error("other top-level field dropped")
	}
}
