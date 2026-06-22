package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/gofrs/flock"
)

// newLockingCursorForTest builds a lock-decorated cursor adapter bound to a
// fresh temp config seeded with `initial`. The decorator (newLockingClient)
// is the production wrapper every NewX() factory now applies, so exercising
// it here covers the real concurrent-write path.
func newLockingCursorForTest(t *testing.T, initial string) (Client, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	c := newLockingClient(&cursorClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "cursor",
		urlField:   "url",
	}})
	return c, path
}

// TestConfigLock_SecureParentCreateRunsEvenWhenParentExists pins bot PR #420
// r17 finding P2a: withConfigLock must call SecureCreateParentDir
// UNCONDITIONALLY before flock.New — NOT only when the parent dir is absent. The
// earlier IsNotExist-guarded form skipped the secure descent when the parent dir
// already existed, so an existing SYMLINKED / reparse-point parent was never
// refused: flock.New would create the lock file THROUGH it. Here the parent dir
// already exists (the test seed writes the config file, creating the dir), yet
// the secure creator MUST still be invoked on the mutating path.
func TestConfigLock_SecureParentCreateRunsEvenWhenParentExists(t *testing.T) {
	// The seed already created the parent dir (it wrote mcp.json into it).
	c, path := newLockingCursorForTest(t, `{"mcpServers":{}}`)
	parent := filepath.Dir(path)
	if _, err := os.Stat(parent); err != nil {
		t.Fatalf("precondition: parent dir must already exist: %v", err)
	}

	prev := SecureCreateParentDir
	t.Cleanup(func() { SecureCreateParentDir = prev })
	var calledWith string
	SecureCreateParentDir = func(dir string) error {
		calledWith = dir
		return prev(dir) // delegate to the real (test fallback) so the flow proceeds
	}

	if err := c.AddEntry(MCPEntry{Name: "a", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("AddEntry: %v", err)
	}
	if calledWith == "" {
		t.Fatal("SecureCreateParentDir must be invoked even when the parent dir already exists (P2a) — it was not")
	}
	if filepath.Clean(calledWith) != filepath.Clean(parent) {
		t.Errorf("SecureCreateParentDir called with %q, want the write-target parent %q", calledWith, parent)
	}
}

// TestConfigLock_ConcurrentAddRemoveBackup_NeverTearsFile hammers a single
// client config file with concurrent AddEntry / RemoveEntry / Backup calls
// from two goroutines. After the storm the file must ALWAYS be a complete,
// valid JSON serialization (never a torn/truncated write) and must preserve
// the seeded top-level field. Run with -race.
//
// Without withConfigLock the underlying writeJSON path (os.WriteFile in the
// test fallback) truncates-then-writes, so two interleaved writers can leave
// the file half-written; the per-path mutex + flock serialize them.
func TestConfigLock_ConcurrentAddRemoveBackup_NeverTearsFile(t *testing.T) {
	c, path := newLockingCursorForTest(t, `{"other":"keep-me","mcpServers":{}}`)

	const iters = 200
	var wg sync.WaitGroup
	wg.Add(3)

	// Goroutine 1: repeatedly add entry "a".
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if err := c.AddEntry(MCPEntry{Name: "a", URL: "http://localhost:9121/mcp"}); err != nil {
				t.Errorf("AddEntry a: %v", err)
				return
			}
		}
	}()
	// Goroutine 2: repeatedly add+remove entry "b".
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if err := c.AddEntry(MCPEntry{Name: "b", URL: "http://localhost:9122/mcp"}); err != nil {
				t.Errorf("AddEntry b: %v", err)
				return
			}
			if err := c.RemoveEntry("b"); err != nil {
				t.Errorf("RemoveEntry b: %v", err)
				return
			}
		}
	}()
	// Goroutine 3: repeatedly Backup (reads the live file, writes a sibling).
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if _, err := c.Backup(); err != nil {
				t.Errorf("Backup: %v", err)
				return
			}
		}
	}()

	wg.Wait()

	// Final invariant: the live file is complete, valid JSON with the seeded
	// top-level field intact and the always-added "a" entry present.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("final file is not valid JSON (torn write): %v\nbytes: %q", err, raw)
	}
	if parsed["other"] != "keep-me" {
		t.Errorf("seeded top-level field lost: %v", parsed["other"])
	}
	servers, ok := parsed["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or wrong type: %T", parsed["mcpServers"])
	}
	if _, ok := servers["a"].(map[string]any); !ok {
		t.Errorf("entry \"a\" missing from final file: %v", servers["a"])
	}
}

// TestConfigLock_FlockSerializesAcrossProcesses proves the cross-process leg:
// two independent flock handles on the SAME lock path cannot both hold the
// lock at once. gofrs/flock is process-shared advisory locking, so this
// two-handle proxy demonstrates the serialization a second process would see
// (a full subprocess test is heavier and unnecessary to prove the property —
// this is the two-handle proxy variant per the task spec).
func TestConfigLock_FlockSerializesAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "mcp.json.lock")

	a := flock.New(lockPath)
	b := flock.New(lockPath)

	locked, err := a.TryLock()
	if err != nil {
		t.Fatalf("handle A TryLock: %v", err)
	}
	if !locked {
		t.Fatal("handle A could not acquire a free lock")
	}

	// While A holds it, B must NOT be able to acquire it.
	got, err := b.TryLock()
	if err != nil {
		t.Fatalf("handle B TryLock: %v", err)
	}
	if got {
		_ = b.Unlock()
		t.Fatal("handle B acquired the lock while handle A held it — flock did not serialize")
	}

	// After A releases, B can acquire it.
	if err := a.Unlock(); err != nil {
		t.Fatalf("handle A Unlock: %v", err)
	}
	got, err = b.TryLock()
	if err != nil {
		t.Fatalf("handle B TryLock after release: %v", err)
	}
	if !got {
		t.Fatal("handle B could not acquire after handle A released")
	}
	if err := b.Unlock(); err != nil {
		t.Fatalf("handle B Unlock: %v", err)
	}
}

// TestConfigLock_MutatingMethodCreatesMissingParentDir pins bot PR #420 r15
// finding 3 at the single-owner level: a mutating method routed through
// withConfigLock must create the write-target's MISSING parent dir before
// acquiring the advisory flock (the lock file lives in that dir, so without the
// create the lock acquisition itself fails). This covers install / register /
// GUI Apply against an otherwise-active profile whose write-target dir does not
// yet exist. Pinned on a generic adapter (cursor) so it asserts the chokepoint
// behavior, not a mimocode specific.
func TestConfigLock_MutatingMethodCreatesMissingParentDir(t *testing.T) {
	parent := t.TempDir()
	// The write target lives in a subdir that does NOT exist yet.
	missingDir := filepath.Join(parent, "does-not-exist-yet")
	path := filepath.Join(missingDir, "mcp.json")
	if _, err := os.Stat(missingDir); !os.IsNotExist(err) {
		t.Fatalf("precondition: %s must NOT exist (stat err=%v)", missingDir, err)
	}

	c := newLockingClient(&cursorClient{jsonMCPClient: &jsonMCPClient{
		path:       path,
		clientName: "cursor",
		urlField:   "url",
	}})

	// A bare AddEntry through the locking decorator must create the missing parent
	// dir (via withConfigLock) and write the file — NOT fail at the flock.
	if err := c.AddEntry(MCPEntry{Name: "a", URL: "http://127.0.0.1:9999/mcp"}); err != nil {
		t.Fatalf("AddEntry must create the missing parent dir and succeed, got: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("write-target file must be created in the previously-missing dir, stat err=%v", err)
	}
	// The created parent dir is owner-only (0o700) on POSIX so a subsequent strict
	// secure-write gate would not reject it. (Mode bits are advisory on Windows.)
	if runtime.GOOS != "windows" {
		st, err := os.Stat(missingDir)
		if err != nil {
			t.Fatalf("stat created parent dir: %v", err)
		}
		if perm := st.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("created parent dir must be owner-only (0o700), got %o", perm)
		}
	}
}
