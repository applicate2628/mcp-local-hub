//go:build !windows

// client_write_init_sec301_posix_test.go — POSIX-only regressions for the
// absent-intent verdict and the read-path no-mutation property.
//
// pr301 r9 (revert): an ABSENT supervisor-intent.json relaxes regardless of
// the state dir's broadening — an absent intent declares NO strict_mode, so it
// must not make the operator strict (the canonical CLAUDE.md "Hardened
// state-file writes" RELAX-by-default posture; STRICT is opt-in via
// MCPHUB_REQUIRE_SINGLE_USER_HOME=1). The pr301 r5/r6/r7 absent-on-delete-
// capable-dir → strict over-reach (and its chmod-heal-avoidance rationale) was
// reverted.
//
// The read-only-resolver no-mutation property is RETAINED as an independent
// good-hygiene invariant: a posture/read path must never chmod the state dir.
// readStrictModeFromIntentBestEffort resolves through DaemonStateDirReadOnly
// (NOT DefaultSupervisorIntentPath → DaemonStateDir, whose POSIX ensureStateRoot
// chmods the dir back to 0700), so the read leaves the dir's mode untouched.
//
// Windows ensureStateRoot performs no DACL change, so the no-mutation property
// is POSIX-specific; the Windows absent-relax coverage lives in
// client_write_init_sec301_windows_test.go.

package api

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadStrictModeFromIntent_DeleteCapableStateDirAbsentFile_Relaxes_NotHealed
// pins two properties on a group/world-writable (0o722, delete-capable) POSIX
// state dir with an ABSENT supervisor-intent.json and the read gate LIVE:
//
//  1. RELAX (return false) — pr301 r9 revert: an absent intent declares no
//     strict_mode, so it must not make the operator strict, even on a
//     delete-capable dir (the deletion-of-a-strict-intent bypass on such a dir
//     is a documented best-effort limitation whose mitigation is the env var);
//     AND
//  2. the read does NOT heal the dir to 0700 — the read-only resolver must
//     leave the mode untouched (a read/posture path must never mutate state).
//
// Assertion 1 REVERTS the pr301 r5/r6/r7 strict assertion; assertion 2 is the
// retained no-mutation invariant.
func TestReadStrictModeFromIntent_DeleteCapableStateDirAbsentFile_Relaxes_NotHealed(t *testing.T) {
	// Read gate LIVE: do NOT set AllowUnhardenedStateReadEnv. Env strict
	// UNSET so only the intent (and the dir posture) governs.
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")

	// State dir under our own subdir so its mode is independent of the
	// t.TempDir() ancestor's mode. Make it group/world-writable (0o722):
	// the delete-capable shape checkStateDirParentWriteSafe rejects.
	stateDir := filepath.Join(t.TempDir(), "delete-capable-state-dir")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	broadenParentForStateFileWriteCapableTest(t, stateDir) // chmod 0o722
	t.Cleanup(SetDaemonStateRootForTest(stateDir))
	t.Cleanup(resetStrictModeIntentCacheForTest)

	// Deliberately write NO supervisor-intent.json — the intent is ABSENT.

	// Precondition: confirm the read-side write-bits gate actually REJECTS
	// this dir (so the test exercises the delete-capable shape — proving that
	// even a delete-capable dir relaxes on absence). If this ever started
	// passing, the test would be vacuous.
	if err := checkStateDirParentWriteSafe(stateDir); err == nil {
		t.Fatal("precondition: checkStateDirParentWriteSafe must reject a 0o722 " +
			"(group/world-writable) state dir; got nil — the broadening did not take, " +
			"so the delete-capable branch under test is not exercised")
	}

	// Snapshot the mode before the read so the no-heal assertion is exact.
	beforeInfo, err := os.Stat(stateDir)
	if err != nil {
		t.Fatalf("stat state dir before: %v", err)
	}
	if got := beforeInfo.Mode().Perm(); got != 0o722 {
		t.Fatalf("precondition: state dir mode = %o, want 0722 before the read", got)
	}

	got := readStrictModeFromIntentBestEffort()

	// Assertion 1 — relax (pr301 r9 revert).
	if got {
		t.Fatal("pr301 r9 revert regression: an ABSENT supervisor-intent.json on a " +
			"DELETE-capable (0o722) POSIX state dir must RELAX (return false) — an absent " +
			"intent declares no strict_mode, so it must not make the operator strict; the " +
			"deletion bypass on such a dir is a documented best-effort limitation whose " +
			"mitigation is MCPHUB_REQUIRE_SINGLE_USER_HOME=1; got true (the reverted r5/r6/r7 " +
			"absent-strict over-reach)")
	}

	// Assertion 2 — the read must NOT have healed the dir. The read-only
	// resolver (DaemonStateDirReadOnly) creates/chmods nothing, so the dir's
	// mode is untouched (a posture/read path must never mutate state).
	afterInfo, err := os.Stat(stateDir)
	if err != nil {
		t.Fatalf("stat state dir after: %v", err)
	}
	if afterMode := afterInfo.Mode().Perm(); afterMode != 0o722 {
		t.Fatalf("read-path no-mutation regression: the absent-intent read chmod-healed "+
			"the state dir from 0o722 to %o — it must use the READ-ONLY resolver and leave the "+
			"mode untouched (a read/posture path must never mutate the state dir)", afterMode)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("readdir state dir after absent-intent read: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("read-path no-side-effect regression: absent-intent read created state-dir entries %v; want empty dir", names)
	}
}

// TestReadStrictModeFromIntent_SafeStateDirAbsentFile_Relaxes is the
// safe-default control: a 0700 (single-user-safe) POSIX state dir with
// an ABSENT supervisor-intent.json is a genuine fresh install and MUST
// relax (return false). The r7 read-only switch must NOT over-reach into
// the benign case — preserving the documented missing-intent →
// default-relax polarity.
func TestReadStrictModeFromIntent_SafeStateDirAbsentFile_Relaxes(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")

	stateDir := filepath.Join(t.TempDir(), "safe-state-dir")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil { // defend vs umask drift
		t.Fatalf("chmod 0o700 state dir: %v", err)
	}
	t.Cleanup(SetDaemonStateRootForTest(stateDir))
	t.Cleanup(resetStrictModeIntentCacheForTest)

	// Deliberately write NO supervisor-intent.json — the intent is ABSENT.

	// Precondition: a 0700 dir must PASS the write-bits gate (else it would
	// be mis-classified delete-capable and the control would assert the
	// wrong branch).
	if err := checkStateDirParentWriteSafe(stateDir); err != nil {
		t.Fatalf("precondition: checkStateDirParentWriteSafe must PASS a 0700 state dir; "+
			"got %v", err)
	}

	if got := readStrictModeFromIntentBestEffort(); got {
		t.Fatal("pr301 r7 Finding 1 over-reach guard: an ABSENT supervisor-intent.json on a " +
			"single-user-safe (0700) POSIX state dir must relax (return false) — it is a genuine " +
			"fresh install; got true (the read-only switch wrongly fired strict on a safe dir)")
	}
}
