package api

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWorkspaceKey_Deterministic(t *testing.T) {
	a := WorkspaceKey("C:/users/dima/projects/foo")
	b := WorkspaceKey("C:/users/dima/projects/foo")
	if a != b {
		t.Errorf("key not deterministic: %q vs %q", a, b)
	}
	if len(a) != 8 {
		t.Errorf("key len = %d, want 8 (hex)", len(a))
	}
	c := WorkspaceKey("C:/users/dima/projects/bar")
	if a == c {
		t.Error("keys for distinct paths should differ")
	}
}

func TestCanonicalWorkspacePath_RelativeResolvedToAbsolute(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	got, err := CanonicalWorkspacePath(".")
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("not absolute: %q", got)
	}
}

func TestCanonicalWorkspacePath_RejectsNonexistent(t *testing.T) {
	_, err := CanonicalWorkspacePath(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestCanonicalWorkspacePath_WindowsDriveLetterLowercased(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only")
	}
	// Create a real directory to canonicalize.
	dir := t.TempDir()
	// Craft an uppercased-drive variant of dir (TempDir returns lowercase on most
	// setups; manually uppercase if possible).
	if len(dir) < 2 || dir[1] != ':' {
		t.Skipf("unexpected temp dir shape: %q", dir)
	}
	upper := strings.ToUpper(string(dir[0])) + dir[1:]
	got, err := CanonicalWorkspacePath(upper)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	if got[0] != dir[0] || strings.ToLower(string(got[0])) != string(got[0]) {
		t.Errorf("drive letter not lowercased: input %q -> %q", upper, got)
	}
}

// TestCanonicalWorkspacePath_RejectsSymlink ensures the "reject symlinks" policy.
// Skipped on Windows when symlink creation requires admin; portable on Linux.
func TestCanonicalWorkspacePath_RejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation on Windows requires developer mode / admin")
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := CanonicalWorkspacePath(link)
	if err == nil {
		t.Fatal("expected error for symlink")
	}
	if !strings.Contains(err.Error(), "symlink") && !strings.Contains(err.Error(), "reparse") {
		t.Errorf("error should mention symlink/reparse: %v", err)
	}
}
