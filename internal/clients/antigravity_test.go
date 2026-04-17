package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newAntigravityForTest(t *testing.T, initial string) *antigravityClient {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	return &antigravityClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "antigravity",
		urlField:   "url",
	}}
}

// TestAntigravity_AddEntry_WritesHTTPSchemaWithDisabled verifies the HTTP
// entry carries url + type + timeout (matching Gemini CLI 0.38+'s HTTP
// schema) plus Antigravity's traditional `disabled` flag.
func TestAntigravity_AddEntry_WritesHTTPSchemaWithDisabled(t *testing.T) {
	a := newAntigravityForTest(t, `{"mcpServers":{"keep":{"command":"x"}}}`)
	if err := a.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	raw, _ := os.ReadFile(a.path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers := parsed["mcpServers"].(map[string]any)
	serena, ok := servers["serena"].(map[string]any)
	if !ok {
		t.Fatalf("serena entry missing: %v", servers)
	}
	if serena["url"] != "http://localhost:9121/mcp" {
		t.Errorf("url = %v, want http://localhost:9121/mcp", serena["url"])
	}
	if serena["type"] != "http" {
		t.Errorf("type = %v, want \"http\"", serena["type"])
	}
	if tm, _ := serena["timeout"].(float64); tm != float64(defaultGeminiHTTPTimeoutMs) {
		t.Errorf("timeout = %v, want %d", serena["timeout"], defaultGeminiHTTPTimeoutMs)
	}
	if d, _ := serena["disabled"].(bool); d != false {
		t.Errorf("disabled = %v, want false", serena["disabled"])
	}
	// Must NOT write legacy httpUrl.
	if _, hasHttpUrl := serena["httpUrl"]; hasHttpUrl {
		t.Errorf("legacy httpUrl must not be present: %v", serena)
	}
	// Unrelated entry preserved.
	if _, hasKeep := servers["keep"]; !hasKeep {
		t.Error("unrelated 'keep' entry dropped")
	}
}

func TestAntigravity_GetEntry_ReadsUrlField(t *testing.T) {
	a := newAntigravityForTest(t, `{"mcpServers":{"serena":{"url":"http://localhost:9121/mcp","type":"http","timeout":10000,"disabled":false}}}`)
	e, err := a.GetEntry("serena")
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if e == nil {
		t.Fatal("GetEntry returned nil")
	}
	if e.URL != "http://localhost:9121/mcp" {
		t.Errorf("URL = %q", e.URL)
	}
}

func TestAntigravity_RemoveEntry_Inherited(t *testing.T) {
	a := newAntigravityForTest(t, `{"mcpServers":{"serena":{"url":"x","type":"http"},"other":{"url":"y"}}}`)
	if err := a.RemoveEntry("serena"); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if e, _ := a.GetEntry("serena"); e != nil {
		t.Errorf("serena still present: %v", e)
	}
	if e, _ := a.GetEntry("other"); e == nil {
		t.Error("other entry should still be present")
	}
}
