package cmakegraph

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestR26WalkTreeUnreadableDiscoveredRootIsCoverageHole(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "vanished.cmake")
	res, err := walkTreeWithOperations(context.Background(), root, root, []string{"*.cmake"}, DefaultOptions(), treeOperations{
		walkDir: func(_ string, visit fs.WalkDirFunc) error {
			if err := visit(root, fakeWalkDirEntry{name: filepath.Base(root), dir: true}, nil); err != nil {
				return err
			}
			return visit(missing, fakeWalkDirEntry{name: "vanished.cmake"}, nil)
		},
		isDirectorySymlink: func(string) bool { return false },
		walkRoot:           (*walker).walkRoot,
	})
	if err != nil {
		t.Fatalf("WalkTree returned a whole-walk error for one unreadable root: %v", err)
	}
	if reason, ok := coverageReasonFor(res, missing); !ok || reason != CoverageFileUnreadable {
		t.Fatalf("coverage for %q = (%q,%v), want file_unreadable", missing, reason, ok)
	}
}

func TestR26CommandDiscoveryIsCappedAndReported(t *testing.T) {
	root := t.TempDir()
	start := filepath.Join(root, "CMakeLists.txt")
	data := strings.Repeat("x()\n", MaxCommandInvocationsLimit+1)
	if err := os.WriteFile(start, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Walk(context.Background(), start, root, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok := coverageReasonFor(res, start); !ok || reason != CoverageCommandCapExceeded {
		t.Fatalf("coverage for %q = (%q,%v), want command_cap_exceeded", start, reason, ok)
	}
}
