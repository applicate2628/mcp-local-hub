// internal/api/serena_routing/resolver_concurrent_generation_test.go
//
// Finding A2 (architecture-adversarial-reverify.md,
// work-items/active/2026-07-25-mcp-front-daemon/), runtime-proven by
// qa-adversarial-falsifiers.md: two overlapping refresh() calls could each
// Load() a DIFFERENT complete registry generation and race to publish
// independently — whichever happened to reach the final r.mu.Lock()
// publish LAST won, regardless of which generation was actually newer, so
// an older generation could silently overwrite a newer one already served
// to callers.
//
// The interleaving below is engineered deterministically through
// refreshLoadedHookForTest (never rely on a naturally-occurring race window
// staying observable): the first refresh to load generation A is paused
// after Load()+SerenaEntries() but before publish; while it is paused, a
// NEWER generation B is written to disk and a second, concurrent refresh is
// triggered for it; only then is the first refresh released.
package serena_routing

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// TestNewReadOnlyWorkspaceResolver_ConcurrentRefreshesCannotRegress is the
// A2 falsifying test.
//
// Mutation-proven: temporarily removing the refreshMu.Lock()/Unlock() pair
// around the reload in refresh() (keeping refreshLoadedHookForTest's call
// site exactly where it is) makes this test fail — the final published
// cache regresses to generation A's port even though generation B was
// already published first. Reverted after confirming the failure; see the
// commit message for the exact transcript.
func TestNewReadOnlyWorkspaceResolver_ConcurrentRefreshesCannotRegress(t *testing.T) {
	root := t.TempDir()
	wsPath := makeWorkspace(t, root, "ConcurrentGen")
	regPath := filepath.Join(root, "workspaces.yaml")

	const (
		portSeed = 19099
		portA    = 19101
		portB    = 19102
	)
	writeGeneration := func(port int, mtime time.Time) {
		t.Helper()
		g := api.NewRegistry(regPath)
		if err := g.PutSerena(api.WorkspaceEntry{
			WorkspaceKey:  api.WorkspaceKey(wsPath),
			WorkspacePath: wsPath,
			Language:      api.SerenaLanguageSentinel,
			Backend:       "serena",
			Port:          port,
			TaskName:      "mcp-local-hub-serena-concurrent",
		}); err != nil {
			t.Fatalf("PutSerena port %d: %v", port, err)
		}
		if err := g.Save(); err != nil {
			t.Fatalf("Save port %d: %v", port, err)
		}
		setFileModTime(t, regPath, mtime)
	}

	base := time.Now().Truncate(time.Second)
	writeGeneration(portSeed, base)

	reg := api.NewRegistry(regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load seed: %v", err)
	}
	resolver := NewReadOnlyWorkspaceResolver(reg, regPath)
	if entries := resolver.snapshot(); len(entries) != 1 || entries[0].Port != portSeed {
		t.Fatalf("prime snapshot = %+v, want single seed entry port %d", entries, portSeed)
	}

	reachedA := make(chan struct{})
	releaseA := make(chan struct{})
	var pauseOnce sync.Once
	t.Cleanup(func() { refreshLoadedHookForTest = nil })
	refreshLoadedHookForTest = func(entries []api.WorkspaceEntry) {
		for _, e := range entries {
			if e.Port == portA {
				pauseOnce.Do(func() {
					close(reachedA)
					<-releaseA
				})
				return
			}
		}
	}

	// Generation A lands on disk BEFORE the paused reload starts, so its own
	// Load() genuinely reads A.
	writeGeneration(portA, base.Add(2*time.Second))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resolver.snapshot() // pauses inside refreshLoadedHookForTest above
	}()

	select {
	case <-reachedA:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the paused reload to reach generation A — the hook never fired, so the interleaving was not engineered as intended")
	}

	// Generation A has been fully loaded by the paused goroutine but not yet
	// published. Write a NEWER generation B while A is still paused, then
	// trigger a second, concurrent reload for it.
	writeGeneration(portB, base.Add(4*time.Second))

	doneB := make(chan struct{})
	go func() {
		defer close(doneB)
		resolver.snapshot()
	}()

	// Bounded window: on the pre-fix (unserialized) implementation this
	// completes quickly, reproducing exactly the interleaving the
	// architecture review named (B publishes while A is still paused). On
	// the fixed (serialized) implementation this goroutine simply blocks on
	// refreshMu the whole time A holds it — that is fine and expected; the
	// decisive assertion is the FINAL published state after both goroutines
	// are joined below, not what happens inside this window.
	select {
	case <-doneB:
	case <-time.After(300 * time.Millisecond):
	}

	close(releaseA)

	waitOrTimeout := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s to finish", what)
		}
	}
	waitOrTimeout(doneB, "the generation-B reload")
	wgDone := make(chan struct{})
	go func() { wg.Wait(); close(wgDone) }()
	waitOrTimeout(wgDone, "the generation-A reload")

	// Read the raw published state directly — deliberately NOT via another
	// snapshot()/refresh() call, which would itself trigger a further
	// reload (the file's current mtime may still differ from whatever the
	// buggy pre-fix code last published) and could self-heal a transient
	// regression that already happened, hiding the very bug this test
	// exists to catch.
	resolver.mu.RLock()
	finalCached := append([]api.WorkspaceEntry(nil), resolver.cached...)
	finalMtime := resolver.lastMtime
	resolver.mu.RUnlock()

	if len(finalCached) != 1 {
		t.Fatalf("final published cache has %d entries, want 1: %+v", len(finalCached), finalCached)
	}
	if finalCached[0].Port != portB {
		t.Fatalf("final published cache port = %d, want %d (generation B) — an older generation (A=%d) overwrote a newer one already published", finalCached[0].Port, portB, portA)
	}
	wantMtime := base.Add(4 * time.Second)
	if !finalMtime.Equal(wantMtime) {
		t.Fatalf("resolver.lastMtime = %v, want %v (generation B's mtime) — the publication token regressed", finalMtime, wantMtime)
	}
}
