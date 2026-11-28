package testkit

import (
	"os"
	"path/filepath"
	"testing"
)

// SnapshotTree returns a map of relative-path → (size, modtime-unixnano) for
// every regular file under root, so the before/after comparison catches a
// created OR modified file.
func SnapshotTree(t *testing.T, root string) map[string][2]int64 {
	t.Helper()
	out := map[string][2]int64{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out[rel] = [2]int64{info.Size(), info.ModTime().UnixNano()}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}
