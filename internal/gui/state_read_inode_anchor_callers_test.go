package gui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInodeAnchorReadPidportRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target-gui.pidport")
	if err := os.WriteFile(target, []byte(formatPidport(111, 222)), 0o600); err != nil {
		t.Fatalf("write target pidport: %v", err)
	}
	link := filepath.Join(dir, "gui.pidport")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported in this environment: %v", err)
	}

	if _, _, err := ReadPidport(link); err == nil {
		t.Fatalf("ReadPidport followed a symlink; want inode-anchor refusal")
	}
}

func TestInodeAnchorSupervisorRestartOwnerRejectsSymlink(t *testing.T) {
	stateDir := t.TempDir()
	target := filepath.Join(stateDir, "target-supervisor.lock.owner.json")
	raw := []byte(`{"pid":123,"started_at":"2026-06-20T00:00:00Z"}`)
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatalf("write target owner: %v", err)
	}
	link := filepath.Join(stateDir, "supervisor.lock.owner.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported in this environment: %v", err)
	}

	if _, _, err := readSupervisorLockOwner(stateDir); err == nil {
		t.Fatalf("readSupervisorLockOwner followed a symlink; want inode-anchor refusal")
	}
}
