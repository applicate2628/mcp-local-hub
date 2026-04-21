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
	t.Chdir(dir)
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

// TestCanonicalWorkspacePath_ResolvesSymlinkToTarget ensures symlinks at
// any level — final OR intermediate — resolve to the real underlying
// directory so the same workspace is never registered twice under
// different aliases. The previous policy rejected final-path symlinks
// outright, but that left intermediate-parent symlinks silently
// un-resolved and producing different WorkspaceKey values for the same
// directory.
// Skipped on Windows where symlink creation requires developer mode.
func TestCanonicalWorkspacePath_ResolvesSymlinkToTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation on Windows requires developer mode / admin")
	}
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	resolvedFromLink, err := CanonicalWorkspacePath(link)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath(link): %v", err)
	}
	resolvedFromReal, err := CanonicalWorkspacePath(realDir)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath(real): %v", err)
	}
	if resolvedFromLink != resolvedFromReal {
		t.Errorf("symlink + real path must produce same canonical; got link=%q real=%q",
			resolvedFromLink, resolvedFromReal)
	}
	if WorkspaceKey(resolvedFromLink) != WorkspaceKey(resolvedFromReal) {
		t.Errorf("WorkspaceKey must match for aliased paths; got link=%q real=%q",
			WorkspaceKey(resolvedFromLink), WorkspaceKey(resolvedFromReal))
	}
}
