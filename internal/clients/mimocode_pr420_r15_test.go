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

	t.Run("config.json BELOW also defines -> decline", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		// Write target (mimocode.json): stdio-LSP shape for `dup`.
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
			`{"mcp":{"dup":`+stdioLS+`,"sole":`+stdioLS+`}}`)
		// Lower layer (config.json) ALSO defines `dup` — removing the write-target
		// copy lets this re-emerge, so the LSP entry stays active.
		writeMimoFile(t, filepath.Join(dir, "config.json"),
			`{"mcp":{"dup":{"type":"remote","url":"http://below/mcp","enabled":true}}}`)
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		got := stdioLSPEntryNames(t, o)
		if got["dup"] {
			t.Errorf("`dup` re-emerges from config.json after RemoveEntry — must DECLINE, got %v", got)
		}
		if !got["sole"] {
			t.Errorf("`sole` is the only definer — must be reported (removable), got %v", got)
		}
	})

	t.Run("a disabled definition in another layer still COUNTS -> decline", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		writeMimoFile(t, filepath.Join(dir, "mimocode.json"),
			`{"mcp":{"dup":`+stdioLS+`}}`)
		// Lower layer defines `dup` with enabled:false. The deep-merge keeps the KEY
		// present (only the matcher drops disabled), so after RemoveEntry the name is
		// still PRESENT in the merged config → the destructive removal did not clear
		// it → decline.
		writeMimoFile(t, filepath.Join(dir, "config.json"),
			`{"mcp":{"dup":{"type":"local","command":["x"],"enabled":false}}}`)
		o := &mimoCodeClient{path: filepath.Join(dir, "mimocode.json")}
		got := stdioLSPEntryNames(t, o)
		if got["dup"] {
			t.Errorf("a disabled same-name definition in another layer must still cause a DECLINE, got %v", got)
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
// RE-EMERGES. So a name defined in BOTH the write target (stdio-LSP) AND
// ~/.claude.json must be DECLINED (cleanup cannot fully remove it).
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

	t.Run("name in BOTH write target AND claude import -> decline", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		t.Setenv(MimoCodeDisableClaudeImportEnv, "") // re-enable the import for this test
		home := t.TempDir()
		globalDir := newGlobalDir(t, home)
		// ~/.claude.json defines `dup` — it re-emerges once the write-target key is gone.
		writeMimoFile(t, filepath.Join(home, ".claude.json"),
			`{"mcpServers":{"dup":{"command":"npx","args":["-y","dup"]}}}`)
		// Write target: `dup` is the stdio-LSP shape (claude currently skips it).
		writeMimoFile(t, filepath.Join(globalDir, "mimocode.json"),
			`{"mcp":{"dup":`+stdioLS+`,"sole":`+stdioLS+`}}`)
		o := &mimoCodeClient{path: filepath.Join(globalDir, "mimocode.json"), claudeHome: home}
		got := stdioLSPEntryNames(t, o)
		if got["dup"] {
			t.Errorf("`dup` re-emerges from the ~/.claude.json import after RemoveEntry — must DECLINE, got %v", got)
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
