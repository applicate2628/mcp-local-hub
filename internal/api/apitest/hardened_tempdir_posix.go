//go:build !windows

// POSIX leg of apitest.HardenedTempDir. Creates a 0700 subdir of
// t.TempDir() so the secure-write parent-dir gate (which inspects
// the IMMEDIATE parent's mode against the 0o077 mask) accepts it.
// Linux/macOS test temp paths inherit 0755 from $TMPDIR.

package apitest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// HardenedTempDir creates a subdir of t.TempDir(), chmod's it to
// 0700, and returns the subdir path. The intermediate t.TempDir()
// ancestors are still 0755 — the parent-dir gate only inspects the
// IMMEDIATE parent of the file being written, which is this 0700
// subdir.
func HardenedTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	hardened := filepath.Join(root, "hardened-parent")
	return HardenedDir(t, hardened)
}

// HardenedDir creates dir if needed and chmods it to 0700. Use it
// when the immediate parent checked by the hardened state reader is a
// child of the temp root.
func HardenedDir(t *testing.T, dir string) string {
	t.Helper()
	if err := HardenedDirForTestMain(dir); err != nil {
		t.Fatalf("apitest.HardenedDir %s: %v", dir, err)
	}
	return dir
}

// HardenedDirForTestMain creates dir if needed and chmods it to 0700. It is
// the error-returning form used by package TestMain functions.
func HardenedDirForTestMain(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	// Defensive: explicit chmod after creation in case umask
	// stripped bits from the mode arg to os.Mkdir.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	return nil
}
