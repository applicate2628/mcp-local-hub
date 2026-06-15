//go:build windows

// hardened_tempdir_windows_test.go — Windows leg of hardenedTempDir.
//
// The default Go test TempDir on Windows lives under %TEMP%, whose
// DACL inherits Authenticated Users (S-1-5-11) read access from the
// per-machine \Users tree. That's outside the spec's allowlist, so
// SecureWriteClientConfig and VerifyHubMcpStateDACL both reject any
// path under t.TempDir() at the parent-dir gate.
//
// Tests that exercise the happy path (round-trip writes, fresh-file
// verifies) need a parent whose DACL is allowlist-conforming. This
// helper creates an intermediate dir under t.TempDir() and applies
// the PROTECTED allowlist DACL (current-user + LocalSystem +
// BuiltinAdministrators GENERIC_ALL).

package api

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// hardenedTempDir creates `<t.TempDir()>/hardened-parent` and applies
// a PROTECTED allowlist-only DACL via SetNamedSecurityInfo. Returns
// the path. Tests should treat the return as their working parent;
// the outer t.TempDir() is still cleaned up by the test framework.
//
// PROTECTED is required because inheritance from %TEMP% would re-add
// Authenticated Users; protecting the DACL strips inherited ACEs.
func hardenedTempDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hardened-parent")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir hardened parent: %v", err)
	}
	currentSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	systemSID, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		t.Fatalf("system sid: %v", err)
	}
	adminSID, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		t.Fatalf("admin sid: %v", err)
	}
	entries := []windows.EXPLICIT_ACCESS{
		explicitAccessAllow(currentSID, windows.TRUSTEE_IS_USER, windows.GENERIC_ALL),
		explicitAccessAllow(systemSID, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_ALL),
		explicitAccessAllow(adminSID, windows.TRUSTEE_IS_GROUP, windows.GENERIC_ALL),
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
		t.Fatalf("SetNamedSecurityInfo on hardened parent: %v", err)
	}
	return dir
}
