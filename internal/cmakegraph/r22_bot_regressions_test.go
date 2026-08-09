package cmakegraph

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestR22AddSubdirectoryRejectsEscapingCMakeListsSymlink(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	child := filepath.Join(workspace, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(workspace, "CMakeLists.txt")
	if err := os.WriteFile(root, []byte("add_subdirectory(child)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.cmake")
	if err := os.WriteFile(outside, []byte("include(leaked.cmake)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(child, "CMakeLists.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	result, err := Walk(context.Background(), root, workspace, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Edges) != 1 {
		t.Fatalf("edges=%+v, want only the rejected add_subdirectory edge", result.Edges)
	}
	edge := result.Edges[0]
	if edge.Status != StatusUnresolved || edge.Reason != ReasonOutsideWorkspace {
		t.Fatalf("edge=%+v, want unresolved/%s", edge, ReasonOutsideWorkspace)
	}
	for _, scanned := range result.Files {
		if scanned == realpath(t, outside) {
			t.Fatalf("outside CMakeLists target was scanned: %v", result.Files)
		}
	}
}
