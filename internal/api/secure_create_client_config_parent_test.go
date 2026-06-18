package api

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSecureCreateClientConfigParentDir_CreatesUnderHome verifies the
// G17 happy path: an absent parent chain under the user home is created
// component-by-component, the resulting directory exists, and on POSIX
// it is owner-only (mode 0700). A subsequent stub write into the created
// parent succeeds.
func TestSecureCreateClientConfigParentDir_CreatesUnderHome(t *testing.T) {
	// hardenedTempDir is owner-only (allowlist DACL on Windows, 0700 on
	// POSIX) so the strict home-anchor gate enforced by
	// SecureCreateClientConfigParentDir passes — exercising the create
	// path with the gate ON, mirroring a single-user-safe home.
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Two missing components below home: .someclient/User.
	cfg := filepath.Join(home, ".someclient", "User", "mcp.json")
	parent := filepath.Dir(cfg)

	if err := SecureCreateClientConfigParentDir(cfg); err != nil {
		t.Fatalf("SecureCreateClientConfigParentDir: %v", err)
	}

	st, err := os.Lstat(parent)
	if err != nil {
		t.Fatalf("parent %s not created: %v", parent, err)
	}
	if !st.IsDir() {
		t.Fatalf("parent %s is not a directory (mode %s)", parent, st.Mode())
	}
	if st.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("parent %s is a symlink, must be a real dir", parent)
	}
	if runtime.GOOS != "windows" {
		if perm := st.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("created parent %s mode %#o exposes bits to group/world; want owner-only", parent, perm)
		}
		// The intermediate component must also be owner-only.
		inter := filepath.Join(home, ".someclient")
		ist, ierr := os.Lstat(inter)
		if ierr != nil {
			t.Fatalf("intermediate %s missing: %v", inter, ierr)
		}
		if perm := ist.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("intermediate %s mode %#o exposes group/world bits", inter, perm)
		}
	}

	// Idempotent: a second call with the parent now present is a no-op
	// success.
	if err := SecureCreateClientConfigParentDir(cfg); err != nil {
		t.Fatalf("second (idempotent) call: %v", err)
	}
}

// TestSecureCreateClientConfigParentDir_RefusesOutsideHome pins the
// blast-radius bound: a config path whose parent is NOT under the user
// home is refused and nothing is created.
func TestSecureCreateClientConfigParentDir_RefusesOutsideHome(t *testing.T) {
	home := t.TempDir()
	other := t.TempDir() // sibling temp dir, NOT under home
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	cfg := filepath.Join(other, "elsewhere", "mcp.json")
	err := SecureCreateClientConfigParentDir(cfg)
	if err == nil {
		t.Fatalf("expected refusal for path outside home, got nil")
	}
	if !strings.Contains(err.Error(), "outside the user home") {
		t.Errorf("error %q does not mention the home-containment refusal", err)
	}
	// Nothing created.
	if _, statErr := os.Stat(filepath.Dir(cfg)); statErr == nil {
		t.Errorf("parent %s was created despite outside-home refusal", filepath.Dir(cfg))
	}
}

// TestSecureCreateClientConfigParentDir_RefusesSymlinkPrefix pins that
// the secure create refuses to descend through a symlinked component in
// the chain — it must not follow the symlink and create directories at
// an attacker-chosen target. POSIX-only (symlink creation needs
// elevation on Windows; the Windows reparse-refusal is exercised by the
// production reparse checks).
func TestSecureCreateClientConfigParentDir_RefusesSymlinkPrefix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Attacker target dir (where a followed symlink would dump files).
	attackerTarget := filepath.Join(home, "attacker-target")
	if err := os.MkdirAll(attackerTarget, 0o700); err != nil {
		t.Fatalf("mkdir attacker target: %v", err)
	}
	// Symlink a chain component to the attacker target.
	link := filepath.Join(home, ".config")
	if err := os.Symlink(attackerTarget, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// Config path descends THROUGH the symlinked .config.
	cfg := filepath.Join(link, "SomeClient", "User", "mcp.json")
	err := SecureCreateClientConfigParentDir(cfg)
	if err == nil {
		t.Fatalf("expected refusal descending through symlink, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") && !strings.Contains(err.Error(), "non-directory") {
		t.Errorf("error %q does not name the symlink/non-dir refusal", err)
	}
	// The attacker target must NOT have received the new SomeClient/User
	// tree (the symlink must not have been followed).
	if _, statErr := os.Stat(filepath.Join(attackerTarget, "SomeClient")); statErr == nil {
		t.Errorf("symlink was followed: SomeClient created under attacker target %s", attackerTarget)
	}
}

// TestSecureCreateClientConfigParentDir_RefusesNonDirPrefix pins that an
// existing-prefix component that is a regular FILE (not a directory)
// makes the create refuse — it must not attempt to create a child under
// a file.
func TestSecureCreateClientConfigParentDir_RefusesNonDirPrefix(t *testing.T) {
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// A regular file where the chain expects a directory.
	regular := filepath.Join(home, ".client")
	if err := os.WriteFile(regular, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}
	cfg := filepath.Join(regular, "sub", "mcp.json")
	err := SecureCreateClientConfigParentDir(cfg)
	if err == nil {
		t.Fatalf("expected refusal under a non-directory prefix, got nil")
	}
}

// TestSecureCreateClientConfigParentDirWithOperatorOpt_StrictModeHonored
// pins the strict-mode interaction: with
// MCPHUB_REQUIRE_SINGLE_USER_HOME=1, a broadened home anchor returns the
// canonical strict-mode error (ErrSecureWriteParentInsecure-wrapped)
// rather than relaxing. On a genuinely owner-only home the create
// succeeds even in strict mode.
//
// On most CI/dev hosts t.TempDir() is owner-only (0700 on POSIX), so the
// strict create succeeds; on a host whose temp dir is broadened the
// strict refusal fires. Both outcomes are asserted as correct: the test
// fails ONLY if strict mode neither creates (owner-only) nor refuses
// with the canonical message (broadened).
func TestSecureCreateClientConfigParentDirWithOperatorOpt_StrictModeHonored(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "1")
	// Isolate the audit-log destination so a relax-lane warn row (if any)
	// does not leak into the operator's real state dir.
	prevStateRoot := daemonStateRootOverride
	daemonStateRootOverride = hardenedTempDir(t)
	t.Cleanup(func() { daemonStateRootOverride = prevStateRoot })
	// Clear the lazy strict-mode-intent cache so the env var governs.
	resetStrictModeIntentCacheForTest()
	t.Cleanup(resetStrictModeIntentCacheForTest)

	cfg := filepath.Join(home, ".strictclient", "mcp.json")
	err := SecureCreateClientConfigParentDirWithOperatorOpt(cfg)
	if err == nil {
		// Owner-only home: the parent must have been created.
		if _, statErr := os.Stat(filepath.Dir(cfg)); statErr != nil {
			t.Errorf("strict-mode create returned nil but parent %s not created: %v", filepath.Dir(cfg), statErr)
		}
		return
	}
	// Broadened home: must be the canonical strict-mode error, and
	// nothing should have been created.
	if !strings.Contains(err.Error(), "MCPHUB_REQUIRE_SINGLE_USER_HOME") {
		t.Fatalf("strict-mode error missing canonical message; got %v", err)
	}
	if _, statErr := os.Stat(filepath.Dir(cfg)); statErr == nil {
		t.Errorf("strict-mode refusal still created %s; create must refuse before mkdir", filepath.Dir(cfg))
	}
}
