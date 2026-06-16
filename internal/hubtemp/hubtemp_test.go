package hubtemp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
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

func TestDir_NonWindowsUsesUserCacheDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("non-Windows user cache branch")
	}
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)

	got, ok := Dir("drmemory")
	if !ok {
		t.Fatalf("Dir returned ok=false on non-Windows")
	}
	want := filepath.Join(root, "mcp-local-hub", "drmemory")
	if got != want {
		t.Errorf("Dir(drmemory) = %q, want %q", got, want)
	}
}

func TestEnsurePrivateDirCreatesOwnerWritableDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scratch")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat ensured dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("ensured path is not a directory")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Errorf("mode = %o, want 700", info.Mode().Perm())
	}
}

func TestSweepStaleSkipsActiveMarkedDirectory(t *testing.T) {
	base := t.TempDir()
	active := filepath.Join(base, "run-active")
	if err := os.Mkdir(active, 0o700); err != nil {
		t.Fatalf("Mkdir active: %v", err)
	}
	cleanup, err := MarkActive(active)
	if err != nil {
		t.Fatalf("MarkActive: %v", err)
	}
	defer cleanup()
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(active, old, old); err != nil {
		t.Fatalf("Chtimes active: %v", err)
	}

	SweepStale(base, "run-", time.Hour)

	if _, err := os.Stat(active); err != nil {
		t.Fatalf("active directory was swept: %v", err)
	}
}

func TestSweepStaleRemovesUnmarkedStaleDirectory(t *testing.T) {
	base := t.TempDir()
	stale := filepath.Join(base, "run-stale")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatalf("Mkdir stale: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("Chtimes stale: %v", err)
	}

	SweepStale(base, "run-", time.Hour)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale directory still exists or unexpected stat error: %v", err)
	}
}
