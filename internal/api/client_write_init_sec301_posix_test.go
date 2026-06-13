//go:build !windows

// client_write_init_sec301_posix_test.go — POSIX-only falsifying
// regressions for pr301 r7 Finding 1 (P2 #704): the absent-intent
// posture check must observe the ACTUAL pre-heal state-dir mode, not a
// mode that DaemonStateDir()/ensureStateRoot already self-healed to
// 0700.
//
// On POSIX, DefaultSupervisorIntentPath() → DaemonStateDir() →
// posixStateDir()/ensureStateRoot() chmods the resolved state dir back
// to 0700 as a side effect. The pr301 r5 deletion-bypass fix
// (absentIntentStrictVerdict, gated on checkStateDirParentWriteSafe of
// the state dir) ran AFTER that heal, so a delete-capable
// (group/world-writable) dir whose supervisor-intent.json was deleted
// was reset to 0700 before the posture check — which then saw a SAFE
// dir and relaxed, defeating the very fix that was meant to close the
// bypass. The r7 fix resolves the path through the READ-ONLY resolver
// (DaemonStateDirReadOnly), which creates/chmods nothing, so the
// posture check observes the broadened mode and fails closed to strict.
//
// Windows ensureStateRoot performs no DACL change, so this bug is
// POSIX-only; the Windows delete-capable coverage lives in
// client_write_init_sec301_windows_test.go.

package api

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadStrictModeFromIntent_DeleteCapableStateDirAbsentFile_NotHealed_FailsClosedToStrict
// is the FALSIFYING CORE of pr301 r7 Finding 1. The state dir is
// group/world-writable (0o722 — the delete-capable shape
// checkStateDirParentWriteSafe rejects), supervisor-intent.json is
// ABSENT, and the read gate is LIVE (AllowUnhardenedStateReadEnv unset).
// readStrictModeFromIntentBestEffort MUST:
//
//  1. fail CLOSED to strict (return true) — an absent intent on a
//     delete-capable dir is indistinguishable from an attacker-deleted
//     one; relaxing turns the deletion into a strict-mode bypass; AND
//  2. NOT heal the state dir to 0700 — the posture check must read the
//     ACTUAL broadened mode, so the resolver it uses must not chmod.
//
// Pre-r7 (DefaultSupervisorIntentPath → DaemonStateDir → ensureStateRoot)
// this chmodded the dir to 0700 BEFORE absentIntentStrictVerdict ran, so
// the verdict saw a SAFE dir and returned FALSE (relax) — the deletion
// bypass the whole change was meant to close still worked on POSIX. Both
// assertions below fail on the pre-r7 code and pass after the fix.
func TestReadStrictModeFromIntent_DeleteCapableStateDirAbsentFile_NotHealed_FailsClosedToStrict(t *testing.T) {
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

	// Deliberately write NO supervisor-intent.json — the intent is ABSENT
	// (indistinguishable from an attacker-deleted one on this dir).

	// Precondition: confirm the read-side write-bits gate actually REJECTS
	// this dir (so the test exercises the delete-capable branch). If this
	// ever started passing, the test would be vacuous.
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

	// Assertion 1 — fail closed to strict.
	if !got {
		t.Fatal("pr301 r7 Finding 1 regression: an ABSENT supervisor-intent.json on a " +
			"DELETE-capable (0o722) POSIX state dir must fail CLOSED to strict (return true) " +
			"— an absent intent there is indistinguishable from an attacker-deleted one, and " +
			"relaxing would turn the deletion into a strict-mode bypass; got false (the pre-r7 " +
			"DaemonStateDir chmod-heal reset the dir to 0700 BEFORE the posture check, so the " +
			"verdict saw a now-SAFE dir and relaxed)")
	}

	// Assertion 2 — the posture check must NOT have healed the dir. If the
	// resolver chmodded it to 0700, that is the exact pre-r7 self-heal that
	// defeated the fix; the read-only resolver must leave the mode untouched.
	afterInfo, err := os.Stat(stateDir)
	if err != nil {
		t.Fatalf("stat state dir after: %v", err)
	}
	if afterMode := afterInfo.Mode().Perm(); afterMode != 0o722 {
		t.Fatalf("pr301 r7 Finding 1 regression: the absent-intent posture check chmod-healed "+
			"the state dir from 0o722 to %o — it must use the READ-ONLY resolver and leave the "+
			"mode untouched, so the posture check observes the ACTUAL pre-heal broadened state "+
			"(a heal to 0700 here is precisely the bug that let the deletion bypass survive)", afterMode)
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
