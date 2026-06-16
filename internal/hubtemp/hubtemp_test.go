package hubtemp

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDir_WindowsUsesLocalAppData(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("LOCALAPPDATA branch is Windows-only")
	}
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)

	got, ok := Dir("drmemory")
	if !ok {
		t.Fatalf("Dir returned ok=false with LOCALAPPDATA set")
	}
	want := filepath.Join(root, "mcp-local-hub", "drmemory")
	if got != want {
		t.Errorf("Dir(drmemory) = %q, want %q", got, want)
	}
}

func TestDir_WindowsFallsBackToHomeWhenLocalAppDataEmpty(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("home fallback is the Windows no-LOCALAPPDATA path")
	}
	t.Setenv("LOCALAPPDATA", "")

	got, ok := Dir("oneapi-run-tmp")
	if !ok {
		t.Fatalf("Dir returned ok=false; expected the home fallback")
	}
	// The fallback must be under a .mcphub dir, never the empty LOCALAPPDATA.
	if filepath.Base(got) != "oneapi-run-tmp" {
		t.Errorf("Dir leaf = %q, want oneapi-run-tmp", filepath.Base(got))
	}
	if parent := filepath.Base(filepath.Dir(got)); parent != ".mcphub" {
		t.Errorf("Dir parent = %q, want .mcphub", parent)
	}
}

func TestDir_NonWindowsUsesTempDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("non-Windows os.TempDir branch")
	}
	got, ok := Dir("drmemory")
	if !ok {
		t.Fatalf("Dir returned ok=false on non-Windows")
	}
	if filepath.Base(got) != "drmemory" {
		t.Errorf("Dir leaf = %q, want drmemory", filepath.Base(got))
	}
}
