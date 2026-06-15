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
	entries := []windows.EXPLICIT_ACCESS{
		explicitAccessAllow(currentSID, windows.TRUSTEE_IS_USER, windows.GENERIC_ALL),
		explicitAccessAllow(authUsersSID, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_READ),
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatalf("ACLFromEntries: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatalf("SetNamedSecurityInfo on parent dir: %v", err)
	}
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
