//go:build !windows

package api

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadStateFileInodeAnchored_FileModeReadBroadenedDefaultRelaxesStrictRejects(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "supervisor-intent.json")
	want := []byte(`{"strict_mode":false}`)
	if err := os.WriteFile(target, want, 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod target read-broadened: %v", err)
	}

	got, err := readStateFileInodeAnchoredWithStrictPolicy(target, func() bool { return false })
	if err != nil {
		t.Fatalf("default mode must read group/world-readable state file via inode-anchored fd: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("read payload = %q, want %q", got, want)
	}

	_, err = readStateFileInodeAnchoredWithStrictPolicy(target, func() bool { return true })
	if err == nil {
		t.Fatalf("strict mode must reject group/world-readable state file")
	}
	if !errors.Is(err, ErrTooLoose) {
		t.Fatalf("strict mode err = %v, want ErrTooLoose", err)
	}
}

func TestReadStateFileInodeAnchored_FileModeWriteBroadenedDefaultRejects(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "supervisor-intent.json")
	if err := os.WriteFile(target, []byte(`{"strict_mode":false}`), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Chmod(target, 0o622); err != nil {
		t.Fatalf("chmod target write-broadened: %v", err)
	}

	_, err := readStateFileInodeAnchoredWithStrictPolicy(target, func() bool { return false })
	if err == nil {
		t.Fatalf("default mode must reject group/world-writable state file")
	}
	if !errors.Is(err, ErrTooLoose) {
		t.Fatalf("err = %v, want ErrTooLoose", err)
	}
}

func TestReadStateFileInodeAnchored_ENOTDIRPreserved(t *testing.T) {
	dir := hardenedTempDir(t)
	notDir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatalf("write non-directory component: %v", err)
	}

	_, err := readStateFileInodeAnchoredWithStrictPolicy(filepath.Join(notDir, "supervisor-intent.json"), func() bool { return false })
	if err == nil {
		t.Fatal("read through a non-directory path component returned nil; want ENOTDIR")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ENOTDIR was reported as missing-file absent semantics: %v", err)
	}
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("err = %v, want errors.Is(syscall.ENOTDIR)", err)
	}
}
