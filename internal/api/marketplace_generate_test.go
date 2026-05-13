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

// TestGenerateDraftManifest_HttpEntryEmitsRemoteHTTPDraft pins G6
// sub-PR 4: http catalog entries now project onto a
// transport=remote-http manifest with the entry URL preserved. The
// draft must NOT include daemons: (schema rejects daemons on
// remote-http) and SHOULD include client_bindings prefilled with the
// adapters that support remote-http per the compatibility matrix.
func TestGenerateDraftManifest_HttpEntryEmitsRemoteHTTPDraft(t *testing.T) {
	e := &MarketplaceEntry{
		ID:        "ctx7",
		Name:      "Context7",
		Transport: "http",
		URL:       "https://mcp.context7.com/mcp",
	}
	yaml, warnings, err := GenerateDraftManifest(e, GenerateOpts{})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(warnings) > 0 {
		t.Errorf("no warnings expected for clean http entry; got %v", warnings)
	}
	for _, want := range []string{
		"transport: remote-http",
		"url: https://mcp.context7.com/mcp",
		"name: ctx7",
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("yaml missing %q\n---\n%s", want, yaml)
		}
	}
	if strings.Contains(yaml, "daemons:") {
		t.Errorf("yaml must not include daemons: for remote-http\n---\n%s", yaml)
	}
	if !strings.Contains(yaml, "client_bindings:") {
		t.Errorf("yaml missing client_bindings:\n---\n%s", yaml)
	}
	for _, c := range []string{"claude-code", "codex-cli", "cursor", "gemini-cli", "vscode"} {
		if !strings.Contains(yaml, "client: "+c) {
			t.Errorf("yaml missing prefilled binding for %s\n---\n%s", c, yaml)
		}
	}
	// Header reminder so operators see the secret-handling rule.
	if !strings.Contains(yaml, "${secret:KEY}") {
		t.Errorf("yaml header missing secret-placeholder reminder\n---\n%s", yaml)
	}
	// Smoke-check guidance in header.
	if !strings.Contains(yaml, "manifest test-remote") {
		t.Errorf("yaml header missing test-remote smoke-check pointer\n---\n%s", yaml)
	}
}

func TestGenerateDraftManifest_HttpEntryEmptyURLRejected(t *testing.T) {
	e := &MarketplaceEntry{
		ID:        "bad",
		Transport: "http",
	}
	_, _, err := GenerateDraftManifest(e, GenerateOpts{})
	if err == nil || !strings.Contains(err.Error(), "url is empty") {
		t.Errorf("expected url-empty rejection, got %v", err)
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
