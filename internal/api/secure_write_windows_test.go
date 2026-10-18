//go:build windows

// secure_write_windows_test.go — Windows-specific DACL synthesis tests
// for SecureWriteClientConfig's parent-dir DACL gate (spec §"Why every
// step uses dirHandle-relative ops" + step 3 of the SecureWriteClientConfig
// sequence).
//
// The reviewer flagged a coverage gap: NO existing test exercised a
// permissive parent-dir DACL. This file adds the synthesis case —
// build a parent dir whose DACL grants read to Authenticated Users
// (S-1-5-11), then call SecureWriteClientConfig with a path inside
// it. The writer MUST refuse with a parent-dir-context error.

package api

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// synthesizeDirWithAuthUsersReadACE applies a PROTECTED DACL to dir
// that grants the current user GENERIC_ALL and Authenticated Users
// (S-1-5-11) GENERIC_READ. The Authenticated Users ACE is the
// disallowed read-capable ACE — verifyWindowsDACLFromHandle MUST
// reject it.
//
// PROTECTED_DACL is required because the default DACL on a t.TempDir()
// inherits from %TEMP% (which already has SYSTEM/Admin entries that
// happen to be allowlist-conforming). Without PROTECTED we'd be
// adding an ACE rather than replacing the DACL, and the inherited
// ACEs would be inside the allowlist — masking the test signal.
func synthesizeDirWithAuthUsersReadACE(t *testing.T, dir string) {
	t.Helper()
	currentSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	authUsersSID, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatalf("Authenticated Users sid: %v", err)
	}
	// Divergent fixture (current-user GA + Authenticated Users READ);
	// not the allowlist triple. Only the apply boilerplate is shared.
	entries := []windows.EXPLICIT_ACCESS{
		explicitAccessAllow(currentSID, windows.TRUSTEE_IS_USER, windows.GENERIC_ALL),
		explicitAccessAllow(authUsersSID, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_READ),
	}
	applyProtectedDACLFromEntries(t, dir, entries)
}

// TestSecureWriteClientConfigRejectsPermissiveParentDACL builds a
// parent dir with an Authenticated Users:GenericRead ACE and asserts
// SecureWriteClientConfig refuses to write a child file because the
// parent-dir DACL falls outside the allowlist.
//
// This is the spec's step-3 gate (sequence at lines 323-326 + "Why
// every step uses dirHandle-relative ops" at lines 422-432). Without
// the gate, an enterprise GPO that broadens %USERPROFILE% to
// Domain Users would silently leak tokens to every domain user.
func TestSecureWriteClientConfigRejectsPermissiveParentDACL(t *testing.T) {
	// Create an intermediate parent so the leaky DACL is applied to a
	// dir we own outright (t.TempDir() itself sits under %TEMP% with
	// inherited ACEs we shouldn't be reshaping).
	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithAuthUsersReadACE(t, parent)

	target := filepath.Join(parent, "client-config.json")
	err := SecureWriteClientConfig(target, []byte(`{"v":1}`))
	if err == nil {
		t.Fatalf("SecureWriteClientConfig must refuse a permissive parent DACL; got nil")
	}
	// The error must mention parent-dir context so operators can
	// resolve the underlying GPO/ACL rather than chase the file path.
	msg := err.Error()
	if !strings.Contains(msg, parent) {
		t.Errorf("error message %q must mention parent dir %q", msg, parent)
	}
	if !strings.Contains(msg, "parent") {
		t.Errorf("error message %q must use the word 'parent' to signal the dir-DACL gate", msg)
	}
	// The destination file must NOT exist — a refusal that leaves a
	// half-written token at the path is worse than the missing gate.
	if _, statErr := os.Stat(target); statErr == nil {
		t.Errorf("destination %q must not exist after refusal", target)
	}
}

// The existing hook places a different native object at the destination after
// the real rename and verification open. Cleanup must follow the original held
// object even after its name changes. All files here belong to this TempDir
// fixture; "foreign" means foreign to the production write, not user state.
func TestSecureWritePostRenameFailurePreservesForeignReplacement(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "client-config.json")
	ownedAside := filepath.Join(dir, "owned-publication.json")
	stagedForeign := filepath.Join(dir, "foreign-publication.json")
	ownedBytes := []byte(`{"writer":"owned"}`)
	foreignBytes := []byte(`{"writer":"foreign"}`)
	if err := os.WriteFile(stagedForeign, foreignBytes, 0o600); err != nil {
		t.Fatalf("stage synthetic replacement: %v", err)
	}
	currentSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	authUsersSID, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatalf("Authenticated Users sid: %v", err)
	}
	applyProtectedDACLFromEntries(t, stagedForeign, []windows.EXPLICIT_ACCESS{
		explicitAccessAllow(currentSID, windows.TRUSTEE_IS_USER, windows.GENERIC_ALL),
		explicitAccessAllow(authUsersSID, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_READ),
	})
	foreignID := windowsFileIdentityForTest(t, stagedForeign)
	foreignDACL := windowsSDDLForTest(t, stagedForeign)
	if foreignDACL == "" {
		t.Fatal("replacement DACL observation is empty")
	}
	forcedFailure := errors.New("synthetic post-rename verification failure after foreign replacement")
	var setupError error
	hookEntered, replacementInstalled := false, false
	previous := postRenameVerifyFailHook
	t.Cleanup(func() { postRenameVerifyFailHook = previous })
	postRenameVerifyFailHook = func() error {
		hookEntered = true
		ownedID := windowsFileIdentityForTest(t, target)
		ownedDACL := windowsSDDLForTest(t, target)
		if sameWindowsAdoptLeaseIdentity(ownedID, foreignID) || ownedDACL == "" || ownedDACL == foreignDACL {
			setupError = errors.New("original and replacement native object/DACL observations are not distinct")
			return setupError
		}
		if err := os.Rename(target, ownedAside); err != nil {
			setupError = fmt.Errorf("move original publication with its native handles held: %w", err)
			return setupError
		}
		if err := os.Rename(stagedForeign, target); err != nil {
			setupError = fmt.Errorf("install synthetic foreign replacement: %w", err)
			return setupError
		}
		if got := windowsFileIdentityForTest(t, target); !sameWindowsAdoptLeaseIdentity(got, foreignID) {
			setupError = errors.New("installed replacement does not have the staged native identity")
			return setupError
		}
		replacementInstalled = true
		return forcedFailure
	}

	capture := newStateFileIOCapture()
	writeErr := secureWriteClientConfigImplObserved(target, ownedBytes, false, capture)
	if !hookEntered || !replacementInstalled || setupError != nil {
		t.Fatalf("native foreign-replacement window was not exercised: hook=%t installed=%t setup=%v write=%v", hookEntered, replacementInstalled, setupError, writeErr)
	}
	if !errors.Is(writeErr, forcedFailure) {
		t.Errorf("original verification failure was lost: %v", writeErr)
	}
	for _, name := range []string{"parent", "temp", "verify"} {
		close := capture.slots[name]
		if !close.Acquired || !close.Attempted || !close.Closed || close.Error != nil {
			t.Errorf("original %s handle did not settle: %+v; write=%v", name, close, writeErr)
		}
		if close.Error != nil && !errors.Is(writeErr, close.Error) {
			t.Errorf("original %s close error was lost from returned failure: %v", name, writeErr)
		}
	}
	if capture.cleanup != nil || !capture.complete() || capture.error() != nil {
		t.Errorf("owned cleanup/close remains unsettled: cleanup=%v capture=%v write=%v", capture.cleanup, capture.error(), writeErr)
	}
	gotBytes, readErr := os.ReadFile(target)
	if readErr != nil || !bytes.Equal(gotBytes, foreignBytes) {
		t.Fatalf("foreign replacement bytes were changed or deleted: read=%v bytes_match=%t", readErr, bytes.Equal(gotBytes, foreignBytes))
	}
	if got := windowsFileIdentityForTest(t, target); !sameWindowsAdoptLeaseIdentity(got, foreignID) {
		t.Error("foreign replacement native object changed during cleanup")
	}
	if got := windowsSDDLForTest(t, target); got != foreignDACL {
		t.Error("foreign replacement DACL changed during cleanup")
	}
	if _, err := os.Lstat(ownedAside); !os.IsNotExist(err) {
		t.Errorf("original moved object was not removed by its held DELETE handle: %v; cleanup=%v write=%v", err, capture.cleanup, writeErr)
	}
}
