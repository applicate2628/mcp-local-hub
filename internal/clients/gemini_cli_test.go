package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newGeminiForTest(t *testing.T, initial string) *geminiCLI {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &geminiCLI{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "gemini-cli",
		urlField:   "url",
	}}
}

// TestGeminiCLI_AddEntry_WritesModernHTTPSchema verifies that AddEntry emits
// the schema used by Gemini CLI 0.38+ (url + type:"http" + timeout),
// NOT the legacy httpUrl+disabled form.
func TestGeminiCLI_AddEntry_WritesModernHTTPSchema(t *testing.T) {
	g := newGeminiForTest(t, `{"other":"keep-me"}`)
	if err := g.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9123/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(g.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers, ok := parsed["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or wrong type: %v", parsed["mcpServers"])
	}
	serena, ok := servers["serena"].(map[string]any)
	if !ok {
		t.Fatalf("serena entry missing: %v", servers)
	}
	if serena["url"] != "http://localhost:9123/mcp" {
		t.Errorf("url = %v, want http://localhost:9123/mcp", serena["url"])
	}
	if serena["type"] != "http" {
		t.Errorf("type = %v, want \"http\" (required by Gemini CLI 0.38+ HTTP schema)", serena["type"])
	}
	// timeout is emitted as a JSON number; after unmarshal into any it's a float64.
	if tm, _ := serena["timeout"].(float64); tm != float64(defaultGeminiHTTPTimeoutMs) {
		t.Errorf("timeout = %v, want %d", serena["timeout"], defaultGeminiHTTPTimeoutMs)
	}
	// Must NOT write legacy fields.
	if _, hasHttpUrl := serena["httpUrl"]; hasHttpUrl {
		t.Errorf("legacy httpUrl field should NOT be present: %v", serena)
	}
	if _, hasDisabled := serena["disabled"]; hasDisabled {
		t.Errorf("legacy disabled field should NOT be present: %v", serena)
	}
	// Unrelated top-level field must survive round-trip.
	if parsed["other"] != "keep-me" {
		t.Error("other top-level field dropped")
	}
}

// TestGeminiCLI_GetEntry_ReadsUrlField confirms GetEntry reads the new `url`
// field (not `httpUrl`).
func TestGeminiCLI_GetEntry_ReadsUrlField(t *testing.T) {
	g := newGeminiForTest(t, `{
  "mcpServers": {
    "serena": {
      "url": "http://localhost:9123/mcp",
      "type": "http",
      "timeout": 10000
    }
  }
}`)
	e, err := g.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e == nil {
		t.Fatal("GetEntry returned nil")
	}
	if e.URL != "http://localhost:9123/mcp" {
		t.Errorf("URL = %q, want http://localhost:9123/mcp", e.URL)
	}
}

// TestGeminiCLI_RemoveEntry_Inherited confirms RemoveEntry (promoted from
// jsonMCPClient) still works through the embedded struct.
func TestGeminiCLI_RemoveEntry_Inherited(t *testing.T) {
	g := newGeminiForTest(t, `{"mcpServers":{"serena":{"url":"http://x","type":"http"},"other":{"url":"http://y","type":"http"}}}`)
	if err := g.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := g.GetEntry("serena"); e != nil {
		t.Errorf("serena still present after Remove: %v", e)
	}
	if e, _ := g.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
}
