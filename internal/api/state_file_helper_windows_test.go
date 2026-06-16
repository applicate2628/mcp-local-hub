//go:build windows

// state_file_helper_windows_test.go — Windows-only test fixtures
// and DACL-verification test for v0.5.0 Fix Group 5
// (WriteStateFileAtomic hardened-pipeline coverage).
//
// Three responsibilities:
//
//   1. broadenParentForStateFileTest — synthesize a parent dir with
//      Authenticated Users (S-1-5-11) GENERIC_READ ACE. Trips the
//      SecureWriteClientConfig parent-dir gate but PASSES the narrower
//      write-bits check in checkStateDirParentWriteSafe.
//
//   2. broadenParentForStateFileWriteCapableTest — synthesize a parent
//      dir with Authenticated Users FILE_DELETE_CHILD ACE. Trips BOTH
//      gates — the relax lane fires, checkStateDirParentWriteSafe
//      refuses, and the writer surfaces "TOCTOU swap risk".
//
//   3. TestWriteStateFileAtomic_PostRenameDACLVerify — happy-path DACL
//      assertion. After a successful write the destination file's DACL
//      must contain ONLY {current-user, LocalSystem, BuiltinAdministrators}.
//      Authenticated Users / Everyone / Domain Users etc. must NOT
//      appear — proves the hardened pipeline (handle-bound DACL apply
//      + post-rename re-verify) wired through correctly.

package api

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// broadenParentForStateFileTest builds a parent dir whose DACL grants
// the current user GENERIC_ALL and Authenticated Users (S-1-5-11)
// GENERIC_READ — the read-only ACE shape. Trips the strict parent-dir
// gate but is tolerable under checkStateDirParentWriteSafe (no
// write/delete/DAC-edit/delete-child bits).
func broadenParentForStateFileTest(t *testing.T, parent string) {
	t.Helper()
	synthesizeDirWithAuthUsersReadACE(t, parent)
}

// broadenParentForStateFileWriteCapableTest builds a parent dir
// whose DACL grants Authenticated Users FILE_DELETE_CHILD (0x40).
// FILE_DELETE_CHILD on a directory lets the principal delete child
// entries — a TOCTOU swap risk between secure-write completion and
// any subsequent read. checkStateDirParentWriteSafe refuses this
// parent in the relax lane.
func broadenParentForStateFileWriteCapableTest(t *testing.T, parent string) {
	t.Helper()
	currentSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	authUsersSID, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatalf("Authenticated Users sid: %v", err)
	}
	// FILE_DELETE_CHILD (0x40) is the canonical directory-only write
	// bit that the write-bits check rejects. Other bits in
	// windowsDACLWriteOrAdminBits (FILE_WRITE_DATA, DELETE,
	// WRITE_DAC, WRITE_OWNER, etc.) work equally; FILE_DELETE_CHILD
	// is the most pointed example of namespace tamper rights.
	// Divergent fixture (current-user GA + Authenticated Users
	// FILE_DELETE_CHILD); not the allowlist triple. Only the apply
	// boilerplate is shared.
	entries := []windows.EXPLICIT_ACCESS{
		explicitAccessAllow(currentSID, windows.TRUSTEE_IS_USER, windows.GENERIC_ALL),
		explicitAccessAllow(authUsersSID, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windowsFileDeleteChild),
	}
	applyProtectedDACLFromEntries(t, parent, entries)
}

// TestWriteStateFileAtomic_PostRenameDACLVerify pins falsifiable
// claim #2 (restrictive DACL installed on file HANDLE before bytes
// write) + claim #8 (post-rename DACL re-verify) via end-state
// observation: after the writer returns success, the file's DACL
// must allowlist-conform. The hardened pipeline runs both the
// handle-bound apply at create time AND the re-open + re-verify
// after rename — a regression that drops either step would produce
// a file with non-allowlisted principals on its DACL.
//
// The test runs on a hardened parent (no permissive ACEs) so the
// strict path succeeds without invoking the relax lane.
func TestWriteStateFileAtomic_PostRenameDACLVerify(t *testing.T) {
	parent := hardenedTempDir(t)
	dst := filepath.Join(parent, "supervisor-intent.json")

	if err := WriteStateFileAtomic(dst, map[string]string{"v": "1"}); err != nil {
		t.Fatalf("write: %v", err)
	}

	pathW, err := windows.UTF16PtrFromString(dst)
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
		t.Fatalf("CreateFile on written file: %v", err)
	}
	defer windows.CloseHandle(h)

	// verifyWindowsDACLFromHandle enforces the {current-user,
	// LocalSystem, BuiltinAdministrators} allowlist. Any ALLOW ACE
	// outside that set with significant access bits surfaces as a
	// non-nil error.
	if err := verifyWindowsDACLFromHandle(h); err != nil {
		t.Errorf("WriteStateFileAtomic produced a file with non-allowlisted DACL — handle-bound apply or post-rename re-verify regressed: %v", err)
	}

	// Sanity: no leftover temp file beside the destination + the
	// per-file flock leaf.
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "supervisor-intent.json" {
			continue
		}
		if name == "supervisor-intent.json.lock" {
			continue
		}
		t.Errorf("unexpected file in parent: %s (want supervisor-intent.json + .lock)", name)
	}
}
