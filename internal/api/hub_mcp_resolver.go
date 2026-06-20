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
	"context"
	"fmt"
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

	// ToolsHidden is the FINE-GRAINED per-tool visibility filter for
	// group scope keys (groups/namespaces Phase 5a, operator decision 3).
	// Keyed scopeKey ("g:<group>") → server name → the raw (un-namespaced)
	// tool names hidden for that server within that group. A session whose
	// scope key has an entry drops those tools at the tools/list MERGE step
	// (assembleToolsListResponse) so they enter NEITHER the merged response
	// NOR the per-session RouteMap; a later tools/call for a hidden tool
	// hits the existing -32601 "Method not found" because the route was
	// never published.
	//
	// CRITICAL invariant: only GROUP scope keys ever appear here. A bare
	// client scope key has NO entry → nil filter → ZERO filtering, so the
	// /clients/ route stays byte-identical (the fence). Carried on the SAME
	// immutable snapshot as Bindings so a session captures a CONSISTENT
	// (bindings, filter) pair in one atomic pointer load — never a torn
	// read where the bindings are new but the filter is stale.
	//
	// nil when there are no groups (the groups-free build never allocates
	// it), preserving additive-by-omission.
	ToolsHidden map[string]map[string][]string

	// Groups is the set of DECLARED group scope keys ("g:<group>") from
	// groups.yaml, regardless of whether each group currently resolves to
	// any live daemon (Bindings). It is the authoritative "is this group
	// known" source the /g/ route's gate-2 (isKnownGroup) reads — NOT the
	// token table (a stale token row would otherwise keep a deleted group
	// "known"). A declared-but-empty group (no live member daemons → no
	// Bindings key) is still present here so it passes gate 2 and then
	// routes nothing (decision claim 5), while a deleted group drops out of
	// the next published snapshot and is immediately unknown.
	//
	// nil when there are no groups (additive-by-omission: the groups-free
	// build never allocates it, and a nil map's lookup is false → no group
	// is ever "known" on a host with no groups.yaml).
	Groups map[string]bool
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
		// Record the group as DECLARED (gate-2 known source) BEFORE the
		// servers loop, so a group with no resolvable member daemons is
		// still "known" (passes gate 2, routes nothing — decision claim 5).
		if snap.Groups == nil {
			snap.Groups = make(map[string]bool)
		}
		snap.Groups[scopeKey] = true
		for _, server := range g.Servers {
			// R4-1 (bot R4): a per-session server (scan.go marks it
			// CanMigrate=false — it MUST stay 1-per-local-client, never folded
			// into a shared hub route) must NOT contribute group bindings even
			// if a hand-edited groups.yaml names it. The RoutableServerNames
			// authoring gate already excludes these, so this is defense for a
			// config that bypassed the GUI. Skip with a structured warn so the
			// drop is observable (mirrors the byServer-miss skip below).
			if perSessionServers[server] {
				_ = LogHubMcpEvent("warn", "group-member-per-session-skipped", map[string]any{
					"group":  g.Name,
					"server": server,
				})
				continue
			}
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

		// Fold the per-tool visibility filter (groups/namespaces Phase 5a)
		// onto the SAME snapshot under the SAME scope key. Carried as a
		// defensive deep copy so a post-publish mutation of the source
		// Group cannot reach into the immutable snapshot. An empty / nil
		// ToolsHidden contributes NO entry, so a group with only `servers`
		// leaves snap.ToolsHidden untouched (Phase-4b-identical) and the
		// bare client path never gains a filter (additive-by-omission).
		if len(g.ToolsHidden) > 0 {
			if snap.ToolsHidden == nil {
				snap.ToolsHidden = make(map[string]map[string][]string)
			}
			filter := make(map[string][]string, len(g.ToolsHidden))
			for server, names := range g.ToolsHidden {
				if len(names) == 0 {
					continue
				}
				cp := make([]string, len(names))
				copy(cp, names)
				filter[server] = cp
			}
			if len(filter) > 0 {
				snap.ToolsHidden[scopeKey] = filter
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

// PublishGroupsSnapshotLocked is the LOCK-SERIALIZED groups-aware publish span
// (P3-2, opus-arch). It SCANS the manifests, reads groups.yaml, ensures the
// per-group token rows, and publishes the merged snapshot — all under ONE held
// hub-mcp.lock — so a concurrent GUI groups OR manifest mutation can never make
// this read an intermediate config and publish a torn / out-of-order topology.
//
// R4-3 (bot R4): the manifest scan is now run UNDER the held lock via the
// supplied `scan` closure rather than passed in pre-scanned from outside. Two
// concurrent manifest mutations each scan-then-publish; before this change each
// scanned OUTSIDE the lock, so a late-arriving stale scan could overwrite a
// newer one (publish ordering decoupled from scan ordering). Scanning under the
// lock makes scan+publish one atomic critical section: concurrent mutations
// serialize on the flock and each publishes the LATEST on-disk manifest set, so
// the last publish always reflects the last write — no stale clobber.
//
// CRITICAL (R4-3 deadlock invariant): the `scan` closure MUST NOT itself
// acquire hub-mcp.lock. The production closure (publishResolverSnapshotForHubBind's
// a.Scan() + a.ManifestGet/config.ParseManifest loop) reads client config,
// the workspace registry, process snapshots, and embed-first manifests — none
// of which touch hub-mcp.lock — so running it under the held lock is safe.
//
// R4-4 (bot R4): the flock is acquired via the CONTEXT-AWARE
// acquireHubMcpLockContext so a stuck sibling holder cannot freeze a caller
// (GUI startup / shutdown publish path) past its ctx budget; ctx cancellation
// unwinds the acquisition within ~10 ms (the retry cadence). A nil ctx falls
// through to the blocking acquire (acquireHubMcpLockContext handles nil).
//
// It is the exported entry point the gui-package publish choke point
// (publishResolverSnapshotForHubBind) calls; the unexported in-flock helpers
// (loadGroupsLocked, ensureHubTokensLocked) live in the api package and cannot
// be reached cross-package, so the whole locked span is owned here.
//
// Reentrancy: every helper it calls is the in-flock ("…Locked") half that does
// NOT re-acquire hub-mcp.lock, and BumpResolverOnConfigChange is a pure
// in-memory build + atomic publish (no lock). So holding the lock across all
// three never deadlocks. NONE of this helper's callers hold hub-mcp.lock when
// they call it (the gate-ON bind path runs AFTER BindHubMcpListener released
// its lock; the GUI mutation tail runs AFTER ReadModifyWriteGroups released
// its lock), so acquiring it here is always a fresh, non-nested acquisition.
//
// A scan error is fatal (returned without publishing) — the caller treats it as
// non-fatal to the bind/mutation and logs a warn, degrading to the prior
// snapshot. A groups.yaml load error degrades to "no groups" with a structured
// warn (additive-by-omission, decision claim 5) so the client bindings still
// publish — byte-identical client routing for a host with a missing/corrupt
// groups.yaml. A token-ensure failure is DEFERRED: the snapshot still publishes
// first (bindings live), then the token error is returned so the caller can
// surface restart_required (without the row the /g/ route cannot auth).
func PublishGroupsSnapshotLocked(ctx context.Context, scan func() ([]config.ServerManifest, error)) error {
	lk, err := acquireHubMcpLockContext(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = lk.Unlock() }()

	// R4-3: scan UNDER the held lock so scan+publish is one atomic critical
	// section. A scan failure aborts WITHOUT publishing (no torn snapshot from
	// a partial scan) and surfaces to the caller.
	manifests, serr := scan()
	if serr != nil {
		return fmt.Errorf("scan manifests under hub-mcp.lock: %w", serr)
	}

	cfg, gerr := loadGroupsLocked()
	if gerr != nil {
		_ = LogHubMcpEvent("warn", "groups-config-load-failed", map[string]any{
			"err": gerr.Error(),
		})
		cfg = GroupsConfig{}
	}

	// Capture the prior published active set only for live-session
	// preservation. It is NOT the durable orphan discriminator: a previous
	// resolver snapshot can be stale after a groups.yaml delete committed but
	// its non-fatal republish failed. The durable discriminator is the
	// group-token orphan tombstone written by ReadModifyWriteGroups when token
	// pruning fails; that marker is consumed below even on the first publish of
	// a process.
	prev := LoadResolverSnapshot()
	var activeGroupKeys map[string]bool
	if prev != nil {
		activeGroupKeys = prev.Groups
	}

	// Ensure a per-group token row for each "g:<group>" scope key UNDER the
	// SAME held lock (in-flock half — no re-acquire). Deferred so the snapshot
	// publishes even when the token-ensure fails.
	var tokenEnsureErr error
	if groupKeys := GroupScopeKeys(cfg.Groups); len(groupKeys) > 0 {
		// First, ROTATE any declared group whose token row is an ORPHAN — a
		// row left behind by a prior delete's failed prune (ErrTokenPruneFailed).
		// The durable tombstone wins over stale previous snapshots and nil
		// first-publish snapshots. For the original in-process delete/recreate
		// case, a declared row absent from the known prior active set also
		// rotates, while still-active rows are preserved so live /g/ sessions
		// survive. A rotation failure is fail-closed for this publish: serving
		// the re-created group with the old row would expose the stale secret
		// this path is specifically trying to retire.
		if _, rerr := rotateReusedGroupTokensLocked(groupKeys, activeGroupKeys, prev != nil); rerr != nil {
			_ = LogHubMcpEvent("warn", "group-tokens-rotate-orphan-failed", map[string]any{
				"err": rerr.Error(),
			})
			return fmt.Errorf("rotate orphaned group token rows: %w", rerr)
		}
		if _, terr := ensureHubTokensLocked(groupKeys); terr != nil {
			_ = LogHubMcpEvent("warn", "group-tokens-ensure-failed", map[string]any{
				"err": terr.Error(),
			})
			tokenEnsureErr = fmt.Errorf("ensure group token rows: %w", terr)
		} else if cerr := clearGroupTokenOrphansLocked(groupKeys); cerr != nil {
			_ = LogHubMcpEvent("warn", "group-token-orphan-clear-failed", map[string]any{
				"err": cerr.Error(),
			})
			tokenEnsureErr = fmt.Errorf("clear group-token orphan tombstones: %w", cerr)
		}
	}

	// In-memory build + atomic publish (no lock taken inside). Held under the
	// hub-mcp.lock so the scan + cfg read above and this publish are one
	// critical section — no interleaving mutation can publish a torn read.
	BumpResolverOnConfigChange(manifests, cfg.Groups)

	return tokenEnsureErr
}
