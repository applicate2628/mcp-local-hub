package api

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMimoCodePresencePostProbe_TableDriven is the characterization test for the
// C1 extraction (ITEM 2): the behavior-PRESERVING move of the inline MiMoCode
// presence-promotion/downgrade block out of ScanFrom into
// mimoCodePresencePostProbe. It calls the extracted seam DIRECTLY and pins each
// of the order-dependent transitions:
//
//   - absent -> ok via a regular FILE layer (config.json below the write target)
//   - absent -> ok via a parseable INLINE layer (MIMOCODE_CONFIG_CONTENT)
//   - absent -> error via a MALFORMED inline-only layer
//   - ok -> error via a MALFORMED inline content layer (downgrade, file present)
//   - ok stays ok with no fault (positive control)
//   - error stays error (a real write-target fault is never promoted)
//
// Each sub-test isolates the MIMOCODE_* env so the developer's real config is
// never read.
func TestMimoCodePresencePostProbe_TableDriven(t *testing.T) {
	t.Run("absent -> ok via a regular file layer below the write target", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "mimocode.json") // write target ABSENT
		// A config.json layer (below the write target) exists as a regular file.
		if err := os.WriteFile(filepath.Join(dir, "config.json"),
			[]byte(`{"mcp":{"time":{"type":"remote","url":"http://x/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := mimoCodePresencePostProbe("mimocode", path, "missing"); got != "ok" {
			t.Errorf("a present regular lower layer must promote missing -> ok, got %q", got)
		}
	})

	t.Run("absent -> ok via a parseable inline layer", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "mimocode.json") // write target ABSENT, no file layers
		t.Setenv("MIMOCODE_CONFIG_CONTENT", `{"mcp":{"time":{"type":"remote","url":"http://x/mcp","enabled":true}}}`)
		if got := mimoCodePresencePostProbe("mimocode", path, "missing"); got != "ok" {
			t.Errorf("a parseable inline layer must promote missing -> ok, got %q", got)
		}
	})

	t.Run("absent -> error via a malformed inline-only layer", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "mimocode.json")                 // write target ABSENT, no file layers
		t.Setenv("MIMOCODE_CONFIG_CONTENT", `{"mcp": { "broken": `) // unterminated
		if got := mimoCodePresencePostProbe("mimocode", path, "missing"); got != "error" {
			t.Errorf("a malformed inline-only layer must promote missing -> error, got %q", got)
		}
	})

	t.Run("ok -> error via a malformed inline content layer (file layer present)", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "mimocode.json")
		// A valid write-target file layer made the client present ("ok").
		if err := os.WriteFile(path,
			[]byte(`{"mcp":{"time":{"type":"remote","url":"http://x/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MIMOCODE_CONFIG_CONTENT", `{"mcp": { "broken": `) // malformed inline on top
		if got := mimoCodePresencePostProbe("mimocode", path, "ok"); got != "error" {
			t.Errorf("a malformed inline layer must downgrade ok -> error, got %q", got)
		}
	})

	t.Run("ok stays ok with a valid inline layer (positive control)", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "mimocode.json")
		if err := os.WriteFile(path,
			[]byte(`{"mcp":{"time":{"type":"remote","url":"http://x/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MIMOCODE_CONFIG_CONTENT", `{"mcp":{"extra":{"type":"remote","url":"http://y/mcp","enabled":true}}}`)
		if got := mimoCodePresencePostProbe("mimocode", path, "ok"); got != "ok" {
			t.Errorf("a valid file+inline layer must stay ok, got %q", got)
		}
	})

	t.Run("error stays error (a real write-target fault is never promoted)", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "mimocode.json")
		// Even with a present regular lower layer, an already-"error" verdict
		// (a real write-target fault) must NOT be promoted to ok.
		if err := os.WriteFile(filepath.Join(dir, "config.json"),
			[]byte(`{"mcp":{"time":{"type":"remote","url":"http://x/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := mimoCodePresencePostProbe("mimocode", path, "error"); got != "error" {
			t.Errorf("a write-target config fault (error) must never be promoted, got %q", got)
		}
	})
}
