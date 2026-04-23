package api

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// withTmpDataDir redirects the LOCALAPPDATA path the dismiss helpers
// resolve against (matching internal/api/logs.go:93,
// settings.go:14, workspace_registry.go:71) to a per-test tempdir.
// Same seam install_test.go uses at line 287. Tests run in parallel,
// so each t.TempDir() yields a fresh directory.
//
// Returns the mcp-local-hub subdirectory path the helpers will actually
// write into, so tests can check for leaked temp files or the dismissed
// JSON content directly.
func withTmpDataDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	return filepath.Join(tmp, "mcp-local-hub")
}

func TestDismissUnknown_EmptyFileReturnsEmptySet(t *testing.T) {
	_ = withTmpDataDir(t)
	names, err := ListDismissedUnknown()
	if err != nil {
		t.Fatalf("ListDismissedUnknown on fresh dir: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected empty set, got %v", names)
	}
}

func TestDismissUnknown_RoundTripsSingleEntry(t *testing.T) {
	_ = withTmpDataDir(t)
	if err := DismissUnknown("fetch"); err != nil {
		t.Fatalf("DismissUnknown: %v", err)
	}
	names, err := ListDismissedUnknown()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := names["fetch"]; !ok {
		t.Errorf("fetch missing from dismissed set: %v", names)
	}
}

func TestDismissUnknown_DedupesRepeatedCalls(t *testing.T) {
	_ = withTmpDataDir(t)
	_ = DismissUnknown("fetch")
	_ = DismissUnknown("fetch")
	_ = DismissUnknown("fetch")
	names, _ := ListDismissedUnknown()
	if len(names) != 1 {
		t.Errorf("expected 1 entry after 3 dismiss calls, got %d: %v", len(names), names)
	}
}

func TestDismissUnknown_PersistsAcrossReads(t *testing.T) {
	dir := withTmpDataDir(t)
	_ = DismissUnknown("fetch")
	_ = DismissUnknown("ripgrep-mcp")
	_ = dir // only confirming the env-redirect worked
	names, err := ListDismissedUnknown()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := names["fetch"]; !ok {
		t.Error("fetch missing")
	}
	if _, ok := names["ripgrep-mcp"]; !ok {
		t.Error("ripgrep-mcp missing")
	}
}

func TestDismissUnknown_GracefulOnCorruptFile(t *testing.T) {
	dir := withTmpDataDir(t)
	_ = os.MkdirAll(dir, 0o755)
	corruptPath := filepath.Join(dir, "gui-dismissed.json")
	if err := os.WriteFile(corruptPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := ListDismissedUnknown()
	if err != nil {
		t.Fatalf("corrupt file should not surface as error: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("corrupt file should read as empty, got %v", names)
	}
}

func TestDismissUnknown_RejectsEmptyName(t *testing.T) {
	_ = withTmpDataDir(t)
	if err := DismissUnknown(""); err == nil {
		t.Error("expected error on empty name")
	}
}

func TestDismissUnknown_WritesAtomically(t *testing.T) {
	dir := withTmpDataDir(t)
	if err := DismissUnknown("stable-name"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "gui-dismissed.json" {
			continue
		}
		if filepath.Ext(name) == ".tmp" {
			t.Errorf("leaked temp file after atomic write: %s", name)
		}
	}
}

func TestDismissUnknown_FailsClosedOnUnknownVersion(t *testing.T) {
	dir := withTmpDataDir(t)
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "gui-dismissed.json")
	future := []byte(`{"version":2,"unknown":["should-be-ignored"],"extra":{"ts":"2026"}}`)
	if err := os.WriteFile(path, future, 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := ListDismissedUnknown()
	if err != nil {
		t.Fatalf("unknown version should not surface as error: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("unknown version should read as empty, got %v", names)
	}
}

func TestDismissUnknown_ConcurrentCallsArePreserved(t *testing.T) {
	_ = withTmpDataDir(t)
	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		name := fmt.Sprintf("server-%02d", i)
		go func() {
			defer wg.Done()
			if err := DismissUnknown(name); err != nil {
				t.Errorf("DismissUnknown(%q): %v", name, err)
			}
		}()
	}
	wg.Wait()
	names, err := ListDismissedUnknown()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != N {
		t.Fatalf("expected %d entries after concurrent dismisses, got %d: %v", N, len(names), names)
	}
	for i := 0; i < N; i++ {
		if _, ok := names[fmt.Sprintf("server-%02d", i)]; !ok {
			t.Errorf("server-%02d missing from dismissed set", i)
		}
	}
}
