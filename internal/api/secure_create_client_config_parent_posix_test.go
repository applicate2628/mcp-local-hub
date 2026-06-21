//go:build !windows

package api

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSecureCreateClientConfigParentDir_PosixStrictAllowsBroadenedExistingPrefix
// LOCKS the intentional POSIX/Windows divergence (F3): in strict mode the
// POSIX leg DACL/mode-gates the HOME ANCHOR only, NOT each existing
// intermediate prefix it descends through. A broadened EXISTING
// intermediate directory under the (owner-only) home is therefore NOT a
// refusal on POSIX — the security boundary is the 0700 mode of the
// directory THIS function creates, which a loose ancestor cannot weaken
// (POSIX permission checks are per-inode, not DACL-inherited).
//
// This is the deliberate counterpart to the Windows test
// TestSecureCreateClientConfigParentDir_WindowsStrictRefusesBroadenedExistingPrefix,
// which DOES refuse — Windows verifies every existing prefix because a
// Windows child folder inherits the parent DACL. The asymmetry mirrors the
// two OS security models; see the package doc in
// secure_create_client_config_parent_posix.go. If a future change makes
// POSIX refuse here (e.g. by adding a per-prefix fchmod check to
// mkdirOrOpenRealDirAt), this test fails loudly so the divergence stays an
// explicit, reviewed decision rather than silent drift.
func TestSecureCreateClientConfigParentDir_PosixStrictAllowsBroadenedExistingPrefix(t *testing.T) {
	// Owner-only home anchor so the strict home-anchor gate passes.
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "1")

	// Isolate the audit-log destination and clear the strict-mode-intent
	// cache so the env var (strict) governs this run.
	prevStateRoot := daemonStateRootOverride
	daemonStateRootOverride = hardenedTempDir(t)
	t.Cleanup(func() { daemonStateRootOverride = prevStateRoot })
	resetStrictModeIntentCacheForTest()
	t.Cleanup(resetStrictModeIntentCacheForTest)

	// An EXISTING intermediate directory under the home, deliberately
	// broadened to group/world-readable (0755). On Windows the equivalent
	// (an inheritable Authenticated-Users ACE) makes the descent refuse;
	// on POSIX it must NOT.
	broadened := filepath.Join(home, ".broadenedclient")
	if err := os.Mkdir(broadened, 0o755); err != nil {
		t.Fatalf("mkdir broadened intermediate: %v", err)
	}
	if err := os.Chmod(broadened, 0o755); err != nil { // survive umask drift
		t.Fatalf("chmod broadened intermediate: %v", err)
	}

	// Config path descends THROUGH the broadened existing prefix and needs
	// one more (absent) component created.
	cfg := filepath.Join(broadened, "User", "mcp.json")
	created := filepath.Dir(cfg)

	// By-design: strict mode does NOT refuse on the broadened existing
	// intermediate. The create succeeds.
	if err := SecureCreateClientConfigParentDirWithOperatorOpt(cfg); err != nil {
		t.Fatalf("strict POSIX run refused a broadened EXISTING intermediate prefix; "+
			"the by-design divergence is that POSIX gates the home anchor only, not "+
			"each existing prefix (see package doc). err=%v", err)
	}

	// The newly-created leaf directory must itself be owner-only (0700) —
	// that 0700 mode, not the loose ancestor, is the POSIX security
	// boundary.
	st, err := os.Lstat(created)
	if err != nil {
		t.Fatalf("created dir %s missing after success: %v", created, err)
	}
	if !st.IsDir() {
		t.Fatalf("created %s is not a directory (mode %s)", created, st.Mode())
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("created leaf %s mode %#o exposes group/world bits; the created dir "+
			"must be owner-only regardless of the broadened ancestor", created, perm)
	}

	// The broadened intermediate must be left exactly as the operator had
	// it — the function does not re-chmod an existing directory it merely
	// descended through.
	bst, err := os.Lstat(broadened)
	if err != nil {
		t.Fatalf("broadened intermediate %s vanished: %v", broadened, err)
	}
	if perm := bst.Mode().Perm(); perm != 0o755 {
		t.Errorf("broadened intermediate %s mode changed to %#o; descent must not "+
			"mutate an existing directory's mode (want unchanged 0755)", broadened, perm)
	}
}
