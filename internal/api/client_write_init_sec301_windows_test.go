//go:build windows

// client_write_init_sec301_windows_test.go — Windows-only falsifying
// regression for pr301 r3 Finding 2.
//
// readStrictModeFromIntentBestEffort must distinguish a genuinely ABSENT
// supervisor-intent.json (fresh install → relax) from a read FAILURE on an
// EXISTING intent (fail-closed → strict). The disambiguation matters on a
// Windows host whose state dir inherited a non-allowlisted WRITE/delete ACE:
// ReadSupervisorIntent runs the read-side parent-DACL gate
// (checkStateDirParentWriteSafe) BEFORE os.ReadFile, so on such a host a
// genuinely missing intent surfaces as a parent-gate error, NOT os.ErrNotExist.
//
// Pre-fix, readStrictModeFromIntentBestEffort treated every non-ENOENT error
// as "intent exists → fail closed strict", so a fresh install on a broadened
// home was mis-classified as strict — first-run client-config writes would use
// the strict-refusal path instead of the documented absent-intent default-relax
// behavior. The fix adds a gate-free os.Lstat probe: an absent path resolves to
// relax even when the read-side gate would have errored.
//
// This case is Windows-specific by construction: on POSIX, DaemonStateDir()'s
// ensureStateRoot chmods the (override) state root back to 0700 before the read
// gate runs, so a missing intent there surfaces as plain os.ErrNotExist and the
// pre-existing ENOENT branch already relaxes. On Windows ensureStateRoot is
// MkdirAll-only and never resets the inherited DACL, so the broadened ACE
// survives to the read gate — the only platform where the mis-classification
// actually fires.

package api

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadStrictModeFromIntent_BroadenedParentAbsentFile_Relaxes is the
// FALSIFYING CORE of pr301 r3 Finding 2. State dir has a non-allowlisted
// write-capable ACE (so the read-side parent gate errors), the intent file is
// ABSENT, and AllowUnhardenedStateReadEnv is UNSET (so the read gate is live).
// readStrictModeFromIntentBestEffort MUST relax (return false), because the
// intent is genuinely absent. Pre-fix it returned true (fail-closed strict)
// because the parent-gate error is not os.ErrNotExist.
func TestReadStrictModeFromIntent_BroadenedParentAbsentFile_Relaxes(t *testing.T) {
	// Read gate LIVE: do NOT set AllowUnhardenedStateReadEnv. Env strict UNSET
	// so only the intent governs.
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedStateReadEnv, "")

	// State root with an Authenticated Users FILE_DELETE_CHILD ACE — the
	// write-capable shape checkStateDirParentWriteSafe (the read-side gate
	// ReadSupervisorIntent runs) rejects. Build it under our own subdir so the
	// PROTECTED DACL replaces (not augments) an inherited %TEMP% DACL.
	stateRoot := filepath.Join(t.TempDir(), "broadened-state-root")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("mkdir broadened state root: %v", err)
	}
	broadenParentForStateFileWriteCapableTest(t, stateRoot)
	t.Cleanup(SetDaemonStateRootForTest(stateRoot))
	t.Cleanup(resetStrictModeIntentCacheForTest)

	// Deliberately write NO supervisor-intent.json — the intent is ABSENT.

	// Precondition: confirm ReadSupervisorIntent does NOT return os.ErrNotExist
	// here (it returns the parent-gate error instead), which is the exact
	// condition that drove the pre-fix mis-classification. If this ever started
	// returning ENOENT, the test would be vacuous, so assert the wire.
	intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
	if _, err := ReadSupervisorIntent(intentPath); err == nil {
		t.Fatal("precondition: ReadSupervisorIntent on a broadened parent with the read gate live " +
			"must error (parent-gate refusal); got nil — the broadened ACE did not take")
	}

	if got := readStrictModeFromIntentBestEffort(); got {
		t.Fatal("pr301 r3 Finding 2 regression: an ABSENT supervisor-intent.json on a broadened " +
			"state dir (read-side parent gate errors, not os.ErrNotExist) must relax (return false) " +
			"— a fresh install on a broadened home must NOT be mis-classified as strict; got true " +
			"(the pre-fix fail-closed-on-any-non-ENOENT-error path)")
	}
}
