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

func TestStateFileReadPaths_WrongOwnerParentDefaultModeRefuse(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("UID-changing capability requires root")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")

	parent := filepath.Join(t.TempDir(), "wrong-owner-read-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	supervisorPath := filepath.Join(parent, supervisorIntentFileLeaf)
	if err := os.WriteFile(supervisorPath, []byte(`{"version":1,"strict_mode":false}`), 0o600); err != nil {
		t.Fatalf("write supervisor intent: %v", err)
	}
	daemonPath := filepath.Join(parent, intentFileLeaf)
	if err := os.WriteFile(daemonPath, []byte(`{"tasks":{}}`), 0o600); err != nil {
		t.Fatalf("write daemon intent: %v", err)
	}
	hubPath := filepath.Join(parent, hubMcpEndpointFileLeaf)
	if err := os.WriteFile(hubPath, []byte(`{"url":"http://127.0.0.1:1234/mcp"}`), 0o600); err != nil {
		t.Fatalf("write hub endpoint: %v", err)
	}

	const nobody = 65534
	if err := os.Chown(parent, nobody, nobody); err != nil {
		t.Skipf("Chown to nobody failed (%v); test only runs as root with chown capability", err)
	}
	t.Cleanup(func() {
		_ = os.Chown(parent, os.Getuid(), os.Getgid())
		_ = os.Chmod(parent, 0o700)
	})

	cases := []struct {
		name string
		read func() error
	}{
		{
			name: "generic inode-anchored state read",
			read: func() error {
				_, err := readStateFileInodeAnchored(supervisorPath)
				return err
			},
		},
		{
			name: "env-strict-only posture state read",
			read: func() error {
				_, err := readStateFileInodeAnchoredEnvStrictOnly(supervisorPath)
				return err
			},
		},
		{
			name: "daemon-intent capped read owner",
			read: func() error {
				_, err := readDaemonIntentFileInodeAnchored(daemonPath)
				return err
			},
		},
		{
			name: "hub-mcp state file read wrapper",
			read: func() error {
				t.Cleanup(SetDaemonStateRootForTest(parent))
				_, err := readHubMcpStateFile(hubMcpEndpointFileLeaf)
				return err
			},
		},
		{
			name: "legacy hub-mcp dacl verifier",
			read: func() error {
				return VerifyHubMcpStateDACL(hubPath)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.read(); !errors.Is(err, ErrWrongOwner) {
				t.Fatalf("wrong-owner parent read err = %v, want ErrWrongOwner", err)
			}
		})
	}
}
