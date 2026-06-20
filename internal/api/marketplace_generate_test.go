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

func TestGenerateDraftManifest_NativeHTTPEntryMapsToNativeHTTP(t *testing.T) {
	e := &MarketplaceEntry{
		ID:        "serena",
		Name:      "Serena",
		Transport: "native-http",
		Command:   "uvx",
		Args:      []string{"serena", "start-mcp-server", "--transport", "streamable-http"},
		Env:       map[string]string{"PYTHONUNBUFFERED": "1"},
	}
	got, warns, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: "/path/to/ws"})
	if err != nil {
		t.Fatalf("GenerateDraftManifest: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	for _, want := range []string{
		"name: serena",
		fmt.Sprintf("transport: %s", config.TransportNativeHTTP),
		"command: uvx",
		"streamable-http",
		"PYTHONUNBUFFERED: \"1\"",
		"port: 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("draft YAML missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, fmt.Sprintf("transport: %s", config.TransportStdioBridge)) {
		t.Errorf("native-http draft must not be downgraded to stdio-bridge:\n---\n%s", got)
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
	// Codex cumulative G6 review closure: draft must list every
	// remote-http-capable adapter (incl. qwen-cli) so the prefilled
	// list matches the install/test surface.
	for _, c := range remoteHTTPCapableClients {
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

func TestUnsafeMarketplaceTextRuneCoversTerminalAndDraftSet(t *testing.T) {
	unsafe := []rune{
		0x00, 0x1F, 0x1B, 0x7F, 0x85,
		0x061C,
		0x200E, 0x200F,
		0x202A, 0x202B, 0x202C, 0x202D, 0x202E,
		0x2028, 0x2029,
		0x2066, 0x2067, 0x2068, 0x2069,
	}
	for _, r := range unsafe {
		if !IsUnsafeMarketplaceTextRune(r) {
			t.Errorf("IsUnsafeMarketplaceTextRune(U+%04X) = false, want true", r)
		}
	}
	for _, r := range []rune{'a', 'Z', '0', '-', '_', '/', '.', '\u2713', '\u060C'} {
		if IsUnsafeMarketplaceTextRune(r) {
			t.Errorf("IsUnsafeMarketplaceTextRune(U+%04X) = true, want false", r)
		}
	}
}

func TestGenerateDraftManifest_RejectsUnsafeDraftBoundStrings(t *testing.T) {
	cases := map[string]*MarketplaceEntry{
		"command RLO": {
			ID:        "bad-command",
			Name:      "Bad",
			Transport: "stdio",
			Command:   "npx\u202E.exe",
		},
		"arg ALM": {
			ID:        "bad-arg",
			Name:      "Bad",
			Transport: "stdio",
			Command:   "npx",
			Args:      []string{"--name=ok\u061Cbad"},
		},
		"env key isolate": {
			ID:        "bad-env-key",
			Name:      "Bad",
			Transport: "stdio",
			Command:   "npx",
			Env:       map[string]string{"BAD\u2066KEY": "value"},
		},
		"env value RLO": {
			ID:        "bad-env-value",
			Name:      "Bad",
			Transport: "stdio",
			Command:   "npx",
			Env:       map[string]string{"BAD": "value\u202E"},
		},
		"http url RLO": {
			ID:        "bad-url",
			Name:      "Bad",
			Transport: "http",
			URL:       "https://example.com/mcp\u202E",
		},
		"http id ALM comment": {
			ID:        "bad\u061Cid",
			Name:      "Bad",
			Transport: "http",
			URL:       "https://example.com/mcp",
		},
		"sensitive warning name RLO": {
			ID:        "bad-warning",
			Name:      "Bad",
			Transport: "stdio",
			Command:   "npx",
			Args:      []string{"${env:AWS_TOKEN\u202E}"},
		},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			yaml, warnings, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: "/tmp/ws"})
			if err == nil {
				t.Fatalf("expected unsafe draft-bound string rejection; got yaml:\n%s\nwarnings=%v", yaml, warnings)
			}
			if !strings.Contains(err.Error(), "unsafe") {
				t.Errorf("error should name unsafe-string rejection: %v", err)
			}
			for _, r := range []rune{0x061C, 0x202E, 0x2066} {
				if strings.ContainsRune(yaml, r) {
					t.Errorf("rejected draft still returned unsafe rune U+%04X in YAML:\n%s", r, yaml)
				}
			}
		})
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

// TestGenerateDraftManifest_HttpEntryControlBytesRejected pins bot
// r2 P1 closure (PR #172): a hostile registry can embed CR/LF/NUL
// in catalog url or id. Interpolating that into the YAML header
// comment would break out of `#` and inject real keys into the
// draft. Reject the entry instead of producing a tainted file.
func TestGenerateDraftManifest_HttpEntryControlBytesRejected(t *testing.T) {
	cases := map[string]*MarketplaceEntry{
		"url with LF": {
			ID:        "ctx7",
			Transport: "http",
			URL:       "https://example.com/mcp\ntransport: stdio-bridge",
		},
		"url with CR": {
			ID:        "ctx7",
			Transport: "http",
			URL:       "https://example.com/mcp\r\nname: pwned",
		},
		"url with NUL": {
			ID:        "ctx7",
			Transport: "http",
			URL:       "https://example.com/\x00",
		},
		"id with LF": {
			ID:        "ctx7\ntransport: stdio-bridge",
			Transport: "http",
			URL:       "https://example.com/mcp",
		},
		"id with ESC": {
			ID:        "ctx7\x1b[31mred",
			Transport: "http",
			URL:       "https://example.com/mcp",
		},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			yaml, _, err := GenerateDraftManifest(e, GenerateOpts{})
			if err == nil {
				t.Fatalf("expected rejection for %s; got yaml:\n%s", name, yaml)
			}
			if !strings.Contains(err.Error(), "unsafe for YAML comments") {
				t.Errorf("error should name control-byte rejection: %v", err)
			}
		})
	}
}

// TestGenerateDraftManifest_NonSensitiveUnsetEnvDoesNotPanic pins
// codex r6 P1 closure (PR #163): a non-sensitive ${env:VAR} that
// resolves to empty must NOT panic with "assignment to entry in nil
// map". PlaceholderExpander.UndefinedEnv must be non-nil before the
// expander is used.

func TestGenerateDraftManifest_HttpEntryUnicodeLineBreaksRejected(t *testing.T) {
	cases := map[string]*MarketplaceEntry{
		"url with NEL": {
			ID:        "ctx7",
			Transport: "http",
			URL:       "https://example.com/mcptransport: stdio-bridge",
		},
		"url with line separator": {
			ID:        "ctx7",
			Transport: "http",
			URL:       "https://example.com/mcp transport: stdio-bridge",
		},
		"url with paragraph separator": {
			ID:        "ctx7",
			Transport: "http",
			URL:       "https://example.com/mcp transport: stdio-bridge",
		},
		"id with line separator": {
			ID:        "ctx7 oops",
			Transport: "http",
			URL:       "https://example.com/mcp",
		},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			yaml, _, err := GenerateDraftManifest(e, GenerateOpts{})
			if err == nil {
				t.Fatalf("expected rejection for %s; got yaml:\n%s", name, yaml)
			}
			if !strings.Contains(err.Error(), "unsafe for YAML comments") {
				t.Errorf("error should name unsafe-character rejection: %v", err)
			}
		})
	}
}

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
