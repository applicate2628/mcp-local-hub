package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

func TestUndismissUnknown_RemovesOnlyNamedEntry(t *testing.T) {
	dir := withTmpDataDir(t)
	if err := DismissUnknown("zeta"); err != nil {
		t.Fatalf("DismissUnknown(zeta): %v", err)
	}
	if err := DismissUnknown("alpha"); err != nil {
		t.Fatalf("DismissUnknown(alpha): %v", err)
	}

	if err := UndismissUnknown("alpha"); err != nil {
		t.Fatalf("UndismissUnknown(alpha): %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, dismissedFileName))
	if err != nil {
		t.Fatal(err)
	}
	var payload dismissedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode persisted dismissal payload: %v", err)
	}
	if !reflect.DeepEqual(payload.Unknown, []string{"zeta"}) {
		t.Fatalf("persisted unknown=%v, want [zeta]", payload.Unknown)
	}

	names, err := ListDismissedUnknown()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Fatalf("dismissed set=%v, want only zeta", names)
	}
	if _, ok := names["zeta"]; !ok {
		t.Fatalf("zeta missing from dismissed set: %v", names)
	}
}

func TestUndismissUnknown_MissingIsNoOp(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		dir := withTmpDataDir(t)
		path := filepath.Join(dir, dismissedFileName)

		if err := UndismissUnknown("absent"); err != nil {
			t.Fatalf("UndismissUnknown(absent): %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("missing dismissal file was created or stat failed unexpectedly: %v", err)
		}
	})

	t.Run("absent key", func(t *testing.T) {
		dir := withTmpDataDir(t)
		if err := DismissUnknown("kept"); err != nil {
			t.Fatalf("DismissUnknown(kept): %v", err)
		}
		path := filepath.Join(dir, dismissedFileName)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		if err := UndismissUnknown("absent"); err != nil {
			t.Fatalf("UndismissUnknown(absent): %v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) {
			t.Fatalf("absent-key restore rewrote dismissal state\nbefore=%s\nafter=%s", before, after)
		}
	})
}

func TestUndismissUnknown_WritesAtomically(t *testing.T) {
	dir := withTmpDataDir(t)
	if err := DismissUnknown("last"); err != nil {
		t.Fatalf("DismissUnknown(last): %v", err)
	}
	if err := UndismissUnknown("last"); err != nil {
		t.Fatalf("UndismissUnknown(last): %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" {
			t.Errorf("leaked temp file after atomic un-dismiss write: %s", entry.Name())
		}
	}

	raw, err := os.ReadFile(filepath.Join(dir, dismissedFileName))
	if err != nil {
		t.Fatal(err)
	}
	var payload dismissedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode persisted dismissal payload: %v", err)
	}
	if payload.Version != 1 {
		t.Fatalf("persisted version=%d, want 1", payload.Version)
	}
	if payload.Unknown == nil {
		t.Fatal("persisted unknown is nil, want non-nil empty slice")
	}
	if len(payload.Unknown) != 0 {
		t.Fatalf("persisted unknown=%v, want empty", payload.Unknown)
	}
}

func TestUndismissUnknown_ConcurrentRemovalsArePreserved(t *testing.T) {
	_ = withTmpDataDir(t)
	for _, name := range []string{"alpha", "beta", "kept"} {
		if err := DismissUnknown(name); err != nil {
			t.Fatalf("DismissUnknown(%s): %v", name, err)
		}
	}

	var wg sync.WaitGroup
	for _, name := range []string{"alpha", "beta"} {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := UndismissUnknown(name); err != nil {
				t.Errorf("UndismissUnknown(%q): %v", name, err)
			}
		}()
	}
	wg.Wait()

	names, err := ListDismissedUnknown()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Fatalf("dismissed set=%v, want only kept", names)
	}
	if _, ok := names["kept"]; !ok {
		t.Fatalf("kept missing from dismissed set: %v", names)
	}
}

func TestUndismissUnknown_RejectsEmptyName(t *testing.T) {
	_ = withTmpDataDir(t)
	if err := UndismissUnknown(""); err == nil {
		t.Fatal("expected error on empty name")
	}
}
