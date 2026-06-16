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
	entries, err := allowlistExplicitAccess()
	if err != nil {
		t.Fatalf("allowlistExplicitAccess: %v", err)
	}
	applyProtectedDACLFromEntries(t, dir, entries)
	return dir
}
