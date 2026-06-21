package api

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRestoreMissingBinaryAside_FallsBackWhenHardLinkCrossDevice(t *testing.T) {
	dir := t.TempDir()
	aside := filepath.Join(dir, platformBinaryName()+".old-20250102T030405Z")
	target := filepath.Join(dir, platformBinaryName())
	if err := os.WriteFile(aside, []byte("aside-binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	var linkCalls int
	var renameCalls int
	ops := restoreMissingBinaryAsideOps{
		link: func(oldname, newname string) error {
			linkCalls++
			return errRestoreCrossDeviceForTest
		},
		rename: func(oldname, newname string) error {
			renameCalls++
			return os.Rename(oldname, newname)
		},
		remove: os.Remove,
	}

	if err := restoreMissingBinaryAsideWithOps(aside, target, ops); err != nil {
		t.Fatalf("restore missing binary aside: %v", err)
	}
	if linkCalls != 1 {
		t.Fatalf("link calls = %d, want 1", linkCalls)
	}
	if renameCalls != 1 {
		t.Fatalf("rename calls = %d, want 1", renameCalls)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "aside-binary" {
		t.Fatalf("target content = %q, want aside-binary", got)
	}
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Fatalf("aside should be consumed by rename fallback: %v", err)
	}
}

func TestRestoreMissingBinaryAside_CopyFallbackWhenRenameCrossDevice(t *testing.T) {
	dir := t.TempDir()
	aside := filepath.Join(dir, platformBinaryName()+".old-20250102T030405Z")
	target := filepath.Join(dir, platformBinaryName())
	if err := os.WriteFile(aside, []byte("aside-binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	ops := restoreMissingBinaryAsideOps{
		link: func(oldname, newname string) error {
			return errRestoreCrossDeviceForTest
		},
		rename: func(oldname, newname string) error {
			return errRestoreCrossDeviceForTest
		},
		remove: os.Remove,
	}

	if err := restoreMissingBinaryAsideWithOps(aside, target, ops); err != nil {
		t.Fatalf("restore missing binary aside: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "aside-binary" {
		t.Fatalf("target content = %q, want aside-binary", got)
	}
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Fatalf("aside should be removed after copy fallback: %v", err)
	}
}

func TestRestoreMissingBinaryAside_CopyFallbackDoesNotClobberPresentTarget(t *testing.T) {
	dir := t.TempDir()
	aside := filepath.Join(dir, platformBinaryName()+".old-20250102T030405Z")
	target := filepath.Join(dir, platformBinaryName())
	if err := os.WriteFile(aside, []byte("aside-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("present-target"), 0o700); err != nil {
		t.Fatal(err)
	}

	ops := restoreMissingBinaryAsideOps{
		link: func(oldname, newname string) error {
			return errRestoreCrossDeviceForTest
		},
		rename: func(oldname, newname string) error {
			return errRestoreCrossDeviceForTest
		},
		remove: os.Remove,
	}

	if err := restoreMissingBinaryAsideWithOps(aside, target, ops); err == nil {
		t.Fatal("restore unexpectedly succeeded with present target")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "present-target" {
		t.Fatalf("target was clobbered: got %q", got)
	}
	if _, err := os.Stat(aside); err != nil {
		t.Fatalf("aside should remain after failed no-clobber copy: %v", err)
	}
}

var errRestoreCrossDeviceForTest = &os.LinkError{
	Op:  "link",
	Err: syscall.EXDEV,
}
