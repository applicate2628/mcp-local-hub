//go:build windows

// client_write_init_sec301_windows_test.go — Windows-only regressions for
// the strict-mode-from-intent verdict on BROADENED state dirs (pr301 r10:
// the strict_mode bit is read GATE-FREE).
//
// pr301 r10 FLEET-SAFETY FIX. readStrictModeFromIntentBestEffort reads the
// strict_mode bit with a GATE-FREE os.ReadFile (NOT through
// ReadSupervisorIntent's parent-dir WRITE-protection gate). On a broadened
// %LOCALAPPDATA% parent (write/delete-capable ACE — the common corp AND many
// solo-dev cases) the pre-r10 gated read REJECTED, the present-but-unreadable
// branch fired, and the verdict became strict=TRUE even when strict_mode was
// FALSE on disk — driving a host whose strict_mode is actually false into the
// STRICT refusal path so ALL client/state writes were refused (a live-fleet
// break). The gate-free read fixes that: a broadened dir now yields
// relax-via-intent unless strict_mode is EXPLICITLY true.
//
// The canonical CLAUDE.md posture ("Hardened state-file writes"): a broadened
// state dir defaults to RELAX; STRICT is opt-in via
// MCPHUB_REQUIRE_SINGLE_USER_HOME=1. Verdicts on a broadened dir:
//
//   - intent ABSENT (read- OR delete-broadened) → RELAX (no strict_mode declared).
//   - intent PRESENT + strict_mode=false (delete-broadened) → RELAX. THIS is
//     the fleet-break the gate-free read fixes; pre-r10 it was strict=TRUE.
//   - intent PRESENT + strict_mode=true (delete-broadened) → STRICT. SEC-F2's
//     real purpose — honor the explicitly-enabled bit — still holds gate-free.
//
// A co-resident attacker on a delete-capable dir who tampers/deletes the bit
// can only force RELAX (never strict) — the documented best-effort limitation
// whose robust mitigation is MCPHUB_REQUIRE_SINGLE_USER_HOME=1.
//
// pr301 history: r3 made absent → relax via a gated read + os.Lstat
// disambiguation; r5/r6/r7 over-reached (delete-capable absent → strict);
// r9 reverted that to absent → relax (still gated, still present→strict);
// r10 makes the READ gate-free, so PRESENT strict_mode=false on a broadened
// dir relaxes (the fleet-safety fix).

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

// TestReadStrictModeFromIntent_EnvBypassTrueEnoentDeleteCapable_Relaxes pins
// that an ABSENT intent on a delete-capable dir relaxes (returns false). With
// the gate-free read (pr301 r10) the strict_mode bit is read via a plain
// os.ReadFile that returns os.ErrNotExist for the absent file regardless of the
// MCPHUB_ALLOW_UNHARDENED_STATE_READ env var (that env var governs
// ReadSupervisorIntent's gate, which the gate-free read no longer calls). The
// env var is set here only to keep the historical scenario shape; the verdict
// is relax either way.
func TestReadStrictModeFromIntent_EnvBypassTrueEnoentDeleteCapable_Relaxes(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedStateReadEnv, "1")

	stateRoot := filepath.Join(t.TempDir(), "envbypass-delete-capable-state-root")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("mkdir envbypass delete-capable state root: %v", err)
	}
	broadenParentForStateFileWriteCapableTest(t, stateRoot)
	t.Cleanup(SetDaemonStateRootForTest(stateRoot))
	t.Cleanup(resetStrictModeIntentCacheForTest)

	// Deliberately write NO supervisor-intent.json — the intent is ABSENT.

	if got := readStrictModeFromIntentBestEffort(); got {
		t.Fatal("pr301 r10: an ABSENT intent on a DELETE-capable broadened dir must RELAX " +
			"(return false) — an absent intent declares no strict_mode; got true")
	}
}

// TestReadStrictModeFromIntent_DeleteCapableParentPresentStrictFalse_Relaxes is
// the FLEET-SAFETY merge-blocker regression for pr301 r10 — it reproduces THIS
// HOST exactly: a broadened, delete-capable state dir (FILE_DELETE_CHILD, which
// ReadSupervisorIntent's parent WRITE gate REJECTS) + a PRESENT
// supervisor-intent.json with strict_mode=FALSE + env unset. The verdict MUST be
// RELAX (false).
//
// Pre-r10 this returned TRUE: the gated ReadSupervisorIntent rejected on the
// broadened parent, the present-but-unreadable branch fired, and the gate became
// strict=TRUE — so a host whose strict_mode is FALSE on disk was driven into the
// STRICT refusal path and ALL client/state writes were refused (the live-fleet
// break). The gate-free read fixes that: the file's OWN owner-only DACL is
// readable, json.Unmarshal yields strict_mode=false, and the verdict relaxes.
//
// This is the test that would have caught the fleet-break before merge.
func TestReadStrictModeFromIntent_DeleteCapableParentPresentStrictFalse_Relaxes(t *testing.T) {
	// Read gate LIVE (do NOT set AllowUnhardenedStateReadEnv); env strict UNSET
	// so only the intent governs — the exact posture of the broken host.
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")

	stateRoot := filepath.Join(t.TempDir(), "present-strictfalse-delete-capable-state-root")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("mkdir delete-capable state root: %v", err)
	}
	broadenParentForStateFileWriteCapableTest(t, stateRoot)
	t.Cleanup(SetDaemonStateRootForTest(stateRoot))
	t.Cleanup(resetStrictModeIntentCacheForTest)

	// Write a PRESENT intent with strict_mode=false directly (the file's own
	// DACL is irrelevant to the gate-free read's success; what the pre-r10 gated
	// read tripped on was the PARENT dir's broadened ACL, not the file's).
	intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
	if err := os.WriteFile(intentPath, []byte(`{"version":1,"strict_mode":false}`), 0o600); err != nil {
		t.Fatalf("write present strict_mode=false intent: %v", err)
	}

	// Precondition: confirm the read-side WRITE gate actually REJECTS this dir —
	// the exact condition that made the pre-r10 gated read fail closed to strict.
	// If this ever started passing, the test would no longer reproduce the
	// fleet-break shape.
	if err := checkStateDirParentWriteSafe(stateRoot); err == nil {
		t.Fatal("precondition: checkStateDirParentWriteSafe must reject a FILE_DELETE_CHILD " +
			"state dir; got nil — the delete-capable ACE did not take, so the fleet-break shape " +
			"is not reproduced")
	}

	if got := readStrictModeFromIntentBestEffort(); got {
		t.Fatal("pr301 r10 FLEET-SAFETY regression: a PRESENT supervisor-intent.json with " +
			"strict_mode=FALSE on a DELETE-capable broadened state dir (env unset) MUST RELAX " +
			"(return false). got true — the pre-r10 gated read rejected on the broadened parent " +
			"and forced strict=TRUE, which would REFUSE all client/state writes and break the " +
			"live fleet on a host whose strict_mode is actually false")
	}
}

// TestReadStrictModeFromIntent_DeleteCapableParentPresentStrictTrue_Strict pins
// that SEC-F2's real purpose survives the gate-free read: a PRESENT intent with
// strict_mode=TRUE on the SAME broadened delete-capable dir (env unset) must
// resolve to STRICT (true). The operator who ran `mcphub strict-mode enable`
// gets their explicitly-enabled strict posture honored even on a broadened
// parent — the gate-free read does not weaken the honest strict case.
func TestReadStrictModeFromIntent_DeleteCapableParentPresentStrictTrue_Strict(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")

	stateRoot := filepath.Join(t.TempDir(), "present-stricttrue-delete-capable-state-root")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("mkdir delete-capable state root: %v", err)
	}
	broadenParentForStateFileWriteCapableTest(t, stateRoot)
	t.Cleanup(SetDaemonStateRootForTest(stateRoot))
	t.Cleanup(resetStrictModeIntentCacheForTest)

	intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
	if err := os.WriteFile(intentPath, []byte(`{"version":1,"strict_mode":true}`), 0o600); err != nil {
		t.Fatalf("write present strict_mode=true intent: %v", err)
	}

	// Precondition: the dir is genuinely delete-capable (the WRITE gate rejects).
	if err := checkStateDirParentWriteSafe(stateRoot); err == nil {
		t.Fatal("precondition: checkStateDirParentWriteSafe must reject a FILE_DELETE_CHILD " +
			"state dir; got nil — the delete-capable ACE did not take")
	}

	if got := readStrictModeFromIntentBestEffort(); !got {
		t.Fatal("pr301 r10: a PRESENT supervisor-intent.json with strict_mode=TRUE on a " +
			"DELETE-capable broadened state dir MUST resolve to STRICT (return true) — the " +
			"gate-free read honors the explicitly-enabled bit; SEC-F2's purpose must survive " +
			"the read becoming gate-free. got false")
	}
}
