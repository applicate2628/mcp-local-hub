package api

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSecureCreateParentDirForConfigLock_CreatesMissingChain pins the happy
// path for the config-lock parent creator (bot PR #420 finding 1): an absent
// parent chain is created component-by-component, owner-only (0700 on POSIX).
// This is the finding-3-ORIGINAL legitimate case — a MIMOCODE_CONFIG_DIR-only
// profile whose GLOBAL dir is missing, NO symlink — which MUST still create the
// write-target parent so install/Apply can write mimocode.json.
func TestSecureCreateParentDirForConfigLock_CreatesMissingChain(t *testing.T) {
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Two missing components: .config/mimocode (the mimo global dir layout).
	dir := filepath.Join(home, ".config", "mimocode")

	if err := SecureCreateParentDirForConfigLock(dir); err != nil {
		t.Fatalf("SecureCreateParentDirForConfigLock: %v", err)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("dir %s not created: %v", dir, err)
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("dir %s is not a real directory (mode %s)", dir, st.Mode())
	}
	if runtime.GOOS != "windows" {
		if perm := st.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("created dir %s mode %#o exposes group/world bits; want owner-only", dir, perm)
		}
	}
	// Idempotent: a second call is a no-op success.
	if err := SecureCreateParentDirForConfigLock(dir); err != nil {
		t.Fatalf("second (idempotent) call: %v", err)
	}
}

// TestSecureCreateParentDirForConfigLock_CreatesOUTSIDEHome is the
// regression-prevention test for bot PR #420 finding 1: UNLIKE the G17
// home-bounded SecureCreateClientConfigParentDir (which REFUSES any path outside
// the user home), the config-lock parent creator MUST succeed for an
// outside-home target — MiMoCode's write target can be $MIMOCODE_HOME/config or
// $XDG_CONFIG_HOME/mimocode, which may sit outside the OS home. A home-bounded
// creator wired into the shared withConfigLock would convert the P1 into an
// install-breaking regression. Here we prove the new creator does NOT refuse an
// outside-home dir while STILL creating it owner-only.
func TestSecureCreateParentDirForConfigLock_CreatesOUTSIDEHome(t *testing.T) {
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// A sibling temp dir NOT under home, simulating MIMOCODE_HOME=/opt/mimo.
	outside := hardenedTempDir(t) // distinct temp root, not under `home`
	dir := filepath.Join(outside, "config", "deep")

	if err := SecureCreateParentDirForConfigLock(dir); err != nil {
		t.Fatalf("outside-home create must SUCCEED (not refuse like the home-bounded G17 creator): %v", err)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("outside-home dir %s not created: %v", dir, err)
	}
	if !st.IsDir() {
		t.Fatalf("outside-home dir %s is not a directory", dir)
	}
	if runtime.GOOS != "windows" {
		if perm := st.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("outside-home dir %s mode %#o exposes group/world bits; want owner-only", dir, perm)
		}
	}
}

// TestSecureCreateParentDirForConfigLock_RefusesSymlinkPrefix pins the P1
// closure (bot PR #420 finding 1): the creator must REFUSE to descend through a
// symlinked prefix and must NOT create the missing parent THROUGH the symlink at
// an attacker-chosen target — the exact vector the r16 blind os.MkdirAll
// followed. POSIX-only (symlink creation needs elevation on Windows; the Windows
// reparse-refusal is exercised by the production reparse checks reused from the
// G17 creator).
func TestSecureCreateParentDirForConfigLock_RefusesSymlinkPrefix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Attacker target dir (where a followed symlink would dump the new chain).
	attackerTarget := filepath.Join(home, "attacker-target")
	if err := os.MkdirAll(attackerTarget, 0o700); err != nil {
		t.Fatalf("mkdir attacker target: %v", err)
	}
	// Symlink a prefix component to the attacker target.
	link := filepath.Join(home, ".config")
	if err := os.Symlink(attackerTarget, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// The dir descends THROUGH the symlinked .config, BELOW which mimocode is
	// missing — exactly the r16 vector (missing parent below an existing
	// symlinked prefix).
	dir := filepath.Join(link, "mimocode")
	err := SecureCreateParentDirForConfigLock(dir)
	if err == nil {
		t.Fatalf("expected refusal descending through symlink, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") &&
		!strings.Contains(err.Error(), "non-directory") &&
		!strings.Contains(err.Error(), "reparse") {
		t.Errorf("error %q does not name the symlink/non-dir/reparse refusal", err)
	}
	// The attacker target must NOT have received the new mimocode dir (the
	// symlink must not have been followed).
	if _, statErr := os.Stat(filepath.Join(attackerTarget, "mimocode")); statErr == nil {
		t.Errorf("SYMLINK FOLLOWED: mimocode created under attacker target %s", attackerTarget)
	}
}

// TestSecureCreateParentDirForConfigLock_RefusesIntermediateSymlinkInAnchor is
// the regression test for the bot PR #420 finding-1 RESIDUAL (security-reviewer
// F1 MEDIUM): the creator must refuse an INTERMEDIATE symlink that sits inside
// the EXISTING-PREFIX portion of the path — the vector the final-component-only
// _RefusesSymlinkPrefix test above misses.
//
// The r17 impl selected the "nearest existing ancestor" and re-opened it by its
// FULL ABSOLUTE PATH with O_NOFOLLOW. O_NOFOLLOW refuses only the TRAILING
// component, so the kernel FOLLOWED an intermediate symlink in that absolute
// anchor path. Layout: `a` is a symlink into attacker space, `attacker/b` is a
// real dir, target is `a/b/c` with `c` missing. The old code's
// nearestExistingAncestor returned anchor `a/b` (its Lstat walk already resolved
// the `a` symlink), re-opened `a/b` by absolute path → FOLLOWED `a` → the anchor
// handle was the attacker's `b` → the missing `c` was created fd-relative under
// the redirected handle (token config published OUTSIDE the intended path). The
// descent-from-volume-root fix opens `a` O_NOFOLLOW-relative to its real parent
// and REFUSES the symlink, so `c` is never created under `attacker/b`.
// POSIX-only (symlink creation needs elevation on Windows; the Windows
// reparse-refusal is exercised by the production reparse checks reused from the
// G17 creator).
func TestSecureCreateParentDirForConfigLock_RefusesIntermediateSymlinkInAnchor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// Attacker space, OUTSIDE `home`, with a real `b` directory already present.
	attacker := hardenedTempDir(t) // distinct 0700 temp root, not under `home`
	attackerB := filepath.Join(attacker, "b")
	if err := os.Mkdir(attackerB, 0o700); err != nil {
		t.Fatalf("mkdir attacker/b: %v", err)
	}

	// `home/a` is a symlink to the attacker dir, so `home/a/b` resolves to the
	// real `attacker/b` THROUGH the symlinked intermediate `a`. The existing
	// prefix of the target therefore contains the symlink `a` as an INTERMEDIATE
	// component (not the trailing one).
	link := filepath.Join(home, "a")
	if err := os.Symlink(attacker, link); err != nil {
		t.Fatalf("create intermediate symlink: %v", err)
	}

	// Target: home/a/b/c — `a` (symlink) and `a/b` (real, via the symlink) exist;
	// `c` is missing. This is the intermediate-symlink-in-anchor vector.
	dir := filepath.Join(link, "b", "c")
	err := SecureCreateParentDirForConfigLock(dir)
	if err == nil {
		t.Fatalf("expected refusal descending through an intermediate symlink in the anchor, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") &&
		!strings.Contains(err.Error(), "non-directory") &&
		!strings.Contains(err.Error(), "reparse") {
		t.Errorf("error %q does not name the symlink/non-dir/reparse refusal", err)
	}
	// The attacker's real `b` must NOT have received the new `c` dir (the
	// intermediate symlink `a` must not have been followed).
	if _, statErr := os.Stat(filepath.Join(attackerB, "c")); statErr == nil {
		t.Errorf("INTERMEDIATE SYMLINK FOLLOWED: c created under attacker target %s", attackerB)
	}
}

// TestSecureCreateParentDirForConfigLock_RefusesNonDirPrefix pins that an
// existing-prefix component that is a regular FILE (not a directory) makes the
// create refuse — it must not attempt to create a child under a file.
func TestSecureCreateParentDirForConfigLock_RefusesNonDirPrefix(t *testing.T) {
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	regular := filepath.Join(home, ".client")
	if err := os.WriteFile(regular, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("seed regular file: %v", err)
	}
	dir := filepath.Join(regular, "sub")
	if err := SecureCreateParentDirForConfigLock(dir); err == nil {
		t.Fatalf("expected refusal under a non-directory prefix, got nil")
	}
}
