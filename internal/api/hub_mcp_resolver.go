// hub_mcp_resolver.go — Phase 3 Task 3.2 (G4 unified hub MCP).
//
// Package-level immutable resolver snapshot, published via
// atomic.Pointer[ResolverSnapshot]. Sessions capture the pointer at
// initialize; tools/call loads the CURRENT pointer and revalidates
// (Client, Server, Daemon) against it. The pointer swap is atomic —
// readers either see the OLD or the NEW snapshot, never a torn read.
//
// Why atomic-pointer instead of a bare-integer resolver-gen counter:
// codex r3 security F-S4 closure. A bare-integer counter can drift
// from the underlying binding state under concurrent mutation
// (writer bumps gen, then crashes mid-way through rebuilding routes;
// readers see the new gen but with the OLD route data). The atomic
// pointer pattern publishes a complete struct in one swap — partial
// states are unobservable.
//
// Bindings:    client_id -> [(server, daemon, port), ...]
//   Used at initialize fan-out time: hub iterates this list to know
//   which daemons to call initialize on for a given calling client.
//
// Routing is per-session, not snapshot-global: the aggregator builds
// each session's RouteMap (atomic.Pointer on hubSession) at
// tools/list merge time from per-daemon responses. ResolverSnapshot
// owns only the binding topology — the (client, server, daemon, port)
// edges read at initialize fan-out time.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Per-hub session model" + §"Resolver state is published via atomic
// snapshot" + §"Tool-name namespacing — route map + canonical rewrite".
// Plan: Task 3.2.

package api

import (
	"sync/atomic"

	"mcp-local-hub/internal/config"
)

// canonicalDaemonRef identifies a daemon participating in a hub
// session. Comparable so it can be a map key (used by InitSuccesses
// to record per-daemon Mcp-Session-Id values).
type canonicalDaemonRef struct {
	Server string
	Daemon string
	Port   int
}

// canonicalToolRef identifies the daemon-side tool the hub forwards
// a tools/call to. RawName is the un-namespaced tool name as the
// daemon expects in params.name; the hub-exposed name is
// "<Server>__<RawName>".
type canonicalToolRef struct {
	Server  string
	Daemon  string
	Port    int
	RawName string
}

// ResolverSnapshot is the immutable per-publication shape of the
// hub's routing state. Once Store'd into resolverSnapshot, the
// struct fields are read-only — DO NOT mutate after publish.
//
// Gen is a monotonically-increasing counter assigned by
// BuildResolverSnapshotFromManifests. It exists for diagnostics
// (status output, log lines) and tie-breaking when comparing two
// snapshots; the authoritative identity for "is the routing state
// the same" comparison is the snapshot pointer itself.
type ResolverSnapshot struct {
	Gen      int64
	Bindings map[string][]canonicalDaemonRef
}

// Package-level atomic pointer to the currently-published snapshot.
// Sessions capture this at initialize. tools/call loads the CURRENT
// pointer and compares (a) is it still the same pointer? (b) is the
// (Server, Daemon) tuple still in the calling client's Bindings?
// If either check fails AND the route's daemon is no longer reachable
// for this client, refuse with -32601 "tool moved out of scope".
var (
	resolverSnapshot atomic.Pointer[ResolverSnapshot]
	resolverGen      atomic.Int64
)

// PublishResolverSnapshot atomically swaps in a new snapshot. Callers
// MUST build the struct off-line, populate every map fully, and then
// hand it to this function — the snapshot is treated as immutable
// after the Store call. Concurrent readers continue to see the
// previous snapshot until the Store completes.
func PublishResolverSnapshot(snap *ResolverSnapshot) {
	resolverSnapshot.Store(snap)
}

// LoadResolverSnapshot returns the currently-published snapshot,
// or nil if no snapshot has been published yet. Returns the SAME
// pointer the publisher Stored — readers may dereference it freely
// because the snapshot is immutable post-publish.
func LoadResolverSnapshot() *ResolverSnapshot {
	return resolverSnapshot.Load()
}

// BuildResolverSnapshotFromManifests walks every manifest in the
// supplied slice and constructs a fresh ResolverSnapshot. Gen is
// advanced via resolverGen.Add(1). Caller invokes
// PublishResolverSnapshot to swap it in (or uses
// BumpResolverOnManifestChange for the build+publish convenience).
//
// Bindings is populated from each manifest's ClientBindings field:
// for every (client, daemon) pair, the bindings list for that client
// gains an entry naming the daemon's port + parent server.
//
// Routes are NOT carried by the snapshot: each session's RouteMap is
// built per-call by the aggregator from per-daemon tools/list
// responses. The snapshot owns only the binding topology used at
// initialize fan-out time.
func BuildResolverSnapshotFromManifests(manifests []config.ServerManifest) *ResolverSnapshot {
	gen := resolverGen.Add(1)
	snap := &ResolverSnapshot{
		Gen:      gen,
		Bindings: make(map[string][]canonicalDaemonRef),
	}
	// codex bot r10 P2 closure on PR #157: dedupe (client, ref) pairs
	// so a manifest that accidentally repeats a ClientBinding row (no
	// uniqueness check in validation yet) cannot make AggregateInitialize
	// fan-out the same daemon twice. Duplicate fan-outs both write to
	// the same InitSuccesses key, leaving one daemon session orphaned
	// (and the other un-tracked for cancellation / cleanup).
	type seenKey struct {
		Client string
		Ref    canonicalDaemonRef
	}
	seen := make(map[seenKey]bool)
	for _, m := range manifests {
		// Index daemons by name so we can resolve port via
		// ClientBinding.Daemon.
		daemonPort := make(map[string]int, len(m.Daemons))
		for _, d := range m.Daemons {
			daemonPort[d.Name] = d.Port
		}
		for _, b := range m.ClientBindings {
			if b.Client == "" || b.Daemon == "" {
				continue
			}
			port, ok := daemonPort[b.Daemon]
			if !ok {
				// Manifest validation should catch this at parse
				// time. Defensive skip rather than panic.
				continue
			}
			ref := canonicalDaemonRef{
				Server: m.Name,
				Daemon: b.Daemon,
				Port:   port,
			}
			key := seenKey{Client: b.Client, Ref: ref}
			if seen[key] {
				continue
			}
			seen[key] = true
			snap.Bindings[b.Client] = append(snap.Bindings[b.Client], ref)
		}
	}
	return snap
}

// BumpResolverOnManifestChange rebuilds + publishes a fresh snapshot
// from the supplied manifests. Convenience wrapper called by the
// install reconciler (Phase 5) and manifest-mutating callers.
func BumpResolverOnManifestChange(manifests []config.ServerManifest) {
	PublishResolverSnapshot(BuildResolverSnapshotFromManifests(manifests))
}
