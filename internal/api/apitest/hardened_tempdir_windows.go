//go:build windows

// Package apitest exposes cross-package test helpers for state-file
// paths that need DACL hardening on Windows. The hardenedTempDir
// helper inside internal/api/*_test.go is package-private and cannot
// be reached from internal/cli/*_test.go; this package mirrors the
// same DACL allowlist (current user + LocalSystem + Builtin
// Administrators GENERIC_ALL, PROTECTED to strip inherited
// Authenticated Users from %TEMP%) so cross-package CLI tests can
// drive the marketplace cache write path on Windows without skipping
// the persistence invariant.
//
// codex deep-sec PR #163 lane 3 P1 closure.
package apitest

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// HardenedTempDir returns `<t.TempDir()>/hardened-parent` with a
// PROTECTED single-user DACL applied. Returns the path; the outer
// t.TempDir() is still cleaned up by the test framework.
func HardenedTempDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hardened-parent")
	return HardenedDir(t, dir)
}

// HardenedDir creates dir if needed and applies the same PROTECTED,
// owner-only DACL as HardenedTempDir. Use it when the immediate parent
// checked by the hardened state reader is a child of the temp root.
func HardenedDir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("apitest.HardenedDir mkdir %s: %v", dir, err)
	}
	currentSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("apitest.HardenedDir currentUserSID: %v", err)
	}
	systemSID, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		t.Fatalf("apitest.HardenedDir system sid: %v", err)
	}
	adminSID, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		t.Fatalf("apitest.HardenedDir admin sid: %v", err)
	}
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(currentSID),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(systemSID),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(adminSID),
			},
		},
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatalf("apitest.HardenedDir ACLFromEntries: %v", err)
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
		t.Fatalf("apitest.HardenedDir SetNamedSecurityInfo %s: %v", dir, err)
	}
	return dir
}

// currentUserSID returns the SID of the process token's user. The
// internal/api package has the same helper but it's package-private;
// duplicating here keeps apitest independent and avoids exporting a
// production-named helper for a test-only seam.
func currentUserSID() (*windows.SID, error) {
	t := windows.GetCurrentProcessToken()
	u, err := t.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid.Copy()
}
