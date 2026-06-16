package cli

import (
	"path/filepath"
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

	// Same path (case-insensitive) never warns.
	if got := pathShadowDiagnostic(running, running); got != "" {
		t.Fatalf("identical paths must not warn; got %q", got)
	}
	if got := pathShadowDiagnostic(running, strings.ToUpper(running)); got != "" {
		t.Fatalf("case-only difference must not warn (case-insensitive compare); got %q", got)
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
