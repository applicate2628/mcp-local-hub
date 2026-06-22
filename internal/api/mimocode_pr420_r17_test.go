package api

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

// TestWithConfigLock_RefusesExistingSymlinkedParent pins bot PR #420 r17 finding
// P2a: when the write-target PARENT dir ALREADY EXISTS as a SYMLINK (not missing),
// the production SecureCreateParentDir swap must still refuse it — so the advisory
// flock is NEVER created THROUGH the symlinked parent at an attacker-chosen target.
// The earlier IsNotExist-guarded form skipped the secure descent for an existing
// parent, leaving the symlinked-existing-parent case unrefused. POSIX-only
// (symlink creation needs elevation on Windows; the Windows reparse refusal is
// covered by the production reparse checks the descent reuses).
func TestWithConfigLock_RefusesExistingSymlinkedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// The attacker target where a followed symlink would dump the lock file.
	attackerTarget := filepath.Join(home, "attacker-target")
	if err := os.MkdirAll(attackerTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	// The write-target PARENT already EXISTS as a symlink to the attacker target.
	parentLink := filepath.Join(home, "cfgdir")
	if err := os.Symlink(attackerTarget, parentLink); err != nil {
		t.Fatalf("symlink parent: %v", err)
	}

	// Call THROUGH the clients-side seam (swapped by api.init() to the production
	// symlink-refusing creator). The config file would live at parentLink/cfg.json,
	// so the parent is the EXISTING symlink — the P2a case.
	err := clients.SecureCreateParentDir(parentLink)
	if err == nil {
		t.Fatalf("an EXISTING symlinked parent dir must be refused (P2a), got nil")
	}
	if !strings.Contains(err.Error(), "symlink") &&
		!strings.Contains(err.Error(), "non-directory") &&
		!strings.Contains(err.Error(), "reparse") {
		t.Errorf("error %q does not name the symlink/non-dir/reparse refusal", err)
	}
}

// TestScanMimoCode_MalformedInlineWithFileLayer_PerClientError pins bot PR #420
// r17 finding B2: a MALFORMED MIMOCODE_CONFIG_CONTENT inline layer must be
// classified as a per-client config-error EVEN WHEN a regular file layer already
// made the client present ("ok"). The earlier promotion block checked the inline
// tri-state ONLY in the absent branch (isPromotableAbsentPresenceState), so a
// present write target skipped it; scanMimoCode then ran, MimoCodeMergedConfig
// hit the inline parse error, and the ENTIRE multi-client scan aborted. The fix
// downgrades an otherwise-"ok" mimocode presence to "error" whenever the inline
// content is present-but-unparseable, REGARDLESS of the file layer — so one bad
// inline layer can no longer take the whole scan down.
func TestScanMimoCode_MalformedInlineWithFileLayer_PerClientError(t *testing.T) {
	t.Run("malformed inline + present write target -> per-client error, scan succeeds", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		tmp := t.TempDir()
		// A VALID, present write-target file layer (generic probe = "ok").
		writeTarget := filepath.Join(tmp, "mimocode.json")
		if err := os.WriteFile(writeTarget,
			[]byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// A MALFORMED inline layer on top. The path (mimocode.json) is a global layer
		// name, so the scan-side client reads MIMOCODE_CONFIG_CONTENT.
		t.Setenv("MIMOCODE_CONFIG_CONTENT", `{"mcp": { "broken": `) // unterminated JSON

		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: writeTarget})
		if err != nil {
			t.Fatalf("ScanFrom must NOT abort the whole scan on a malformed inline layer, got: %v", err)
		}
		if got := res.ClientConfigPresence["mimocode"]; got != "error" {
			t.Errorf("a malformed inline layer (with a present file layer) must yield a per-client config-error, got %q", got)
		}
	})

	t.Run("valid inline + present write target -> stays ok (positive control)", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		tmp := t.TempDir()
		writeTarget := filepath.Join(tmp, "mimocode.json")
		if err := os.WriteFile(writeTarget,
			[]byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MIMOCODE_CONFIG_CONTENT", `{"mcp":{"extra":{"type":"remote","url":"http://localhost:9124/mcp","enabled":true}}}`)

		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: writeTarget})
		if err != nil {
			t.Fatalf("ScanFrom with a valid inline layer must succeed: %v", err)
		}
		if got := res.ClientConfigPresence["mimocode"]; got != "ok" {
			t.Errorf("a valid inline layer + present file layer must stay ok, got %q", got)
		}
	})
}
