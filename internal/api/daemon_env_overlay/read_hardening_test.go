// read_hardening_test.go — tests for the platform-specific hardened
// open used by Load (Task 2.4 of the v0.5.x Servers matrix revamp).
//
// Coverage:
//   - TestLoadRejectsDirectoryAtPath: opening a directory at the
//     overlay path returns an error mentioning the regular-file check
//     (proves the Load + hardenedOpen + IsRegular chain runs).
//   - TestCheckStateDirParentReadSafe_DefaultRelax: when neither
//     strict mode nor the unhardened-read opt-out is set, the
//     parent-DACL read gate returns nil on a host where the write-side
//     parent gate also returns nil. Skipped if the underlying gate
//     rejects (corp-managed runner) since we cannot portably contrive
//     a safe parent on every CI.
//   - TestLoadRejectsSymlink (POSIX only): a symlink at the overlay
//     path is refused by O_NOFOLLOW; the error surfaces from open().
//
// Spec: docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md
// §"Read-side hardening" + B-V4-1, B-V4-4.

package daemon_env_overlay

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	api "mcp-local-hub/internal/api"
)

// TestLoadRejectsDirectoryAtPath verifies that opening a directory at
// the overlay path is refused. The hardenedOpen + Load pipeline first
// opens the path, then Stat's the file descriptor and rejects anything
// that is not a regular file. A directory is the easiest cross-platform
// "not a regular file" probe — on Windows, hardenedOpen uses
// FILE_FLAG_BACKUP_SEMANTICS so a directory open succeeds at the
// CreateFile layer, and the IsRegular() check inside Load is what
// surfaces the rejection. On POSIX, os.OpenFile on a directory likewise
// succeeds and IsRegular() rejects.
func TestLoadRejectsDirectoryAtPath(t *testing.T) {
	dir := t.TempDir()
	overlayPath := filepath.Join(dir, "overlay-as-dir")
	if err := os.Mkdir(overlayPath, 0o700); err != nil {
		t.Fatalf("mkdir(%s): %v", overlayPath, err)
	}

	_, err := Load(overlayPath)
	if err == nil {
		t.Fatalf("Load(directory): expected error, got nil")
	}
	// We do not pin the exact substring because the platform-specific
	// hardenedOpen may surface "not a regular file" or "is a directory"
	// depending on which layer rejects. Both are acceptable evidence
	// that the irregular-file guard ran.
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "regular file") && !strings.Contains(low, "directory") {
		t.Errorf("Load(directory): error %q does not mention regular-file/directory check", err)
	}
}

// TestCheckStateDirParentReadSafe_DefaultRelax exercises the default-
// mode path of the read-side parent-DACL gate. On a host where the
// write-side parent check returns nil (no broadening of the parent
// ACL), the read-side gate should also return nil — symmetric with the
// write side.
//
// The test cannot portably contrive a parent that fails the gate
// (Windows ACL fixtures need elevation; POSIX needs a hostile uid).
// Instead the test verifies that on a tmp directory the default-relax
// path is a no-op when the write gate is also a no-op. If the write
// gate rejects the tmp dir (corp-managed host where %TEMP% is owned by
// a different SID), the test skips.
func TestCheckStateDirParentReadSafe_DefaultRelax(t *testing.T) {
	if v := os.Getenv("MCPHUB_REQUIRE_SINGLE_USER_HOME"); v == "1" || strings.EqualFold(v, "true") {
		t.Skip("strict mode active; default-relax path not exercised")
	}
	dir := t.TempDir()

	// Precondition: the write-side gate must accept the tmp dir.
	// Otherwise this test cannot distinguish "read-side gate accepted
	// because write-side accepted" from "read-side gate accepted
	// because relax fired". Skip rather than assert.
	if err := api.CheckStateDirParentWriteSafe(dir); err != nil {
		t.Skipf("write-side parent gate rejects tmp dir on this host (cannot exercise default-relax symmetry): %v", err)
	}

	if err := checkStateDirParentReadSafe(dir); err != nil {
		t.Fatalf("checkStateDirParentReadSafe(%s): unexpected error in default-relax mode: %v", dir, err)
	}
}

// TestLoadRejectsSymlink verifies that a symlink at the overlay path
// is refused. POSIX: O_NOFOLLOW causes the open to fail with ELOOP.
// Windows: skipped because creating a symlink without elevation is
// version-dependent and the test infrastructure cannot guarantee
// SeCreateSymbolicLinkPrivilege.
func TestLoadRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation on Windows requires SeCreateSymbolicLinkPrivilege; covered by reparse-point refusal in hardened-open Windows impl")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	if err := os.WriteFile(target, []byte("version: 1\ndaemons: {}\n"), 0o600); err != nil {
		t.Fatalf("write real target: %v", err)
	}
	link := filepath.Join(dir, "overlay-as-symlink.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := Load(link)
	if err == nil {
		t.Fatalf("Load(symlink): expected error, got nil")
	}
	low := strings.ToLower(err.Error())
	// Accept the canonical kernel signal (ELOOP) or any of the open()
	// surface phrasings Go's wrapping produces.
	if !strings.Contains(low, "too many levels") && !strings.Contains(low, "eloop") && !strings.Contains(low, "symbolic link") && !strings.Contains(low, "open") {
		t.Errorf("Load(symlink): error %q does not look like a symlink-refusal error", err)
	}
}
