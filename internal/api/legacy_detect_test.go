package api

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Codex config with two disabled mcp-language-server entries plus one
// unrelated HTTP server. The detector must return only the two disabled
// legacy stdio entries.
const legacyCodexFixture = `
[mcp_servers.python-lsp]
command = "mcp-language-server"
args = ["--workspace", "D:\\projects\\foo", "--lsp", "pyright-langserver"]
enabled = false

[mcp_servers.rust-lsp]
command = "mcp-language-server"
args = ["--workspace", "D:\\projects\\foo", "--lsp", "rust-analyzer"]
enabled = false

[mcp_servers.serena]
url = "http://localhost:9121/mcp"
`

// Codex config with one ENABLED mcp-language-server entry. The detector
// must ignore it — migration is opt-in and an enabled entry is still in
// active use.
const legacyCodexEnabledFixture = `
[mcp_servers.python-lsp]
command = "mcp-language-server"
args = ["--workspace", "D:\\projects\\foo", "--lsp", "pyright-langserver"]
enabled = true
`

// Claude Code JSON fixture with one disabled stdio entry and one unrelated
// HTTP entry.
const legacyClaudeFixture = `{
  "mcpServers": {
    "ts-lsp": {
      "type": "stdio",
      "command": "mcp-language-server",
      "args": ["--workspace", "D:\\projects\\bar", "--lsp", "typescript-language-server", "--stdio"],
      "disabled": true
    },
    "serena": { "type": "http", "url": "http://localhost:9121/mcp" }
  }
}
`

// Gemini-settings shape reused here only as evidence that the detector
// ignores files outside its Codex + Claude scan set. Not parsed.
const hubOnlyCodexFixture = `
[mcp_servers.serena]
url = "http://localhost:9121/mcp"
`

// withHomeDir redirects HOME/USERPROFILE to a fresh temp directory so
// DetectLegacyLanguageServerEntries resolves against a hermetic filesystem.
// Returns the temp directory for fixture placement.
func withHomeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

// TestDetectLegacyLSP_CodexConfig seeds a Codex TOML with two disabled
// entries and confirms both appear in the returned slice with their
// parsed workspace + language.
func TestDetectLegacyLSP_CodexConfig(t *testing.T) {
	dir := withHomeDir(t)
	_ = os.MkdirAll(filepath.Join(dir, ".codex"), 0755)
	if err := os.WriteFile(filepath.Join(dir, ".codex", "config.toml"), []byte(legacyCodexFixture), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := DetectLegacyLanguageServerEntries()
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 codex entries, got %d: %+v", len(got), got)
	}
	byLang := map[string]LegacyLSEntry{}
	for _, e := range got {
		byLang[e.Language] = e
	}
	if e := byLang["python"]; e.Client != "codex-cli" || e.Workspace == "" || e.LspCommand == "" {
		t.Errorf("python entry malformed: %+v", e)
	}
	if e := byLang["rust"]; e.Client != "codex-cli" || e.Workspace == "" || e.LspCommand == "" {
		t.Errorf("rust entry malformed: %+v", e)
	}
}

// TestDetectLegacyLSP_ClaudeConfig seeds Claude .claude.json with one
// disabled stdio entry and confirms it is detected.
func TestDetectLegacyLSP_ClaudeConfig(t *testing.T) {
	dir := withHomeDir(t)
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(legacyClaudeFixture), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := DetectLegacyLanguageServerEntries()
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 claude entry, got %d: %+v", len(got), got)
	}
	if got[0].Client != "claude-code" {
		t.Errorf("client = %q, want claude-code", got[0].Client)
	}
	if got[0].Language != "typescript" {
		t.Errorf("language = %q, want typescript", got[0].Language)
	}
	if got[0].Workspace == "" {
		t.Errorf("workspace empty: %+v", got[0])
	}
}

// TestDetectLegacyLSP_GeminiConfig_NotScanned confirms the detector does
// NOT scan Gemini settings.json. Gemini never hosted the legacy stdio
// shape so there is no migration surface there; including it would be
// dead code.
func TestDetectLegacyLSP_GeminiConfig_NotScanned(t *testing.T) {
	dir := withHomeDir(t)
	_ = os.MkdirAll(filepath.Join(dir, ".gemini"), 0755)
	// Seed a disabled-looking entry in Gemini's settings to prove it is
	// ignored; if the detector ever starts scanning Gemini this test will
	// surface it.
	gemini := `{
  "mcpServers": {
    "stray-lsp": {
      "command": "mcp-language-server",
      "args": ["--workspace", "/tmp/x", "--lsp", "rust-analyzer"],
      "disabled": true
    }
  }
}`
	_ = os.WriteFile(filepath.Join(dir, ".gemini", "settings.json"), []byte(gemini), 0644)
	got, err := DetectLegacyLanguageServerEntries()
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 entries (gemini out of scope), got %d: %+v", len(got), got)
	}
}

// TestDetectLegacyLSP_IgnoresEnabledEntries proves an enabled Codex entry
// is NOT in the returned list. Migration is opt-in.
func TestDetectLegacyLSP_IgnoresEnabledEntries(t *testing.T) {
	dir := withHomeDir(t)
	_ = os.MkdirAll(filepath.Join(dir, ".codex"), 0755)
	if err := os.WriteFile(filepath.Join(dir, ".codex", "config.toml"), []byte(legacyCodexEnabledFixture), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := DetectLegacyLanguageServerEntries()
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 entries (all enabled), got %d: %+v", len(got), got)
	}
}

// TestDetectLegacyLSP_ExtractsWorkspaceAndLanguage confirms parsing of
// --workspace and --lsp flags across both Codex and Claude fixtures. This
// is the round-trip test that keeps parseWorkspaceAndLsp honest.
func TestDetectLegacyLSP_ExtractsWorkspaceAndLanguage(t *testing.T) {
	dir := withHomeDir(t)
	_ = os.MkdirAll(filepath.Join(dir, ".codex"), 0755)
	_ = os.WriteFile(filepath.Join(dir, ".codex", "config.toml"), []byte(legacyCodexFixture), 0644)
	_ = os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(legacyClaudeFixture), 0644)
	got, err := DetectLegacyLanguageServerEntries()
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries across codex+claude, got %d: %+v", len(got), got)
	}
	// Every entry must have a non-empty workspace + inferred language.
	for _, e := range got {
		if e.Workspace == "" {
			t.Errorf("entry %s/%s has empty Workspace: %+v", e.Client, e.EntryName, e)
		}
		if e.Language == "" {
			t.Errorf("entry %s/%s has empty Language: %+v", e.Client, e.EntryName, e)
		}
	}
	// Sanity: collected languages cover python, rust, typescript.
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.Language] = true
	}
	want := []string{"python", "rust", "typescript"}
	sort.Strings(want)
	for _, w := range want {
		if !seen[w] {
			t.Errorf("missing language %q in detection output", w)
		}
	}
}

// TestDetectLegacyLSP_IgnoresHubHttpEntries seeds a config that contains
// only the HTTP hub entry the user would see AFTER migration. The detector
// must return an empty list — the hub entries are not mcp-language-server
// stdio rows.
func TestDetectLegacyLSP_IgnoresHubHttpEntries(t *testing.T) {
	dir := withHomeDir(t)
	_ = os.MkdirAll(filepath.Join(dir, ".codex"), 0755)
	_ = os.WriteFile(filepath.Join(dir, ".codex", "config.toml"), []byte(hubOnlyCodexFixture), 0644)
	got, err := DetectLegacyLanguageServerEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 legacy entries, got %d: %+v", len(got), got)
	}
}

// TestLanguageFromLsp spot-checks the binary-to-language inference table.
func TestLanguageFromLsp(t *testing.T) {
	cases := []struct {
		bin  string
		want string
	}{
		{"pyright-langserver", "python"},
		{"pyright-langserver.exe", "python"},
		{"typescript-language-server", "typescript"},
		{"rust-analyzer", "rust"},
		{"clangd", "clangd"},
		{"fortls", "fortran"},
		{"gopls", "go"},
		{"vscode-css-language-server", "vscode-css"},
		{"vscode-html-language-server", "vscode-html"},
		{"", ""},
		{"unknown-lsp", ""},
	}
	for _, c := range cases {
		got := languageFromLsp(c.bin)
		if got != c.want {
			t.Errorf("languageFromLsp(%q) = %q, want %q", c.bin, got, c.want)
		}
	}
}
