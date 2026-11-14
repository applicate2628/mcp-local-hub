package sonnet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func makeRecordlessWorkspace(t *testing.T, root, name string, modified time.Time) string {
	t.Helper()
	dir := filepath.Join(root, "runs", name)
	for _, child := range []string{"work", "control"} {
		if err := os.MkdirAll(filepath.Join(dir, child), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, leaf := range []string{"work/sample.son", "control/.record-interrupted.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(leaf)), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, modified, modified)
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStartupReapsOnlyExpiredOwnedRecordlessWorkspaces(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * runStoreRetention)
	expired := makeRecordlessWorkspace(t, root, "run-100001", old)
	fresh := makeRecordlessWorkspace(t, root, "run-100002", time.Now())
	foreign := makeRecordlessWorkspace(t, root, "foreign-old", old)
	foreignRun := makeRecordlessWorkspace(t, root, "run-foreign", old)
	recentChild := makeRecordlessWorkspace(t, root, "run-100003", old)
	now := time.Now()
	if err := os.Chtimes(filepath.Join(recentChild, "work", "sample.son"), now, now); err != nil {
		t.Fatal(err)
	}
	active := makeRecordlessWorkspace(t, root, "run-100004", old)
	if err := os.WriteFile(filepath.Join(active, activeMarkerLeaf), []byte("active"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(active, old, old); err != nil {
		t.Fatal(err)
	}
	s := NewServer(testConfig(root, &fakeDriver{}))
	defer s.Close(context.Background())
	if s.initErr != nil {
		t.Fatal(s.initErr)
	}
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("expired recordless workspace retained: %v", err)
	}
	for _, dir := range []string{fresh, foreign, foreignRun, recentChild, active} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("protected workspace %q removed: %v", dir, err)
		}
	}
}

func TestRecordlessSweepRequiresExclusiveStoreLease(t *testing.T) {
	root := t.TempDir()
	owner := NewServer(testConfig(root, &fakeDriver{}))
	defer owner.Close(context.Background())
	if owner.initErr != nil {
		t.Fatal(owner.initErr)
	}
	dir := makeRecordlessWorkspace(t, root, "run-200001", time.Now().Add(-2*runStoreRetention))
	other := NewServer(testConfig(root, &fakeDriver{}))
	defer other.Close(context.Background())
	if !errors.Is(other.initErr, ErrRunStoreOwned) {
		t.Fatalf("second owner admission = %v", other.initErr)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("failed owner swept existing workspace: %v", err)
	}
}

func TestRecordlessSweepPreservesCorruptSnapshot(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * runStoreRetention)
	dir := makeRecordlessWorkspace(t, root, "run-300001", old)
	path := filepath.Join(dir, "control", "00000001.json")
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, filepath.Dir(path), dir} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServer(testConfig(root, &fakeDriver{}))
	defer s.Close(context.Background())
	if !errors.Is(s.initErr, ErrRunStoreInvalid) {
		t.Fatalf("corrupt store admission = %v", s.initErr)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "corrupt" {
		t.Fatalf("corrupt snapshot evidence removed: %q, %v", data, err)
	}
}
