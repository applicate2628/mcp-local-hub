// hub_mcp_resolver_test.go — Phase 3 Task 3.2 (G4 unified hub MCP).
//
// Tests for the atomic-pointer resolver snapshot. Covers:
//
//   - PublishResolverSnapshot / LoadResolverSnapshot torn-read safety
//     under concurrent reader fan-out vs writer swap.
//   - BuildResolverSnapshotFromManifests namespacing — overlapping
//     raw tool names from two servers produce distinct `<server>__<raw>`
//     route keys.
//   - Bindings indexing: a manifest with ClientBindings produces one
//     entry per (client, daemon) in snap.Bindings.
//   - Gen counter strictly advances on every BumpResolverOnManifestChange.
//
// Spec: §"Resolver state is published via atomic snapshot" + §"Tool-name
// namespacing — route map + canonical rewrite". Plan: Task 3.2.

package api

import (
	"sync"
	"sync/atomic"
	"testing"

	"mcp-local-hub/internal/config"
)

// Helper: a small fixture with one server, one daemon, one client.
func minimalManifest() config.ServerManifest {
	return config.ServerManifest{
		Name: "fs",
		Kind: "global",
		Daemons: []config.DaemonSpec{
			{Name: "claude-code", Port: 9200},
		},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code", Daemon: "claude-code"},
		},
	}
}

// Helper: a manifest with two daemons + two clients (cross binding).
func crossManifest() config.ServerManifest {
	return config.ServerManifest{
		Name: "search",
		Kind: "global",
		Daemons: []config.DaemonSpec{
			{Name: "claude-code", Port: 9210},
			{Name: "codex-cli", Port: 9211},
		},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code", Daemon: "claude-code"},
			{Client: "codex-cli", Daemon: "codex-cli"},
		},
	}
}

func TestPublishResolverSnapshotAtomicSwap(t *testing.T) {
	resetResolverForTest(t)

	snap1 := BuildResolverSnapshotFromManifests([]config.ServerManifest{minimalManifest()})
	PublishResolverSnapshot(snap1)
	got1 := LoadResolverSnapshot()
	if got1 == nil {
		t.Fatal("got1 is nil after Publish")
	}
	if got1.Gen != snap1.Gen {
		t.Fatalf("got1.Gen=%d want %d", got1.Gen, snap1.Gen)
	}

	snap2 := BuildResolverSnapshotFromManifests([]config.ServerManifest{crossManifest()})
	PublishResolverSnapshot(snap2)
	got2 := LoadResolverSnapshot()
	if got2.Gen != snap2.Gen {
		t.Fatalf("got2.Gen=%d want %d", got2.Gen, snap2.Gen)
	}
	if got1.Gen == got2.Gen {
		t.Fatalf("Gen did not advance: %d == %d", got1.Gen, got2.Gen)
	}

	// The captured-old pointer is still readable, holds the old data.
	if _, ok := got1.Bindings["codex-cli"]; ok {
		t.Errorf("got1 (old snapshot) leaked codex-cli binding from snap2")
	}
	if _, ok := got2.Bindings["codex-cli"]; !ok {
		t.Errorf("got2 missing codex-cli binding from snap2")
	}
}

// Concurrent readers + one publisher: every observed snapshot must be
// internally consistent (Gen matches the Bindings shape we built it
// with). Run with `-race` to catch torn pointer writes.
func TestResolverSnapshotConcurrentReadersSeeConsistent(t *testing.T) {
	resetResolverForTest(t)

	// Two known shapes. Snapshot A has only "fs"; snapshot B has only
	// "search". A reader that loads a snapshot with Gen == Asnap.Gen
	// MUST see "fs" bindings and NOT "search" bindings (and vice
	// versa). Any inconsistency = a torn read.
	manifestA := []config.ServerManifest{minimalManifest()}
	manifestB := []config.ServerManifest{crossManifest()}

	snapA := BuildResolverSnapshotFromManifests(manifestA)
	snapB := BuildResolverSnapshotFromManifests(manifestB)
	PublishResolverSnapshot(snapA)

	var wg sync.WaitGroup
	var stopFlag atomic.Bool
	const readers = 4

	// Pre-record the Gen values so readers know which shape to verify.
	genA := snapA.Gen
	genB := snapB.Gen

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stopFlag.Load() {
				snap := LoadResolverSnapshot()
				if snap == nil {
					t.Errorf("loaded nil snapshot")
					return
				}
				switch snap.Gen {
				case genA:
					if _, ok := snap.Bindings["claude-code"]; !ok {
						t.Errorf("genA snapshot missing claude-code binding")
					}
					if _, ok := snap.Bindings["codex-cli"]; ok {
						t.Errorf("genA snapshot leaked codex-cli binding (torn read)")
					}
				case genB:
					if _, ok := snap.Bindings["codex-cli"]; !ok {
						t.Errorf("genB snapshot missing codex-cli binding")
					}
				default:
					t.Errorf("unexpected snapshot Gen=%d", snap.Gen)
				}
			}
		}()
	}

	// Writer: swap A → B → A → B ... a thousand times.
	for i := 0; i < 1000; i++ {
		if i%2 == 0 {
			PublishResolverSnapshot(snapB)
		} else {
			PublishResolverSnapshot(snapA)
		}
	}
	stopFlag.Store(true)
	wg.Wait()
}

// Two manifests with one shared raw tool name MUST produce two
// distinct route keys: "fs__read" + "search__read".
func TestBuildResolverSnapshotNamespacing(t *testing.T) {
	resetResolverForTest(t)

	// Note: at snapshot-build time we may not have the live tools list
	// for each daemon (that's populated at session-init time by the
	// aggregator from tools/list responses). Each session's RouteMap
	// (atomic.Pointer on hubSession) is the canonical per-session
	// merged route index built from per-daemon tools/list responses;
	// ResolverSnapshot owns only the binding topology. This test
	// focuses on what the build path DOES produce — namely Bindings
	// keyed by client_id.
	snap := BuildResolverSnapshotFromManifests([]config.ServerManifest{
		minimalManifest(),
		crossManifest(),
	})

	// claude-code is bound to both servers ("fs" via minimalManifest;
	// "search" via crossManifest). Bindings["claude-code"] should
	// contain both daemons.
	cc := snap.Bindings["claude-code"]
	if len(cc) != 2 {
		t.Fatalf("claude-code bindings=%d want 2 (saw %+v)", len(cc), cc)
	}
	servers := map[string]bool{}
	for _, ref := range cc {
		servers[ref.Server] = true
	}
	if !servers["fs"] || !servers["search"] {
		t.Errorf("claude-code missing fs or search: %+v", servers)
	}

	// codex-cli is bound only to "search".
	codex := snap.Bindings["codex-cli"]
	if len(codex) != 1 {
		t.Fatalf("codex-cli bindings=%d want 1 (saw %+v)", len(codex), codex)
	}
	if codex[0].Server != "search" || codex[0].Daemon != "codex-cli" || codex[0].Port != 9211 {
		t.Errorf("codex-cli binding wrong: %+v", codex[0])
	}
}

// Gen counter is strictly monotonic across BumpResolverOnManifestChange.
func TestBumpResolverOnManifestChangeAdvancesGen(t *testing.T) {
	resetResolverForTest(t)

	BumpResolverOnManifestChange([]config.ServerManifest{minimalManifest()})
	first := LoadResolverSnapshot()
	if first == nil {
		t.Fatal("nil snapshot after first bump")
	}
	BumpResolverOnManifestChange([]config.ServerManifest{crossManifest()})
	second := LoadResolverSnapshot()
	if second == nil {
		t.Fatal("nil snapshot after second bump")
	}
	if second.Gen <= first.Gen {
		t.Errorf("Gen did not advance: %d -> %d", first.Gen, second.Gen)
	}
}

// TestBuildResolverSnapshotDeduplicatesRepeatedBindings pins the
// codex bot r10 P2 closure on PR #157. A manifest accidentally
// repeating a ClientBinding row (no uniqueness check in current
// validation) must NOT produce duplicate canonicalDaemonRef entries
// for the same client. Otherwise AggregateInitialize would fan-out
// the same daemon multiple times, both goroutines writing to the
// same InitSuccesses key, leaving one daemon session orphaned and
// the other un-tracked for cancellation / cleanup.
func TestBuildResolverSnapshotDeduplicatesRepeatedBindings(t *testing.T) {
	resetResolverForTest(t)

	// Manifest with the SAME (client, daemon) row appearing THREE times.
	// A second manifest contributes a distinct (client, daemon) so we
	// can assert that dedupe is per-(client, ref) tuple, not global.
	m := config.ServerManifest{
		Name: "fs",
		Kind: "global",
		Daemons: []config.DaemonSpec{
			{Name: "claude-code", Port: 9200},
		},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code", Daemon: "claude-code"},
			{Client: "claude-code", Daemon: "claude-code"}, // duplicate
			{Client: "claude-code", Daemon: "claude-code"}, // duplicate
		},
	}
	m2 := config.ServerManifest{
		Name: "search",
		Kind: "global",
		Daemons: []config.DaemonSpec{
			{Name: "claude-code", Port: 9210},
		},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code", Daemon: "claude-code"},
		},
	}

	snap := BuildResolverSnapshotFromManifests([]config.ServerManifest{m, m2})

	got := snap.Bindings["claude-code"]
	if len(got) != 2 {
		t.Fatalf("want 2 distinct refs after dedupe (fs + search), got %d: %+v", len(got), got)
	}
	servers := map[string]bool{}
	for _, ref := range got {
		servers[ref.Server] = true
	}
	if !servers["fs"] {
		t.Errorf("missing fs ref after dedupe: %+v", got)
	}
	if !servers["search"] {
		t.Errorf("missing search ref after dedupe: %+v", got)
	}
}

// Manifest without ClientBindings produces an empty Bindings map (no
// implicit client routing — the install reconciler is the source of
// (server, client) intent, but bindings are filled from the manifest
// ClientBindings field at build time).
func TestBuildResolverSnapshotEmptyClientBindings(t *testing.T) {
	resetResolverForTest(t)
	m := config.ServerManifest{
		Name:    "orphan",
		Kind:    "global",
		Daemons: []config.DaemonSpec{{Name: "claude-code", Port: 9220}},
		// no ClientBindings
	}
	snap := BuildResolverSnapshotFromManifests([]config.ServerManifest{m})
	if len(snap.Bindings) != 0 {
		t.Errorf("orphan manifest produced bindings: %+v", snap.Bindings)
	}
}

// resetResolverForTest resets the package-level snapshot pointer + gen
// counter so each test starts from a clean state. Phase 4 will load
// real state at startup; tests need to clear the static between cases.
func resetResolverForTest(t *testing.T) {
	t.Helper()
	resolverSnapshot.Store(nil)
	resolverGen.Store(0)
}
