package sonnet

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOutputManifestClassifiesBaselineCreatedModifiedAndRemoved(t *testing.T) {
	root := t.TempDir()
	write := func(n, s string) {
		p := filepath.Join(root, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("baseline.s2p", "old")
	write("changed.txt", "old")
	write("removed.txt", "gone")
	before, err := snapshotOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	write("changed.txt", "new")
	write("created.csv", "new")
	if err := os.Remove(filepath.Join(root, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	after, err := compareOutput(root, before)
	if err != nil {
		t.Fatal(err)
	}
	if after.Reportable || after.FailureID != "SONNET_OUTPUT_UNATTRIBUTABLE" {
		t.Fatalf("removed output=%+v", after)
	}
	states := map[string]string{}
	for _, f := range after.Files {
		states[f.Path] = f.State
	}
	if states["baseline.s2p"] != "unchanged" || states["changed.txt"] != "modified" || states["created.csv"] != "created" || states["removed.txt"] != "removed" {
		t.Fatalf("states=%v", states)
	}
}

func TestOutputManifestRejectsLinksAndLimits(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("missing", filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := snapshotOutput(root); err == nil {
		t.Fatal("link accepted")
	}
}
