package api

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveInstallTaskSnapshotExactBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		xml  []byte
	}{
		{"utf8", []byte("<Task>Привет-🙂</Task>")},
		{"utf16le", []byte{0xff, 0xfe, 0x3c, 0, 0x54, 0, 0x3e, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := hardenedTempDir(t)
			path, err := saveInstallTaskSnapshot(root, "scheduler-recovery-fixed.json", "literal task", tc.xml)
			if err != nil {
				t.Fatal(err)
			}
			if path != readInstallRecoverySnapshot(t, root, "literal task", tc.xml) {
				t.Fatal("returned path differs from saved snapshot")
			}
		})
	}
}

func TestSaveInstallTaskSnapshotRefusesExistingFile(t *testing.T) {
	root := hardenedTempDir(t)
	const leaf = "scheduler-recovery-existing.json"
	want := []byte("existing recovery material")
	path := filepath.Join(root, leaf)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	gotPath, err := saveInstallTaskSnapshot(root, leaf, "task", []byte("<Task/>"))
	if gotPath != path || !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing file = %q, %v; want occupied-path refusal", gotPath, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("existing recovery material was replaced")
	}
}

func TestSaveInstallTaskSnapshotSizeLimitBeforeWrite(t *testing.T) {
	for _, size := range []int{0, maxStateFileBytes * 3 / 4, maxStateFileBytes + 1} {
		root := hardenedTempDir(t)
		path, err := saveInstallTaskSnapshot(root, "scheduler-recovery-large.json", "task", bytes.Repeat([]byte{'x'}, size))
		if err == nil {
			t.Fatalf("snapshot size %d unexpectedly accepted", size)
		}
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("snapshot size %d wrote a file: %v", size, statErr)
		}
	}
}
