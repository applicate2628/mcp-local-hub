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

	if err := os.WriteFile(running, []byte("mcp-local-hub 0.4.29\ncommit: 32fab6d\n"), 0o755); err != nil {
		t.Fatalf("write running binary fixture: %v", err)
	}
	if err := os.WriteFile(shadow, []byte("mcp-local-hub 0.4.29\ncommit: 32fab6d\n"), 0o755); err != nil {
		t.Fatalf("write byte-identical alternate fixture: %v", err)
	}

	// Different locations carrying byte-identical binaries are informational,
	// not a stale-PATH warning.
	got := pathShadowDiagnostic(running, shadow)
	if !strings.Contains(got, "equivalent alternate path") {
		t.Fatalf("byte-identical alternate path must be informational; got %q", got)
	}
	if strings.Contains(got, "identity differs") || strings.Contains(got, "identity-unverified") {
		t.Fatalf("byte-identical alternate path must not be a warning; got %q", got)
	}
}

func TestPathShadowDiagnosticWarnsWhenSameVersionBytesDiffer(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "running-mcphub.exe")
	shadow := filepath.Join(dir, "shadow-mcphub.exe")
	if err := os.WriteFile(running, []byte("mcp-local-hub 0.4.29\ncommit: 32fab6d\nbody=A\n"), 0o755); err != nil {
		t.Fatalf("write running binary fixture: %v", err)
	}
	if err := os.WriteFile(shadow, []byte("mcp-local-hub 0.4.29\ncommit: 32fab6d\nbody=B\n"), 0o755); err != nil {
		t.Fatalf("write same-version alternate fixture: %v", err)
	}

	got := pathShadowDiagnostic(running, shadow)
	if !strings.Contains(got, "identity differs") {
		t.Fatalf("same-version different bytes must warn about identity drift; got %q", got)
	}
	assertShadowReconciliationCommand(t, got)
}

func TestPathShadowDiagnosticWarnsWhenVersionOrCommitBytesDiffer(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "running-mcphub.exe")
	shadow := filepath.Join(dir, "shadow-mcphub.exe")
	if err := os.WriteFile(running, []byte("mcp-local-hub 0.4.29\ncommit: 32fab6d\n"), 0o755); err != nil {
		t.Fatalf("write running binary fixture: %v", err)
	}
	if err := os.WriteFile(shadow, []byte("mcp-local-hub 0.4.30\ncommit: deadbee\n"), 0o755); err != nil {
		t.Fatalf("write different-version alternate fixture: %v", err)
	}

	got := pathShadowDiagnostic(running, shadow)
	if !strings.Contains(got, "identity differs") {
		t.Fatalf("different version or commit must warn about identity drift; got %q", got)
	}
	assertShadowReconciliationCommand(t, got)
}

func TestPathShadowDiagnosticWarnsWhenAlternateIdentityIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "running-mcphub.exe")
	missingAlternate := filepath.Join(dir, "missing-mcphub.exe")
	if err := os.WriteFile(running, []byte("mcp-local-hub 0.4.29\ncommit: 32fab6d\n"), 0o755); err != nil {
		t.Fatalf("write running binary fixture: %v", err)
	}

	got := pathShadowDiagnostic(running, missingAlternate)
	if !strings.Contains(got, "identity-unverified") {
		t.Fatalf("unreadable alternate must warn that identity is unverified; got %q", got)
	}
	assertShadowReconciliationCommand(t, got)
}

func assertShadowReconciliationCommand(t *testing.T, diagnostic string) {
	t.Helper()
	const want = "mcphub setup"
	if !strings.Contains(diagnostic, want) {
		t.Fatalf("actionable diagnostic must contain %q; got %q", want, diagnostic)
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
	if !strings.Contains(warn, "identity differs") {
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
