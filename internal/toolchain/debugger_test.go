package toolchain

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeDebugger writes an empty file named like a debugger binary into dir so the
// filesystem probe (os.Stat) treats dir as a debugger toolchain dir.
func fakeDebugger(t *testing.T, dir string) {
	t.Helper()
	name := "gdb"
	if runtime.GOOS == "windows" {
		name = "gdb.exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatalf("write fake debugger: %v", err)
	}
}

// TestDebuggerDirs_OverrideDetectsAndSkips covers the cross-platform override
// path deterministically: a dir holding a debugger binary is detected; an empty
// dir is skipped (the no-op contract).
func TestDebuggerDirs_OverrideDetectsAndSkips(t *testing.T) {
	withDbg := t.TempDir()
	fakeDebugger(t, withDbg)
	empty := t.TempDir()

	t.Setenv(OverrideEnvVar, withDbg+string(os.PathListSeparator)+empty)
	got := DebuggerDirs()

	foundWith, foundEmpty := false, false
	for _, d := range got {
		if d == withDbg {
			foundWith = true
		}
		if d == empty {
			foundEmpty = true
		}
	}
	if !foundWith {
		t.Errorf("override dir holding a debugger not detected: got %v", got)
	}
	if foundEmpty {
		t.Errorf("override dir with NO debugger must be skipped: got %v", got)
	}
}

// TestDebuggerDirs_Dedup ensures a dir listed twice (override + detection, or
// twice in the override) appears once.
func TestDebuggerDirs_Dedup(t *testing.T) {
	dir := t.TempDir()
	fakeDebugger(t, dir)
	sep := string(os.PathListSeparator)
	t.Setenv(OverrideEnvVar, dir+sep+dir)

	count := 0
	for _, d := range DebuggerDirs() {
		if d == dir {
			count++
		}
	}
	if count != 1 {
		t.Errorf("duplicate dir should appear once, appeared %d times", count)
	}
}

// TestDefaultLldbPath_AbsoluteWhenDetected returns an absolute lldb path when a
// detected dir actually holds lldb, else the bare name.
func TestDefaultLldbPath_AbsoluteWhenDetected(t *testing.T) {
	dir := t.TempDir()
	lldbName := "lldb"
	if runtime.GOOS == "windows" {
		lldbName = "lldb.exe"
	}
	if err := os.WriteFile(filepath.Join(dir, lldbName), []byte("x"), 0o644); err != nil {
		t.Fatalf("write fake lldb: %v", err)
	}
	t.Setenv(OverrideEnvVar, dir)

	got := DefaultLldbPath()
	want := filepath.Join(dir, lldbName)
	if got != want {
		t.Errorf("DefaultLldbPath = %q, want the detected absolute path %q", got, want)
	}
}

// TestDefaultLldbPath_BareFallback: with the override pointing at a dir that has
// gdb but NOT lldb, DefaultLldbPath falls back to the bare name (PATH resolution
// at spawn) rather than returning a non-existent path.
func TestDefaultLldbPath_BareFallback(t *testing.T) {
	// Point MSYS2_ROOT at an empty dir so the Windows MSYS2 probe finds no real
	// lldb; otherwise a dev host with msys2 lldb would (correctly) return that
	// absolute path instead of the bare fallback this test exercises.
	t.Setenv("MSYS2_ROOT", t.TempDir())
	dir := t.TempDir()
	fakeDebugger(t, dir) // gdb only, no lldb
	t.Setenv(OverrideEnvVar, dir)

	bare := "lldb"
	if runtime.GOOS == "windows" {
		bare = "lldb.exe"
	}
	if got := DefaultLldbPath(); got != bare {
		t.Errorf("DefaultLldbPath with no lldb in detected dir = %q, want bare %q", got, bare)
	}
}
