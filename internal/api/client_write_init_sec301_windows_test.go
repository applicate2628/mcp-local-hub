//go:build windows

// client_write_init_sec301_windows_test.go — Windows-only regressions for
// the absent-intent strict-mode verdict (pr301 r9: absent → RELAX).
//
// readStrictModeFromIntentBestEffort classifies a GENUINELY ABSENT
// supervisor-intent.json as RELAX regardless of the state dir's broadening,
// because an ABSENT intent declares NO strict_mode — it must not make the
// operator strict. This is the canonical CLAUDE.md posture ("Hardened
// state-file writes"): a broadened state dir defaults to RELAX; STRICT is
// opt-in via MCPHUB_REQUIRE_SINGLE_USER_HOME=1.
//
//   - state dir merely READ-broadened (Authenticated Users GENERIC_READ —
//     the common solo-dev %LOCALAPPDATA% case) + intent absent → RELAX.
//   - state dir WRITE/DELETE-capable broadened (Authenticated Users
//     FILE_DELETE_CHILD) + intent absent → RELAX. The deletion-of-a-strict-
//     intent bypass on such a dir is a KNOWN, DOCUMENTED limitation whose
//     robust mitigation is the env var; intent-file strict is best-effort.
//
// pr301 history: r3 made absent → relax; r5/r6/r7 over-reached to make a
// delete-capable absent dir → strict (a deletion-bypass guard); r9 REVERTED
// that over-reach as canon-contradicting (an absent intent declares no
// strict_mode, and the documented mitigation for the delete-capable host is
// MCPHUB_REQUIRE_SINGLE_USER_HOME=1, not making an absence strict). Both the
// delete-capable and read-broadened absent cases relax here.

package api

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadStrictModeFromIntent_DeleteCapableParentAbsentFile_Relaxes pins the
// canon-aligned verdict (pr301 r9 revert): an ABSENT supervisor-intent.json on
// a DELETE-capable broadened state dir (FILE_DELETE_CHILD, so the read-side
// write-bits gate checkStateDirParentWriteSafe REJECTS it) relaxes (returns
// false). An absent intent declares no strict_mode, so it must not make the
// operator strict; the deletion-of-a-strict-intent bypass on this dir shape is
// a KNOWN limitation whose mitigation is MCPHUB_REQUIRE_SINGLE_USER_HOME=1, not
// an absent-strict verdict.
//
// This REVERTS the pr301 r5/r6/r7 assertion (which expected strict on this exact
// shape). The deletion-bypass concern is intentionally accepted as a documented
// best-effort limitation (CLAUDE.md "Hardened state-file writes").
func TestReadStrictModeFromIntent_DeleteCapableParentAbsentFile_Relaxes(t *testing.T) {
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
	// dir (so the test exercises the delete-capable shape). If this ever started
	// passing, the test would be vacuous (it would no longer prove that even a
	// delete-capable dir relaxes on absence).
	if err := checkStateDirParentWriteSafe(stateRoot); err == nil {
		t.Fatal("precondition: checkStateDirParentWriteSafe must reject a FILE_DELETE_CHILD " +
			"state dir; got nil — the delete-capable ACE did not take, so the branch under " +
			"test is not exercised")
	}

	if got := readStrictModeFromIntentBestEffort(); got {
		t.Fatal("pr301 r9 revert regression: an ABSENT supervisor-intent.json on a " +
			"DELETE-capable broadened state dir must RELAX (return false) — an absent intent " +
			"declares no strict_mode, so it must not make the operator strict; the deletion " +
			"bypass on such a dir is a documented best-effort limitation whose mitigation is " +
			"MCPHUB_REQUIRE_SINGLE_USER_HOME=1; got true (the reverted r5/r6/r7 absent-strict " +
			"over-reach that contradicted the canonical RELAX-by-default posture)")
	}
}

// TestReadStrictModeFromIntent_ReadBroadenedParentAbsentFile_Relaxes is the
// benign-case control: a READ-broadened state dir (Authenticated Users
// GENERIC_READ — read-only, no delete capability) with an ABSENT intent and the
// read gate live relaxes (returns false). Unchanged across the r9 revert (it
// already relaxed under r5/r6/r7 too) — kept as coverage that the common
// read-broadened solo-dev %LOCALAPPDATA% case relaxes.
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

	// Precondition: the read-only ACE must PASS the write-bits gate (so this is
	// genuinely the read-broadened, not delete-capable, shape). A read-only-
	// broadened dir is exactly the case the gate tolerates.
	if err := checkStateDirParentWriteSafe(stateRoot); err != nil {
		t.Fatalf("precondition: checkStateDirParentWriteSafe must PASS a read-only "+
			"(GENERIC_READ) broadened dir (no write/delete bits); got %v — the dir is being "+
			"mis-classified as delete-capable", err)
	}

	if got := readStrictModeFromIntentBestEffort(); got {
		t.Fatal("an ABSENT supervisor-intent.json on a READ-only-broadened state dir must " +
			"relax (return false) — it is a genuine fresh install; got true")
	}
}

// TestReadStrictModeFromIntent_EnvBypassTrueEnoentDeleteCapable_Relaxes pins the
// direct os.ErrNotExist branch (read gate bypassed via
// MCPHUB_ALLOW_UNHARDENED_STATE_READ=1) on a delete-capable dir: a genuinely
// absent intent surfaces as a TRUE os.ErrNotExist, and that branch relaxes
// (returns false), the same canon-aligned verdict as the os.Lstat branch.
//
// This REVERTS the pr301 r5 (REFINE) assertion that gated this branch to strict.
// Both absent branches now relax (pr301 r9).
func TestReadStrictModeFromIntent_EnvBypassTrueEnoentDeleteCapable_Relaxes(t *testing.T) {
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
	// the os.Lstat branch).
	intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
	if _, err := ReadSupervisorIntent(intentPath); err == nil {
		t.Fatal("precondition: ReadSupervisorIntent with the read gate bypassed must still error " +
			"on an absent file (os.ErrNotExist); got nil")
	}

	if got := readStrictModeFromIntentBestEffort(); got {
		t.Fatal("pr301 r9 revert regression: an ABSENT intent reaching the direct os.ErrNotExist " +
			"branch (read gate bypassed via MCPHUB_ALLOW_UNHARDENED_STATE_READ=1) on a " +
			"DELETE-capable dir must RELAX (return false) — an absent intent declares no " +
			"strict_mode; got true (the reverted r5 REFINE absent-strict gating of this branch)")
	}
}
