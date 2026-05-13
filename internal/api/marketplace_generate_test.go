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

// TestGenerateDraftManifest_NonSensitiveUnsetEnvDoesNotPanic pins
// codex r6 P1 closure (PR #163): a non-sensitive ${env:VAR} that
// resolves to empty must NOT panic with "assignment to entry in nil
// map". PlaceholderExpander.UndefinedEnv must be non-nil before the
// expander is used.
func TestGenerateDraftManifest_NonSensitiveUnsetEnvDoesNotPanic(t *testing.T) {
	// VAR is non-sensitive (no _TOKEN/_SECRET/etc suffix) and unset
	// in the test process. Pre-fix this would crash; post-fix it
	// returns a draft with an empty value plus an "expanded to
	// empty" warning.
	const unsetVar = "MCPHUB_TEST_UNSET_NON_SENSITIVE_VAR_PR163"
	t.Setenv(unsetVar, "") // explicit empty for determinism
	e := &MarketplaceEntry{
		ID:        "with-unset-env",
		Name:      "Unset",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"--something", "${env:" + unsetVar + "}"},
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GenerateDraftManifest panicked on unset non-sensitive env: %v", r)
		}
	}()
	got, warns, err := GenerateDraftManifest(e, GenerateOpts{})
	if err != nil {
		t.Fatalf("GenerateDraftManifest: %v", err)
	}
	if !strings.Contains(got, "with-unset-env") {
		t.Errorf("draft missing entry id name")
	}
	if len(warns) == 0 {
		t.Errorf("expected an empty-resolution warning for unset non-sensitive env; got none")
	} else {
		joined := strings.Join(warns, "\n")
		if !strings.Contains(joined, unsetVar) {
			t.Errorf("warnings missing unset var name: %s", joined)
		}
		if !strings.Contains(joined, "empty") {
			t.Errorf("warnings should describe empty-resolution: %s", joined)
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
