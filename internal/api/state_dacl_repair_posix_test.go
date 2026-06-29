//go:build !windows

package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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

func TestFindStateFileDACLRepairCandidates_POSIXListsOnlyLooseStateFiles(t *testing.T) {
	stateDir := isolateStateDir(t)
	unsafe := filepath.Join(stateDir, "workspaces.yaml")
	safe := filepath.Join(stateDir, "supervisor-intent.json")
	if err := os.WriteFile(unsafe, []byte("version: 1\nworkspaces: []\n"), 0o600); err != nil {
		t.Fatalf("write unsafe: %v", err)
	}
	if err := os.WriteFile(safe, []byte(`{"strict_mode":false}`), 0o600); err != nil {
		t.Fatalf("write safe: %v", err)
	}
	if err := os.Chmod(unsafe, 0o660); err != nil {
		t.Fatalf("chmod unsafe: %v", err)
	}

	candidates, err := FindStateFileDACLRepairCandidates(stateDir)
	if err != nil {
		t.Fatalf("FindStateFileDACLRepairCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1 (%+v)", len(candidates), candidates)
	}
	if candidates[0].Path != unsafe {
		t.Fatalf("candidate path = %q, want %q", candidates[0].Path, unsafe)
	}
}
