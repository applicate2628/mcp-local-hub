//go:build linux

package api

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRepairStateFileDACL_LinuxFchmodat2FallbackRepairsOwnedFileWithoutOwnerRead(t *testing.T) {
	stateDir := isolateStateDir(t)
	t.Setenv(RequireSingleUserHomeEnv, "1")

	var fallbackForced int
	restore := setFchmodatEmptyPathForTest(func(fd int, mode uint32) error {
		fallbackForced++
		return unix.EOPNOTSUPP
	})
	t.Cleanup(restore)

	for _, mode := range []os.FileMode{0o200, 0o022} {
		target := filepath.Join(stateDir, "mode-"+strconv.FormatUint(uint64(mode), 8)+".yaml")
		if err := os.WriteFile(target, []byte("version: 1\nworkspaces: []\n"), 0o600); err != nil {
			t.Fatalf("write stale registry: %v", err)
		}
		if err := os.Chmod(target, mode); err != nil {
			t.Fatalf("chmod stale registry to %04o: %v", mode, err)
		}

		report, err := RepairStateFileDACL(target)
		if err != nil {
			t.Fatalf("RepairStateFileDACL mode %04o: %v", mode, err)
		}
		if report.Status != StateFileDACLRepairStatusRepaired {
			t.Fatalf("repair status for mode %04o = %q, want %q", mode, report.Status, StateFileDACLRepairStatusRepaired)
		}
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("stat repaired file: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("mode after fallback repair = %04o, want 0600", got)
		}
	}
	if fallbackForced != 2 {
		t.Fatalf("fallback hook calls = %d, want 2", fallbackForced)
	}
}

func setFchmodatEmptyPathForTest(fn func(fd int, mode uint32) error) func() {
	orig := fchmodatEmptyPathForRepair
	fchmodatEmptyPathForRepair = fn
	return func() { fchmodatEmptyPathForRepair = orig }
}
