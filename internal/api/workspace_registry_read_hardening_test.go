package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistry_LoadRejectsSymlinkTarget(t *testing.T) {
	dir := hardenedTempDir(t)
	realPath := filepath.Join(dir, "real-workspaces.yaml")
	linkPath := filepath.Join(dir, "workspaces.yaml")

	if err := WriteStateFileBytesLockHeld(realPath, []byte("version: 1\nworkspaces: []\n")); err != nil {
		t.Fatalf("seed real registry: %v", err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}

	reg := NewRegistry(linkPath)
	err := reg.Load()
	if err == nil {
		t.Fatalf("Registry.Load followed symlink target; workspaces=%+v", reg.Workspaces)
	}
	if !errors.Is(err, ErrIrregularFile) {
		t.Fatalf("Registry.Load err = %v, want ErrIrregularFile", err)
	}
}
