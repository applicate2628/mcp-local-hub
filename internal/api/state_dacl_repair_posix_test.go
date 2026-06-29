//go:build !windows

package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRepairStateFileDACL_POSIXRepairsLooseCurrentUserFileAndHardenedReadPasses(t *testing.T) {
	stateDir := isolateStateDir(t)
	t.Setenv(RequireSingleUserHomeEnv, "1")

	target := filepath.Join(stateDir, "workspaces.yaml")
	if err := os.WriteFile(target, []byte("version: 1\nworkspaces: []\n"), 0o666); err != nil {
		t.Fatalf("write stale registry: %v", err)
	}
	if err := os.Chmod(target, 0o666); err != nil {
		t.Fatalf("chmod stale registry: %v", err)
	}

	if _, err := readStateFileInodeAnchored(target); err == nil {
		t.Fatalf("strict hardened read must reject loose stale file before repair")
	} else if !errors.Is(err, ErrTooLoose) {
		t.Fatalf("pre-repair read err = %v, want ErrTooLoose", err)
	}

	report, err := RepairStateFileDACL(target)
	if err != nil {
		t.Fatalf("RepairStateFileDACL: %v", err)
	}
	if report.Status != StateFileDACLRepairStatusRepaired {
		t.Fatalf("repair status = %q, want %q", report.Status, StateFileDACLRepairStatusRepaired)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat repaired file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode after repair = %04o, want 0600", got)
	}
	if _, err := readStateFileInodeAnchored(target); err != nil {
		t.Fatalf("hardened read after repair: %v", err)
	}
}

func TestRepairStateFileDACL_POSIXRepairsOwnedFileWithoutOwnerRead(t *testing.T) {
	stateDir := isolateStateDir(t)
	t.Setenv(RequireSingleUserHomeEnv, "1")

	for _, mode := range []os.FileMode{0o200, 0o022} {
		target := filepath.Join(stateDir, "mode-"+mode.String()+".yaml")
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
			t.Fatalf("mode after repair = %04o, want 0600", got)
		}
	}
}

func TestRepairStateFileDACL_POSIXRefusesFIFOWithoutBlocking(t *testing.T) {
	stateDir := isolateStateDir(t)
	target := filepath.Join(stateDir, "workspaces.yaml")
	if err := unix.Mkfifo(target, 0o666); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	type result struct {
		report StateFileDACLRepairReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := RepairStateFileDACL(target)
		done <- result{report: report, err: err}
	}()

	select {
	case got := <-done:
		if !errors.Is(got.err, ErrIrregularFile) {
			t.Fatalf("RepairStateFileDACL FIFO err = %v, want ErrIrregularFile", got.err)
		}
		if got.report.Status != StateFileDACLRepairStatusRefused {
			t.Fatalf("repair status = %q, want refused", got.report.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("RepairStateFileDACL blocked on FIFO open")
	}
}
