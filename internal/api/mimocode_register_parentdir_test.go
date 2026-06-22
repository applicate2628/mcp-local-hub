package api

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/clients"
)

// TestMimoCode_RegisterSequence_CreatesMissingWriteTargetParentDir pins bot PR
// #420 r15 finding 3 (register parent-dir create). When MiMoCode is active ONLY
// via MIMOCODE_CONFIG_DIR (an overlay layer) and the GLOBAL write-target dir
// (~/.config/mimocode, here redirected via XDG_CONFIG_HOME) does NOT yet exist,
// Exists() returns true (the overlay makes the profile active), so register
// proceeds straight to the client write (AddEntry). The PRODUCTION secure writer
// (SecureWriteClientConfig, wired by client_write_init.go's init()) only OPENS
// the write-target parent — it does NOT create it — and the locking decorator's
// advisory flock lives INSIDE that same dir, so without a parent create the very
// lock acquisition fails and register would abort + roll back.
//
// The fix lives in the config-lock chokepoint (clients.withConfigLock MkdirAll's
// the missing parent dir before the flock), so every mutating adapter method —
// AddEntry here, exactly as the register write loop calls it — creates the dir
// for free. This test exercises the REAL NewMimoCode client (locking decorator)
// and the PRODUCTION secure write pipeline (this is the api package, where init()
// swapped clients.WriteConfigFile to the secure writer), and asserts the
// write-target mimocode.json IS created in the previously-missing global dir by a
// bare AddEntry — no separate parent-ensure step in the register path.
//
// State-safe: every path is under t.TempDir(); XDG_CONFIG_HOME redirects the
// global dir, HOME/USERPROFILE redirect the ~/.claude.json import home, and the
// import is DISABLED, so the test never reads or writes the developer's real
// ~/.config/mimocode or ~/.claude.json.
func TestMimoCode_RegisterSequence_CreatesMissingWriteTargetParentDir(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "") // default relax posture (not strict)

	// Clear inherited MiMoCode env so nothing from the developer's shell leaks in.
	for _, k := range []string{"MIMOCODE_CONFIG", "MIMOCODE_CONFIG_CONTENT", "MIMOCODE_CONFIG_DIR", "MIMOCODE_HOME"} {
		t.Setenv(k, "")
	}
	// Disable the ~/.claude.json import (state-safe — no real home read).
	t.Setenv(clients.MimoCodeDisableClaudeImportEnv, "1")

	// A hardened temp dir so the production secure writer's parent-dir gate is
	// satisfied (on Windows %TEMP% is Authenticated-Users-readable otherwise).
	root := hardenedTempDir(t)

	// Redirect HOME so any home-anchored read stays inside the sandbox.
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// The GLOBAL config dir resolves to <XDG>/mimocode and is intentionally ABSENT.
	xdg := filepath.Join(root, "xdg")
	if err := os.Mkdir(xdg, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)
	globalDir := filepath.Join(xdg, "mimocode")
	writeTarget := filepath.Join(globalDir, "mimocode.json")
	if _, err := os.Stat(globalDir); !os.IsNotExist(err) {
		t.Fatalf("precondition: global dir %s must NOT exist yet (stat err=%v)", globalDir, err)
	}

	// The profile is active ONLY via a MIMOCODE_CONFIG_DIR overlay that defines a
	// server — so Exists() is true even though the global write-target dir is gone.
	overlay := filepath.Join(root, "overlay")
	if err := os.Mkdir(overlay, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlay, "mimocode.json"),
		[]byte(`{"mcp":{"memory":{"type":"remote","url":"http://localhost:9123/mcp","enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIMOCODE_CONFIG_DIR", overlay)

	client, err := clients.NewMimoCode()
	if err != nil {
		t.Fatalf("NewMimoCode: %v", err)
	}
	if client.ConfigPath() != writeTarget {
		t.Fatalf("write target = %q, want %q", client.ConfigPath(), writeTarget)
	}
	if !client.Exists() {
		t.Fatal("profile must be ACTIVE via the MIMOCODE_CONFIG_DIR overlay (Exists() true) even with the global dir absent")
	}

	// The register write loop's exact call: a bare AddEntry (no separate
	// parent-ensure step in the register path). Through the locking decorator the
	// config-lock chokepoint MkdirAll's the missing parent dir before the flock, so
	// AddEntry succeeds and lands the entry in the freshly-created write target.
	// Without the fix this fails at the lock ("config lock ...mimocode.json.lock:
	// ... cannot find the path").
	if err := client.AddEntry(clients.MCPEntry{Name: "newsrv", URL: "http://127.0.0.1:9999/mcp"}); err != nil {
		t.Fatalf("AddEntry must create the missing write-target parent dir and succeed, got: %v", err)
	}

	// The write-target mimocode.json must now exist in the previously-missing dir.
	if st, err := os.Stat(writeTarget); err != nil {
		t.Fatalf("write-target mimocode.json must be created, stat err=%v", err)
	} else if !st.Mode().IsRegular() {
		t.Fatalf("write target must be a regular file, got mode %v", st.Mode())
	}
	// The hub entry must be in the merged read (proving the write landed in the
	// write target, not the overlay).
	if e, err := client.GetEntry("newsrv"); err != nil || e == nil || e.URL != "http://127.0.0.1:9999/mcp" {
		t.Fatalf("hub-written entry must round-trip through the new write target: entry=%+v err=%v", e, err)
	}
}
