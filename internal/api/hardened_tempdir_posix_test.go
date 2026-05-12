//go:build !windows

// hardened_tempdir_posix_test.go — POSIX shim for hardenedTempDir.
// On POSIX the per-user 0700 trust boundary already covers t.TempDir(),
// so the function is a no-op pass-through that returns t.TempDir().

package api

import "testing"

// hardenedTempDir returns a directory whose DACL (Windows) or mode
// (POSIX) is single-user-safe per the spec's allowlist. On POSIX
// t.TempDir() already lives under the per-user $TMPDIR with 0700, so
// no extra hardening is needed.
//
// Windows callers see the leg in hardened_tempdir_windows_test.go,
// which synthesizes an allowlist-only DACL on an intermediate parent
// (strips the inherited Authenticated Users ACE that %TEMP% carries).
func hardenedTempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
