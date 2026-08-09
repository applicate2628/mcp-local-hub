package cmakegraph

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestR18WalkTreeBoundsAggregateRetainedRootPathBytes(t *testing.T) {
	root := t.TempDir()
	const candidates = 10000
	longComponent := strings.Repeat("x", 1024)
	walked := 0
	result, err := walkTreeWithOperations(context.Background(), root, root, []string{"*.cmake"}, DefaultOptions(), treeOperations{
		walkDir: func(_ string, visit fs.WalkDirFunc) error {
			if err := visit(root, fakeWalkDirEntry{name: filepath.Base(root), dir: true}, nil); err != nil {
				return err
			}
			for i := 0; i < candidates; i++ {
				path := filepath.Join(root, longComponent, "entry.cmake")
				if err := visit(path, fakeWalkDirEntry{name: "entry.cmake"}, nil); err != nil {
					if errors.Is(err, fs.SkipAll) {
						return nil
					}
					return err
				}
			}
			return nil
		},
		isDirectorySymlink: func(string) bool { return false },
		walkRoot: func(_ *walker, start string) (string, error) {
			walked++
			return start, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.RootEnumerationCapped {
		t.Fatal("aggregate retained root-path bytes were not capped")
	}
	if walked >= candidates {
		t.Fatalf("walked=%d, want a strict bounded subset of %d candidates", walked, candidates)
	}
	if reason, ok := coverageReasonFor(result, filepath.Join(root, longComponent)); !ok || reason != CoverageRootEnumerationCapped {
		t.Fatalf("coverage=(%q,%v), want root_enumeration_capped", reason, ok)
	}
}
