//go:build !windows

package api

import (
	"os"
	"testing"
)

// snapshotTrapStateDir returns a hardened (owner-only) state root. POSIX
// DaemonStateDir sanity-rejects a group/world-WRITABLE state root, so the
// snapshot's parent cannot be write-broadened here; the file's own 0600 mode is
// the owner-only assertion. On POSIX the plain backup copy is ALSO 0600, so the
// meaningful DACL-inheritance-strip regression trap is the Windows leg plus the
// shared WriteStateFileBytesAtomic pipeline's own broadened-parent test.
func snapshotTrapStateDir(t *testing.T) string {
	t.Helper()
	return isolateStateDir(t)
}

// assertAdoptSnapshotOwnerOnly asserts the pinned snapshot file at path is
// owner-only (mode 0600 — no group/world bits) on POSIX. The Windows leg asserts
// the equivalent owner-only DACL via the production verifyWindowsDACLFromHandle
// gate. Both prove design claim 8: the secret-bearing snapshot is written through
// the hardened state-file pipeline, never the backup lane's plain copy.
func assertAdoptSnapshotOwnerOnly(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat snapshot %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("snapshot %s mode = %04o, want owner-only (no group/world bits)", path, perm)
	}
}
