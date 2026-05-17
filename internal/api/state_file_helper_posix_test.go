//go:build !windows

// state_file_helper_posix_test.go — POSIX test fixtures for v0.5.0
// Fix Group 5 (WriteStateFileAtomic hardened-pipeline coverage).
//
// Two parent-dir shapes:
//
//   - broadenParentForStateFileTest:
//       group/world READ bits (0o755 — read+exec, no write). This
//       fails the SecureWriteClientConfig parent-dir gate (which
//       rejects ANY non-0700 mode) so the strict path returns
//       ErrSecureWriteParentInsecure, but does NOT fail the narrower
//       write-bits check inside checkStateDirParentWriteSafe. Used by
//       the strict-mode + default-relax-happy-path tests.
//
//   - broadenParentForStateFileWriteCapableTest:
//       group/world WRITE bits (0o722). This fails both gates.
//       checkStateDirParentWriteSafe surfaces the "TOCTOU swap risk"
//       error in the default-relax lane.

package api

import (
	"os"
	"testing"
)

// broadenParentForStateFileTest sets parent's mode to 0o755 (owner
// rwx, group/world r-x). This is the canonical "broad read but no
// write" shape that the strict parent-dir gate rejects while
// checkStateDirParentWriteSafe accepts.
func broadenParentForStateFileTest(t *testing.T, parent string) {
	t.Helper()
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatalf("chmod 0o755 parent: %v", err)
	}
}

// broadenParentForStateFileWriteCapableTest sets parent's mode to
// 0o722 (owner rwx, group/world -w-). The 0o022 write bits trip
// checkStateDirParentWriteSafe even in the default-relax lane,
// returning a "TOCTOU swap risk" error.
func broadenParentForStateFileWriteCapableTest(t *testing.T, parent string) {
	t.Helper()
	if err := os.Chmod(parent, 0o722); err != nil {
		t.Fatalf("chmod 0o722 parent: %v", err)
	}
}
