//go:build !windows

// hardened_tempdir_posix_test.go — POSIX shim for hardenedTempDir.
// Creates a subdirectory under t.TempDir() and chmod's it to 0700 so
// the secure-write parent-dir gate (0o077 mask via
// verifyPosixParentDirFromFd) accepts it. Default Go test temp paths
// resolve to 0755 on most Linux hosts because $TMPDIR inherits the
// system umask; that 0o022 group/world-read bit would otherwise cause
// the new gate to reject every happy-path test before the behavior
// under test is reached (codex bot r5 P1 closure).

package api

import (
	"os"
	"path/filepath"
	"testing"
)

// hardenedTempDir returns a directory whose DACL (Windows) or mode
// (POSIX) is single-user-safe per the spec's allowlist. On POSIX we
// create a subdirectory of t.TempDir(), chmod it to 0700, and return
// the subdir. The intermediate t.TempDir() ancestors are still
// 0755 — but the parent-dir gate only inspects the IMMEDIATE parent
// of the file being written, which is this 0700 subdir.
//
// Windows callers see the leg in hardened_tempdir_windows_test.go,
// which synthesizes an allowlist-only DACL on an intermediate parent
// (strips the inherited Authenticated Users ACE that %TEMP% carries).
func hardenedTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	hardened := filepath.Join(root, "hardened")
	if err := os.Mkdir(hardened, 0o700); err != nil {
		t.Fatalf("hardenedTempDir mkdir: %v", err)
	}
	// Defensive: explicit chmod after creation in case umask
	// stripped bits from the mode arg to os.Mkdir (the syscall
	// applies mode & ~umask, so an unusual umask could leave us
	// with 0700 anyway, but the explicit chmod normalizes the
	// post-mkdir state and survives any future umask drift).
	if err := os.Chmod(hardened, 0o700); err != nil {
		t.Fatalf("hardenedTempDir chmod: %v", err)
	}
	return hardened
}
