package cmakegraph

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestR32CMakeListsCaseFollowsActualFilesystem(t *testing.T) {
	dir := t.TempDir()
	root := writeFile(t, dir, "cmakelists.txt", "include(target.cmake)\n")
	target := writeFile(t, dir, "target.cmake", "# leaf\n")

	actualInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	canonicalInfo, canonicalErr := os.Stat(filepath.Join(dir, "CMakeLists.txt"))
	canonicalSpellingNamesSameFile := canonicalErr == nil && os.SameFile(actualInfo, canonicalInfo)

	tree, err := WalkTree(context.Background(), dir, dir, []string{"CMakeLists.txt"}, DefaultOptions())
	if err != nil {
		t.Fatalf("WalkTree: %v", err)
	}
	if got := fileInResult(tree, root); got != canonicalSpellingNamesSameFile {
		t.Fatalf("lowercase discovery=%v, want actual-filesystem result %v; files=%v", got, canonicalSpellingNamesSameFile, tree.Files)
	}
	if got := fileInResult(tree, target); got != canonicalSpellingNamesSameFile {
		t.Fatalf("lowercase traversal=%v, want actual-filesystem result %v; files=%v", got, canonicalSpellingNamesSameFile, tree.Files)
	}

	direct, err := Walk(context.Background(), root, dir, DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(direct.Edges) != 1 {
		t.Fatalf("edges=%+v, want one include call site", direct.Edges)
	}
	edge := direct.Edges[0]
	if canonicalSpellingNamesSameFile {
		if edge.Status != StatusResolved {
			t.Fatalf("edge=%+v, canonical spelling resolves to same file, want resolved", edge)
		}
	} else if edge.Status != StatusUnresolved || edge.Reason != ReasonUnverifiedSourceDir {
		t.Fatalf("edge=%+v, canonical spelling does not resolve to same file, want unresolved/%s", edge, ReasonUnverifiedSourceDir)
	}
}

type r32FileInfo struct {
	id string
}

func (info *r32FileInfo) Name() string       { return info.id }
func (info *r32FileInfo) Size() int64        { return 0 }
func (info *r32FileInfo) Mode() fs.FileMode  { return 0 }
func (info *r32FileInfo) ModTime() time.Time { return time.Time{} }
func (info *r32FileInfo) IsDir() bool        { return false }
func (info *r32FileInfo) Sys() any           { return nil }

func TestR32CaseFoldAloneCannotVerifyCMakeLists(t *testing.T) {
	dir := filepath.Join("tree", "project")
	lowerPath := filepath.Join(dir, "cmakelists.txt")
	canonicalPath := filepath.Join(dir, "CMakeLists.txt")
	lowerInfo := &r32FileInfo{id: "lower"}
	otherInfo := &r32FileInfo{id: "other"}

	for _, test := range []struct {
		name          string
		canonicalInfo fs.FileInfo
		canonicalErr  error
		want          bool
	}{
		{name: "canonical spelling absent", canonicalErr: fs.ErrNotExist},
		{name: "canonical spelling is a different file", canonicalInfo: otherInfo},
		{name: "canonical spelling identifies the same file", canonicalInfo: lowerInfo, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := filesystemNameMatchesWith(lowerPath, "CMakeLists.txt", fileIdentityOperations{
				stat: func(path string) (fs.FileInfo, error) {
					switch path {
					case lowerPath:
						return lowerInfo, nil
					case canonicalPath:
						return test.canonicalInfo, test.canonicalErr
					default:
						return nil, errors.New("unexpected path")
					}
				},
				sameFile: func(left, right fs.FileInfo) bool { return left == right },
			})
			if got != test.want {
				t.Fatalf("filesystemNameMatchesWith=%v, want %v", got, test.want)
			}
		})
	}
}
