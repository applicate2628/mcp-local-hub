package api

import (
	"fmt"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
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
	// codex deep-sec PR #163 lane 2 P3 closure: use config constants
	// for transport names instead of string literals. A future rename
	// of config.TransportStdioBridge would otherwise silently leave
	// this test passing against the stale literal.
	for _, want := range []string{
		"name: filesystem",
		"kind: global",
		fmt.Sprintf("transport: %s", config.TransportStdioBridge),
		"command: npx",
		"/path/to/ws",
		"LOG_LEVEL: info",
		"port: 0", // operator must pick a real port before save
	} {
		if !strings.Contains(got, want) {
			t.Errorf("draft YAML missing %q\n---\n%s", want, got)
		}
	}
	// Negative-pin: the draft MUST NOT carry native-http transport.
	// codex r1 P1 closure regression guard.
	if strings.Contains(got, fmt.Sprintf("transport: %s", config.TransportNativeHTTP)) {
		t.Errorf("draft YAML contains native-http transport (codex r1 P1 regression):\n---\n%s", got)
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

// TestGenerateDraftManifest_WorkspaceTraversalSurfacesWarning pins
// codex deep-sec PR #163 lane 2 P2 closure: a catalog entry that
// places `..` after `${workspaceFolder}` resolves outside the
// workspace once expanded. The generator must surface this as a
// warning so the operator-edit gate isn't relied on alone.
func TestGenerateDraftManifest_WorkspaceTraversalSurfacesWarning(t *testing.T) {
	e := &MarketplaceEntry{
		ID:        "traversal-actor",
		Name:      "bad",
		Transport: "stdio",
		Command:   "npx",
		Args: []string{
			"--root", "${workspaceFolder}/../../etc/passwd",
			"--ok", "${workspaceFolder}/db.sqlite",
		},
	}
	_, warns, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: "/path/to/ws"})
	if err != nil {
		t.Fatalf("GenerateDraftManifest: %v", err)
	}
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, "../../etc/passwd") {
		t.Errorf("warnings missing traversal alert for the bad arg; warnings=%v", warns)
	}
	if strings.Contains(joined, "db.sqlite") {
		t.Errorf("warnings should NOT flag legitimate ${workspaceFolder}/db.sqlite; warnings=%v", warns)
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
