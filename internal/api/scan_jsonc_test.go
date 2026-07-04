package api

import (
	"os"
	"path/filepath"
	"testing"
)

// The Servers scan turned into a 500 because Zed's (and VS Code's)
// settings.json is JSONC — comments + trailing commas — which strict
// encoding/json rejects with "invalid character '/' looking for beginning of
// value". scanZed/scanVSCode now route bytes through the JSONC preprocessor.

func TestScanZed_TolerateJSONC(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "settings.json")
	jsonc := []byte(`{
  // a leading comment, which Zed allows and strict JSON rejects
  "context_servers": {
    "serena": { "command": "serena-mcp", }, /* trailing comma + block comment */
  },
}`)
	if err := os.WriteFile(cfg, jsonc, 0o600); err != nil {
		t.Fatal(err)
	}
	entries := map[string]*ScanEntry{}
	if err := scanZed(entries, cfg); err != nil {
		t.Fatalf("scanZed must tolerate JSONC (comments + trailing commas), got: %v", err)
	}
	if _, ok := entries["serena"]; !ok {
		t.Fatalf("expected the serena entry parsed from the JSONC config; got %v", entries)
	}
}

func TestScanVSCode_TolerateJSONC(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "settings.json")
	jsonc := []byte("{\n  // vscode settings.json is JSONC too\n  \"servers\": {\n    \"mem\": { \"url\": \"http://x\" },\n  },\n}")
	if err := os.WriteFile(cfg, jsonc, 0o600); err != nil {
		t.Fatal(err)
	}
	entries := map[string]*ScanEntry{}
	if err := scanVSCode(entries, cfg); err != nil {
		t.Fatalf("scanVSCode must tolerate JSONC, got: %v", err)
	}
	if _, ok := entries["mem"]; !ok {
		t.Fatalf("expected the mem entry parsed from the JSONC config; got %v", entries)
	}
}

func TestScanCodeBuddy_TolerateJSONC(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "mcp.json")
	jsonc := []byte(`{
  // CodeBuddy documents mcp.json as JSON/JSONC.
  "mcpServers": {
    "memory": { "url": "http://localhost:9123/mcp", "type": "http", },
  },
}`)
	if err := os.WriteFile(cfg, jsonc, 0o600); err != nil {
		t.Fatal(err)
	}
	entries := map[string]*ScanEntry{}
	if err := scanCodeBuddy(entries, cfg); err != nil {
		t.Fatalf("scanCodeBuddy must tolerate JSONC (comments + trailing commas), got: %v", err)
	}
	entry, ok := entries["memory"]
	if !ok {
		t.Fatalf("expected the memory entry parsed from the JSONC config; got %v", entries)
	}
	if got := entry.ClientPresence["codebuddy"].Transport; got != "http" {
		t.Fatalf("CodeBuddy memory transport = %q, want http", got)
	}
}

// Deep-audit finding (client-adapters × wire): the ADAPTERS read/write these
// client configs via hujson (JSONC-tolerant), but the matching SCANNERS used
// strict encoding/json — a `//` comment or trailing comma an operator hand-adds
// (the docs say they DO hand-edit these files) made the scan error and drop
// EVERY row for that client from the Servers matrix, even though mcphub can
// read/write the file fine. scanClaude/scanCursor/scanGemini/scanQwen/
// scanAntigravity + the scanMCPServersJSON family now route through the JSONC
// preprocessor, matching scanVSCode/scanZed.

func jsoncMcpServers(t *testing.T, dir, file, name string) string {
	t.Helper()
	cfg := filepath.Join(dir, file)
	jsonc := []byte("{\n  // operator hand-edited this config (JSONC comment)\n  \"mcpServers\": {\n    \"" + name + "\": { \"url\": \"http://127.0.0.1:9121/mcp\", \"type\": \"http\", },\n  },\n}")
	if err := os.WriteFile(cfg, jsonc, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestScanCursor_TolerateJSONC(t *testing.T) {
	cfg := jsoncMcpServers(t, t.TempDir(), "mcp.json", "serena")
	entries := map[string]*ScanEntry{}
	if err := scanCursor(entries, cfg); err != nil {
		t.Fatalf("scanCursor must tolerate JSONC, got: %v", err)
	}
	if _, ok := entries["serena"]; !ok {
		t.Fatalf("serena row dropped from a JSONC cursor config; got %v", entries)
	}
}

func TestScanClaude_TolerateJSONC(t *testing.T) {
	cfg := jsoncMcpServers(t, t.TempDir(), ".claude.json", "serena")
	entries := map[string]*ScanEntry{}
	if err := scanClaude(entries, cfg); err != nil {
		t.Fatalf("scanClaude must tolerate JSONC, got: %v", err)
	}
	if _, ok := entries["serena"]; !ok {
		t.Fatalf("serena row dropped from a JSONC claude config; got %v", entries)
	}
}

func TestScanGemini_TolerateJSONC(t *testing.T) {
	cfg := jsoncMcpServers(t, t.TempDir(), "settings.json", "mem")
	entries := map[string]*ScanEntry{}
	if err := scanGemini(entries, cfg); err != nil {
		t.Fatalf("scanGemini must tolerate JSONC, got: %v", err)
	}
	if _, ok := entries["mem"]; !ok {
		t.Fatalf("mem row dropped from a JSONC gemini config; got %v", entries)
	}
}

func TestScanQwen_TolerateJSONC(t *testing.T) {
	cfg := jsoncMcpServers(t, t.TempDir(), "settings.json", "mem")
	entries := map[string]*ScanEntry{}
	if err := scanQwen(entries, cfg); err != nil {
		t.Fatalf("scanQwen must tolerate JSONC, got: %v", err)
	}
	if _, ok := entries["mem"]; !ok {
		t.Fatalf("mem row dropped from a JSONC qwen config; got %v", entries)
	}
}

func TestScanAntigravity_TolerateJSONC(t *testing.T) {
	// Antigravity keys its endpoint on "serverUrl" (shapeAntigravityEntry,
	// scan.go), so use the real key — a JSONC config must both survive the
	// strip AND classify as an http entry.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "mcp_config.json")
	jsonc := []byte("{\n  // operator hand-edited (JSONC comment)\n  \"mcpServers\": {\n    \"mem\": { \"serverUrl\": \"http://127.0.0.1:9123/mcp\", },\n  },\n}")
	if err := os.WriteFile(cfg, jsonc, 0o600); err != nil {
		t.Fatal(err)
	}
	entries := map[string]*ScanEntry{}
	if err := scanAntigravity(entries, cfg); err != nil {
		t.Fatalf("scanAntigravity must tolerate JSONC, got: %v", err)
	}
	entry, ok := entries["mem"]
	if !ok {
		t.Fatalf("mem row dropped from a JSONC antigravity config; got %v", entries)
	}
	if got := entry.ClientPresence["antigravity"].Transport; got != "http" {
		t.Fatalf("antigravity mem transport = %q, want http (serverUrl not honored through the strip)", got)
	}
}

func TestScanKiro_TolerateJSONC_FamilyDelegate(t *testing.T) {
	// scanKiro delegates to scanMCPServersJSON, which used the strict (identity)
	// prepare — now the JSONC stripper, so the whole kiro/windsurf/cline/... family
	// tolerates hand-edited JSONC.
	cfg := jsoncMcpServers(t, t.TempDir(), "mcp.json", "serena")
	entries := map[string]*ScanEntry{}
	if err := scanKiro(entries, cfg); err != nil {
		t.Fatalf("scanKiro (scanMCPServersJSON family) must tolerate JSONC, got: %v", err)
	}
	if _, ok := entries["serena"]; !ok {
		t.Fatalf("serena row dropped from a JSONC kiro config; got %v", entries)
	}
}

// Regression lock: the JSONC strip is string-aware, so a `//` inside a
// quoted value (URL / Windows path) survives — critical because every
// scanned MCP client config is dense with "url":"http://..." values. A
// future refactor that broke string-awareness would silently corrupt
// every URL-based config the scanners read; this test fails loudly first.
func TestStripJSONC_PreservesDoubleSlashInStrings(t *testing.T) {
	in := []byte(`{"url":"http://127.0.0.1:9121/mcp","path":"C:\a\b","n":1,}`)
	out := string(stripJSONCommentsAndTrailingCommas(in))
	if want := `"http://127.0.0.1:9121/mcp"`; !containsSub(out, want) {
		t.Fatalf("URL `//` eaten by strip (string-awareness broken): %s", out)
	}
	if want := `"C:\a\b"`; !containsSub(out, want) {
		t.Fatalf("Windows path escape mangled by strip: %s", out)
	}
	if containsSub(out, "1,}") {
		t.Fatalf("trailing comma not stripped: %s", out)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Deep-audit follow-up (sonnet premine caught these two scanners the first
// pass missed): scanOpenCode + scanOpenClaw were still strict encoding/json
// while their adapters (internal/clients/opencode.go, openclaw.go) both parse
// via parseJSONCBytes/hujson and DOCUMENT operator hand-editing with comments.

func TestScanOpenCode_TolerateJSONC(t *testing.T) {
	// OpenCode config shape: {"mcp": {"<name>": {...}}}.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "opencode.json")
	jsonc := []byte("{\n  // OpenCode config is JSONC (it supports a .jsonc variant)\n  \"mcp\": {\n    \"serena\": { \"type\": \"remote\", \"url\": \"http://127.0.0.1:9121/mcp\", },\n  },\n}")
	if err := os.WriteFile(cfg, jsonc, 0o600); err != nil {
		t.Fatal(err)
	}
	entries := map[string]*ScanEntry{}
	if err := scanOpenCode(entries, cfg); err != nil {
		t.Fatalf("scanOpenCode must tolerate JSONC (adapter uses parseJSONCBytes), got: %v", err)
	}
	if _, ok := entries["serena"]; !ok {
		t.Fatalf("serena row dropped from a JSONC opencode config; got %v", entries)
	}
}

func TestScanOpenClaw_TolerateJSONC(t *testing.T) {
	// OpenClaw config shape: {"mcp": {"servers": {"<name>": {...}}}}.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "openclaw.json")
	jsonc := []byte("{\n  // openclaw config carries comments + trailing commas\n  /* and block comments */\n  \"mcp\": {\n    \"servers\": {\n      \"serena\": { \"url\": \"http://127.0.0.1:9121/mcp\", },\n    },\n  },\n}")
	if err := os.WriteFile(cfg, jsonc, 0o600); err != nil {
		t.Fatal(err)
	}
	entries := map[string]*ScanEntry{}
	if err := scanOpenClaw(entries, cfg); err != nil {
		t.Fatalf("scanOpenClaw must tolerate JSONC (adapter uses parseJSONCBytes), got: %v", err)
	}
	if _, ok := entries["serena"]; !ok {
		t.Fatalf("serena row dropped from a JSONC openclaw config; got %v", entries)
	}
}

// Sibling feature reading the SAME operator-editable client configs: the
// "Create manifest" flow (ExtractManifestFromClient) read them strict too, so
// the matrix could show a JSONC row while Create-manifest failed on it. The
// opencode case already stripped (scan.go, 2026-06-30 precedent); the other
// six now match.
func TestExtractManifestFromClient_TolerateJSONC(t *testing.T) {
	tmp := t.TempDir()
	cursorPath := filepath.Join(tmp, "mcp.json")
	jsonc := []byte("{\n  // operator hand-edited ~/.cursor/mcp.json\n  \"mcpServers\": {\n    \"fetch\": { \"command\": \"uvx\", \"args\": [\"mcp-server-fetch\"], },\n  },\n}")
	if err := os.WriteFile(cursorPath, jsonc, 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("cursor", "fetch", ScanOpts{
		CursorConfigPath: cursorPath,
		ManifestDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient must tolerate JSONC, got: %v", err)
	}
	if !containsSub(yaml, "name: fetch") || !containsSub(yaml, "command: uvx") {
		t.Fatalf("extracted manifest missing expected fields from a JSONC cursor config:\n%s", yaml)
	}
}

// Third read surface: migrate-legacy detection (extractLegacyFromClaudeJSON)
// parses ~/.claude.json strict too.
func TestExtractLegacyFromClaudeJSON_TolerateJSONC(t *testing.T) {
	jsonc := []byte("{\n  // parked language-server entry an operator commented\n  \"mcpServers\": {\n    \"go-ls\": {\n      \"command\": \"mcp-language-server\",\n      \"args\": [\"--workspace\", \"/w\", \"--lsp\", \"gopls\"],\n      \"disabled\": true,\n    },\n  },\n}")
	got, err := extractLegacyFromClaudeJSON(jsonc)
	if err != nil {
		t.Fatalf("extractLegacyFromClaudeJSON must tolerate JSONC, got: %v", err)
	}
	if len(got) != 1 || got[0].EntryName != "go-ls" {
		t.Fatalf("disabled mcp-language-server entry not detected through the strip: %+v", got)
	}
}

// Completeness follow-up (sonnet completeness-verify): ExtractManifestFromClient
// widened SIX cases but only the cursor case had a JSONC regression-lock. The
// vscode case reads a structurally-different "servers" key and the antigravity
// case has extra relay-loop-guard logic after the parse — both deserve their
// own lock so a future refactor of those branches cannot silently regress the
// JSONC path.
func TestExtractManifestFromClient_VSCode_TolerateJSONC(t *testing.T) {
	tmp := t.TempDir()
	// VS Code's ExtractManifest case keys on "servers" (not "mcpServers").
	path := filepath.Join(tmp, "mcp.json")
	jsonc := []byte("{\n  // operator hand-edited .vscode/mcp.json\n  \"servers\": {\n    \"fetch\": { \"command\": \"uvx\", \"args\": [\"mcp-server-fetch\"], },\n  },\n}")
	if err := os.WriteFile(path, jsonc, 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("vscode", "fetch", ScanOpts{
		VSCodeConfigPath: path,
		ManifestDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient(vscode) must tolerate JSONC, got: %v", err)
	}
	if !containsSub(yaml, "name: fetch") || !containsSub(yaml, "command: uvx") {
		t.Fatalf("extracted manifest missing expected fields from a JSONC vscode config:\n%s", yaml)
	}
}

func TestExtractManifestFromClient_Antigravity_TolerateJSONC(t *testing.T) {
	tmp := t.TempDir()
	// A genuine stdio entry (not a mcphub relay), so the post-parse
	// relay-loop-guard passes and the manifest extracts.
	path := filepath.Join(tmp, "mcp_config.json")
	jsonc := []byte("{\n  // operator hand-edited antigravity mcp_config.json\n  \"mcpServers\": {\n    \"fetch\": { \"command\": \"uvx\", \"args\": [\"mcp-server-fetch\"], },\n  },\n}")
	if err := os.WriteFile(path, jsonc, 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	yaml, err := a.ExtractManifestFromClient("antigravity", "fetch", ScanOpts{
		AntigravityConfigPath: path,
		ManifestDir:           t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ExtractManifestFromClient(antigravity) must tolerate JSONC, got: %v", err)
	}
	if !containsSub(yaml, "name: fetch") || !containsSub(yaml, "command: uvx") {
		t.Fatalf("extracted manifest missing expected fields from a JSONC antigravity config:\n%s", yaml)
	}
}
