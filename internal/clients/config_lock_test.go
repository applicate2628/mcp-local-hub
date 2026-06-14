package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
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
