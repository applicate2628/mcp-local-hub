package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPathShadowDiagnostic exercises the pure shadow-comparison helper. It
// uses real on-disk paths under t.TempDir() so EvalSymlinks resolves, and
// touches NO state files (safe to run in isolation).
func TestPathShadowDiagnostic(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "running-mcphub.exe")
	shadow := filepath.Join(dir, "shadow-mcphub.exe")

	// Empty inputs never warn.
	if got := pathShadowDiagnostic("", shadow); got != "" {
		t.Fatalf("empty runningExe must not warn; got %q", got)
	}
	if got := pathShadowDiagnostic(running, ""); got != "" {
		t.Fatalf("empty pathResolved must not warn; got %q", got)
	}

	// Same path never warns.
	if got := pathShadowDiagnostic(running, running); got != "" {
		t.Fatalf("identical paths must not warn; got %q", got)
	}
	if runtime.GOOS == "windows" {
		if got := pathShadowDiagnostic(running, strings.ToUpper(running)); got != "" {
			t.Fatalf("case-only difference must not warn on Windows; got %q", got)
		}
	}

	// Genuinely different binaries warn, naming BOTH locations.
	warn := pathShadowDiagnostic(running, shadow)
	if warn == "" {
		t.Fatal("different binaries must warn")
	}
	if !strings.Contains(warn, filepath.Clean(shadow)) || !strings.Contains(warn, filepath.Clean(running)) {
		t.Fatalf("warning must name both locations; got %q", warn)
	}
}

func TestPathShadowDiagnosticWarnsForCaseOnlyDistinctPOSIXPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows executable paths are compared case-insensitively")
	}

	dir := t.TempDir()
	runningDir := filepath.Join(dir, "bin")
	shadowDir := filepath.Join(dir, "BIN")
	if err := os.Mkdir(runningDir, 0o755); err != nil {
		t.Fatalf("create running dir: %v", err)
	}
	if err := os.Mkdir(shadowDir, 0o755); err != nil {
		t.Fatalf("create shadow dir: %v", err)
	}
	running := filepath.Join(runningDir, "mcphub")
	shadow := filepath.Join(shadowDir, "mcphub")
	if err := os.WriteFile(running, []byte("running"), 0o755); err != nil {
		t.Fatalf("write running binary placeholder: %v", err)
	}
	if err := os.WriteFile(shadow, []byte("shadow"), 0o755); err != nil {
		t.Fatalf("write shadow binary placeholder: %v", err)
	}

	warn := pathShadowDiagnostic(running, shadow)
	if warn == "" {
		t.Fatalf("expected warning for distinct case-only paths %q and %q", running, shadow)
	}
	if !strings.Contains(warn, filepath.Clean(shadow)) || !strings.Contains(warn, filepath.Clean(running)) {
		t.Fatalf("warning must name both locations; got %q", warn)
	}
}

func TestNormalizeExePath(t *testing.T) {
	if _, err := normalizeExePath(""); err == nil {
		t.Fatal("empty path must error")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "a", "..", "b", "mcphub.exe")
	got, err := normalizeExePath(p)
	if err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	// The ".." segment must be cleaned away.
	if strings.Contains(got, "..") {
		t.Fatalf("normalized path still has ..: %q", got)
	}
}
