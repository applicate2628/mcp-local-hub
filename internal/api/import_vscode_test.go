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
	// G7 skips http/sse types — they describe remote URLs that require
	// the G6 backlog item (Remote MCP manifests, v0.4.x) before they
	// can produce valid mcp-local-hub manifests. Codex bot P1 on PR
	// #151 line 289 caught the original invalid emission.
	if !result.EmptyResult {
		t.Errorf("EmptyResult should be true when only http/sse entries exist: %+v", result)
	}
	if result.YAML != "" {
		t.Errorf("YAML should be empty (http skipped); got:\n%s", result.YAML)
	}
	foundSkip := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "remote MCP server") && strings.Contains(w, "G6") {
			foundSkip = true
			break
		}
	}
	if !foundSkip {
		t.Errorf("expected warning referencing G6 backlog deferral; got: %v", result.Warnings)
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
	// implicit-http inferred to type=http → skipped per G7 contract
	// (remote URLs land in G6, v0.4.x). YAML must NOT contain a
	// native-http projection for the url-only entry.
	if strings.Contains(result.YAML, "native-http") {
		t.Errorf("YAML must not project http to native-http (G6 territory): %s", result.YAML)
	}
	if len(result.Servers) != 1 || result.Servers[0] != "implicit-stdio" {
		t.Errorf("only implicit-stdio should project; got servers = %v", result.Servers)
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
