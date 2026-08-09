package cmakegraph

import (
	"os"
	"path/filepath"
	"testing"
)

func TestR20PinnedRootReadRefusesRetargetOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	canonicalRoot := filepath.Join(workspace, "CMakeLists.txt")
	if err := os.WriteFile(canonicalRoot, []byte("# inside\n"), 0o644); err != nil {
		t.Fatalf("write inside root: %v", err)
	}
	outsideFile := filepath.Join(outside, "outside.cmake")
	if err := os.WriteFile(outsideFile, []byte("include(secret.cmake)\n"), 0o644); err != nil {
		t.Fatalf("write outside root: %v", err)
	}

	w, err := newWalker(t.Context(), workspace, Options{})
	if err != nil {
		t.Fatalf("newWalker: %v", err)
	}
	defer w.close()
	if err := os.Remove(canonicalRoot); err != nil {
		t.Fatalf("remove verified root: %v", err)
	}
	if err := os.Symlink(outsideFile, canonicalRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if _, err := w.readPinnedRoot(canonicalRoot); err == nil {
		t.Fatal("retargeted root escaped workspace through stale canonical path")
	}
}
