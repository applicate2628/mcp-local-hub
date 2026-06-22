package api

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

// TestScanMimoCode_NonRegularActiveLayer_PerClientConfigError pins bot PR #420
// finding 4: when ONE active mimo layer is a regular file ("ok") but ANOTHER
// active layer (config.json / overlay) is a directory/non-regular, scanMimoCode's
// merged read would return a non-regular read error and FAIL THE ENTIRE
// multi-client scan with `mimocode: ...`. The fix downgrades the mimocode
// presence to the per-client config-error "error" state (skipping scanMimoCode)
// instead of aborting — so a bad mimo layer can't take the whole scan down.
func TestScanMimoCode_NonRegularActiveLayer_PerClientConfigError(t *testing.T) {
	t.Run("write target ok but config.json layer is a directory -> per-client error, scan succeeds", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		tmp := t.TempDir()
		// Write target is a valid regular file (generic probe = "ok").
		writeTarget := filepath.Join(tmp, "mimocode.json")
		if err := os.WriteFile(writeTarget, []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// An ACTIVE lower layer config.json is a DIRECTORY (non-regular).
		if err := os.Mkdir(filepath.Join(tmp, "config.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: writeTarget})
		if err != nil {
			t.Fatalf("ScanFrom must NOT fail the whole scan on one bad mimo layer, got: %v", err)
		}
		if got := res.ClientConfigPresence["mimocode"]; got != "error" {
			t.Errorf("a non-regular active mimo layer must yield a per-client config-error, got %q", got)
		}
	})

	t.Run("all-regular layers still scan normally (positive control)", func(t *testing.T) {
		isolateMimoCodeScanEnv(t)
		tmp := t.TempDir()
		writeTarget := filepath.Join(tmp, "mimocode.json")
		if err := os.WriteFile(writeTarget,
			[]byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, "config.json"), []byte(`{"mcp":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		a := NewAPI()
		res, err := a.ScanFrom(ScanOpts{MimoCodeConfigPath: writeTarget})
		if err != nil {
			t.Fatalf("ScanFrom: %v", err)
		}
		if got := res.ClientConfigPresence["mimocode"]; got != "ok" {
			t.Errorf("an all-regular mimo layer set must stay ok, got %q", got)
		}
	})
}

// TestGatedOnClients_DisabledMimoAggregate_NotGateOn pins bot PR #420 finding 5:
// a mimo `mcphub-hub` aggregate entry in a DISABLED state (enabled:false) must
// NOT count as gate-ON — the client never loads it, so a hub port reset would
// orphan nothing. Counting it gate-ON would FALSELY block `mcphub gui
// --reset-port`. An ENABLED aggregate IS gate-ON (positive control).
func TestGatedOnClients_DisabledMimoAggregate_NotGateOn(t *testing.T) {
	seedMimoAggregate := func(t *testing.T, enabled bool) {
		t.Helper()
		home := hermeticHome(t)
		// mimo's live global dir = $XDG_CONFIG_HOME/mimocode = $home/.config/mimocode
		// (hermeticHome sets XDG_CONFIG_HOME=$home/.config). Seed the write-target
		// mimocode.json with a `mcphub-hub` aggregate in the requested state.
		globalDir := filepath.Join(home, ".config", "mimocode")
		if err := os.MkdirAll(globalDir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"mcp":{"mcphub-hub":{"type":"remote","url":"http://127.0.0.1:3439/clients/mimocode/mcp","enabled":` +
			map[bool]string{true: "true", false: "false"}[enabled] + `}}}`
		if err := os.WriteFile(filepath.Join(globalDir, "mimocode.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("disabled aggregate -> NOT gate-on", func(t *testing.T) {
		seedMimoAggregate(t, false)
		if gated := GatedOnClients(); slices.Contains(gated, "mimocode") {
			t.Errorf("a DISABLED mimo aggregate must NOT be reported gate-ON (finding 5), got %v", gated)
		}
	})

	t.Run("enabled aggregate -> gate-on (positive control)", func(t *testing.T) {
		seedMimoAggregate(t, true)
		if gated := GatedOnClients(); !slices.Contains(gated, "mimocode") {
			t.Errorf("an ENABLED mimo aggregate must be reported gate-ON, got %v", gated)
		}
	})
}

// TestWithConfigLockParentCreate_SwapInForce confirms internal/api/init() swaps
// clients.SecureCreateParentDir to the production symlink-refusing creator (bot
// PR #420 finding 1): a direct call to the package-level seam refuses a symlinked
// prefix exactly like SecureCreateParentDirForConfigLock. This proves the
// withConfigLock chokepoint inherits the hardened create in any process that
// imports internal/api (every production entry point does). POSIX-only.
func TestWithConfigLockParentCreate_SwapInForce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	attackerTarget := filepath.Join(home, "attacker-target")
	if err := os.MkdirAll(attackerTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".config")
	if err := os.Symlink(attackerTarget, link); err != nil {
		t.Fatal(err)
	}
	// Call THROUGH the clients-side seam (swapped by api.init()).
	err := clients.SecureCreateParentDir(filepath.Join(link, "mimocode"))
	if err == nil {
		t.Fatal("the swapped clients.SecureCreateParentDir must refuse a symlinked prefix")
	}
	if _, statErr := os.Stat(filepath.Join(attackerTarget, "mimocode")); statErr == nil {
		t.Errorf("SYMLINK FOLLOWED through the seam: mimocode created under attacker target")
	}
	if !strings.Contains(err.Error(), "symlink") &&
		!strings.Contains(err.Error(), "non-directory") &&
		!strings.Contains(err.Error(), "reparse") {
		t.Errorf("error %q does not name the symlink/non-dir/reparse refusal", err)
	}
}
