package clients

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMimoCodeNameAtOrAboveWriteTarget pins the SCAN-side ownership predicate
// MimoCodeNameAtOrAboveWriteTarget(path, name) — the single owner of "is this
// mimo server name hub-OWNABLE?" that internal/api/scan.go consults to stamp
// ClientEntry.Inherited. It wraps the content-bearing
// mimoCodeOwnedAtOrAboveWriteTarget (an enabled-only {enabled:true/false}
// overlay carries no URL/command so it does NOT own the lower/import URL) on
// the SAME mimoCodeClientForScanPath construction MimoCodeMergedConfig /
// MimoCodeHasClaudeImport use. NOTE: GetEntry separately uses the bare-presence
// mimoCodeDefinedAtOrAboveWriteTarget for the no-copy-up SourceBelowWriteTarget
// stamp — the two callers need different semantics, hence the split predicate.
//
// State-safety: every sub-test isolates the MIMOCODE_* env and (for the import
// case) redirects HOME/USERPROFILE to a temp dir so the developer's real
// ~/.config/mimocode and ~/.claude.json are never read.
func TestMimoCodeNameAtOrAboveWriteTarget(t *testing.T) {
	t.Run("write-target entry => (true, nil)", func(t *testing.T) {
		isolateMimoCodeEnv(t) // sets MIMOCODE_DISABLE_CLAUDE_CODE_MCP=1
		dir := t.TempDir()
		path := filepath.Join(dir, "mimocode.json")
		// `time` is defined in the WRITE target (mimocode.json) → at/above →
		// hub-ownable.
		if err := os.WriteFile(path,
			[]byte(`{"mcp":{"time":{"type":"remote","url":"http://localhost:9120/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		atOrAbove, err := MimoCodeNameAtOrAboveWriteTarget(path, "time")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !atOrAbove {
			t.Errorf("a write-target-resident entry must be at/above (hub-ownable) => true, got false")
		}
	})

	t.Run("config.json-below-only entry => (false, nil)", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "mimocode.json")
		// Write target EXISTS but does NOT define `time`.
		if err := os.WriteFile(path, []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// `time` is defined ONLY in the config.json layer STRICTLY BELOW the
		// write target — a layer the hub never writes → NOT hub-ownable.
		if err := os.WriteFile(filepath.Join(dir, "config.json"),
			[]byte(`{"mcp":{"time":{"type":"remote","url":"http://localhost:9120/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		atOrAbove, err := MimoCodeNameAtOrAboveWriteTarget(path, "time")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if atOrAbove {
			t.Errorf("a config.json-below-only entry must NOT be at/above (not hub-ownable) => false, got true")
		}
	})

	t.Run("write-target ENABLED-ONLY overlay over import hub URL => (false, nil)", func(t *testing.T) {
		// bot PR #420 r19 finding 1: the write target carries ONLY a bare
		// {enabled:true} overlay for `time`; the actual hub URL lives in the
		// ~/.claude.json import (a layer the hub never wrote). The enabled-only stub
		// does NOT own the URL, so the predicate must report NOT-at-or-above
		// (Inherited=true → "via-hub-inherited" read-only) — NOT true (which would
		// offer a demigrate the hub cannot complete).
		isolateMimoCodeEnv(t)
		home := mimoCodeEnableClaudeImportForTest(t)
		globalDir := filepath.Join(home, ".config", "mimocode")
		if err := os.MkdirAll(globalDir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(globalDir, "mimocode.json")
		// Write target defines `time` but ONLY as an enabled-only overlay (no url).
		if err := os.WriteFile(path, []byte(`{"mcp":{"time":{"enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// The hub URL is supplied by the ~/.claude.json import.
		if err := os.WriteFile(filepath.Join(home, ".claude.json"),
			[]byte(`{"mcpServers":{"time":{"type":"remote","url":"http://localhost:9120/mcp"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		atOrAbove, err := MimoCodeNameAtOrAboveWriteTarget(path, "time")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if atOrAbove {
			t.Errorf("a write-target enabled-only overlay over an import URL does NOT own the URL => false, got true")
		}
	})

	t.Run("write-target ENABLED-ONLY overlay over below config.json URL => (false, nil)", func(t *testing.T) {
		// Same bug shape but the URL lives in the config.json layer strictly BELOW
		// the write target. The enabled-only write-target stub still does not own it.
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "mimocode.json")
		if err := os.WriteFile(path, []byte(`{"mcp":{"time":{"enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"),
			[]byte(`{"mcp":{"time":{"type":"remote","url":"http://localhost:9120/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		atOrAbove, err := MimoCodeNameAtOrAboveWriteTarget(path, "time")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if atOrAbove {
			t.Errorf("a write-target enabled-only overlay over a below-config.json URL does NOT own it => false, got true")
		}
	})

	t.Run("write-target DISABLING enabled-only overlay over import URL => (false, nil)", func(t *testing.T) {
		// Either polarity of an enabled-only overlay is not content-bearing: an
		// {enabled:false} stub over an import URL still does not own the URL, so the
		// hub cannot demigrate it (RemoveEntry would clear only the disable flag).
		isolateMimoCodeEnv(t)
		home := mimoCodeEnableClaudeImportForTest(t)
		globalDir := filepath.Join(home, ".config", "mimocode")
		if err := os.MkdirAll(globalDir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(globalDir, "mimocode.json")
		if err := os.WriteFile(path, []byte(`{"mcp":{"time":{"enabled":false}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".claude.json"),
			[]byte(`{"mcpServers":{"time":{"type":"remote","url":"http://localhost:9120/mcp"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		atOrAbove, err := MimoCodeNameAtOrAboveWriteTarget(path, "time")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if atOrAbove {
			t.Errorf("a write-target enabled:false overlay over an import URL does NOT own it => false, got true")
		}
	})

	t.Run("write-target URL-BEARING entry stays at/above (demigratable) => (true, nil)", func(t *testing.T) {
		// Regression guard: a genuine hub-written url entry in the write target is
		// content-bearing → owned → stays demigratable (the content-bearing
		// tightening must NOT flip a real hub entry to read-only).
		isolateMimoCodeEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "mimocode.json")
		if err := os.WriteFile(path,
			[]byte(`{"mcp":{"time":{"type":"remote","url":"http://localhost:9120/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		atOrAbove, err := MimoCodeNameAtOrAboveWriteTarget(path, "time")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !atOrAbove {
			t.Errorf("a write-target url-bearing entry is content-bearing (hub-ownable) => true, got false")
		}
	})

	t.Run("claude-import-only entry => (false, nil)", func(t *testing.T) {
		isolateMimoCodeEnv(t)
		// Re-enable the import and point HOME/USERPROFILE at a temp dir so the
		// import reads a FIXTURE ~/.claude.json, never the developer's real one.
		home := mimoCodeEnableClaudeImportForTest(t)
		// The mimo write target lives under the import home's global config dir so
		// mimoCodeClientForScanPath resolves the SAME claudeHome the merge uses.
		globalDir := filepath.Join(home, ".config", "mimocode")
		if err := os.MkdirAll(globalDir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(globalDir, "mimocode.json")
		// Write target EXISTS but does NOT define `time` (no file/inline layer
		// defines it either).
		if err := os.WriteFile(path, []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// `time` resolves ONLY from the ~/.claude.json mcpServers import (TOP
		// layer, skip-if-name-exists) — a layer the hub NEVER writes. The predicate
		// deliberately EXCLUDES the import, so it must report NOT at/above.
		if err := os.WriteFile(filepath.Join(home, ".claude.json"),
			[]byte(`{"mcpServers":{"time":{"command":"npx","args":["-y","time-mcp"]}}}`), 0o600); err != nil {
			t.Fatal(err)
		}

		// Sanity: the merged read DOES surface the import (proving `time` is
		// genuinely import-sourced, not just missing).
		merged, mErr := MimoCodeMergedConfig(path)
		if mErr != nil {
			t.Fatalf("MimoCodeMergedConfig: %v", mErr)
		}
		servers, _ := merged[mimoCodeMCPKey].(map[string]any)
		if _, ok := servers["time"]; !ok {
			t.Fatalf("setup bug: `time` must appear in the merged read via the import, got %+v", servers)
		}

		atOrAbove, err := MimoCodeNameAtOrAboveWriteTarget(path, "time")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if atOrAbove {
			t.Errorf("a claude-import-only entry must NOT be at/above (hub never wrote it) => false, got true")
		}
	})
}
