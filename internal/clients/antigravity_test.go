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
		urlField:   "serverUrl",
	}}
}

// TestAntigravity_AddEntry_WritesServerUrlSchema verifies the HTTP entry
// uses Antigravity's specific schema: serverUrl + disabled (+ optional
// headers). No `url`, no `type:"http"`, no `timeout` — those are
// Gemini-CLI shapes that the Antigravity Cascade agent silently drops.
func TestAntigravity_AddEntry_WritesServerUrlSchema(t *testing.T) {
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
	if serena["serverUrl"] != "http://localhost:9121/mcp" {
		t.Errorf("serverUrl = %v, want http://localhost:9121/mcp", serena["serverUrl"])
	}
	if d, _ := serena["disabled"].(bool); d != false {
		t.Errorf("disabled = %v, want false", serena["disabled"])
	}
	// Regression guards: none of the Gemini-CLI-style fields may appear.
	for _, bad := range []string{"url", "type", "timeout", "httpUrl"} {
		if _, has := serena[bad]; has {
			t.Errorf("unexpected Gemini-style field %q in Antigravity entry: %v", bad, serena)
		}
	}
	if _, hasKeep := servers["keep"]; !hasKeep {
		t.Error("unrelated 'keep' entry dropped")
	}
}

func TestAntigravity_GetEntry_ReadsServerUrlField(t *testing.T) {
	a := newAntigravityForTest(t, `{"mcpServers":{"serena":{"serverUrl":"http://localhost:9121/mcp","disabled":false}}}`)
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
	a := newAntigravityForTest(t, `{"mcpServers":{"serena":{"serverUrl":"x"},"other":{"serverUrl":"y"}}}`)
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
