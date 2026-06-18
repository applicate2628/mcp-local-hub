// hub_mcp_groups_round4_test.go — groups/namespaces bot round-4 fixes.
//
// Covers the three round-4 behaviors:
//   - R4-1: a per-session server (perSessionServers) is excluded from BOTH
//     the resolver group-fold AND RoutableServerNames (the group picker +
//     groupsUpsert known-server save-gate source).
//   - R4-3: PublishGroupsSnapshotLocked runs the manifest scan UNDER the held
//     hub-mcp.lock (scan + publish is one atomic critical section, so two
//     concurrent publishes serialize and never interleave their scans).
//   - R4-4: PublishGroupsSnapshotLocked acquires the flock ctx-bounded — a
//     pre-canceled ctx fails fast instead of blocking.
//
// State-safety: the lock/scan tests go through hubMcpStateTestHelper, which
// installs a hardened per-test state root (no live supervisor / hub state is
// touched). The pure-builder R4-1 test needs no state. The RoutableServerNames
// test seeds its own MCPHUB_MANIFEST_DIR_OVERRIDE temp dir.

package api

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"mcp-local-hub/internal/config"
)

// perSessionTestServerName returns one server name from the perSessionServers
// map so the test never hardcodes a value that drifts if the map changes.
func perSessionTestServerName(t *testing.T) string {
	t.Helper()
	for name := range perSessionServers {
		return name
	}
	t.Fatalf("perSessionServers is empty; round-4 exclusion test needs at least one entry")
	return ""
}

// TestRound4_PerSessionServerExcludedFromGroupFold (R4-1): a group naming a
// per-session server must bind NOTHING for it, even when that server has a
// live manifest with daemons. Per-session servers must stay 1-per-local-client
// (scan.go marks them CanMigrate=false) and can never be folded into a shared
// /g/<group>/mcp route. A co-resolvable normal server in the same group still
// binds, proving the skip is surgical (only the per-session member is dropped).
func TestRound4_PerSessionServerExcludedFromGroupFold(t *testing.T) {
	resetResolverForTest(t)
	perSession := perSessionTestServerName(t)

	// Manifest set: the per-session server WITH a daemon (so a missing-daemon
	// skip is NOT what drops it — the per-session guard is), plus "memory".
	manifests := []config.ServerManifest{
		{
			Name: perSession,
			Kind: "global",
			Daemons: []config.DaemonSpec{
				{Name: "claude-code", Port: 9401},
			},
		},
		{
			Name: "memory",
			Kind: "global",
			Daemons: []config.DaemonSpec{
				{Name: "claude-code", Port: 9402},
			},
		},
	}

	groups := []Group{{Name: "mixed", Servers: []string{perSession, "memory"}}}
	snap := BuildResolverSnapshotFromManifestsAndGroups(manifests, groups)

	groupRefs := snap.Bindings[GroupScopeKey("mixed")]
	// Only "memory" must resolve; the per-session server is skipped despite
	// having a live daemon.
	if len(groupRefs) != 1 {
		t.Fatalf("g:mixed should bind only the non-per-session 'memory' (1 ref), got %d: %+v", len(groupRefs), groupRefs)
	}
	if groupRefs[0].Server != "memory" {
		t.Fatalf("g:mixed sole ref should be 'memory', got %+v (per-session %q must be skipped)", groupRefs[0], perSession)
	}

	// A group naming ONLY the per-session server binds nothing — but the group
	// is still DECLARED (gate-2 known) so it routes nothing rather than 404.
	resetResolverForTest(t)
	groupsOnlyPerSession := []Group{{Name: "iso", Servers: []string{perSession}}}
	snap2 := BuildResolverSnapshotFromManifestsAndGroups(manifests, groupsOnlyPerSession)
	if refs := snap2.Bindings[GroupScopeKey("iso")]; len(refs) != 0 {
		t.Fatalf("g:iso (per-session-only) must bind nothing, got %+v", refs)
	}
	if !snap2.Groups[GroupScopeKey("iso")] {
		t.Fatal("g:iso must still be DECLARED (known) even though it binds nothing")
	}
}

// TestRound4_RoutableServerNamesExcludesPerSession (R4-1): RoutableServerNames
// is the group picker + groupsUpsert known-server save-gate source, so a
// per-session server must NOT appear there (an operator must not be able to
// add it to a group in the first place). A normal server WITH a daemon still
// appears, proving only the per-session entry is filtered.
func TestRound4_RoutableServerNamesExcludesPerSession(t *testing.T) {
	perSession := perSessionTestServerName(t)

	root := t.TempDir()
	// A routable normal server (has a static daemons[] block).
	routableYAML := `name: routable-demo
kind: global
transport: stdio-bridge
command: echo
base_args: ["x"]
daemons:
  - name: default
    port: 9411
`
	// The per-session server, ALSO with a daemons[] block — so its exclusion
	// is the per-session guard, not the daemonless filter.
	perSessionYAML := `name: ` + perSession + `
kind: global
transport: stdio-bridge
command: echo
base_args: ["x"]
daemons:
  - name: default
    port: 9412
`
	for name, body := range map[string]string{
		"routable-demo": routableYAML,
		perSession:      perSessionYAML,
	} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %q: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0o600); err != nil {
			t.Fatalf("write %q manifest: %v", name, err)
		}
	}
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", root)

	names, err := NewAPI().RoutableServerNames()
	if err != nil {
		t.Fatalf("RoutableServerNames: %v", err)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if got[perSession] {
		t.Fatalf("RoutableServerNames must EXCLUDE per-session server %q, got %v", perSession, names)
	}
	if !got["routable-demo"] {
		t.Fatalf("RoutableServerNames must INCLUDE the routable normal server, got %v", names)
	}
}

// TestRound4_PublishScansUnderLock (R4-3): the manifest scan closure runs UNDER
// the held hub-mcp.lock, so two concurrent PublishGroupsSnapshotLocked calls
// serialize — their scan closures NEVER overlap. We prove mutual exclusion: an
// in-flight counter incremented at scan entry must never exceed 1, and both
// publishes must complete. (If the scan ran outside the lock, the two scans
// could interleave and the counter would reach 2.)
func TestRound4_PublishScansUnderLock(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	var inFlight atomic.Int32
	var maxObserved atomic.Int32
	scanRuns := func() ([]config.ServerManifest, error) {
		cur := inFlight.Add(1)
		// Record the peak concurrency seen at scan entry.
		for {
			prev := maxObserved.Load()
			if cur <= prev || maxObserved.CompareAndSwap(prev, cur) {
				break
			}
		}
		// Busy-spin briefly to widen any interleaving window a non-locked
		// scan would expose. Deterministic upper bound (not a sleep race):
		// if the lock did NOT serialize, the sibling goroutine's scan would
		// enter during this window and push inFlight to 2.
		for i := 0; i < 200_000; i++ {
			_ = i * i
		}
		inFlight.Add(-1)
		return []config.ServerManifest{
			{Name: "memory", Kind: "global", Daemons: []config.DaemonSpec{{Name: "claude-code", Port: 9421}}},
		}, nil
	}

	const goroutines = 6
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = PublishGroupsSnapshotLocked(context.Background(), scanRuns)
		}(g)
	}
	wg.Wait()

	for idx, err := range errs {
		if err != nil {
			t.Fatalf("PublishGroupsSnapshotLocked goroutine %d: %v", idx, err)
		}
	}
	if mx := maxObserved.Load(); mx != 1 {
		t.Fatalf("scan closures ran concurrently (max in-flight=%d) — scan is NOT serialized under hub-mcp.lock", mx)
	}
	// A snapshot must have been published by the serialized publishes.
	if LoadResolverSnapshot() == nil {
		t.Fatal("no snapshot published after PublishGroupsSnapshotLocked runs")
	}
}

// TestRound4_PublishScanError_NoPublish (R4-3): a scan failure aborts WITHOUT
// publishing — a partial scan must never produce a torn snapshot. The prior
// snapshot (if any) is left untouched and the error is surfaced.
func TestRound4_PublishScanError_NoPublish(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	// Seed a known-good snapshot first so we can prove the failed publish did
	// NOT overwrite it.
	PublishResolverSnapshot(BuildResolverSnapshotFromManifests([]config.ServerManifest{
		{Name: "memory", Kind: "global", Daemons: []config.DaemonSpec{{Name: "claude-code", Port: 9431}}},
	}))
	before := LoadResolverSnapshot()
	if before == nil {
		t.Fatal("test setup: seed snapshot is nil")
	}

	wantErr := os.ErrPermission
	err := PublishGroupsSnapshotLocked(context.Background(), func() ([]config.ServerManifest, error) {
		return nil, wantErr
	})
	if err == nil {
		t.Fatal("PublishGroupsSnapshotLocked must return the scan error, got nil")
	}
	// The published snapshot pointer must be UNCHANGED (no torn publish from a
	// failed scan).
	if LoadResolverSnapshot() != before {
		t.Fatal("a failed scan published a new snapshot — must leave the prior snapshot untouched")
	}
}

// TestRound4_PublishCtxCanceled_FailsFast (R4-4): a pre-canceled ctx makes the
// ctx-aware flock acquisition fail fast WITHOUT running the scan closure or
// publishing. (The blocking acquire would instead wait on a sibling holder.)
func TestRound4_PublishCtxCanceled_FailsFast(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before the call

	var scanRan atomic.Bool
	err := PublishGroupsSnapshotLocked(ctx, func() ([]config.ServerManifest, error) {
		scanRan.Store(true)
		return nil, nil
	})
	if err == nil {
		t.Fatal("PublishGroupsSnapshotLocked with a canceled ctx must return an error (ctx-bounded acquire)")
	}
	if scanRan.Load() {
		t.Fatal("scan closure ran even though the ctx-bounded lock acquire should have failed first")
	}
	if LoadResolverSnapshot() != nil {
		t.Fatal("a ctx-canceled publish must NOT publish a snapshot")
	}
}
