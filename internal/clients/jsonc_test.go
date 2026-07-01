package clients

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures below all carry the four JSONC features that broke strict
// encoding/json before this fix: a `//` line-comment header, a `/* */` block
// comment, a trailing comma, AND an unrelated top-level key the operator owns
// (mirroring Zed's wsl_connections / theme). Each adapter test asserts:
//
//	(a) the read path parses the JSONC file without error,
//	(b) AddEntry adds the MCP entry AND the comments + unrelated key survive,
//	(c) RemoveEntry removes the MCP entry AND preserves comments + unrelated key,
//	(d) a JSONC BACKUP file round-trips through RestoreEntryFromBackup.

// parseJSONCFile re-reads a written config and parses it through the same
// JSONC-tolerant helper the production read path uses, so an assertion can
// inspect the resulting map even though the written file still contains the
// operator's comments.
func parseJSONCFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m, err := parseJSONCBytes(data)
	if err != nil {
		t.Fatalf("parse JSONC %s: %v", path, err)
	}
	return m
}

// assertCommentsAndUnrelatedSurvive is the shared (b)/(c) assertion: the raw
// bytes on disk still carry the operator's // header, /* */ block comment, and
// their unrelated top-level key after a write.
func assertCommentsAndUnrelatedSurvive(t *testing.T, path, unrelatedKey string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := string(raw)
	for _, want := range []string{
		"// Zed settings header",       // line comment
		"/* hand-written block note */", // block comment
		unrelatedKey,                    // operator's unrelated key
	} {
		if !strings.Contains(out, want) {
			t.Errorf("write dropped %q from config (comments/keys must survive); got:\n%s", want, out)
		}
	}
}

// -------------------- json_mcp family (gemini-cli) --------------------

// jsoncGeminiFixture is a JSONC gemini settings.json: header line comment,
// block comment, an unrelated `theme` key, an existing mcpServers entry, and a
// trailing comma after the last member.
const jsoncGeminiFixture = `{
  // Zed settings header (gemini uses the same JSONC family)
  /* hand-written block note */
  "theme": "dark",
  "mcpServers": {
    "keep-me": {"url": "https://api.example.com/mcp", "type": "http"},
  },
}`

func TestJSONC_Gemini_ReadParsesCommentsAndTrailingComma(t *testing.T) {
	g := newGeminiForTest(t, jsoncGeminiFixture)
	// (a) read path parses JSONC without error.
	e, err := g.GetEntry("keep-me")
	if err != nil {
		t.Fatalf("GetEntry on JSONC config errored (read path must tolerate comments): %v", err)
	}
	if e == nil || e.URL != "https://api.example.com/mcp" {
		t.Fatalf("GetEntry = %+v, want the keep-me url parsed from JSONC", e)
	}
}

func TestJSONC_Gemini_AddEntryPreservesCommentsAndKeys(t *testing.T) {
	g := newGeminiForTest(t, jsoncGeminiFixture)
	if err := g.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry on JSONC config: %v", err)
	}
	// (b) comments + unrelated key survive.
	assertCommentsAndUnrelatedSurvive(t, g.path, `"theme"`)
	// New entry is present AND the pre-existing entry was not clobbered.
	m := parseJSONCFile(t, g.path)
	servers, _ := m["mcpServers"].(map[string]any)
	if _, ok := servers["serena"]; !ok {
		t.Errorf("serena entry not added: %v", servers)
	}
	if _, ok := servers["keep-me"]; !ok {
		t.Errorf("pre-existing keep-me entry dropped: %v", servers)
	}
	// theme value intact.
	if got, _ := m["theme"].(string); got != "dark" {
		t.Errorf("theme = %q, want dark (unrelated key value must survive)", got)
	}
}

func TestJSONC_Gemini_RemoveEntryPreservesCommentsAndKeys(t *testing.T) {
	g := newGeminiForTest(t, jsoncGeminiFixture)
	if err := g.RemoveEntry("keep-me"); err != nil {
		t.Fatalf("RemoveEntry on JSONC config: %v", err)
	}
	// (c) comments + unrelated key survive a removal.
	assertCommentsAndUnrelatedSurvive(t, g.path, `"theme"`)
	m := parseJSONCFile(t, g.path)
	servers, _ := m["mcpServers"].(map[string]any)
	if _, ok := servers["keep-me"]; ok {
		t.Errorf("keep-me entry should have been removed: %v", servers)
	}
}

func TestJSONC_Gemini_RestoreEntryFromJSONCBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// Live config (also JSONC) currently holds a hub entry for serena.
	if err := os.WriteFile(path, []byte(`{
  // Zed settings header
  /* hand-written block note */
  "theme": "dark",
  "mcpServers": {
    "serena": {"url": "http://localhost:9121/mcp", "type": "http"},
  },
}`), 0600); err != nil {
		t.Fatal(err)
	}
	// JSONC backup holds the PRE-HUB form of serena (a remote https url).
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{
  // pre-hub backup, hand-edited
  "mcpServers": {
    "serena": {"url": "https://remote.example.com/mcp", "type": "http"}, // user's original
  },
}`), 0600); err != nil {
		t.Fatal(err)
	}
	g := &geminiCLI{jsonMCPClient: &jsonMCPClient{path: path, clientName: "gemini-cli", urlField: "url"}}
	// (d) restore round-trips through a JSONC backup without parse error.
	if err := g.RestoreEntryFromBackup(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackup from JSONC backup: %v", err)
	}
	// Live comments + unrelated key survive; serena now carries the pre-hub url.
	assertCommentsAndUnrelatedSurvive(t, path, `"theme"`)
	m := parseJSONCFile(t, path)
	servers, _ := m["mcpServers"].(map[string]any)
	serena, _ := servers["serena"].(map[string]any)
	if got, _ := serena["url"].(string); got != "https://remote.example.com/mcp" {
		t.Errorf("serena url = %q, want the pre-hub url restored from the JSONC backup", got)
	}
}

// -------------------- zed (standalone, context_servers) --------------------

// jsoncZedFixture mirrors a real Zed settings.json: the `// Zed settings`
// header, a block comment, the unrelated `wsl_connections` key, an existing
// context_servers entry, and a trailing comma.
const jsoncZedFixture = `// Zed settings header
{
  /* hand-written block note */
  "wsl_connections": [{"distro_name": "Ubuntu"}],
  "context_servers": {
    "keep-me": {"command": "node", "args": ["server.js"]},
  },
}`

func TestJSONC_Zed_ReadParsesCommentsAndTrailingComma(t *testing.T) {
	z := newZedForTest(t, jsoncZedFixture)
	// (a) read path parses JSONC without error.
	e, err := z.GetEntry("keep-me")
	if err != nil {
		t.Fatalf("GetEntry on JSONC Zed config errored: %v", err)
	}
	if e == nil {
		t.Fatal("GetEntry(keep-me) = nil; want the entry parsed from JSONC")
	}
}

func TestJSONC_Zed_AddEntryPreservesCommentsAndKeys(t *testing.T) {
	z := newZedForTest(t, jsoncZedFixture)
	exe := filepath.Join(t.TempDir(), "mcphub.exe")
	if err := z.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp", RelayExePath: exe}); err != nil {
		t.Fatalf("AddEntry on JSONC Zed config: %v", err)
	}
	// (b) the // header, block comment, AND wsl_connections survive.
	assertCommentsAndUnrelatedSurvive(t, z.path, `"wsl_connections"`)
	m := parseJSONCFile(t, z.path)
	servers, _ := m[contextServersKey].(map[string]any)
	if _, ok := servers["serena"]; !ok {
		t.Errorf("serena entry not added: %v", servers)
	}
	if _, ok := servers["keep-me"]; !ok {
		t.Errorf("pre-existing keep-me entry dropped: %v", servers)
	}
	// wsl_connections value structurally intact.
	if _, ok := m["wsl_connections"].([]any); !ok {
		t.Errorf("wsl_connections must survive as an array: %v", m["wsl_connections"])
	}
}

func TestJSONC_Zed_RemoveEntryPreservesCommentsAndKeys(t *testing.T) {
	z := newZedForTest(t, jsoncZedFixture)
	if err := z.RemoveEntry("keep-me"); err != nil {
		t.Fatalf("RemoveEntry on JSONC Zed config: %v", err)
	}
	// (c) comments + wsl_connections survive a removal.
	assertCommentsAndUnrelatedSurvive(t, z.path, `"wsl_connections"`)
	m := parseJSONCFile(t, z.path)
	servers, _ := m[contextServersKey].(map[string]any)
	if _, ok := servers["keep-me"]; ok {
		t.Errorf("keep-me entry should have been removed: %v", servers)
	}
}

func TestJSONC_Zed_RestoreEntryFromJSONCBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`// Zed settings header
{
  /* hand-written block note */
  "wsl_connections": [{"distro_name": "Ubuntu"}],
  "context_servers": {
    "serena": {"command": "C:/mcphub.exe", "args": ["relay", "--url", "http://localhost:9121/mcp"]},
  },
}`), 0600); err != nil {
		t.Fatal(err)
	}
	// JSONC backup with the pre-hub form (a plain stdio server).
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{
  // pre-hub backup
  "context_servers": {
    "serena": {"command": "uvx", "args": ["serena", "start-mcp-server"]}, // original
  },
}`), 0600); err != nil {
		t.Fatal(err)
	}
	z := &zedClient{path: path}
	// (d) restore round-trips through a JSONC backup.
	if err := z.RestoreEntryFromBackup(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackup from JSONC backup: %v", err)
	}
	assertCommentsAndUnrelatedSurvive(t, path, `"wsl_connections"`)
	m := parseJSONCFile(t, path)
	servers, _ := m[contextServersKey].(map[string]any)
	serena, _ := servers["serena"].(map[string]any)
	if got, _ := serena["command"].(string); got != "uvx" {
		t.Errorf("serena command = %q, want uvx restored from the JSONC backup", got)
	}
}

// -------------------- antigravity (relay-stdio, mcpServers) --------------------

const jsoncAntigravityFixture = `{
  // Zed settings header (antigravity shares the JSONC family)
  /* hand-written block note */
  "wsl_connections": ["placeholder"],
  "mcpServers": {
    "keep-me": {"command": "uvx", "args": ["other"], "disabled": false},
  },
}`

func TestJSONC_Antigravity_ReadParsesCommentsAndTrailingComma(t *testing.T) {
	a := newAntigravityForTest(t, jsoncAntigravityFixture)
	// (a) read path parses JSONC without error.
	e, err := a.GetEntry("keep-me")
	if err != nil {
		t.Fatalf("GetEntry on JSONC antigravity config errored: %v", err)
	}
	if e == nil {
		t.Fatal("GetEntry(keep-me) = nil; want the entry parsed from JSONC")
	}
}

func TestJSONC_Antigravity_AddEntryPreservesCommentsAndKeys(t *testing.T) {
	a := newAntigravityForTest(t, jsoncAntigravityFixture)
	exe := filepath.Join(t.TempDir(), "mcphub.exe")
	err := a.AddEntry(MCPEntry{
		Name:         "serena",
		RelayServer:  "serena",
		RelayDaemon:  "claude",
		RelayExePath: exe,
	})
	if err != nil {
		t.Fatalf("AddEntry on JSONC antigravity config: %v", err)
	}
	// (b) comments + unrelated key survive; the relay entry shape is intact.
	assertCommentsAndUnrelatedSurvive(t, a.path, `"wsl_connections"`)
	m := parseJSONCFile(t, a.path)
	servers, _ := m["mcpServers"].(map[string]any)
	serena, ok := servers["serena"].(map[string]any)
	if !ok {
		t.Fatalf("serena relay entry not added: %v", servers)
	}
	if cmd, _ := serena["command"].(string); cmd != exe {
		t.Errorf("serena command = %q, want the relay exe path (entry-shape logic preserved)", cmd)
	}
	if _, ok := servers["keep-me"]; !ok {
		t.Errorf("pre-existing keep-me entry dropped: %v", servers)
	}
}

func TestJSONC_Antigravity_RemoveEntryPreservesCommentsAndKeys(t *testing.T) {
	a := newAntigravityForTest(t, jsoncAntigravityFixture)
	if err := a.RemoveEntry("keep-me"); err != nil {
		t.Fatalf("RemoveEntry on JSONC antigravity config: %v", err)
	}
	// (c) comments + unrelated key survive a removal.
	assertCommentsAndUnrelatedSurvive(t, a.path, `"wsl_connections"`)
	m := parseJSONCFile(t, a.path)
	servers, _ := m["mcpServers"].(map[string]any)
	if _, ok := servers["keep-me"]; ok {
		t.Errorf("keep-me entry should have been removed: %v", servers)
	}
}

func TestJSONC_Antigravity_RestoreEntryFromJSONCBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp_config.json")
	if err := os.WriteFile(path, []byte(`{
  // Zed settings header
  /* hand-written block note */
  "wsl_connections": ["placeholder"],
  "mcpServers": {
    "serena": {"command": "C:/mcphub.exe", "args": ["relay", "--server", "serena", "--daemon", "claude"], "disabled": false},
  },
}`), 0600); err != nil {
		t.Fatal(err)
	}
	// JSONC backup with the pre-hub form (a plain stdio command).
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{
  // pre-hub backup
  "mcpServers": {
    "serena": {"command": "uvx", "args": ["serena", "start-mcp-server"]}, // original
  },
}`), 0600); err != nil {
		t.Fatal(err)
	}
	a := &antigravityClient{jsonMCPClient: &jsonMCPClient{path: path, clientName: "antigravity", urlField: "command"}}
	// (d) restore round-trips through a JSONC backup.
	if err := a.RestoreEntryFromBackup(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackup from JSONC backup: %v", err)
	}
	assertCommentsAndUnrelatedSurvive(t, path, `"wsl_connections"`)
	m := parseJSONCFile(t, path)
	servers, _ := m["mcpServers"].(map[string]any)
	serena, _ := servers["serena"].(map[string]any)
	if got, _ := serena["command"].(string); got != "uvx" {
		t.Errorf("serena command = %q, want uvx restored from the JSONC backup", got)
	}
}

// TestJSONC_EmptyFileFallbackIsCleanJSON asserts the empty/absent-file write
// fallback produces clean indented JSON (NOT a single packed hujson line),
// since there are no comments to preserve in that case.
func TestJSONC_EmptyFileFallbackIsCleanJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// Absent file: AddEntry must create a clean stub.
	g := &geminiCLI{jsonMCPClient: &jsonMCPClient{path: path, clientName: "gemini-cli", urlField: "url"}}
	if err := g.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry on absent file: %v", err)
	}
	raw, _ := os.ReadFile(path)
	out := string(raw)
	if !strings.Contains(out, "\n  \"mcpServers\"") {
		t.Errorf("empty-file fallback should be indented JSON, got:\n%s", out)
	}
	m := parseJSONCFile(t, path)
	servers, _ := m["mcpServers"].(map[string]any)
	if _, ok := servers["serena"]; !ok {
		t.Errorf("serena not added in empty-file fallback: %v", servers)
	}
}

// TestJSONC_DeleteAbsentIsNoOp asserts deleteMember against a missing entry in
// a comment-bearing file neither errors nor strips the comments.
func TestJSONC_DeleteAbsentIsNoOp(t *testing.T) {
	z := newZedForTest(t, jsoncZedFixture)
	if err := z.RemoveEntry("does-not-exist"); err != nil {
		t.Fatalf("RemoveEntry of absent entry should be a no-op, got: %v", err)
	}
	assertCommentsAndUnrelatedSurvive(t, z.path, `"wsl_connections"`)
}

// -------------------- vscode (standalone, top-level servers) --------------------

// jsoncVSCodeFixture mirrors a real VS Code mcp.json: the // header line
// comment, a block comment, an unrelated `inputs` key, an existing top-level
// `servers` entry, and a trailing comma after the last member.
const jsoncVSCodeFixture = `{
  // Zed settings header (vscode mcp.json is JSONC too)
  /* hand-written block note */
  "inputs": [{"id": "token", "type": "promptString"}],
  "servers": {
    "keep-me": {"type": "http", "url": "https://api.example.com/mcp"},
  },
}`

func TestJSONC_VSCode_ReadParsesCommentsAndTrailingComma(t *testing.T) {
	v := newVSCodeForTest(t, jsoncVSCodeFixture)
	// (a) read path parses JSONC without error.
	e, err := v.GetEntry("keep-me")
	if err != nil {
		t.Fatalf("GetEntry on JSONC vscode config errored (read path must tolerate comments): %v", err)
	}
	if e == nil || e.URL != "https://api.example.com/mcp" {
		t.Fatalf("GetEntry = %+v, want the keep-me url parsed from JSONC", e)
	}
}

func TestJSONC_VSCode_AddEntryPreservesCommentsAndKeys(t *testing.T) {
	v := newVSCodeForTest(t, jsoncVSCodeFixture)
	if err := v.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry on JSONC vscode config: %v", err)
	}
	// (b) comments + unrelated key survive.
	assertCommentsAndUnrelatedSurvive(t, v.path, `"inputs"`)
	m := parseJSONCFile(t, v.path)
	servers, _ := m[vscodeServersKey].(map[string]any)
	if _, ok := servers["serena"]; !ok {
		t.Errorf("serena entry not added: %v", servers)
	}
	if _, ok := servers["keep-me"]; !ok {
		t.Errorf("pre-existing keep-me entry dropped: %v", servers)
	}
	// inputs value structurally intact (unrelated key must survive).
	if _, ok := m["inputs"].([]any); !ok {
		t.Errorf("inputs must survive as an array: %v", m["inputs"])
	}
	// Entry-shape logic preserved: vscode writes type:"http" under `servers`.
	serena, _ := servers["serena"].(map[string]any)
	if got, _ := serena["type"].(string); got != "http" {
		t.Errorf("serena type = %q, want http (vscode entry-shape preserved)", got)
	}
}

func TestJSONC_VSCode_RemoveEntryPreservesCommentsAndKeys(t *testing.T) {
	v := newVSCodeForTest(t, jsoncVSCodeFixture)
	if err := v.RemoveEntry("keep-me"); err != nil {
		t.Fatalf("RemoveEntry on JSONC vscode config: %v", err)
	}
	// (c) comments + unrelated key survive a removal.
	assertCommentsAndUnrelatedSurvive(t, v.path, `"inputs"`)
	m := parseJSONCFile(t, v.path)
	servers, _ := m[vscodeServersKey].(map[string]any)
	if _, ok := servers["keep-me"]; ok {
		t.Errorf("keep-me entry should have been removed: %v", servers)
	}
}

func TestJSONC_VSCode_RestoreEntryFromJSONCBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	// Live config (also JSONC) currently holds a hub entry for serena.
	if err := os.WriteFile(path, []byte(`{
  // Zed settings header
  /* hand-written block note */
  "inputs": [],
  "servers": {
    "serena": {"type": "http", "url": "http://localhost:9121/mcp"},
  },
}`), 0600); err != nil {
		t.Fatal(err)
	}
	// JSONC backup holds the PRE-HUB form of serena (a remote https url).
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{
  // pre-hub backup, hand-edited
  "servers": {
    "serena": {"type": "http", "url": "https://remote.example.com/mcp"}, // user's original
  },
}`), 0600); err != nil {
		t.Fatal(err)
	}
	v := &vscodeClient{path: path}
	// (d) restore round-trips through a JSONC backup without parse error.
	if err := v.RestoreEntryFromBackup(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackup from JSONC backup: %v", err)
	}
	// Live comments + unrelated key survive; serena now carries the pre-hub url.
	assertCommentsAndUnrelatedSurvive(t, path, `"inputs"`)
	m := parseJSONCFile(t, path)
	servers, _ := m[vscodeServersKey].(map[string]any)
	serena, _ := servers["serena"].(map[string]any)
	if got, _ := serena["url"].(string); got != "https://remote.example.com/mcp" {
		t.Errorf("serena url = %q, want the pre-hub url restored from the JSONC backup", got)
	}
}

// -------------------- opencode (standalone, top-level mcp) --------------------

// jsoncOpenCodeFixture mirrors a real OpenCode opencode.json/.jsonc: the //
// header, a block comment, an unrelated `$schema` key, an existing `mcp` entry,
// and a trailing comma.
const jsoncOpenCodeFixture = `{
  // Zed settings header (opencode supports a .jsonc variant)
  /* hand-written block note */
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "keep-me": {"type": "remote", "url": "https://api.example.com/mcp", "enabled": true},
  },
}`

func TestJSONC_OpenCode_ReadParsesCommentsAndTrailingComma(t *testing.T) {
	o := newOpenCodeForTest(t, jsoncOpenCodeFixture)
	e, err := o.GetEntry("keep-me")
	if err != nil {
		t.Fatalf("GetEntry on JSONC opencode config errored: %v", err)
	}
	if e == nil || e.URL != "https://api.example.com/mcp" {
		t.Fatalf("GetEntry = %+v, want the keep-me url parsed from JSONC", e)
	}
}

func TestJSONC_OpenCode_AddEntryPreservesCommentsAndKeys(t *testing.T) {
	o := newOpenCodeForTest(t, jsoncOpenCodeFixture)
	if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry on JSONC opencode config: %v", err)
	}
	assertCommentsAndUnrelatedSurvive(t, o.path, `"$schema"`)
	m := parseJSONCFile(t, o.path)
	servers, _ := m[openCodeMCPKey].(map[string]any)
	if _, ok := servers["serena"]; !ok {
		t.Errorf("serena entry not added: %v", servers)
	}
	if _, ok := servers["keep-me"]; !ok {
		t.Errorf("pre-existing keep-me entry dropped: %v", servers)
	}
	if got, _ := m["$schema"].(string); got != "https://opencode.ai/config.json" {
		t.Errorf("$schema = %q, want the original (unrelated key value must survive)", got)
	}
	// Entry-shape logic preserved: opencode writes type:"remote" + enabled:true.
	serena, _ := servers["serena"].(map[string]any)
	if got, _ := serena["type"].(string); got != "remote" {
		t.Errorf("serena type = %q, want remote (opencode entry-shape preserved)", got)
	}
	if enabled, _ := serena["enabled"].(bool); !enabled {
		t.Errorf("serena enabled = %v, want true (opencode entry-shape preserved)", serena["enabled"])
	}
}

func TestJSONC_OpenCode_RemoveEntryPreservesCommentsAndKeys(t *testing.T) {
	o := newOpenCodeForTest(t, jsoncOpenCodeFixture)
	if err := o.RemoveEntry("keep-me"); err != nil {
		t.Fatalf("RemoveEntry on JSONC opencode config: %v", err)
	}
	assertCommentsAndUnrelatedSurvive(t, o.path, `"$schema"`)
	m := parseJSONCFile(t, o.path)
	servers, _ := m[openCodeMCPKey].(map[string]any)
	if _, ok := servers["keep-me"]; ok {
		t.Errorf("keep-me entry should have been removed: %v", servers)
	}
}

func TestJSONC_OpenCode_RestoreEntryFromJSONCBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(path, []byte(`{
  // Zed settings header
  /* hand-written block note */
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "serena": {"type": "remote", "url": "http://localhost:9121/mcp", "enabled": true},
  },
}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{
  // pre-hub backup
  "mcp": {
    "serena": {"type": "remote", "url": "https://remote.example.com/mcp", "enabled": true}, // original
  },
}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &openCodeClient{path: path}
	if err := o.RestoreEntryFromBackup(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackup from JSONC backup: %v", err)
	}
	assertCommentsAndUnrelatedSurvive(t, path, `"$schema"`)
	m := parseJSONCFile(t, path)
	servers, _ := m[openCodeMCPKey].(map[string]any)
	serena, _ := servers["serena"].(map[string]any)
	if got, _ := serena["url"].(string); got != "https://remote.example.com/mcp" {
		t.Errorf("serena url = %q, want the pre-hub url restored from the JSONC backup", got)
	}
}

// -------------------- claude-code (standalone, top-level mcpServers) --------------------

// jsoncClaudeCodeFixture mirrors a hand-edited ~/.claude.json: the // header, a
// block comment, an unrelated `numStartups` key, an existing `mcpServers`
// entry, and a trailing comma.
const jsoncClaudeCodeFixture = `{
  // Zed settings header (.claude.json may be hand-edited)
  /* hand-written block note */
  "numStartups": 42,
  "mcpServers": {
    "keep-me": {"type": "http", "url": "https://api.example.com/mcp"},
  },
}`

func TestJSONC_ClaudeCode_ReadParsesCommentsAndTrailingComma(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(jsoncClaudeCodeFixture), 0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	e, err := c.GetEntry("keep-me")
	if err != nil {
		t.Fatalf("GetEntry on JSONC .claude.json errored: %v", err)
	}
	if e == nil || e.URL != "https://api.example.com/mcp" {
		t.Fatalf("GetEntry = %+v, want the keep-me url parsed from JSONC", e)
	}
}

func TestJSONC_ClaudeCode_AddEntryPreservesCommentsAndKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(jsoncClaudeCodeFixture), 0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	if err := c.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry on JSONC .claude.json: %v", err)
	}
	assertCommentsAndUnrelatedSurvive(t, path, `"numStartups"`)
	m := parseJSONCFile(t, path)
	servers, _ := m[claudeCodeMCPServersKey].(map[string]any)
	if _, ok := servers["serena"]; !ok {
		t.Errorf("serena entry not added: %v", servers)
	}
	if _, ok := servers["keep-me"]; !ok {
		t.Errorf("pre-existing keep-me entry dropped: %v", servers)
	}
	// numStartups value intact (parsed back as a float64 by encoding/json).
	if got, _ := m["numStartups"].(float64); got != 42 {
		t.Errorf("numStartups = %v, want 42 (unrelated key value must survive)", m["numStartups"])
	}
}

func TestJSONC_ClaudeCode_RemoveEntryPreservesCommentsAndKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(jsoncClaudeCodeFixture), 0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	if err := c.RemoveEntry("keep-me"); err != nil {
		t.Fatalf("RemoveEntry on JSONC .claude.json: %v", err)
	}
	assertCommentsAndUnrelatedSurvive(t, path, `"numStartups"`)
	m := parseJSONCFile(t, path)
	servers, _ := m[claudeCodeMCPServersKey].(map[string]any)
	if _, ok := servers["keep-me"]; ok {
		t.Errorf("keep-me entry should have been removed: %v", servers)
	}
}

func TestJSONC_ClaudeCode_RestoreEntryFromJSONCBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(`{
  // Zed settings header
  /* hand-written block note */
  "numStartups": 42,
  "mcpServers": {
    "serena": {"type": "http", "url": "http://localhost:9121/mcp"},
  },
}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{
  // pre-hub backup
  "mcpServers": {
    "serena": {"type": "http", "url": "https://remote.example.com/mcp"}, // original
  },
}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &claudeCode{path: path}
	if err := c.RestoreEntryFromBackup(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackup from JSONC backup: %v", err)
	}
	assertCommentsAndUnrelatedSurvive(t, path, `"numStartups"`)
	m := parseJSONCFile(t, path)
	servers, _ := m[claudeCodeMCPServersKey].(map[string]any)
	serena, _ := servers["serena"].(map[string]any)
	if got, _ := serena["url"].(string); got != "https://remote.example.com/mcp" {
		t.Errorf("serena url = %q, want the pre-hub url restored from the JSONC backup", got)
	}
}

// -------------------- openclaw (standalone, NESTED mcp.servers) --------------------

// jsoncOpenClawFixture mirrors a hand-edited ~/.openclaw/openclaw.json: the //
// header, a block comment, an unrelated top-level `$schema` key, a sibling key
// on the `mcp` object (sessionIdleTtlMs) that must survive alongside the
// servers map, an existing nested `mcp.servers` entry, and trailing commas.
const jsoncOpenClawFixture = `{
  // Zed settings header (openclaw config is hand-edited)
  /* hand-written block note */
  "$schema": "https://openclaw.ai/config.json",
  "mcp": {
    "sessionIdleTtlMs": 60000,
    "servers": {
      "keep-me": {"url": "https://api.example.com/mcp", "transport": "streamable-http", "enabled": true},
    },
  },
}`

func TestJSONC_OpenClaw_ReadParsesCommentsAndTrailingComma(t *testing.T) {
	o := newOpenClawForTest(t, jsoncOpenClawFixture)
	e, err := o.GetEntry("keep-me")
	if err != nil {
		t.Fatalf("GetEntry on JSONC openclaw config errored: %v", err)
	}
	if e == nil || e.URL != "https://api.example.com/mcp" {
		t.Fatalf("GetEntry = %+v, want the keep-me url parsed from nested JSONC", e)
	}
}

func TestJSONC_OpenClaw_AddEntryPreservesCommentsAndKeys(t *testing.T) {
	o := newOpenClawForTest(t, jsoncOpenClawFixture)
	if err := o.AddEntry(MCPEntry{Name: "serena", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry on JSONC openclaw config: %v", err)
	}
	// (b) comments + unrelated top-level key survive.
	assertCommentsAndUnrelatedSurvive(t, o.path, `"$schema"`)
	// The sibling key on the `mcp` object must ALSO survive (nested-key path).
	raw, err := os.ReadFile(o.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "sessionIdleTtlMs") {
		t.Errorf("write dropped the mcp sibling key sessionIdleTtlMs; got:\n%s", raw)
	}
	m := parseJSONCFile(t, o.path)
	servers := serversFromMap(m)
	if servers == nil {
		t.Fatalf("nested mcp.servers missing after AddEntry: %v", m)
	}
	if _, ok := servers["serena"]; !ok {
		t.Errorf("serena entry not added: %v", servers)
	}
	if _, ok := servers["keep-me"]; !ok {
		t.Errorf("pre-existing keep-me entry dropped: %v", servers)
	}
	// Entry-shape logic preserved: openclaw writes url + transport + enabled.
	serena, _ := servers["serena"].(map[string]any)
	if got, _ := serena["transport"].(string); got != "streamable-http" {
		t.Errorf("serena transport = %q, want streamable-http (openclaw entry-shape preserved)", got)
	}
	// The mcp sibling value is structurally intact.
	mcp, _ := m[openClawMCPKey].(map[string]any)
	if got, _ := mcp["sessionIdleTtlMs"].(float64); got != 60000 {
		t.Errorf("mcp.sessionIdleTtlMs = %v, want 60000 (mcp sibling value must survive)", mcp["sessionIdleTtlMs"])
	}
}

func TestJSONC_OpenClaw_RemoveEntryPreservesCommentsAndKeys(t *testing.T) {
	o := newOpenClawForTest(t, jsoncOpenClawFixture)
	if err := o.RemoveEntry("keep-me"); err != nil {
		t.Fatalf("RemoveEntry on JSONC openclaw config: %v", err)
	}
	assertCommentsAndUnrelatedSurvive(t, o.path, `"$schema"`)
	raw, err := os.ReadFile(o.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "sessionIdleTtlMs") {
		t.Errorf("write dropped the mcp sibling key sessionIdleTtlMs; got:\n%s", raw)
	}
	m := parseJSONCFile(t, o.path)
	servers := serversFromMap(m)
	if _, ok := servers["keep-me"]; ok {
		t.Errorf("keep-me entry should have been removed: %v", servers)
	}
}

func TestJSONC_OpenClaw_RestoreEntryFromJSONCBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	if err := os.WriteFile(path, []byte(`{
  // Zed settings header
  /* hand-written block note */
  "$schema": "https://openclaw.ai/config.json",
  "mcp": {
    "sessionIdleTtlMs": 60000,
    "servers": {
      "serena": {"url": "http://localhost:9121/mcp", "transport": "streamable-http", "enabled": true},
    },
  },
}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := path + ".bak-mcp-local-hub-20260101-000000"
	if err := os.WriteFile(backup, []byte(`{
  // pre-hub backup
  "mcp": {
    "servers": {
      "serena": {"url": "https://remote.example.com/mcp", "transport": "streamable-http", "enabled": true}, // original
    },
  },
}`), 0600); err != nil {
		t.Fatal(err)
	}
	o := &openClawClient{path: path}
	if err := o.RestoreEntryFromBackup(backup, "serena"); err != nil {
		t.Fatalf("RestoreEntryFromBackup from JSONC backup: %v", err)
	}
	assertCommentsAndUnrelatedSurvive(t, path, `"$schema"`)
	m := parseJSONCFile(t, path)
	servers := serversFromMap(m)
	serena, _ := servers["serena"].(map[string]any)
	if got, _ := serena["url"].(string); got != "https://remote.example.com/mcp" {
		t.Errorf("serena url = %q, want the pre-hub url restored from the JSONC backup", got)
	}
}

// -------------------- parseJSONCBytes helper contracts --------------------

// TestParseJSONCBytes_DoesNotMutateInput pins the single-owner defensive-copy
// fix (deep-review P4): hujson.Standardize overwrites comment bytes with spaces
// IN PLACE, so without the internal copy parseJSONCBytes would clobber the
// caller's slice — corrupting a later comment-preserving Pack() of the same
// bytes. The input must be byte-for-byte unchanged after the call.
func TestParseJSONCBytes_DoesNotMutateInput(t *testing.T) {
	src := []byte("{\n  // a line comment\n  \"a\": 1, /* trailing */\n}")
	orig := make([]byte, len(src))
	copy(orig, src)

	m, err := parseJSONCBytes(src)
	if err != nil {
		t.Fatalf("parseJSONCBytes: %v", err)
	}
	if got, ok := m["a"]; !ok || got != float64(1) {
		t.Fatalf("parsed map = %#v, want a==1 (parse must still work on the internal copy)", m)
	}
	if !bytes.Equal(src, orig) {
		t.Fatalf("parseJSONCBytes mutated its input in place:\n got: %q\nwant: %q", src, orig)
	}
}

// TestParseJSONCBytes_EmptyAndNull covers the empty/whitespace and literal-null
// coercions the doc guarantees (a nil map must never escape to callers).
func TestParseJSONCBytes_EmptyAndNull(t *testing.T) {
	for _, in := range [][]byte{nil, {}, []byte("  \n\t "), []byte("null")} {
		m, err := parseJSONCBytes(in)
		if err != nil {
			t.Fatalf("parseJSONCBytes(%q): %v", in, err)
		}
		if m == nil {
			t.Fatalf("parseJSONCBytes(%q) returned a nil map; callers must never have to nil-check", in)
		}
		if len(m) != 0 {
			t.Fatalf("parseJSONCBytes(%q) = %#v, want empty map", in, m)
		}
	}
}
