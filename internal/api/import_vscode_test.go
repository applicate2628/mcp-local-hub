// Package api — tests for G7 VS Code workspace import.

package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMCPJSON(t *testing.T, workspace, body string) {
	t.Helper()
	dir := filepath.Join(workspace, ".vscode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .vscode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write mcp.json: %v", err)
	}
}

func TestImportVSCodeWorkspace_StdioServer(t *testing.T) {
	ws := t.TempDir()
	writeMCPJSON(t, ws, `{
  "servers": {
    "fetch": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-fetch"],
      "env": {"FETCH_TIMEOUT": "5000"}
    }
  }
}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(result.Servers) != 1 || result.Servers[0] != "fetch" {
		t.Errorf("servers = %v, want [fetch]", result.Servers)
	}
	if !strings.Contains(result.YAML, "name: fetch") {
		t.Errorf("YAML missing name: %s", result.YAML)
	}
	if !strings.Contains(result.YAML, "transport: stdio-bridge") {
		t.Errorf("YAML missing transport: %s", result.YAML)
	}
	if !strings.Contains(result.YAML, "command: npx") {
		t.Errorf("YAML missing command: %s", result.YAML)
	}
	if !strings.Contains(result.YAML, "FETCH_TIMEOUT") {
		t.Errorf("YAML missing env: %s", result.YAML)
	}
}

func TestImportVSCodeWorkspace_HTTPServer(t *testing.T) {
	ws := t.TempDir()
	writeMCPJSON(t, ws, `{
  "servers": {
    "remote": {
      "type": "http",
      "url": "https://example.com/mcp",
      "headers": {"Authorization": "Bearer abc"}
    }
  }
}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// G6 sub-PR 4: http/sse entries now project onto transport=
	// remote-http drafts. The YAML must carry url + headers and
	// must NOT include a daemons: block (schema rejects daemons on
	// remote-http).
	if result.EmptyResult {
		t.Errorf("EmptyResult should be false now that http entries project: %+v", result)
	}
	if !strings.Contains(result.YAML, "transport: remote-http") {
		t.Errorf("YAML missing transport: remote-http; got:\n%s", result.YAML)
	}
	if !strings.Contains(result.YAML, "https://example.com/mcp") {
		t.Errorf("YAML missing url; got:\n%s", result.YAML)
	}
	if !strings.Contains(result.YAML, "Authorization: Bearer abc") {
		t.Errorf("YAML missing Authorization header; got:\n%s", result.YAML)
	}
	if strings.Contains(result.YAML, "daemons:") {
		t.Errorf("YAML must NOT include daemons: for remote-http; got:\n%s", result.YAML)
	}
	if !strings.Contains(result.YAML, "client_bindings:") {
		t.Errorf("YAML missing client_bindings:; got:\n%s", result.YAML)
	}
	// Codex cumulative G6 review closure: prefilled bindings must
	// include every remote-http-capable adapter so the draft matches
	// the install-plan capability gate.
	for _, c := range remoteHTTPCapableClients {
		if !strings.Contains(result.YAML, "client: "+c) {
			t.Errorf("YAML missing prefilled binding for %s\n---\n%s", c, result.YAML)
		}
	}
}

// TestImportVSCodeWorkspace_HTTPServer_URLPlaceholderUnsetSkips pins
// bot r1 P2 closure (PR #172): a workspace using ${env:VAR} in url
// where the env var is unset must skip with a clear warning instead
// of emitting a draft with an empty url (which manifest validation
// would reject later anyway). Check is post-expansion.
func TestImportVSCodeWorkspace_HTTPServer_URLPlaceholderUnsetSkips(t *testing.T) {
	ws := t.TempDir()
	writeMCPJSON(t, ws, `{
  "servers": {
    "placeholder": {"type": "http", "url": "${env:NEVER_SET_VAR_FOR_G6_TEST}"}
  }
}`)
	// Ensure the env var is genuinely unset.
	t.Setenv("NEVER_SET_VAR_FOR_G6_TEST", "")
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !result.EmptyResult {
		t.Errorf("EmptyResult should be true when url placeholder expands to empty; got %+v", result)
	}
	foundSkip := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "expanded to empty") && strings.Contains(w, "placeholder") {
			foundSkip = true
			break
		}
	}
	if !foundSkip {
		t.Errorf("expected expanded-to-empty skip warning; got: %v", result.Warnings)
	}
}

// TestImportVSCodeWorkspace_HTTPServer_PlaintextHTTPSkips pins bot
// r3 P2 closure (PR #172): a workspace url using plaintext http://
// must skip with a clear "not https://" warning, not project to a
// remote-http draft that manifest validation rejects later with the
// wrong diagnostic.
func TestImportVSCodeWorkspace_HTTPServer_PlaintextHTTPSkips(t *testing.T) {
	ws := t.TempDir()
	writeMCPJSON(t, ws, `{
  "servers": {
    "insecure": {"type": "http", "url": "http://localhost:9000/mcp"}
  }
}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !result.EmptyResult {
		t.Errorf("EmptyResult should be true for plaintext http; got %+v", result)
	}
	foundSkip := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "not https") && strings.Contains(w, "insecure") {
			foundSkip = true
			break
		}
	}
	if !foundSkip {
		t.Errorf("expected not-https skip warning; got: %v", result.Warnings)
	}
}

// TestImportVSCodeWorkspace_HTTPServer_HeaderCRLFRejected pins codex
// cumulative G6 review P1 closure: PlaceholderExpander resolves
// ${env:VAR} but does not guard against CR/LF in expanded values,
// unlike ExpandSecrets. A hostile workspace using `${env:EVIL}`
// where EVIL holds `\r\nX-Injected: 1` would project a draft with
// CRLF in a header — install-time clients (or HTTP libraries) might
// splice it into the wire. Skip with a clear warning instead.
func TestImportVSCodeWorkspace_HTTPServer_HeaderCRLFRejected(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("CRLF_EVIL_FOR_G6_TEST", "good\r\nX-Injected: 1")
	writeMCPJSON(t, ws, `{
  "servers": {
    "evil": {
      "type": "http",
      "url": "https://example.com/mcp",
      "headers": {"Authorization": "Bearer ${env:CRLF_EVIL_FOR_G6_TEST}"}
    }
  }
}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !result.EmptyResult {
		t.Errorf("EmptyResult should be true on CRLF header; got %+v", result)
	}
	foundSkip := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "control bytes") && strings.Contains(w, "evil") {
			foundSkip = true
			break
		}
	}
	if !foundSkip {
		t.Errorf("expected control-bytes skip warning; got: %v", result.Warnings)
	}
}

// TestImportVSCodeWorkspace_HTTPServer_URLCRLFRejected pins the
// same guard for URL placeholders that expand to CR/LF.
func TestImportVSCodeWorkspace_HTTPServer_URLCRLFRejected(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("CRLF_URL_FOR_G6_TEST", "https://example.com/mcp\r\ntransport: stdio-bridge")
	writeMCPJSON(t, ws, `{
  "servers": {
    "evil-url": {"type": "http", "url": "${env:CRLF_URL_FOR_G6_TEST}"}
  }
}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !result.EmptyResult {
		t.Errorf("EmptyResult should be true on CRLF url; got %+v", result)
	}
	foundSkip := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "control bytes") && strings.Contains(w, "evil-url") {
			foundSkip = true
			break
		}
	}
	if !foundSkip {
		t.Errorf("expected url control-bytes skip warning; got: %v", result.Warnings)
	}
}

// TestImportVSCodeWorkspace_HTTPServer_UppercaseHTTPSSkips pins bot
// r4 P2 closure (PR #172): manifest validator's https:// check is
// case-sensitive (literal lowercase HasPrefix). The projector must
// match exactly — otherwise URLs like "HTTPS://example.com/mcp"
// project here and then fail manifest validation, the same wrong-
// diagnostic flow bot r3 fixed for plaintext http://.
func TestImportVSCodeWorkspace_HTTPServer_UppercaseHTTPSSkips(t *testing.T) {
	ws := t.TempDir()
	writeMCPJSON(t, ws, `{
  "servers": {
    "yelling": {"type": "http", "url": "HTTPS://example.com/mcp"}
  }
}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !result.EmptyResult {
		t.Errorf("EmptyResult should be true for uppercase HTTPS; got %+v", result)
	}
	foundSkip := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "not https") && strings.Contains(w, "yelling") {
			foundSkip = true
			break
		}
	}
	if !foundSkip {
		t.Errorf("expected not-https skip warning; got: %v", result.Warnings)
	}
}

func TestImportVSCodeWorkspace_HTTPServer_NoURLSkips(t *testing.T) {
	ws := t.TempDir()
	writeMCPJSON(t, ws, `{
  "servers": {
    "bad": {"type": "http"}
  }
}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !result.EmptyResult {
		t.Errorf("EmptyResult should be true (http without url is skipped); got %+v", result)
	}
	foundSkip := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "no url") && strings.Contains(w, "bad") {
			foundSkip = true
			break
		}
	}
	if !foundSkip {
		t.Errorf("expected no-url skip warning; got: %v", result.Warnings)
	}
}

func TestImportVSCodeWorkspace_BothSchemas_ServersWins(t *testing.T) {
	ws := t.TempDir()
	// Same name "shared" in both — servers entry should win, mcpServers entry warned about.
	writeMCPJSON(t, ws, `{
  "servers": {
    "shared": {"type": "stdio", "command": "new-cmd"},
    "only-new": {"type": "stdio", "command": "exclusive-new"}
  },
  "mcpServers": {
    "shared": {"type": "stdio", "command": "old-cmd"},
    "only-old": {"type": "stdio", "command": "exclusive-old"}
  }
}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(result.Servers) != 3 {
		t.Errorf("got %d servers, want 3", len(result.Servers))
	}
	// shared must show new-cmd (servers precedence).
	if !strings.Contains(result.YAML, "command: new-cmd") {
		t.Errorf("YAML missing new-cmd: %s", result.YAML)
	}
	if strings.Contains(result.YAML, "command: old-cmd") {
		t.Errorf("YAML contains old-cmd — servers precedence broken: %s", result.YAML)
	}
	// shadow warning must be present.
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "shadowed") || strings.Contains(w, "shared") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("warnings missing shared-name conflict: %v", result.Warnings)
	}
}

func TestImportVSCodeWorkspace_PlaceholderExpansion(t *testing.T) {
	ws := t.TempDir()
	writeMCPJSON(t, ws, `{
  "servers": {
    "test": {
      "type": "stdio",
      "command": "${workspaceFolder}${pathSeparator}bin${pathSeparator}server",
      "args": ["${env:TEST_VAR}", "${userHome}", "literal"]
    }
  }
}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{
		Getenv: func(name string) string {
			if name == "TEST_VAR" {
				return "from-env"
			}
			return ""
		},
		UserHome:      func() (string, error) { return "/home/test", nil },
		PathSeparator: "/",
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// workspaceFolder + pathSeparator expanded.
	wantCmd := ws + "/bin/server"
	if !strings.Contains(result.YAML, "command: "+yamlEscape(wantCmd)) {
		t.Errorf("YAML missing expanded command %q; YAML:\n%s", wantCmd, result.YAML)
	}
	// env + home + literal in args.
	if !strings.Contains(result.YAML, "from-env") {
		t.Errorf("env placeholder not expanded: %s", result.YAML)
	}
	if !strings.Contains(result.YAML, "/home/test") {
		t.Errorf("home placeholder not expanded: %s", result.YAML)
	}
}

func TestImportVSCodeWorkspace_EnvUndefinedWarning(t *testing.T) {
	ws := t.TempDir()
	writeMCPJSON(t, ws, `{
  "servers": {
    "test": {
      "type": "stdio",
      "command": "tool",
      "env": {"VAL": "${env:NEVER_SET_HOPEFULLY}"}
    }
  }
}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{
		Getenv: func(string) string { return "" }, // every env empty
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "NEVER_SET_HOPEFULLY") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("warnings missing undefined-env entry: %v", result.Warnings)
	}
}

func TestImportVSCodeWorkspace_JSON5_CommentsAndTrailingCommas(t *testing.T) {
	ws := t.TempDir()
	writeMCPJSON(t, ws, `{
  // Line comment before block.
  "servers": {
    "fetch": {
      "type": "stdio",
      "command": "tool", /* inline block */
      "args": [
        "one",
        "two", // trailing comma below + line comment
      ],
    }, /* trailing comma after object */
  },
  /* multi-line
     block comment */
}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err != nil {
		t.Fatalf("import (should tolerate comments + trailing commas): %v", err)
	}
	if len(result.Servers) != 1 {
		t.Errorf("got %d servers, want 1 (parser dropped entries?): %v", len(result.Servers), result.Servers)
	}
}

func TestImportVSCodeWorkspace_MissingFile_Error(t *testing.T) {
	ws := t.TempDir() // no .vscode/mcp.json written
	a := NewAPI()
	_, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err == nil {
		t.Fatal("expected error reading missing mcp.json")
	}
	if !strings.Contains(err.Error(), "mcp.json") {
		t.Errorf("error should mention mcp.json: %v", err)
	}
}

func TestImportVSCodeWorkspace_EmptyServers_EmptyResultFlag(t *testing.T) {
	ws := t.TempDir()
	writeMCPJSON(t, ws, `{"servers": {}}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !result.EmptyResult {
		t.Errorf("EmptyResult should be true for empty servers map")
	}
	if result.YAML != "" {
		t.Errorf("YAML should be empty, got: %q", result.YAML)
	}
}

func TestImportVSCodeWorkspace_UnknownType_WarnsAndSkips(t *testing.T) {
	ws := t.TempDir()
	writeMCPJSON(t, ws, `{
  "servers": {
    "weird": {"type": "websocket", "url": "ws://example.com"},
    "ok": {"type": "stdio", "command": "valid"}
  }
}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(result.Servers) != 1 || result.Servers[0] != "ok" {
		t.Errorf("servers = %v, want [ok] (weird should be skipped)", result.Servers)
	}
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "websocket") && strings.Contains(w, "weird") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("warnings missing unknown-type entry: %v", result.Warnings)
	}
}

func TestImportVSCodeWorkspace_TypeInferenceFromCommandOrURL(t *testing.T) {
	ws := t.TempDir()
	writeMCPJSON(t, ws, `{
  "servers": {
    "implicit-stdio": {"command": "tool"},
    "implicit-http":  {"url": "https://example.com/mcp"}
  }
}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.Contains(result.YAML, "stdio-bridge") {
		t.Errorf("YAML missing stdio-bridge transport (implicit from command): %s", result.YAML)
	}
	// G6 sub-PR 4: implicit type=http (inferred from url-only entry)
	// now projects onto transport=remote-http. The legacy "skip
	// with G6 warning" contract is replaced by an actual draft.
	if !strings.Contains(result.YAML, "transport: remote-http") {
		t.Errorf("YAML missing remote-http projection for url-only entry: %s", result.YAML)
	}
	if strings.Contains(result.YAML, "native-http") {
		t.Errorf("YAML must not confuse http with native-http (different transports): %s", result.YAML)
	}
	if len(result.Servers) != 2 {
		t.Errorf("expected both servers to project; got %v", result.Servers)
	}
}

func TestImportVSCodeWorkspace_LegacyOnlyMcpServersKey(t *testing.T) {
	ws := t.TempDir()
	writeMCPJSON(t, ws, `{
  "mcpServers": {
    "legacy": {"type": "stdio", "command": "old-tool"}
  }
}`)
	a := NewAPI()
	result, err := a.ImportVSCodeWorkspace(ws, VSCodeImportOpts{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(result.Servers) != 1 || result.Servers[0] != "legacy" {
		t.Errorf("legacy-only mcpServers key not recognized: %v", result.Servers)
	}
}

func TestImportVSCodeWorkspace_EmptyWorkspace_RejectsEarly(t *testing.T) {
	a := NewAPI()
	_, err := a.ImportVSCodeWorkspace("", VSCodeImportOpts{})
	if err == nil {
		t.Fatal("expected error for empty workspace path")
	}
}

// TestIsSensitiveEnvName covers the sensitive-name policy used by G5's
// catalog placeholder expansion (Phase 0: shared PlaceholderExpander).
// G7 imports are NOT affected because they leave SkipSensitiveEnv at
// its default (false); this test exercises the predicate directly.
//
// codex deep-sec PR #163 lane 2: predicate expanded from suffix-only
// to suffix + prefix + substring + exact-name shapes. The cases below
// cover all four match families and a sample of intentional
// non-matches.
func TestIsSensitiveEnvName(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		// Non-sensitive: ordinary config / shell vars.
		{"PATH", false},
		{"HOME", false},
		{"WORKSPACE_FOLDER", false},
		{"LOG_LEVEL", false},
		{"USER", false},
		// Suffix matches (classic name shapes).
		{"AWS_SECRET_ACCESS_KEY", true},
		{"GITHUB_TOKEN", true},
		{"OPENAI_API_KEY", true},
		{"FOO_SECRET", true},
		{"FOO_PASSWORD", true},
		{"FOO_PASSWD", true},
		{"FOO_KEY", true},
		{"FOO_AUTH", true},
		{"FOO_DSN", true},
		// Prefix matches (cloud provider namespaces).
		{"AZURE_TENANT_ID", true},
		{"GCP_PROJECT", true},
		{"GOOGLE_API_KEY", true},
		{"OAUTH_REDIRECT", true},
		// Substring matches (codex r5 P1 broadening: infix detection).
		{"MY_TOKEN_VALUE", true},     // infix TOKEN — was false pre-broaden
		{"SECRET_HOLDER", true},      // infix SECRET
		{"BEARER_HEADER", true},      // infix BEARER
		{"MY_CREDENTIAL_PATH", true}, // infix CREDENTIAL
		{"USE_PRIVATE_KEY_FILE", true}, // infix PRIVATE_KEY
		{"SLACK_PASSWD_HASH", true},  // infix PASSWD
		// Exact-name matches (specific names without a name-shape tell).
		{"DATABASE_URL", true},                    // was false pre-broaden
		{"CONNECTION_STRING", true},               // was missing pre-broaden
		{"DSN", true},                              // was missing pre-broaden
		{"AUTHORIZATION", true},                    // was missing pre-broaden
		{"OAUTH", true},                            // was missing pre-broaden
		{"GOOGLE_APPLICATION_CREDENTIALS", true},   // was missing pre-broaden
		// Case-insensitivity sanity.
		{"github_token", true},
		{"database_url", true},
	} {
		got := IsSensitiveEnvName(c.name)
		if got != c.want {
			t.Errorf("IsSensitiveEnvName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
