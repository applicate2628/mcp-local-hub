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
