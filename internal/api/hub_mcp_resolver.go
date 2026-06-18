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
//
// This is the groups-free form, preserved byte-identical for every
// existing caller: it delegates to
// BuildResolverSnapshotFromManifestsAndGroups with nil groups, which
// adds NO keys to Bindings — the client (bare-key) path is untouched.
func BuildResolverSnapshotFromManifests(manifests []config.ServerManifest) *ResolverSnapshot {
	return BuildResolverSnapshotFromManifestsAndGroups(manifests, nil)
}

// BuildResolverSnapshotFromManifestsAndGroups builds a fresh
// ResolverSnapshot carrying BOTH per-client bindings (bare-key, from each
// manifest's ClientBindings) AND per-group bindings (kind-namespaced
// "g:<group>" key, from the supplied groups). The dispatch path cannot
// tell the two apart — both are entries in the shared Bindings map keyed
// by an opaque scope string (groups/namespaces Phase 4a).
//
// Group resolution (groups/namespaces decision §"Config model"): each
// group names member SERVERS (Group.Servers, matching ServerManifest.Name);
// every daemon of each named server is added as a canonicalDaemonRef under
// the group's scope key. A group naming a server with NO live manifest /
// daemon degrades to a skipped binding — never a fault (mirrors the
// missing-daemon-port defensive skip in the client path). The (scopeKey,
// ref) dedupe spans BOTH sources so a server bound to two groups, or a
// repeated member, never fans the same daemon out twice.
//
// Kind-namespacing makes a group and a client of the same name produce
// DISJOINT keys ("g:frontend" vs "frontend"), so they can never collide
// in Bindings by construction (operator decision 2).
func BuildResolverSnapshotFromManifestsAndGroups(manifests []config.ServerManifest, groups []Group) *ResolverSnapshot {
	gen := resolverGen.Add(1)
	snap := &ResolverSnapshot{
		Gen:      gen,
		Bindings: make(map[string][]canonicalDaemonRef),
	}
	// codex bot r10 P2 closure on PR #157: dedupe (scopeKey, ref) pairs
	// so a manifest that accidentally repeats a ClientBinding row (no
	// uniqueness check in validation yet) cannot make AggregateInitialize
	// fan-out the same daemon twice. Duplicate fan-outs both write to
	// the same InitSuccesses key, leaving one daemon session orphaned
	// (and the other un-tracked for cancellation / cleanup). The same
	// dedupe now also covers the group path (a server named by two
	// groups, or twice by one group).
	type seenKey struct {
		ScopeKey string
		Ref      canonicalDaemonRef
	}
	seen := make(map[seenKey]bool)
	add := func(scopeKey string, ref canonicalDaemonRef) {
		key := seenKey{ScopeKey: scopeKey, Ref: ref}
		if seen[key] {
			return
		}
		seen[key] = true
		snap.Bindings[scopeKey] = append(snap.Bindings[scopeKey], ref)
	}

	// Per-server daemon index, reused by BOTH the client-binding path
	// (resolve ClientBinding.Daemon → port) and the group path (resolve
	// a member server → all its daemons).
	type serverDaemons struct {
		// portByDaemon resolves a daemon name to its port.
		portByDaemon map[string]int
		// refs are all canonicalDaemonRefs for this server, in manifest
		// daemon order — the group binding set for a member server.
		refs []canonicalDaemonRef
	}
	byServer := make(map[string]serverDaemons, len(manifests))
	for _, m := range manifests {
		sd := serverDaemons{
			portByDaemon: make(map[string]int, len(m.Daemons)),
			refs:         make([]canonicalDaemonRef, 0, len(m.Daemons)),
		}
		for _, d := range m.Daemons {
			sd.portByDaemon[d.Name] = d.Port
			sd.refs = append(sd.refs, canonicalDaemonRef{
				Server: m.Name,
				Daemon: d.Name,
				Port:   d.Port,
			})
		}
		byServer[m.Name] = sd
	}

	// Client path — UNCHANGED behavior: bare-<client> scope keys from
	// each manifest's ClientBindings. Resolve ClientBinding.Daemon → port
	// via the per-server index built above.
	for _, m := range manifests {
		sd := byServer[m.Name]
		for _, b := range m.ClientBindings {
			if b.Client == "" || b.Daemon == "" {
				continue
			}
			port, ok := sd.portByDaemon[b.Daemon]
			if !ok {
				// Manifest validation should catch this at parse
				// time. Defensive skip rather than panic.
				continue
			}
			add(b.Client, canonicalDaemonRef{
				Server: m.Name,
				Daemon: b.Daemon,
				Port:   port,
			})
		}
	}

	// Group path — ADDITIVE, kind-namespaced "g:<group>" scope keys. A
	// group binds every daemon of each member server. A member server
	// with no manifest (byServer miss) or no daemons degrades to a
	// skipped binding so a stale / bad group config never faults the
	// snapshot (decision claim 5).
	for _, g := range groups {
		if validateGroupName(g.Name) != nil {
			// LoadGroups rejects bad names at parse time; this guards a
			// programmatically-constructed group. A bad name must not
			// reach the scope keyspace.
			continue
		}
		scopeKey := GroupScopeKey(g.Name)
		for _, server := range g.Servers {
			sd, ok := byServer[server]
			if !ok {
				// Group names a server with no live manifest/daemon —
				// skip (validation warning surfaced in the GUI in a
				// later phase; never a hub-start fault here).
				continue
			}
			for _, ref := range sd.refs {
				add(scopeKey, ref)
			}
		}
	}

	return snap
}

// BumpResolverOnManifestChange rebuilds + publishes a fresh snapshot
// from the supplied manifests. Convenience wrapper called by the
// install reconciler (Phase 5) and manifest-mutating callers. Groups-free
// (nil groups) — see BumpResolverOnConfigChange for the groups-aware form.
func BumpResolverOnManifestChange(manifests []config.ServerManifest) {
	PublishResolverSnapshot(BuildResolverSnapshotFromManifests(manifests))
}

// BumpResolverOnConfigChange rebuilds + publishes a fresh snapshot from
// BOTH manifests (client bindings) AND groups (kind-namespaced group
// bindings). This is the groups-aware build+publish convenience wrapper
// the gate-ON hub-listener publish choke point uses (groups/namespaces
// Phase 4a). With nil/empty groups it is byte-equivalent to
// BumpResolverOnManifestChange.
func BumpResolverOnConfigChange(manifests []config.ServerManifest, groups []Group) {
	PublishResolverSnapshot(BuildResolverSnapshotFromManifestsAndGroups(manifests, groups))
}
