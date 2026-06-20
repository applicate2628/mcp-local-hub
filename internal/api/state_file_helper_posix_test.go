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
	"errors"
	"os"
	"path/filepath"
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

func TestWriteStateFileAtomic_WrongOwnerParentDefaultModeRefuses(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("UID-changing capability requires root")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedClientWriteEnv, "")

	parent := filepath.Join(t.TempDir(), "wrong-owner-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	const nobody = 65534
	if err := os.Chown(parent, nobody, nobody); err != nil {
		t.Skipf("Chown to nobody failed (%v); test only runs as root with chown capability", err)
	}
	t.Cleanup(func() {
		_ = os.Chown(parent, os.Getuid(), os.Getgid())
		_ = os.Chmod(parent, 0o700)
	})

	dst := filepath.Join(parent, "supervisor-intent.json")
	err := WriteStateFileAtomic(dst, map[string]string{"v": "1"})
	if err == nil {
		t.Fatalf("wrong-owner parent must be refused in default mode; got nil")
	}
	if !errors.Is(err, ErrWrongOwner) {
		t.Fatalf("err = %v, want ErrWrongOwner", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("wrong-owner refusal leaked a write at %s (stat err = %v)", dst, statErr)
	}
}
