package api

import (
	"strings"
	"testing"
)

// TestGenerateDraftManifest_StdioEntryMapsToStdioBridge pins codex
// r1 P1 closure: stdio entries must map to TransportStdioBridge,
// NOT native-http.
func TestGenerateDraftManifest_StdioEntryMapsToStdioBridge(t *testing.T) {
	e := &MarketplaceEntry{
		ID:        "filesystem",
		Name:      "Filesystem",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-filesystem", "${workspaceFolder}"},
		Env:       map[string]string{"LOG_LEVEL": "info"},
	}
	got, warns, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: "/path/to/ws"})
	if err != nil {
		t.Fatalf("GenerateDraftManifest: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	for _, want := range []string{
		"name: filesystem",
		"kind: global",
		"transport: stdio-bridge", // not native-http
		"command: npx",
		"/path/to/ws",
		"LOG_LEVEL: info",
		"port: 0", // operator must pick a real port before save
	} {
		if !strings.Contains(got, want) {
			t.Errorf("draft YAML missing %q\n---\n%s", want, got)
		}
	}
}

// TestGenerateDraftManifest_HttpEntryRefusedWithG6Workaround pins
// codex r1 P2 closure: G6 deferral message names today's workaround.
func TestGenerateDraftManifest_HttpEntryRefusedWithG6Workaround(t *testing.T) {
	e := &MarketplaceEntry{
		ID:        "ctx7",
		Name:      "Context7",
		Transport: "http",
		URL:       "https://mcp.context7.com/mcp",
	}
	_, _, err := GenerateDraftManifest(e, GenerateOpts{})
	if err == nil {
		t.Fatal("expected G6-deferral error for http entry; got nil")
	}
	for _, want := range []string{"G6", "wait", "workaround"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("error must mention %q for operator clarity; got %q", want, err.Error())
		}
	}
}

// TestGenerateDraftManifest_SensitiveEnvLeftVerbatim pins codex r1
// P1 closure: catalog-controlled ${env:NAME} matching the sensitive
// allowlist is left as literal ${env:NAME} in the draft + a warning
// is returned. Operator must consciously redact/replace.
func TestGenerateDraftManifest_SensitiveEnvLeftVerbatim(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "should-not-leak-into-yaml")
	t.Setenv("LOG_LEVEL", "debug")
	e := &MarketplaceEntry{
		ID:        "bad-actor",
		Name:      "bad",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"--key", "${env:AWS_SECRET_ACCESS_KEY}", "--log", "${env:LOG_LEVEL}"},
	}
	got, warns, err := GenerateDraftManifest(e, GenerateOpts{})
	if err != nil {
		t.Fatalf("GenerateDraftManifest: %v", err)
	}
	if strings.Contains(got, "should-not-leak-into-yaml") {
		t.Errorf("sensitive value leaked into draft:\n---\n%s", got)
	}
	if !strings.Contains(got, "${env:AWS_SECRET_ACCESS_KEY}") {
		t.Errorf("sensitive placeholder not preserved verbatim:\n---\n%s", got)
	}
	if !strings.Contains(got, "debug") {
		t.Errorf("non-sensitive placeholder failed to expand:\n---\n%s", got)
	}
	if len(warns) == 0 {
		t.Errorf("expected at least one warning about sensitive env")
	} else {
		joined := strings.Join(warns, "\n")
		if !strings.Contains(joined, "AWS_SECRET_ACCESS_KEY") {
			t.Errorf("warnings missing sensitive name: %s", joined)
		}
	}
}
