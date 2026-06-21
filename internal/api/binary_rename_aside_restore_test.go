package api

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRestoreMissingBinaryAside_CopiesRunningAsideContent(t *testing.T) {
	dir := t.TempDir()
	aside := filepath.Join(dir, platformBinaryName()+".old-20250102T030405Z")
	target := filepath.Join(dir, platformBinaryName())
	want := []byte("aside-binary\nnon-empty\n")
	if err := os.WriteFile(aside, want, 0o700); err != nil {
		t.Fatal(err)
	}

	runningAside, err := os.OpenFile(aside, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open running aside: %v", err)
	}
	defer runningAside.Close()

	if err := restoreMissingBinaryAside(aside, target); err != nil {
		t.Fatalf("restore missing binary aside: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("target content = %q, want %q", got, want)
	}
	if info, err := os.Stat(target); err != nil {
		t.Fatalf("stat target: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("target mode = %v, want owner executable bit preserved", info.Mode().Perm())
	}

	if _, err := runningAside.WriteAt([]byte("mutated"), 0); err != nil {
		t.Fatalf("mutate still-open aside handle: %v", err)
	}
	if err := runningAside.Sync(); err != nil {
		t.Fatalf("sync mutated aside handle: %v", err)
	}
	gotAfterMutation, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target after aside mutation: %v", err)
	}
	if string(gotAfterMutation) != string(want) {
		t.Fatalf("target aliases running aside after restore: got %q, want original %q", gotAfterMutation, want)
	}

	if _, err := os.Stat(target + ".restore-tmp"); !os.IsNotExist(err) {
		t.Fatalf("restore temp should not remain: %v", err)
	}
}
