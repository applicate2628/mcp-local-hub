package clients

import (
	"os"
	"path/filepath"
	"testing"
)

// writeMimoFile is a small helper for the r15 multi-layer tests.
func writeMimoFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// stdioLSPEntryNames returns the set of names FindStdioLanguageServerEntries
// reports for a client.
func stdioLSPEntryNames(t *testing.T, o *mimoCodeClient) map[string]bool {
	t.Helper()
	ls, err := o.FindStdioLanguageServerEntries()
	if err != nil {
		t.Fatalf("FindStdioLanguageServerEntries: %v", err)
	}
	got := map[string]bool{}
	for _, e := range ls {
		got[e.Name] = true
	}
	return got
}

// TestMimoCode_FindStdioLanguageServer_DeclinesWhenAnotherLayerDefines pins bot PR
// #420 r15 finding 2 (false-success delete). FindStdioLanguageServerEntries' two
// consumers are DESTRUCTIVE (RemoveEntry on each returned entry, write-target
// ONLY). When the SAME name is the stdio-LSP shape in the write target AND is also
// defined in ANOTHER layer, RemoveEntry only deletes the write-target copy and the
// other layer RE-EMERGES in the merged read — so the cleanup must DECLINE (report
// only when the write target is the SOLE definer). A genuinely sole write-target
// stdio LSP entry must still be reported (removable).
func TestMimoCode_FindStdioLanguageServer_DeclinesWhenAnotherLayerDefines(t *testing.T) {
	const stdioLS = `{"type":"local","command":["mcp-language-server","--lsp","go"],"enabled":true}`

	t.Run("config.json BELOW re-emerges a NON-LSP dup -> active LSP IS removable (reported)", func(t *testing.T) {
		// bot PR #425 re-resolve redesign (architect PASS): the LSP cleanup's
		// re-resolve predicate now tests post-removal ACTIVE mcp-language-server
		// membership (findLanguageServerStdioInMap), NOT bare name-presence. The
		// write target's `dup` IS the stdio-LSP entry; config.json BELOW defines a
		// SAME-NAMED but NON-LSP (remote) `dup`. Under the replace-by-name merge,
		// removing the write-target key leaves the config.json REMOTE — which is not
		// an active mcp-language-server. So after RemoveEntry NO active LSP `dup`
		// survives → the duplicate-LSP cleanup's job is done → `dup` must be REPORTED
		// removable. RemoveEntry touches only the write target; the operator's lower
		// remote re-emerges untouched (no wrong-delete, no orphan).
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
			`{"mcp":{"dup":`+stdioLS+`,"sole":`+stdioLS+`}}`)
		writeMimoFile(t, filepath.Join(dir, "config.json"),
			`{"mcp":{"dup":{"type":"remote","url":"http://below/mcp","enabled":true}}}`)
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		got := stdioLSPEntryNames(t, o)
		if !got["dup"] {
			t.Errorf("a NON-LSP config.json-below `dup` is not an active mcp-language-server — the active write-target LSP IS removable, must be REPORTED, got %v", got)
		}
		if !got["sole"] {
			t.Errorf("`sole` is the only definer — must be reported (removable), got %v", got)
		}
	})

	t.Run("an ACTIVE LSP-shaped re-emergence in config.json BELOW still blocks removal -> decline", func(t *testing.T) {
		// Decline-path coverage (replaces the former bare-name control): the ONLY
		// re-emergence that blocks the LSP cleanup now is one that re-emerges as
		// ANOTHER ACTIVE stdio mcp-language-server. config.json BELOW defines `dup`
		// as an ENABLED mcp-language-server (--lsp go) just like the write target.
		// After RemoveEntry clears the write-target key, the config.json LSP entry
		// RE-EMERGES active → the duplicate LSP stays → the cleanup must DECLINE.
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
			`{"mcp":{"dup":`+stdioLS+`}}`)
		writeMimoFile(t, filepath.Join(dir, "config.json"),
			`{"mcp":{"dup":`+stdioLS+`}}`)
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		got := stdioLSPEntryNames(t, o)
		if got["dup"] {
			t.Errorf("an ENABLED LSP-shaped config.json-below `dup` re-emerges as an active mcp-language-server after RemoveEntry — must DECLINE, got %v", got)
		}
	})

	t.Run("a DISABLED config.json-below definition does NOT block removal -> reported", func(t *testing.T) {
		// bot PR #425 P2 — "do not skip removable entries behind disabled lower
		// layers". The write target has the ACTIVE stdio-LSP `dup`; config.json BELOW
		// has a SAME-NAMED but DISABLED (enabled:false) entry. mimoCodeDropDisabled
		// drops that disabled below entry from the merged-effective view, so after
		// RemoveEntry clears the write-target key NOTHING active re-emerges — the
		// active stdio-LSP IS removable and must be REPORTED. Pre-fix the below-layer
		// check used bare name-presence (mimoCodeFileDefines) and wrongly DECLINED,
		// leaving the active direct gopls in place → duplicate LSP.
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
			`{"mcp":{"dup":`+stdioLS+`}}`)
		writeMimoFile(t, filepath.Join(dir, "config.json"),
			`{"mcp":{"dup":{"type":"local","command":["x"],"enabled":false}}}`)
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		got := stdioLSPEntryNames(t, o)
		if !got["dup"] {
			t.Errorf("a DISABLED config.json-below entry cannot re-emerge active — `dup` must be REPORTED (removable), got %v", got)
		}
	})

	t.Run("an ENABLED NON-LSP stdio config.json-below re-emergence -> active LSP IS removable (reported)", func(t *testing.T) {
		// bot PR #425 re-resolve redesign (architect PASS): an ENABLED but NON-LSP
		// stdio re-emergence ({command:[x]}, no --lsp) is an active stdio server but
		// NOT an active mcp-language-server, so it does NOT satisfy the LSP cleanup's
		// re-resolve predicate (findLanguageServerStdioInMap). Removing the
		// write-target LSP value leaves only the non-LSP `x` server → no duplicate
		// active mcp-language-server survives → `dup` must be REPORTED removable. (The
		// LSP-shaped-re-emergence decline path is pinned by the subtest above.)
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
			`{"mcp":{"dup":`+stdioLS+`}}`)
		writeMimoFile(t, filepath.Join(dir, "config.json"),
			`{"mcp":{"dup":{"type":"local","command":["x"],"enabled":true}}}`)
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		got := stdioLSPEntryNames(t, o)
		if !got["dup"] {
			t.Errorf("an ENABLED NON-LSP stdio config.json-below `dup` is not an active mcp-language-server — the active write-target LSP IS removable, must be REPORTED, got %v", got)
		}
	})

	t.Run("MIMOCODE_CONFIG_DIR overlay (higher) also defines -> decline", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		overlayDir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
			`{"mcp":{"dup":`+stdioLS+`}}`)
		// A higher overlay layer also defines `dup`.
		writeMimoFile(t, filepath.Join(overlayDir, "mimocode.json"),
			`{"mcp":{"dup":{"type":"remote","url":"http://overlay/mcp","enabled":true}}}`)
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json"), overlayDir: overlayDir}
		got := stdioLSPEntryNames(t, o)
		if got["dup"] {
			t.Errorf("`dup` re-resolves from the overlay after RemoveEntry — must DECLINE, got %v", got)
		}
	})

	t.Run("sole write-target stdio LSP entry IS reported", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
			`{"mcp":{"only":`+stdioLS+`}}`)
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		got := stdioLSPEntryNames(t, o)
		if !got["only"] {
			t.Errorf("a sole write-target stdio LSP entry must be reported (removable), got %v", got)
		}
	})
}

// TestMimoCode_FindStdioLanguageServer_ClaudeImportReEmergence pins the CRITICAL
// r15 finding 2 asymmetry: the ~/.claude.json import IS a re-emergence source for
// the destructive cleanup (the OPPOSITE polarity from the AddEntry shadow guard,
// which correctly EXCLUDES the import). The import is SKIP-IF-NAME-EXISTS: while
// the write target defines the name the import is skipped, but once RemoveEntry
// deletes the write-target key the import no longer skips it → the entry
// RE-EMERGES.
//
// bot PR #425 re-resolve redesign (architect PASS): the LSP cleanup's re-resolve
// predicate now tests post-removal ACTIVE mcp-language-server membership
// (findLanguageServerStdioInMap), not bare name-presence. So a same-name import
// re-emergence only DECLINES the LSP cleanup when it re-emerges as ANOTHER ACTIVE
// stdio mcp-language-server; a NON-LSP import re-emergence (e.g. `npx`) leaves no
// active LSP duplicate → the write-target LSP IS removable (reported).
func TestMimoCode_FindStdioLanguageServer_ClaudeImportReEmergence(t *testing.T) {
	const stdioLS = `{"type":"local","command":["mcp-language-server","--lsp","go"],"enabled":true}`

	newGlobalDir := func(t *testing.T, home string) string {
		t.Helper()
		globalDir := filepath.Join(home, ".config", "mimocode")
		if err := os.MkdirAll(globalDir, 0o755); err != nil {
			t.Fatal(err)
		}
		return globalDir
	}

	t.Run("NON-LSP claude import re-emergence -> active LSP IS removable (reported)", func(t *testing.T) {
		// The import re-emerges `dup` as a NON-LSP stdio server (`npx`), not an
		// mcp-language-server. After RemoveEntry clears the write-target LSP value, no
		// active mcp-language-server `dup` survives → `dup` must be REPORTED removable.
		isolateMimoCodeEnv(t)
		t.Setenv(MimoCodeDisableClaudeImportEnv, "") // re-enable the import for this test
		home := t.TempDir()
		globalDir := newGlobalDir(t, home)
		writeMimoFile(t, filepath.Join(home, ".claude.json"),
			`{"mcpServers":{"dup":{"command":"npx","args":["-y","dup"]}}}`)
		// Write target: `dup` is the stdio-LSP shape (claude currently skips it).
		writeMimoFile(t, filepath.Join(globalDir, "mimocode.json"),
			`{"mcp":{"dup":`+stdioLS+`,"sole":`+stdioLS+`}}`)
		o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json"), claudeHome: home}
		got := stdioLSPEntryNames(t, o)
		if !got["dup"] {
			t.Errorf("a NON-LSP `npx` claude import re-emergence is not an active mcp-language-server — the active write-target LSP IS removable, must be REPORTED, got %v", got)
		}
		if !got["sole"] {
			t.Errorf("`sole` (no other layer / no import) must still be reported, got %v", got)
		}
	})

	t.Run("an LSP-shaped claude import re-emergence -> decline", func(t *testing.T) {
		// Decline-path coverage for the import source: ~/.claude.json defines `dup`
		// as a mcp-language-server (--lsp go). mimoCodeFromClaude projects it to a
		// local entry with command [mcp-language-server, --lsp, go]. After RemoveEntry
		// clears the write-target key, skip-if-name-exists stops firing and the import
		// RE-EMERGES an ACTIVE mcp-language-server → the duplicate LSP stays → DECLINE.
		isolateMimoCodeEnv(t)
		t.Setenv(MimoCodeDisableClaudeImportEnv, "")
		home := t.TempDir()
		globalDir := newGlobalDir(t, home)
		writeMimoFile(t, filepath.Join(home, ".claude.json"),
			`{"mcpServers":{"dup":{"command":"mcp-language-server","args":["--lsp","go"]}}}`)
		writeMimoFile(t, filepath.Join(globalDir, "mimocode.json"),
			`{"mcp":{"dup":`+stdioLS+`,"sole":`+stdioLS+`}}`)
		o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json"), claudeHome: home}
		got := stdioLSPEntryNames(t, o)
		if got["dup"] {
			t.Errorf("an LSP-shaped ~/.claude.json import re-emerges as an active mcp-language-server after RemoveEntry — must DECLINE, got %v", got)
		}
		if !got["sole"] {
			t.Errorf("`sole` (no other layer / no import) must still be reported, got %v", got)
		}
	})

	t.Run("import disabled -> no re-emergence -> reported", func(t *testing.T) {
		isolateMimoCodeEnv(t) // sets MIMOCODE_DISABLE_CLAUDE_CODE_MCP=1
		home := t.TempDir()
		globalDir := newGlobalDir(t, home)
		// The same claude.json exists, but the import is DISABLED, so it can never
		// re-emerge — the write target is the sole definer → report.
		writeMimoFile(t, filepath.Join(home, ".claude.json"),
			`{"mcpServers":{"dup":{"command":"npx"}}}`)
		writeMimoFile(t, filepath.Join(globalDir, "mimocode.json"),
			`{"mcp":{"dup":`+stdioLS+`}}`)
		o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json"), claudeHome: home}
		got := stdioLSPEntryNames(t, o)
		if !got["dup"] {
			t.Errorf("with the import disabled there is no re-emergence — `dup` must be reported, got %v", got)
		}
	})
}

// TestMimoCodeHasClaudeImport pins bot PR #420 r15 finding 1 (the scan-side
// helper): MimoCodeHasClaudeImport reports a parseable ~/.claude.json import that
// yields at least one importable entry, using the SAME state-safe env gate as the
// read path (a temp/non-global path or the disable flag -> no import).
func TestMimoCodeHasClaudeImport(t *testing.T) {
	newGlobalDir := func(t *testing.T, home string) string {
		t.Helper()
		globalDir := filepath.Join(home, ".config", "mimocode")
		if err := os.MkdirAll(globalDir, 0o755); err != nil {
			t.Fatal(err)
		}
		return globalDir
	}

	t.Run("parseable import with entries -> true", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		t.Setenv(MimoCodeDisableClaudeImportEnv, "")
		home := t.TempDir()
		globalDir := newGlobalDir(t, home)
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		writeMimoFile(t, filepath.Join(home, ".claude.json"),
			`{"mcpServers":{"ctx7":{"command":"npx"}}}`)
		if !MimoCodeHasClaudeImport(filepath.Join(globalDir, "mimocode.json")) {
			t.Error("a parseable ~/.claude.json with importable entries must report HasClaudeImport=true")
		}
	})

	t.Run("import disabled -> false (state-safe gate)", func(t *testing.T) {
		isolateMimoCodeEnv(t) // disable flag set
		home := t.TempDir()
		globalDir := newGlobalDir(t, home)
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		writeMimoFile(t, filepath.Join(home, ".claude.json"),
			`{"mcpServers":{"ctx7":{"command":"npx"}}}`)
		if MimoCodeHasClaudeImport(filepath.Join(globalDir, "mimocode.json")) {
			t.Error("MIMOCODE_DISABLE_CLAUDE_CODE_MCP must suppress the import → HasClaudeImport=false")
		}
	})

	t.Run("temp/non-global path -> false (no env-resolved import)", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		t.Setenv(MimoCodeDisableClaudeImportEnv, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		writeMimoFile(t, filepath.Join(home, ".claude.json"),
			`{"mcpServers":{"ctx7":{"command":"npx"}}}`)
		// A non-global-layer basename collapses to single-file mode (claudeHome "").
		if MimoCodeHasClaudeImport(filepath.Join(t.TempDir(), "explicit-override.json")) {
			t.Error("a temp/non-global override path must NOT resolve the import (state-safe) → false")
		}
	})

	t.Run("malformed claude json -> false (best-effort, imports nothing)", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		t.Setenv(MimoCodeDisableClaudeImportEnv, "")
		home := t.TempDir()
		globalDir := newGlobalDir(t, home)
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		writeMimoFile(t, filepath.Join(home, ".claude.json"), `{ this is not json`)
		if MimoCodeHasClaudeImport(filepath.Join(globalDir, "mimocode.json")) {
			t.Error("a malformed ~/.claude.json must import nothing → HasClaudeImport=false")
		}
	})

	t.Run("absent claude json -> false", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		t.Setenv(MimoCodeDisableClaudeImportEnv, "")
		home := t.TempDir()
		globalDir := newGlobalDir(t, home)
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		// No ~/.claude.json written.
		if MimoCodeHasClaudeImport(filepath.Join(globalDir, "mimocode.json")) {
			t.Error("an absent ~/.claude.json must report HasClaudeImport=false")
		}
	})
}
