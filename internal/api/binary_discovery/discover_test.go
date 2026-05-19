package binary_discovery

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// binaryNameForOS returns the platform-correct filename for a binary stem.
// On Windows, Discover should resolve `<bin>` via `<bin>.exe` first; tests
// seed `<bin>.exe` so the discoverer's `.exe`-first lookup wins.
func binaryNameForOS(stem string) string {
	if runtime.GOOS == "windows" {
		return stem + ".exe"
	}
	return stem
}

// seedBinary creates an executable-ish file at dir/name. Content is empty;
// mode 0o755 is sufficient — Discover only checks for existence (os.Stat),
// not for the executable bit semantics of POSIX exec().
func seedBinary(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte{}, 0o755); err != nil {
		t.Fatalf("seed %s: %v", p, err)
	}
	return p
}

// TestDiscover_FoundInFirstHint verifies that when a binary exists in the
// first hint directory, Discover returns the absolute path from hints[0].
func TestDiscover_FoundInFirstHint(t *testing.T) {
	hint0 := t.TempDir()
	hint1 := t.TempDir()
	seedBinary(t, hint0, binaryNameForOS("python3"))

	got, err := Discover(context.Background(), []string{"python3"}, []string{hint0, hint1})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := filepath.Join(hint0, binaryNameForOS("python3"))
	if got["python3"] != want {
		t.Fatalf("python3: got %q, want %q", got["python3"], want)
	}
}

// TestDiscover_Missing verifies that a binary that exists in NO hint
// directory maps to "" (empty string) without producing an error.
// Discovery is best-effort: missing binaries are not errors.
func TestDiscover_Missing(t *testing.T) {
	hint0 := t.TempDir()

	got, err := Discover(context.Background(), []string{"nonexistent-binary"}, []string{hint0})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got["nonexistent-binary"] != "" {
		t.Fatalf("nonexistent: got %q, want empty string", got["nonexistent-binary"])
	}
}

// TestDiscover_WalksInOrder verifies that when a binary exists only in the
// second hint directory, Discover walks past hint[0] and returns the
// hint[1] path. This confirms the "first hit wins, but missing entries
// don't short-circuit the walk" contract.
func TestDiscover_WalksInOrder(t *testing.T) {
	hint0 := t.TempDir() // empty
	hint1 := t.TempDir()
	seedBinary(t, hint1, binaryNameForOS("node"))

	got, err := Discover(context.Background(), []string{"node"}, []string{hint0, hint1})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := filepath.Join(hint1, binaryNameForOS("node"))
	if got["node"] != want {
		t.Fatalf("node: got %q, want %q", got["node"], want)
	}
}

// TestDefaultHints_NonEmpty verifies that the per-OS DefaultHints() returns
// a non-empty list on the current build platform. The build tags on
// hints_{windows,linux,darwin}.go must compile-time provide an
// implementation for whatever GOOS is running this test.
func TestDefaultHints_NonEmpty(t *testing.T) {
	hints := DefaultHints()
	if len(hints) == 0 {
		t.Fatalf("DefaultHints() returned empty list for GOOS=%s", runtime.GOOS)
	}
}
