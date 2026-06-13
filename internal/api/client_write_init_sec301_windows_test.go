//go:build windows

// client_write_init_sec301_windows_test.go — Windows-only falsifying
// regressions for the absent-intent strict-mode verdict.
//
// readStrictModeFromIntentBestEffort must classify a GENUINELY ABSENT
// supervisor-intent.json by the state dir's DELETE-capability (pr301 r5
// Finding 1), not by absence alone:
//
//   - state dir merely READ-broadened (Authenticated Users GENERIC_READ —
//     the common solo-dev %LOCALAPPDATA% case) + intent absent → RELAX. A
//     non-allowlisted principal cannot delete the intent there, so absence
//     is a genuine fresh install. (Preserves pr301 r3 Finding 2's
//     missing-intent → default-relax polarity for the benign case.)
//   - state dir WRITE/DELETE-capable broadened (Authenticated Users
//     FILE_DELETE_CHILD) + intent absent → STRICT (fail closed). An absent
//     intent there is indistinguishable from an attacker-DELETED one;
//     relaxing would turn the deletion into a strict-mode bypass (the bot's
//     pr301 r4-review Finding 1).
//
// The pr301 r3 fix made the write-capable+absent case RELAX (to avoid
// mis-classifying a fresh install on a broadened corp home as strict). The
// bot's r4-review reversed that: on a delete-capable dir, relax is a
// deletion bypass, and CLAUDE.md ("Hardened state-file writes — corp-policy
// posture") already says such hosts SHOULD be strict (and MUST set
// MCPHUB_REQUIRE_SINGLE_USER_HOME=1). The r3 assertion is therefore flipped
// to strict here; the benign read-broadened case still relaxes.

package api

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadStrictModeFromIntent_DeleteCapableParentAbsentFile_FailsClosedToStrict
// is the FALSIFYING CORE of pr301 r5 Finding 1. State dir has a
// non-allowlisted DELETE-capable ACE (FILE_DELETE_CHILD, so the read-side
// write-bits gate checkStateDirParentWriteSafe REJECTS it), the intent file is
// ABSENT, and AllowUnhardenedStateReadEnv is UNSET (so the read gate is live).
// readStrictModeFromIntentBestEffort MUST fail closed to strict (return true):
// on a delete-capable dir an absent intent cannot be distinguished from an
// attacker-deleted one, so relaxing would turn the deletion into a strict-mode
// bypass.
//
// This OVERTURNS the pr301 r3 assertion (which expected relax on this exact
// shape). The r3 concern — "fresh install on a broadened home must not be
// mis-classified as strict" — is preserved only for the READ-broadened case
// (covered by TestReadStrictModeFromIntent_ReadBroadenedParentAbsentFile_Relaxes
// below), NOT the delete-capable case.
func TestReadStrictModeFromIntent_DeleteCapableParentAbsentFile_FailsClosedToStrict(t *testing.T) {
	// Read gate LIVE: do NOT set AllowUnhardenedStateReadEnv. Env strict UNSET
	// so only the intent governs.
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")

	// State root with an Authenticated Users FILE_DELETE_CHILD ACE — the
	// write/delete-capable shape checkStateDirParentWriteSafe rejects. Build it
	// under our own subdir so the PROTECTED DACL replaces (not augments) an
	// inherited %TEMP% DACL.
	stateRoot := filepath.Join(t.TempDir(), "delete-capable-state-root")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("mkdir delete-capable state root: %v", err)
	}
	broadenParentForStateFileWriteCapableTest(t, stateRoot)
	t.Cleanup(SetDaemonStateRootForTest(stateRoot))
	t.Cleanup(resetStrictModeIntentCacheForTest)

	// Deliberately write NO supervisor-intent.json — the intent is ABSENT.

	// Precondition: confirm the read-side write-bits gate actually REJECTS this
	// dir (so the test exercises the delete-capable branch). If this ever
	// started passing, the test would be vacuous.
	if err := checkStateDirParentWriteSafe(stateRoot); err == nil {
		t.Fatal("precondition: checkStateDirParentWriteSafe must reject a FILE_DELETE_CHILD " +
			"state dir; got nil — the delete-capable ACE did not take, so the branch under " +
			"test is not exercised")
	}

	if got := readStrictModeFromIntentBestEffort(); !got {
		t.Fatal("pr301 r5 Finding 1 regression: an ABSENT supervisor-intent.json on a " +
			"DELETE-capable broadened state dir must fail CLOSED to strict (return true) — an " +
			"absent intent there is indistinguishable from an attacker-deleted one, and relaxing " +
			"would turn the deletion into a strict-mode bypass; got false (the pre-r5 " +
			"unconditional-relax-on-absence that the bot flagged)")
	}
}

// TestReadStrictModeFromIntent_ReadBroadenedParentAbsentFile_Relaxes is the
// safe-default control that preserves the benign half of pr301 r3 Finding 2.
// State dir is READ-broadened (Authenticated Users GENERIC_READ — read-only, no
// delete capability), the intent file is ABSENT, and the read gate is live.
// readStrictModeFromIntentBestEffort MUST relax (return false): a non-allowlisted
// principal can read but NOT delete the intent, so absence is a genuine fresh
// install. The pr301 r5 delete-capability discriminator must NOT over-reach into
// the read-only-broadened case.
func TestReadStrictModeFromIntent_ReadBroadenedParentAbsentFile_Relaxes(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")

	// State root with an Authenticated Users GENERIC_READ ACE — trips the strict
	// parent-dir gate but PASSES the narrower write-bits check
	// checkStateDirParentWriteSafe (no FILE_DELETE_CHILD / write / DAC bits).
	stateRoot := filepath.Join(t.TempDir(), "read-broadened-state-root")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("mkdir read-broadened state root: %v", err)
	}
	broadenParentForStateFileTest(t, stateRoot)
	t.Cleanup(SetDaemonStateRootForTest(stateRoot))
	t.Cleanup(resetStrictModeIntentCacheForTest)

	// Deliberately write NO supervisor-intent.json — the intent is ABSENT.

	// Precondition: the read-only ACE must PASS the write-bits gate (else the
	// dir would be classified delete-capable and the test would assert the wrong
	// branch). A read-only-broadened dir is exactly the case the gate tolerates.
	if err := checkStateDirParentWriteSafe(stateRoot); err != nil {
		t.Fatalf("precondition: checkStateDirParentWriteSafe must PASS a read-only "+
			"(GENERIC_READ) broadened dir (no write/delete bits); got %v — the dir is being "+
			"mis-classified as delete-capable", err)
	}

	if got := readStrictModeFromIntentBestEffort(); got {
		t.Fatal("pr301 r5 Finding 1 over-reach guard: an ABSENT supervisor-intent.json on a " +
			"READ-only-broadened state dir must relax (return false) — a non-allowlisted " +
			"principal cannot delete the intent there, so absence is a genuine fresh install; " +
			"got true (the delete-capability discriminator wrongly fired on a read-only dir)")
	}
}

// TestReadStrictModeFromIntent_EnvBypassTrueEnoentDeleteCapable_FailsClosedToStrict
// pins the SECOND absent branch (the direct os.ErrNotExist path) the advisory
// REFINE flagged. With MCPHUB_ALLOW_UNHARDENED_STATE_READ=1 the read-side gate
// in ReadSupervisorIntent is SKIPPED, so on a delete-capable dir a genuinely
// (attacker-)deleted intent surfaces as a TRUE os.ErrNotExist (not a parent-gate
// error). That branch must ALSO consult absentIntentStrictVerdict and fail
// closed to strict — gating only the os.Lstat branch would leave this exact
// config exploitable.
func TestReadStrictModeFromIntent_EnvBypassTrueEnoentDeleteCapable_FailsClosedToStrict(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "")
	// Read gate BYPASSED: ReadSupervisorIntent skips checkStateDirParentWriteSafe,
	// so os.ReadFile returns a TRUE os.ErrNotExist for the absent file.
	t.Setenv(AllowUnhardenedStateReadEnv, "1")

	stateRoot := filepath.Join(t.TempDir(), "envbypass-delete-capable-state-root")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("mkdir envbypass delete-capable state root: %v", err)
	}
	broadenParentForStateFileWriteCapableTest(t, stateRoot)
	t.Cleanup(SetDaemonStateRootForTest(stateRoot))
	t.Cleanup(resetStrictModeIntentCacheForTest)

	// Deliberately write NO supervisor-intent.json — the intent is ABSENT.

	// Precondition: with the read gate bypassed, ReadSupervisorIntent must return
	// a TRUE os.ErrNotExist (so this test exercises the direct-ENOENT branch, not
	// the os.Lstat branch). If it returned a parent-gate error instead, the test
	// would not be pinning the line the REFINE targeted.
	intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
	if _, err := ReadSupervisorIntent(intentPath); err == nil {
		t.Fatal("precondition: ReadSupervisorIntent with the read gate bypassed must still error " +
			"on an absent file (os.ErrNotExist); got nil")
	}

	if got := readStrictModeFromIntentBestEffort(); !got {
		t.Fatal("pr301 r5 Finding 1 (REFINE) regression: an ABSENT intent reaching the direct " +
			"os.ErrNotExist branch (read gate bypassed via MCPHUB_ALLOW_UNHARDENED_STATE_READ=1) " +
			"on a DELETE-capable dir must fail CLOSED to strict (return true) — otherwise a " +
			"genuinely deleted intent silently disables the gate on exactly the config the env " +
			"var is set for; got false (the direct-ENOENT branch was not gated)")
	}
}
