//go:build windows

package api

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// snapshotTrapStateDir returns a state root whose DACL grants a non-allowlisted
// principal (Authenticated Users) an INHERITABLE (container+object) Modify ACE —
// the codex-sandbox %LOCALAPPDATA% broadening shape. The nested
// adopt-provenance/<manifest>/ dirs MkdirAll creates under it inherit that ACE,
// so a snapshot written via the plain backup copy would inherit it too and FAIL
// assertAdoptSnapshotOwnerOnly; only the hardened WriteStateFileBytesAtomic
// pipeline (PROTECTED owner-only DACL, inheritance stripped) yields an owner-only
// file. This makes A5 a real regression guard (mirrors the
// TestRegistrySave_BroadenedParent gold-standard trap), not an owner-only-under-
// an-already-owner-only-parent tautology.
//
// The current-user grant is ALSO inheritable full control so the owner can create
// the intermediate directories (a non-inheritable owner grant would leave the
// owner without FILE_ADD_SUBDIRECTORY on the inherited subdirs and MkdirAll would
// fail Access-Denied).
func snapshotTrapStateDir(t *testing.T) string {
	t.Helper()
	statePathsHelper(t)
	t.Setenv(RequireSingleUserHomeEnv, "") // default-relax so the broadened parent is tolerated
	dir := filepath.Join(t.TempDir(), "broadened-state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir broadened state dir: %v", err)
	}

	currentSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	authUsersSID, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatalf("Authenticated Users sid: %v", err)
	}
	const inh = windows.CONTAINER_INHERIT_ACE | windows.OBJECT_INHERIT_ACE
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inh,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(currentSID),
			},
		},
		{
			// Inheritable Modify for a non-allowlisted principal — a plain,
			// non-hardened child write would inherit this and read as broadened.
			AccessPermissions: windows.GENERIC_READ | windows.GENERIC_WRITE,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inh,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(authUsersSID),
			},
		},
	}
	applyProtectedDACLFromEntries(t, dir, entries)
	daemonStateRootOverride = dir
	return dir
}

// assertAdoptSnapshotOwnerOnly asserts the pinned snapshot file at path carries
// an owner-only DACL ({current user, LocalSystem, BuiltinAdministrators} — no
// Authenticated Users / CodexSandboxUsers / orphan SID), the same posture proven
// for every other WriteStateFileBytesAtomic output (design claim 8). Reuses the
// production verifyWindowsDACLFromHandle gate — a regression that wrote the
// secret-bearing snapshot through the plain backup copy (inheriting the broadened
// parent's Modify ACE) fails here.
func assertAdoptSnapshotOwnerOnly(t *testing.T, path string) {
	t.Helper()
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	h, err := windows.CreateFile(
		pathW,
		windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile on snapshot %s: %v", path, err)
	}
	defer windows.CloseHandle(h)
	if err := verifyWindowsDACLFromHandle(h); err != nil {
		t.Errorf("snapshot %s has a non-allowlisted DACL (secret-bearing snapshot must be hardened owner-only): %v", path, err)
	}
}
