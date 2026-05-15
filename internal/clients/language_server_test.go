// Tests for the FindStdioLanguageServerEntries surface added in B3.
//
// The matcher itself (isLanguageServerBinary, extractLspLanguageArg,
// matchLanguageServerStdio, findLanguageServerStdioInMap) is exercised
// directly so the per-adapter wrappers can stay thin. The adapter
// integration tests verify the format-specific parsing (TOML vs JSON,
// mcpServers vs servers vs mcp_servers).

package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestIsLanguageServerBinary(t *testing.T) {
	cases := []struct {
		in   string
		want bool
		why  string
	}{
		{"", false, "empty rejected"},
		{"mcp-language-server", true, "bare basename"},
		{"mcp-language-server.exe", true, ".exe stripped"},
		{"MCP-Language-Server.EXE", true, "case-insensitive"},
		{`C:\Users\u\.local\bin\mcp-language-server.exe`, true, "Windows absolute path (matches on every OS via basenameAcrossSeparators)"},
		{`D:\Tools\mcp-language-server`, true, "Windows path no extension"},
		{"/usr/local/bin/mcp-language-server", true, "POSIX absolute path"},
		{"./mcp-language-server", true, "relative path"},
		{`.\bin\mcp-language-server.exe`, true, "Windows relative path"},
		{"clangd", false, "different binary"},
		{"mcp-language-server-clangd", false, "hub-managed name not the binary"},
		{"mcp-language-server-helper", false, "suffix not bare match"},
		{"mcp_language_server", false, "underscore not dash"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := isLanguageServerBinary(tc.in)
			if got != tc.want {
				t.Errorf("isLanguageServerBinary(%q) = %v, want %v (%s)", tc.in, got, tc.want, tc.why)
			}
		})
	}
}

func TestExtractLspLanguageArg(t *testing.T) {
	cases := []struct {
		name string
		args any
		want string
	}{
		{"two-token --lsp clangd", []any{"--lsp", "clangd"}, "clangd"},
		{"single-token --lsp=clangd", []any{"--lsp=clangd"}, "clangd"},
		{"surrounded by other flags", []any{"--verbose", "--lsp", "pylsp", "--workspace", "/foo"}, "pylsp"},
		{"--lsp at end without value", []any{"--lsp"}, ""},
		{"missing flag", []any{"--workspace", "/foo"}, ""},
		{"nil args", nil, ""},
		{"non-list args", "string instead of list", ""},
		{"non-string element ignored", []any{1, "--lsp", "rust"}, "rust"},
		{"empty list", []any{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractLspLanguageArg(tc.args)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMatchLanguageServerStdio(t *testing.T) {
	cases := []struct {
		name      string
		entry     map[string]any
		wantOk    bool
		wantLang  string
	}{
		{
			name: "stdio mcp-language-server clangd matches",
			entry: map[string]any{
				"command": "mcp-language-server",
				"args":    []any{"--lsp", "clangd"},
			},
			wantOk:   true,
			wantLang: "clangd",
		},
		{
			name: "single-token --lsp=pylsp matches",
			entry: map[string]any{
				"command": "mcp-language-server.exe",
				"args":    []any{"--lsp=pylsp"},
			},
			wantOk:   true,
			wantLang: "pylsp",
		},
		{
			name: "no --lsp arg rejected (codex bot r1 P1.2: command-only must NOT match)",
			entry: map[string]any{
				"command": "mcp-language-server",
				"args":    []any{"--help"},
			},
			wantOk: false,
		},
		{
			name: "command match + --lsp= form matches",
			entry: map[string]any{
				"command": "mcp-language-server",
				"args":    []any{"--lsp=rust"},
			},
			wantOk:   true,
			wantLang: "rust",
		},
		{
			name: "Windows-style command path matches on every OS",
			entry: map[string]any{
				"command": `C:\Users\u\.local\bin\mcp-language-server.exe`,
				"args":    []any{"--lsp", "clangd"},
			},
			wantOk:   true,
			wantLang: "clangd",
		},
		{
			name: "different binary name rejected",
			entry: map[string]any{
				"command": "clangd",
				"args":    []any{"--stdio"},
			},
			wantOk: false,
		},
		{
			name: "HTTP entry (no command) rejected",
			entry: map[string]any{
				"url":  "http://localhost:9200/mcp",
				"type": "http",
			},
			wantOk: false,
		},
		{
			name: "hub-managed mcp-language-server-clangd entry not the binary",
			entry: map[string]any{
				"url":  "http://localhost:9201/mcp",
				"type": "http",
			},
			wantOk: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, lang, ok := matchLanguageServerStdio(tc.entry)
			if ok != tc.wantOk {
				t.Errorf("ok = %v, want %v", ok, tc.wantOk)
			}
			if ok && lang != tc.wantLang {
				t.Errorf("lang = %q, want %q", lang, tc.wantLang)
			}
		})
	}
}

func TestFindLanguageServerStdioInMap_SortedStable(t *testing.T) {
	servers := map[string]any{
		"clangd": map[string]any{
			"command": "mcp-language-server",
			"args":    []any{"--lsp", "clangd"},
		},
		"pylsp": map[string]any{
			"command": "mcp-language-server",
			"args":    []any{"--lsp", "pylsp"},
		},
		"unrelated": map[string]any{
			"command": "node",
			"args":    []any{"server.js"},
		},
		"hub-managed": map[string]any{
			"url":  "http://localhost:9200/mcp",
			"type": "http",
		},
		"experimental-no-lsp": map[string]any{
			// Codex bot r1 P1.2: command matches the LSP binary
			// but there is no --lsp arg, so cleanup must NOT
			// touch this entry (operator-experimental config).
			"command": "mcp-language-server",
			"args":    []any{"--debug"},
		},
	}
	got := findLanguageServerStdioInMap(servers)
	if len(got) != 2 {
		t.Fatalf("got %d matches, want 2: %+v", len(got), got)
	}
	// Stable sort by Name guarantees clangd < pylsp.
	if got[0].Name != "clangd" || got[1].Name != "pylsp" {
		t.Errorf("unexpected order: %+v", got)
	}
	if got[0].Language != "clangd" || got[1].Language != "pylsp" {
		t.Errorf("language extraction failed: %+v", got)
	}
}

func TestFindLanguageServerStdioInMap_EmptyAndNil(t *testing.T) {
	if got := findLanguageServerStdioInMap(nil); got != nil {
		t.Errorf("nil map: got %+v, want nil", got)
	}
	if got := findLanguageServerStdioInMap(map[string]any{}); got != nil {
		t.Errorf("empty map: got %+v, want nil", got)
	}
}

// --- Adapter integration tests --------------------------------------

func TestCodexCLI_FindStdioLanguageServerEntries(t *testing.T) {
	initial := `[mcp_servers.clangd]
command = "mcp-language-server"
args = ["--lsp", "clangd"]

[mcp_servers.pythonls]
command = "mcp-language-server.exe"
args = ["--lsp=pylsp"]

[mcp_servers.gdb]
url = "http://localhost:9129/mcp"
startup_timeout_sec = 10.0

[mcp_servers.user-clangd-unrelated]
command = "clangd"
args = ["--stdio"]
`
	path := setupCodexConfig(t, initial)
	c := &codexCLI{path: path}
	got, err := c.FindStdioLanguageServerEntries()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerEntries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	wantNames := []string{"clangd", "pythonls"}
	gotNames := []string{got[0].Name, got[1].Name}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("names = %v, want %v", gotNames, wantNames)
	}
	if got[0].Language != "clangd" || got[1].Language != "pylsp" {
		t.Errorf("languages = %v/%v, want clangd/pylsp", got[0].Language, got[1].Language)
	}
}

func TestCodexCLI_FindStdioLanguageServerEntries_EmptyConfig(t *testing.T) {
	path := setupCodexConfig(t, "")
	c := &codexCLI{path: path}
	got, err := c.FindStdioLanguageServerEntries()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerEntries: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want empty", got)
	}
}

func TestClaudeCode_FindStdioLanguageServerEntries(t *testing.T) {
	initial := `{
  "mcpServers": {
    "clangd": {
      "type": "stdio",
      "command": "mcp-language-server",
      "args": ["--lsp", "clangd"]
    },
    "mcp-language-server-clangd": {
      "type": "http",
      "url": "http://localhost:9200/mcp"
    },
    "user-stdio": {
      "command": "node",
      "args": ["server.js"]
    }
  }
}`
	path := setupClaudeConfig(t, initial)
	c := &claudeCode{path: path}
	got, err := c.FindStdioLanguageServerEntries()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerEntries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1: %+v", len(got), got)
	}
	if got[0].Name != "clangd" || got[0].Language != "clangd" {
		t.Errorf("got %+v", got[0])
	}
}

func TestJsonMCPClient_FindStdioLanguageServerEntries(t *testing.T) {
	// Exercises the shared jsonMCPClient implementation that backs
	// cursor / qwen / gemini / antigravity. Antigravity entries
	// have command=mcphub and never match.
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	initial := map[string]any{
		"mcpServers": map[string]any{
			"clangd": map[string]any{
				"command": "mcp-language-server",
				"args":    []any{"--lsp", "clangd"},
			},
			"antigravity-shape": map[string]any{
				"command":  "mcphub",
				"args":     []any{"relay", "--server", "memory", "--daemon", "default"},
				"disabled": false,
			},
		},
	}
	raw, _ := json.MarshalIndent(initial, "", "  ")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	j := &jsonMCPClient{path: path, clientName: "test", urlField: "url"}
	got, err := j.FindStdioLanguageServerEntries()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerEntries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1: %+v", len(got), got)
	}
	if got[0].Name != "clangd" {
		t.Errorf("got %q, want clangd", got[0].Name)
	}
}

func TestVSCode_FindStdioLanguageServerEntries(t *testing.T) {
	// VS Code uses "servers" (NOT "mcpServers") at the top level —
	// this regression-protects the format-specific lookup key.
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	initial := map[string]any{
		"servers": map[string]any{
			"clangd": map[string]any{
				"type":    "stdio",
				"command": "mcp-language-server",
				"args":    []any{"--lsp", "clangd"},
			},
			"hub-managed": map[string]any{
				"type": "http",
				"url":  "http://localhost:9200/mcp",
			},
		},
	}
	raw, _ := json.MarshalIndent(initial, "", "  ")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	v := &vscodeClient{path: path}
	got, err := v.FindStdioLanguageServerEntries()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerEntries: %v", err)
	}
	if len(got) != 1 || got[0].Name != "clangd" {
		t.Errorf("got %+v, want one clangd entry", got)
	}
}

// TestAllAdapters_FindStdioLanguageServerEntries_EmptyClients verifies
// that every adapter returns a clean empty/nil slice (no error) when
// the config file is missing, so callers can iterate AllClients()
// without branching on per-client state.
func TestAllAdapters_FindStdioLanguageServerEntries_EmptyClients(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		newC    func(p string) Client
		relPath string
	}{
		{"claude-code", func(p string) Client { return &claudeCode{path: p} }, "claude.json"},
		{"codex-cli", func(p string) Client { return &codexCLI{path: p} }, "codex.toml"},
		{"vscode", func(p string) Client { return &vscodeClient{path: p} }, "vscode-mcp.json"},
		{"jsonMCP-gemini-like", func(p string) Client {
			return &jsonMCPClient{path: p, clientName: "test", urlField: "url"}
		}, "gemini.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.relPath)
			c := tc.newC(path)
			// File does not exist — adapter readers return empty map,
			// matcher returns nil.
			got, err := c.FindStdioLanguageServerEntries()
			if err != nil {
				t.Fatalf("FindStdioLanguageServerEntries: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("got %+v, want empty", got)
			}
		})
	}
}
