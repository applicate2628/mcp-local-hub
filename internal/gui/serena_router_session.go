// internal/gui/serena_router_session.go
//
// routerSessionStore: the authoritative registry of client sessions that
// were minted by a prior `initialize` AT THIS ROUTER, plus the protocol
// version each one negotiated.
//
// Why this exists (P2 findings 4 + 5 + 7): the router synthesizes
// `initialize` itself (serena_router_lifecycle.go) and mints a client
// Mcp-Session-Id, but until this store existed nothing recorded that the
// id was minted here, nor the version it negotiated. Three findings need
// that record:
//
//   - Finding 4 (tools/list): require a minted/known router session before
//     enumerating tools, so a client that skipped initialize cannot drive
//     handshakes/cache writes with a random id.
//   - Finding 5 (tools/list cache): key the cursorless cache by the
//     session's negotiated protocol version so a client that negotiated
//     2025-06-18 cannot be served a payload fetched under 2025-11-25.
//   - Finding 7 (tool-call): enforce + thread the session's negotiated
//     version (not the per-request header) into the upstream handshake.
//
// It is DISTINCT from both the sticky-routing sessionRouter
// (client-session -> workspace) and the daemonSessionStore
// (client-session -> upstream daemon session). Those map the session to a
// downstream target; this records that the session is legitimate and what
// it negotiated. Keeping it here (rather than extending the cross-package
// sessionRouter interface) follows the same rationale daemonSessionStore
// documents.
//
// Conventions mirror daemonSessionStore byte-for-byte: sync.Mutex,
// injectable clock, idle-TTL = daemonSessionTTL (24h), expire-on-read in
// lookup, an unbind. A successful lookup refreshes lastSeen so an active
// session never expires.
package gui

import (
	"container/list"
	"context"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

// maxRouterSessions caps the number of live router-minted session entries
// the store retains (P2, Codex PR #249). Without a cap every valid
// initialize allocated a routerSessionStore entry kept until DELETE or the
// 24h idle sweep, with NO bound: a buggy/looping client (or, on a shared
// host, a malicious local one) could loop initialize with supported
// protocol versions and grow the map unbounded long before the idle sweeper
// runs.
//
// At-capacity policy: LRU-EVICTION (drop the least-recently-seen entry to
// make room for the newest), mirroring the hub session store's GLOBAL-cap
// behavior (internal/api/hub_mcp_session.go: "At global cap, Create evicts
// the LRU session to make room"; defaultMaxSessionsGlobal = 256). The hub
// REJECTS only at its PER-CLIENT sub-cap (16) — a dimension this store does
// not have (it is keyed by session id alone, with no client-id grouping), so
// the global-cap eviction is the policy that applies here.
//
// Eviction (vs the hub's per-client reject) is the right choice for THIS
// store specifically because of the Phase-3 reconcile probe
// (internal/api/serena_client_reconcile.go): the probe sends initialize
// every reconcile cycle and NEVER DELETEs, so probe sessions accumulate
// until the idle sweep. With LRU-eviction this is self-limiting — the oldest
// probe entries evict first, and a legitimately-active client (whose entry
// is moved to the front of the LRU on every accepted request via touch) is
// never locked out by a churning initialize loop. Reject-at-capacity would
// instead let a frequent probe / churn loop fill the cap and start refusing
// legitimate initialize calls, so eviction is the probe-safe policy.
//
// The cap is GENEROUS (4096, vs the hub's 256 global) precisely so the probe
// + a realistic live-client population never approach it; the hub pairs its
// 256 global with a 16 per-client sub-cap as a backstop, and this store has
// no such second dimension, so a larger single global is the equivalent
// headroom. Bounded growth is the invariant; the exact value only sets where
// eviction begins.
const maxRouterSessions = 4096

// routerSessionBinding records a client session minted at the router: the
// protocol version it negotiated at initialize, plus lastSeen for idle
// expiry.
type routerSessionBinding struct {
	negotiatedVersion string
	lastSeen          time.Time
}

// routerSessionStore maps a router-minted client Mcp-Session-Id to the
// session's negotiated protocol version. handleInitialize populates it
// when it mints a session; the tools/list + tool-call paths read it; the
// DELETE teardown (and any future session-close) unbinds it.
//
// Bound (P2, Codex PR #249): the map is capped at maxRouterSessions with
// LRU-eviction (see maxRouterSessions). lru is a *list.List ordered
// most-recently-seen at the FRONT; lruIndex maps each session id to its list
// element so store/touch/remove are O(1). Both are protected by the SAME mu
// as bindings — never acquired separately — mirroring the hub session
// store's single-lock LRU discipline
// (internal/api/hub_mcp_session.go: "LRU index: a *list.List protected by
// store.mu (same Lock as sessions)").
//
// Concurrency: the map is shared across concurrent requests; every access
// holds mu.
//
// §3 fail-loud (workspace→session reverse index): wsByID + idsByWS map a
// session to the workspace it is routed to and back, so a backend-loss
// event for a dead daemon's workspace can enumerate EVERY router session
// bound to it and tear them all down (3-store teardown). The index is
// maintained under the SAME mu as bindings/lru so it never drifts: every
// removeLocked drops the reverse-index entry too, and bindWorkspace is the
// only writer. A session with no recorded workspace (initialized but never
// path-routed) simply has no reverse-index entry — it is not reachable by
// workspace enumeration, which is correct (it has no daemon to lose).
type routerSessionStore struct {
	mu       sync.Mutex
	bindings map[string]*routerSessionBinding
	lru      *list.List               // values are session-id strings; front == most-recently-seen
	lruIndex map[string]*list.Element // session id -> its lru element
	clock    func() time.Time         // injectable; nil -> time.Now

	// wsByID maps a session id -> the workspace key it is currently routed
	// to (its last bindWorkspace). idsByWS is the inverse: workspace key ->
	// the set of session ids routed to it. Both are kept in lockstep with
	// bindings under mu (removeLocked drops both directions).
	wsByID  map[string]string
	idsByWS map[string]map[string]struct{}
}

func (st *routerSessionStore) now() time.Time {
	if st.clock != nil {
		return st.clock()
	}
	return time.Now()
}

// ensureInitLocked lazily allocates the maps + LRU list. Caller MUST hold mu.
// The store is used zero-value (no constructor), so every mutating path that
// can be the first call routes through here.
func (st *routerSessionStore) ensureInitLocked() {
	if st.bindings == nil {
		st.bindings = make(map[string]*routerSessionBinding)
	}
	if st.lru == nil {
		st.lru = list.New()
	}
	if st.lruIndex == nil {
		st.lruIndex = make(map[string]*list.Element)
	}
	if st.wsByID == nil {
		st.wsByID = make(map[string]string)
	}
	if st.idsByWS == nil {
		st.idsByWS = make(map[string]map[string]struct{})
	}
}

// removeLocked deletes a session id from BOTH the bindings map and the LRU
// index, keeping the two in lockstep. Idempotent — a missing id is a no-op.
// Caller MUST hold mu. Every delete site (expire-on-read, unbind, sweep,
// eviction) routes through here so the LRU never drifts from the map.
func (st *routerSessionStore) removeLocked(clientSessionID string) {
	if _, ok := st.bindings[clientSessionID]; ok {
		delete(st.bindings, clientSessionID)
	}
	if el, ok := st.lruIndex[clientSessionID]; ok {
		if st.lru != nil {
			st.lru.Remove(el)
		}
		delete(st.lruIndex, clientSessionID)
	}
	// §3 fail-loud: drop the workspace reverse-index entry in lockstep so a
	// removed session can never be re-surfaced by workspace enumeration.
	st.removeWorkspaceIndexLocked(clientSessionID)
}

// removeWorkspaceIndexLocked drops clientSessionID from both directions of
// the workspace reverse index (wsByID + idsByWS), pruning an emptied
// per-workspace set so idsByWS does not accumulate empty maps. Idempotent —
// a missing id is a no-op. Caller MUST hold mu. It is split out of
// removeLocked so bindWorkspace can re-home a session (drop the OLD workspace
// edge before adding the new one) without touching bindings/lru.
func (st *routerSessionStore) removeWorkspaceIndexLocked(clientSessionID string) {
	wsKey, ok := st.wsByID[clientSessionID]
	if !ok {
		return
	}
	delete(st.wsByID, clientSessionID)
	if set, ok := st.idsByWS[wsKey]; ok {
		delete(set, clientSessionID)
		if len(set) == 0 {
			delete(st.idsByWS, wsKey)
		}
	}
}

// bindWorkspace records that clientSessionID is routed to wsKey, updating the
// reverse index (wsByID + idsByWS). It is the §3 fail-loud counterpart to the
// sticky deps.Sessions.BindSession + serenaDaemonSessions.store calls: the
// handler binds a router session to a workspace exactly when it establishes
// the daemon binding, so this is called alongside those so the index stays in
// step with where the session actually routes. A workspace switch on the same
// session re-homes the edge (drops the OLD workspace, adds the NEW one). An
// empty id or wsKey is ignored. It does NOT mint a bindings entry — only an
// already-known router session has a workspace edge worth tracking (a session
// not in bindings would be an orphan edge a sweep/unbind never cleans), so a
// missing bindings entry skips the index write.
func (st *routerSessionStore) bindWorkspace(clientSessionID, wsKey string) {
	if clientSessionID == "" || wsKey == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.ensureInitLocked()
	// Only index a session the store actually knows. A path-only/legacy caller
	// with no router-minted session has no bindings entry; indexing it would
	// leave an edge no expire/unbind path (which all route through removeLocked
	// on a known id) would ever clean.
	if _, known := st.bindings[clientSessionID]; !known {
		return
	}
	// Re-home: drop any prior workspace edge for this id before adding the new
	// one, so a workspace switch does not leave the session listed under both.
	if prior, ok := st.wsByID[clientSessionID]; ok {
		if prior == wsKey {
			return
		}
		st.removeWorkspaceIndexLocked(clientSessionID)
	}
	st.wsByID[clientSessionID] = wsKey
	set := st.idsByWS[wsKey]
	if set == nil {
		set = make(map[string]struct{})
		st.idsByWS[wsKey] = set
	}
	set[clientSessionID] = struct{}{}
}

// sessionsForWorkspace returns a snapshot of every router session id currently
// routed to wsKey (the §3 fail-loud enumerate-by-workspace primitive). The
// returned slice is a copy taken under mu, so the caller can iterate + tear
// down each id without holding the store lock (the per-id 3-store unbind
// touches OTHER stores, so it must run lock-free — same lock-ordering rule
// coordinateExpiredRouterSessionUnbind documents). Returns nil when no session
// is routed to wsKey.
func (st *routerSessionStore) sessionsForWorkspace(wsKey string) []string {
	if wsKey == "" {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	set, ok := st.idsByWS[wsKey]
	if !ok || len(set) == 0 {
		return nil
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids
}

// knownWorkspaceKeys returns a snapshot of every workspace key that has at
// least one router session routed to it (the keys of idsByWS). The IPC
// reconcile fallback uses it to scope its backend-loss check to exactly the
// serena workspaces the router actually has live sessions for — so it does not
// need to classify IPC status rows by backend, and does nothing when the
// router has no sessions. Returns nil when no workspace has a routed session.
func (st *routerSessionStore) knownWorkspaceKeys() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.idsByWS) == 0 {
		return nil
	}
	keys := make([]string, 0, len(st.idsByWS))
	for wsKey := range st.idsByWS {
		keys = append(keys, wsKey)
	}
	return keys
}

// evictLRULocked drops the least-recently-seen entry (the LRU list BACK) to
// free room for a new store at capacity and returns the evicted client session
// id (or "" when the list is empty / nothing was evicted). Caller MUST hold mu.
// Mirrors the hub's evictLRULocked (internal/api/hub_mcp_session.go) — but the
// router store has no in-flight concept, so there is no skip-if-busy walk: the
// eldest entry is always evictable.
//
// Finding 2 (P2, Codex PR #249 round-2): the evicted id is RETURNED rather than
// silently dropped so the CALLER (handleInitialize, which holds the *Server/deps)
// can coordinate the downstream sticky + daemon unbind for the evicted session
// AFTER the store lock is released. coordinateExpiredRouterSessionUnbind touches
// OTHER stores, so it must NOT run under routerSessionStore.mu (lock-ordering /
// deadlock risk) — the id is carried out instead.
func (st *routerSessionStore) evictLRULocked() string {
	if st.lru == nil {
		return ""
	}
	back := st.lru.Back()
	if back == nil {
		return ""
	}
	id, _ := back.Value.(string)
	st.removeLocked(id)
	return id
}

// store records (clientSessionID -> negotiatedVersion), replacing any
// prior binding for the same client session. An empty client session id
// is ignored so the map never holds a keyless entry.
//
// Bound (P2, Codex PR #249): a fresh insert that would exceed
// maxRouterSessions first EVICTS the least-recently-seen entry, so the map
// size never grows past the cap (LRU-eviction — see maxRouterSessions). The
// new/refreshed entry is promoted to the FRONT of the LRU so it is the LAST
// to be evicted. Re-storing an existing id (a re-bind on the same session)
// updates its version + moves it to the front WITHOUT consuming a cap slot,
// so it can never trigger an eviction.
//
// Finding 2 (P2, Codex PR #249 round-2): when a fresh insert evicts the eldest
// entry, the evicted client session id is RETURNED (evictedID != "") so the
// caller can coordinate the downstream sticky + daemon unbind for it. Without
// this, an evicted router session whose sticky/daemon bindings survive would get
// later pathless calls classified as routerSessionAbsent and routed as LEGACY,
// bypassing the negotiated-version checks + coordinated expiry router sessions
// get (the same reanimation class the ticker sweep + on-read expiry close). The
// caller (handleInitialize) invokes coordinateExpiredRouterSessionUnbind on the
// returned id AFTER this returns and the store lock is released — that coordinator
// touches OTHER stores, so calling it under st.mu here would risk a lock-ordering
// deadlock. evictedID is "" on the no-eviction path (re-store of an existing id,
// or a fresh insert below the cap).
func (st *routerSessionStore) store(clientSessionID, negotiatedVersion string) (evictedID string) {
	if clientSessionID == "" {
		return ""
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.ensureInitLocked()
	if el, ok := st.lruIndex[clientSessionID]; ok {
		// Existing id: update in place + promote. No cap pressure (the entry
		// already occupies its slot).
		st.bindings[clientSessionID] = &routerSessionBinding{
			negotiatedVersion: negotiatedVersion,
			lastSeen:          st.now(),
		}
		st.lru.MoveToFront(el)
		return ""
	}
	// New id: enforce the cap by evicting the eldest entry BEFORE inserting,
	// so the post-insert size is <= maxRouterSessions. Capture the evicted id so
	// the caller can coordinate its downstream unbind (Finding 2). A self-eviction
	// (the eldest id happening to equal clientSessionID) cannot occur here: this
	// branch only runs when clientSessionID is NOT already in lruIndex, so it is
	// not an LRU entry and cannot be the eviction victim.
	if len(st.bindings) >= maxRouterSessions {
		evictedID = st.evictLRULocked()
	}
	st.bindings[clientSessionID] = &routerSessionBinding{
		negotiatedVersion: negotiatedVersion,
		lastSeen:          st.now(),
	}
	st.lruIndex[clientSessionID] = st.lru.PushFront(clientSessionID)
	return evictedID
}

// peekNegotiatedVersion returns the protocol version a client session
// negotiated at initialize WITHOUT refreshing lastSeen. ok=false means the
// id was never minted at this router (or its binding idle-expired).
//
// Finding 4 (non-touching peek for PRE-gate validation): the tool-call /
// tools-list / DELETE / cancellation paths read the negotiated version to
// VALIDATE the request's MCP-Protocol-Version BEFORE the request is accepted.
// If that read refreshed lastSeen (as the original negotiatedVersion did), a
// request REJECTED for a version mismatch would still keep the session alive —
// a buggy/hostile client could spam invalid mismatched requests to make an
// otherwise-idle initialized session un-reclaimable by the sweeper. So the
// pre-gate validation uses this peek; lastSeen is refreshed only AFTER the
// gate passes, via touch (mirrors the hub splitting GetAndTouch back into
// Get + post-gate Touch, internal/api/hub_mcp_handler.go:395-407).
//
// Expire-on-read (mirrors daemonSessionStore.lookup, P2 finding 3) is
// PRESERVED here: a binding idle longer than daemonSessionTTL is treated as a
// miss AND deleted, so an already-expired binding is still a miss even though
// peek does not refresh a live binding. This is self-contained — it does not
// depend on an external cleanup ticker.
func (st *routerSessionStore) peekNegotiatedVersion(clientSessionID string) (string, bool) {
	version, state := st.peekVersionState(clientSessionID)
	return version, state == routerSessionLive
}

// routerSessionState is the tri-state a pathless tool call needs to tell an
// EXPIRED-and-just-deleted router session apart from one that was NEVER minted
// here (Round-13 / consultant finding — the pathless reanimation close). The
// boolean peekNegotiatedVersion returns collapses both "absent" cases into
// ok=false, which is enough for the version-validation sites (a missing session
// just means "no negotiated version to validate against") but NOT enough for
// the pathless branch: there, an EXPIRED router session must terminate the call
// and unbind the sticky+daemon bindings (no reanimation), while a TRUE-legacy
// caller (never minted here) must keep routing via its sticky binding.
type routerSessionState int

const (
	// routerSessionLive: the id was minted here and is within its idle TTL.
	routerSessionLive routerSessionState = iota
	// routerSessionExpired: the id WAS minted here but has aged past the idle
	// TTL; peekVersionState deleted the entry on this read (expire-on-read).
	routerSessionExpired
	// routerSessionAbsent: no entry for the id (never minted at this router).
	routerSessionAbsent
)

// peekVersionState is the tri-state form of peekNegotiatedVersion: it reports
// whether the id is live / just-expired-and-deleted / absent, WITHOUT
// refreshing lastSeen. It owns the single expire-on-read + delete decision that
// peekNegotiatedVersion delegates to (one owner — no duplicated TTL logic).
//
// Round-13 / consultant finding: the pathless branch calls THIS (not the
// boolean peek) so it can distinguish routerSessionExpired (a router session
// that idle-expired on read — must NOT reanimate via the sticky binding) from
// routerSessionAbsent (a true-legacy caller — keep sticky routing). An
// already-expired entry is deleted here exactly as peekNegotiatedVersion did,
// so the on-read expiry semantics are unchanged for every existing caller.
func (st *routerSessionStore) peekVersionState(clientSessionID string) (string, routerSessionState) {
	if clientSessionID == "" {
		return "", routerSessionAbsent
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	b, ok := st.bindings[clientSessionID]
	if !ok || b == nil {
		return "", routerSessionAbsent
	}
	if st.now().Sub(b.lastSeen) > daemonSessionTTL {
		// Expire-on-read: drop from BOTH the map and the LRU index so the cap
		// accounting stays exact (P2 — removeLocked keeps the two in lockstep).
		st.removeLocked(clientSessionID)
		return "", routerSessionExpired
	}
	return b.negotiatedVersion, routerSessionLive
}

// touch refreshes lastSeen for a live (non-idle-expired) binding so an
// ACCEPTED request keeps its session reclaimable-only-after-idle. It is the
// post-gate companion to peekNegotiatedVersion (Finding 4): the validation
// path peeks (no refresh) so a REJECTED request never extends the session,
// and only a request that PASSES the gate calls touch. It is expire-on-read
// like peek — an already-expired binding is dropped, not resurrected (so a
// touch arriving after the idle window does not revive a dead session).
//
// Round-10 (Finding 1 — report liveness): touch returns whether it actually
// refreshed a LIVE (present, non-idle-expired) binding. It is the router's
// equivalent of the hub's post-gate Touch returning bool
// (internal/api/hub_mcp_handler.go:402-409): a request that PEEKED a valid
// session pre-gate can be swept by the cleanup ticker / a client DELETE
// BEFORE the accepted-path touch runs. Returning false on a missing/expired
// binding lets the accepted-path call site ABORT (return a "session
// terminated" -32600) instead of proceeding to proxy upstream or RECREATE
// daemon/sticky bindings for a session that no longer exists — which would
// defeat immediate-revocation + idle-sweep. false on unknown/expired/empty;
// true only when a live binding was refreshed.
func (st *routerSessionStore) touch(clientSessionID string) bool {
	if clientSessionID == "" {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	b, ok := st.bindings[clientSessionID]
	if !ok || b == nil {
		return false
	}
	if st.now().Sub(b.lastSeen) > daemonSessionTTL {
		// Expire-on-read: drop from BOTH the map and the LRU index (P2).
		st.removeLocked(clientSessionID)
		return false
	}
	b.lastSeen = st.now()
	// P2: an accepted request promotes the session to the FRONT of the LRU so
	// an actively-used session is the LAST to be evicted at capacity.
	if el, ok := st.lruIndex[clientSessionID]; ok && st.lru != nil {
		st.lru.MoveToFront(el)
	}
	return true
}

// negotiatedVersion returns the protocol version a client session negotiated
// at initialize, refreshing lastSeen on a hit (peek + touch in one). It is
// retained for callers that legitimately combine the read with a refresh.
//
// NOTE: the four PRE-gate version-validation sites (tool-call, tools/list,
// DELETE, cancellation) MUST NOT use this — they use peekNegotiatedVersion so
// a rejected request never refreshes lastSeen (Finding 4), then touch on the
// accepted path.
func (st *routerSessionStore) negotiatedVersion(clientSessionID string) (string, bool) {
	v, ok := st.peekNegotiatedVersion(clientSessionID)
	if ok {
		st.touch(clientSessionID)
	}
	return v, ok
}

// known reports whether clientSessionID was minted at this router and is
// not idle-expired, WITHOUT refreshing lastSeen (it peeks). An existence
// probe must not extend the session's idle life (Finding 4).
func (st *routerSessionStore) known(clientSessionID string) bool {
	_, ok := st.peekNegotiatedVersion(clientSessionID)
	return ok
}

// unbind drops the mapping for clientSessionID (no-op if absent). Called
// by the DELETE teardown alongside the daemon + sticky unbinds.
func (st *routerSessionStore) unbind(clientSessionID string) {
	if clientSessionID == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	// P2: removeLocked drops from BOTH the map and the LRU index in lockstep.
	st.removeLocked(clientSessionID)
}

// cleanup drops bindings idle longer than ttl before now and returns the
// count dropped. Bounded growth is already tied to live client sessions;
// cleanup exists for symmetry with daemonSessionStore.cleanup and an
// optional periodic sweep. It is a thin count-only wrapper over cleanupExpired.
func (st *routerSessionStore) cleanup(now time.Time, ttl time.Duration) int {
	return len(st.cleanupExpired(now, ttl))
}

// cleanupExpired drops bindings idle longer than ttl before now and returns
// the IDs it reclaimed (Finding 3). SweepSerenaSessions needs the concrete ids
// so it can coordinate the other two router-owned stores — an expired router
// session must be unbound from serenaDaemonSessions AND the sticky
// deps.Sessions so a swept router session is fully terminated everywhere and
// can never keep routing a path-less tool call via a not-yet-swept sticky
// binding (the desync this finding closes). Returns nil when nothing expired.
func (st *routerSessionStore) cleanupExpired(now time.Time, ttl time.Duration) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	cutoff := now.Add(-ttl)
	var expired []string
	for id, b := range st.bindings {
		if b == nil || !b.lastSeen.After(cutoff) {
			// P2: removeLocked drops from BOTH the map and the LRU index.
			// Deleting the current key from the map being ranged is safe in Go;
			// removeLocked only also touches the sibling LRU list/index.
			st.removeLocked(id)
			expired = append(expired, id)
		}
	}
	return expired
}

// SweepSerenaSessions drops router-owned serena session bindings idle
// longer than ttl before now from BOTH router-owned session stores, and
// returns the total count dropped (Finding 2). It is the periodic-sweep
// entry point the GUI lifecycle wires into its existing session-cleanup
// ticker (internal/cli.runSessionCleanupTicker).
//
// Why this is needed even though both stores already expire-on-read: the
// expire-on-read path in routerSessionStore.negotiatedVersion /
// daemonSessionStore.lookup only fires when a session id is reused or
// DELETEd. A client (or the Phase-3 reconcile probe, which initializes
// every reconcile cycle and never DELETEs) that initializes then
// disconnects leaves its entry untouched forever — unbounded growth on a
// long-running GUI. This sweep is the only thing that reclaims those
// orphaned entries. It reuses the EXISTING ticker goroutine (no new
// background goroutine) alongside the cross-package sticky-routing
// SessionRouter sweep, so all three serena session stores age on the same
// idle clock with one correctly-shutdown ticker. ttl is the shared
// idle-TTL the caller already passes the sticky sweep
// (serena_routing.DefaultSessionTTL == daemonSessionTTL == 24h).
//
// Finding 3 (coordinated expiry — no tombstones): when this reclaims an
// expired routerSessionStore entry, it ALSO unbinds that session id from the
// sticky deps.Sessions (UnbindSession) and serenaDaemonSessions, so a swept
// router session is fully terminated everywhere. Without this, a path-less tool
// call whose router session idle-expired (peeked + dropped on read) but whose
// sticky binding had NOT yet been swept (the two stores aged independently)
// would refresh the sticky binding and route as a LEGACY session — re-handshake
// a daemon and keep routing until the next sticky sweep, defeating the
// expire-on-read/session-terminated guard. Coordinating the three stores makes
// the legacy-vs-expired distinction correct: a TRUE legacy caller never had a
// router session (never in routerSessionStore, never swept from it, sticky
// binding untouched → still routes); an EXPIRED router session is swept from
// all three together → its sticky binding is gone → the path-less call finds
// nothing and is treated as unknown (not routed).
//
// Round-13 (consultant finding — the ON-READ pathless reanimation is now
// CLOSED too): the prior residual was that a router session idle-expiring on a
// pathless read (peekNegotiatedVersion deletes the entry the moment a request
// observes it past-TTL) would, in the window before the next sweep, refresh the
// still-present sticky binding via LookupSession and keep routing as a "legacy"
// caller indefinitely. The pathless branch in serenaRouterHandler now peeks the
// tri-state (peekVersionState) BEFORE the sticky refresh and, on
// routerSessionExpired, runs the SAME per-id coordinated unbind this sweep does
// (coordinateExpiredRouterSessionUnbind) and aborts -32600 "session terminated"
// — so an expired router session can no longer reanimate via a pathless call,
// whether the expiry is observed by this ticker OR on the read itself.
//
// Residual (unchanged, documented, no tombstone machinery): the orphaned
// UPSTREAM daemon session (the serena daemon's own session, distinct from the
// router-local serenaDaemonSessions binding which IS unbound) ages out on the
// daemon's own idle clock rather than being eagerly DELETEd here — the same
// TTL-reclaim posture round-9 takes for a displaced workspace-switch session.
// This avoids an upstream DELETE on the sweep / on-read paths (the named
// referent coordinates LOCAL stores only) and is bounded by the daemon's idle
// TTL.
func (s *Server) SweepSerenaSessions(now time.Time, ttl time.Duration) int {
	// Reclaim expired router sessions first and capture their ids so the other
	// two stores can be coordinated (Finding 3).
	expiredRouter := s.serenaRouterSessions.cleanupExpired(now, ttl)

	// Coordinate: every expired router session id is unbound from the daemon
	// store and the sticky session router so the session is terminated
	// everywhere. Unbinding an id that has no binding in either store is a
	// no-op, so a router session with no downstream bindings is handled cleanly.
	var sticky sessionRouter
	if deps := s.serenaRouterDepsProd(); deps != nil {
		sticky = deps.Sessions
	}
	for _, id := range expiredRouter {
		s.coordinateExpiredRouterSessionUnbind(id, sticky)
	}

	// Then sweep the daemon store's OWN idle entries (those not already removed
	// by the coordination above). The total dropped count is the expired router
	// sessions plus the daemon store's independently-idle bindings.
	daemonDropped := s.serenaDaemonSessions.cleanup(now, ttl)
	return len(expiredRouter) + daemonDropped
}

// coordinateExpiredRouterSessionUnbind terminates an EXPIRED router session
// everywhere its downstream bindings live: the router-local daemon-session
// store and the sticky cross-package sessionRouter. The router-session store
// entry itself is assumed already removed by the caller (cleanupExpired in the
// ticker sweep, or the expire-on-read delete in peekVersionState on the
// pathless path) — this is the per-id coordination of the OTHER two stores.
//
// Unbinding an id absent from a store is a no-op, so a router session with no
// downstream bindings is handled cleanly. sticky may be nil (no production
// sessionRouter wired) — only the daemon store is then unbound. Extracted so
// the ticker SweepSerenaSessions and the on-read pathless-expiry close
// (serenaRouterHandler) share ONE owner for the coordinated-unbind decision
// rather than duplicating it (Round-13).
func (s *Server) coordinateExpiredRouterSessionUnbind(id string, sticky sessionRouter) {
	s.serenaDaemonSessions.unbind(id)
	if sticky != nil {
		sticky.UnbindSession(id)
	}
}

// coordinateBackendLossUnbind is the §3 fail-loud 3-STORE teardown for a
// single session id on backend loss. Unlike coordinateExpiredRouterSessionUnbind
// (which assumes the caller already removed the routerSessionStore entry via
// expire-on-read), backend loss has NO such caller — the session is still LIVE
// in routerSessionStore, which is exactly why /serena/mcp keeps returning HTTP
// 200 on a dead backend (the 2026-06-10 zombie). So this removes the
// routerSessionStore entry FIRST, then the daemon-session store and the sticky
// router — the session is gone from ALL THREE.
//
// Concurrency: each store owns its own lock; this acquires them one at a time
// (never nested), so there is no lock-ordering hazard. routerSessionStore.unbind
// routes through removeLocked, which also drops the workspace reverse-index
// entry, so a re-enumeration of the same workspace will not re-surface this id.
// Unbinding an id absent from a store is a no-op, so a session with no
// downstream binding is handled cleanly. sticky may be nil (no production
// sessionRouter wired) — only the two router-local stores are then unbound.
func (s *Server) coordinateBackendLossUnbind(id string, sticky sessionRouter) {
	// Router store FIRST — this is the store that returns 200 on a dead
	// backend, so dropping it is what makes the next request fail loud.
	s.serenaRouterSessions.unbind(id)
	s.serenaDaemonSessions.unbind(id)
	if sticky != nil {
		sticky.UnbindSession(id)
	}
}

// terminateSerenaSessionsForWorkspace is the §3 fail-loud backend-loss entry
// point: it enumerates EVERY router session bound to wsKey (via the
// workspace→session reverse index) and tears each out of all three session
// stores. After this returns, the next /serena/mcp request on any of those
// sessions finds no router-session entry → the existing -32600 "session
// terminated" / 503 missing_session path fires → the client re-initializes,
// instead of a zombie 200 forwarded to a dead daemon.
//
// It is the common owner shared by the always-on forward-failure floor (the
// in-process trigger) and the IPC pid_generation reconcile fallback, so both
// backend-loss signals run identical teardown. Returns the number of sessions
// torn down (0 when no session was routed to wsKey). The enumeration snapshot
// is taken under the router store lock; each per-id unbind runs lock-free
// (coordinateBackendLossUnbind touches OTHER stores), the same lock-ordering
// discipline the sweep follows.
func (s *Server) terminateSerenaSessionsForWorkspace(wsKey string) int {
	if wsKey == "" {
		return 0
	}
	ids := s.serenaRouterSessions.sessionsForWorkspace(wsKey)
	if len(ids) == 0 {
		return 0
	}
	var sticky sessionRouter
	if deps := s.serenaRouterDepsProd(); deps != nil {
		sticky = deps.Sessions
	}
	for _, id := range ids {
		s.coordinateBackendLossUnbind(id, sticky)
	}
	return len(ids)
}

func (s *Server) seedSerenaBackendPIDBaseline(ctx context.Context, ws *api.WorkspaceEntry, replaceStaleBaseline bool) {
	if ws == nil || ws.WorkspacePath == "" {
		return
	}
	statusFn := serenaBackendStatusFn
	if statusFn == nil {
		return
	}
	rows, err := statusFn(ctx)
	if err != nil {
		return
	}
	pid := 0
	for _, row := range rows {
		if row.Workspace != ws.WorkspacePath {
			continue
		}
		if row.PID > 0 {
			pid = row.PID
		} else if row.StalePID > 0 {
			pid = row.StalePID
		}
		break
	}
	if pid <= 0 {
		return
	}
	s.serenaBackendPIDMu.Lock()
	if s.serenaBackendLastPID == nil {
		s.serenaBackendLastPID = map[string]int{}
	}
	if _, exists := s.serenaBackendLastPID[ws.WorkspacePath]; !exists || replaceStaleBaseline {
		s.serenaBackendLastPID[ws.WorkspacePath] = pid
	}
	s.serenaBackendPIDMu.Unlock()
}

// handleSerenaBackendLossOnForwardFailure is the ALWAYS-ON FLOOR of the §3.x
// backend-loss trigger (§12 Phase 1). The serena router calls it from the
// in-process forward-failure sites — a tool-call forward OR the upstream
// handshake getting a CONNECTION error (dial refused / dead backend), as
// opposed to a timeout on a slow-but-live daemon. It tears every session
// bound to the dead daemon's workspace out of all three session stores so the
// NEXT /serena/mcp request returns the existing -32600 "session terminated" /
// 503 missing_session and the client re-initializes, instead of the
// 2026-06-10 zombie (a live routerSessionStore entry keeping /serena/mcp at
// HTTP 200 on a dead backend).
//
// failedSessionID is the session that hit the dead forward; it is torn down
// directly too even if its workspace edge was not yet recorded (e.g. a
// sessionless one-shot, or a race where the bindWorkspace had not run) — the
// per-id unbind is a cheap no-op when there is nothing to drop. The audit is
// best-effort (a nil/erroring auditFn is tolerated) and never blocks teardown.
//
// TODO(§3.x signal #1 — PREFERRED upgrade, not required here): subscribe the
// router to a cross-process supervisor child-exit → GUI `daemon-failed` event
// (supervisor_state_machine.go EvChildExit → controller crashCh →
// internal/gui/events.go SSE bus) so a daemon death is observed even when NO
// client request is in flight to surface it. The GUI event bus lacks a
// `daemon-failed` event today (§11.9), so this floor + the IPC pid_generation
// reconcile fallback (reconcileSerenaBackendLossViaIPC) ship first; signal #1
// is a later upgrade once the event is added.
func (s *Server) handleSerenaBackendLossOnForwardFailure(wsKey, failedSessionID string, cause error, auditFn func(level, event string, fields map[string]any) error) {
	var sticky sessionRouter
	if deps := s.serenaRouterDepsProd(); deps != nil {
		sticky = deps.Sessions
	}
	// Tear down the failed session directly first (covers a session whose
	// workspace edge was not yet indexed, incl. the sessionless one-shot case
	// where failedSessionID is "" — coordinateBackendLossUnbind no-ops an empty
	// id). Then enumerate-and-tear-down every OTHER session bound to wsKey.
	if failedSessionID != "" {
		s.coordinateBackendLossUnbind(failedSessionID, sticky)
	}
	n := s.terminateSerenaSessionsForWorkspace(wsKey)
	if auditFn != nil {
		errStr := ""
		if cause != nil {
			errStr = cause.Error()
		}
		_ = auditFn("warn", "serena-backend-loss-session-teardown", map[string]any{
			"workspace_key":     wsKey,
			"sessions_torndown": n,
			"trigger":           "forward-failure",
			"err":               errStr,
		})
	}
}

// serenaBackendStatusFn is the seam for the IPC reconcile fallback's status
// read. Production wires it (via SetSerenaBackendStatusFn at GUI boot) to
// api.DialSupervisorIPCStatus so the gui package does not hard-depend on the
// IPC status implementation; tests inject a fake. A nil seam (no supervisor IPC
// wired) makes ReconcileSerenaBackendLossViaIPC a no-op.
var serenaBackendStatusFn func(ctx context.Context) ([]api.DaemonStatus, error)

const serenaBackendPostIdleGraceTicks = 2

// SetSerenaBackendStatusFn wires the IPC status reader the §3.x backend-loss
// reconcile fallback uses. CLI boot (internal/cli/gui.go) calls it with
// api.DialSupervisorIPCStatus. Passing nil disables the fallback (the
// always-on forward-failure floor still covers real backend loss).
func SetSerenaBackendStatusFn(fn func(ctx context.Context) ([]api.DaemonStatus, error)) {
	serenaBackendStatusFn = fn
}

// ReconcileSerenaBackendLossViaIPC is the §3.x backend-loss FALLBACK signal
// (signal #2 — IPC status reconciliation). It is the safety net behind the
// always-on forward-failure floor: a serena daemon can restart (advancing its
// PID) or vanish WITHOUT a client request being in flight to surface the dead
// forward; this catches that on a reconcile tick. The §3.x spec frames it as
// "pid_generation advanced (restart) or the daemon absent" — the supervisor
// IPC status payload (internal/api.DaemonStatus) does NOT carry pid_generation,
// so the code-true equivalent is the per-workspace CurrentPID: a workspace
// whose PID changed vs the previous tick (a restart) OR that disappeared from
// the status list (a stop/death) is a backend-loss signal. (DEVIATION from the
// spec's literal pid_generation wording, re-confirmed against
// internal/api/supervisor_ipc_status_client.go — PID is the surfaced restart
// witness.)
//
// It is scoped to ONLY the workspaces the router currently has live sessions
// for (routerSessionStore.knownWorkspaceKeys), so it does nothing when the
// router is idle and never has to classify IPC rows by backend. For each such
// workspace it maps WorkspaceKey -> WorkspacePath (the IPC status keys
// workspace by PATH — supervisor_intent_build.go sets Workspace = WorkspacePath)
// via the router's resolver, compares the fresh CurrentPID to the previous
// tick's snapshot, and on a restart/disappearance tears down that workspace's
// router sessions (terminateSerenaSessionsForWorkspace). A workspace seen for
// the FIRST time (no prior snapshot) is NOT treated as a loss — only a change
// from a known prior PID is. Returns the number of sessions torn down across
// all workspaces this tick.
func (s *Server) ReconcileSerenaBackendLossViaIPC(ctx context.Context) int {
	knownKeys := s.serenaRouterSessions.knownWorkspaceKeys()
	if len(knownKeys) == 0 {
		return 0
	}
	statusFn := serenaBackendStatusFn
	if statusFn == nil {
		return 0
	}
	deps := s.serenaRouterDepsProd()
	if deps == nil || deps.Resolver == nil {
		return 0
	}
	lister, ok := deps.Resolver.(workspaceLister)
	if !ok {
		return 0
	}

	// Map the router's known workspace KEYS -> their PATHS (the IPC status key).
	pathToKey := make(map[string]string, len(knownKeys))
	pathToTaskName := make(map[string]string, len(knownKeys))
	wantPaths := make(map[string]struct{}, len(knownKeys))
	knownSet := make(map[string]struct{}, len(knownKeys))
	for _, k := range knownKeys {
		knownSet[k] = struct{}{}
	}
	for _, ws := range lister.ListWorkspaces() {
		if ws == nil {
			continue
		}
		if _, want := knownSet[ws.WorkspaceKey]; !want {
			continue
		}
		pathToKey[ws.WorkspacePath] = ws.WorkspaceKey
		pathToTaskName[ws.WorkspacePath] = ws.TaskName
		wantPaths[ws.WorkspacePath] = struct{}{}
	}
	if len(wantPaths) == 0 {
		return 0
	}

	rows, err := statusFn(ctx)
	if err != nil {
		// IPC unavailable / transient: do NOT tear down sessions on a status
		// READ failure (that would be a false positive — the daemons may be
		// fine and only the supervisor IPC momentarily unreachable). The
		// always-on forward-failure floor still covers real backend loss; this
		// fallback simply skips this tick. The snapshot is left UNCHANGED so the
		// next successful tick compares against the last known-good PIDs.
		return 0
	}

	// Build the fresh per-workspace-PATH PID snapshot, restricted to the
	// workspaces the router cares about.
	now := time.Now().UTC()
	fresh := make(map[string]int, len(wantPaths))
	deadBackend := make(map[string]bool, len(wantPaths))
	idleStopped := make(map[string]bool, len(wantPaths))
	// restarting marks paths whose daemon is in the supervisor's port-stale
	// terminate-restart window (state "Restarting"): the IPC status moves the
	// real PID to stale_pid and reports current_pid=0 while the daemon ROW
	// stays present in the list (internal/cli/supervise_status.go). A transient
	// current_pid=0 is NOT a confirmed new daemon generation — the same PID may
	// recover, or a genuinely new PID may appear — so it must not be classified
	// as a backend loss, and a 0 must not be persisted as a PID baseline (that
	// would make the eventual real PID look like a fresh change next tick →
	// spurious double-teardown of a just-re-established healthy session).
	restarting := make(map[string]bool, len(wantPaths))
	for _, row := range rows {
		if row.Workspace == "" {
			continue
		}
		if _, want := wantPaths[row.Workspace]; !want {
			continue
		}
		// Last writer wins if the status somehow lists a workspace twice; the
		// supervisor emits one row per daemon so this is the daemon's PID.
		fresh[row.Workspace] = row.PID
		taskName := row.TaskName
		if taskName == "" {
			taskName = pathToTaskName[row.Workspace]
		} else {
			pathToTaskName[row.Workspace] = taskName
		}
		// A port-stale Restarting daemon: the daemon is ALIVE but its port
		// ownership could not be reverified, so the supervisor parks the real
		// PID in StalePID, reports current_pid=0, sets state "Restarting", and
		// keeps the row present (internal/cli/supervise_status.go:73-88 — set
		// ONLY inside the `state==running && !live` branch). StalePID!=0 is the
		// UNIQUE witness of that benign alive-but-port-stale window: it is the
		// only signal that a real PID is parked behind the transient 0.
		//
		// Do NOT widen this to `row.PID==0 || row.State=="Restarting"`. Those
		// disjuncts ALSO match genuinely-dead daemons the §3 IPC floor MUST tear
		// down, and StalePID stays 0 for all of them:
		//   - a CRASHED daemon (supervisor state "backoff"/"spawning" ->
		//     supervisorStatusGUIState maps to "Restarting", CurrentPID=0,
		//     StalePID never set) -> row {PID:0, State:"Restarting", StalePID:0}
		//   - a STOPPED/exited daemon ("idle" -> "Stopped", CurrentPID=0) ->
		//     row {PID:0, State:"Stopped", StalePID:0}
		//   - a QUARANTINED daemon ("quarantine" -> "Quarantined", CurrentPID=0)
		// Marking those "restarting" would carry the dead PID forward and SKIP
		// loss classification, leaving sessions zombie-bound to a dead backend
		// until the next client request hits the forward-failure floor — a
		// fail-loud path silently turned fail-quiet. Gating on StalePID!=0 lets
		// the genuine port-stale window carry forward while a real crash/stop
		// (StalePID==0) falls through to the loss branch as before.
		if row.StalePID != 0 {
			restarting[row.Workspace] = true
		}
		if row.PID == 0 && row.StalePID == 0 && !strings.EqualFold(row.State, "Running") {
			if serenaTaskHasActiveIdleStop(taskName, now) {
				idleStopped[row.Workspace] = true
				continue
			}
			deadBackend[row.Workspace] = true
		}
	}

	s.serenaBackendPIDMu.Lock()
	prior := s.serenaBackendLastPID
	if prior == nil {
		prior = map[string]int{}
	}
	priorIdle := s.serenaBackendIdlePaths
	if priorIdle == nil {
		priorIdle = map[string]int{}
	}
	// persisted is the snapshot the next tick compares against. It starts as the
	// fresh per-PATH PIDs; transient Restarting rows carry the prior real PID
	// forward (below) instead of persisting their 0.
	persisted := make(map[string]int, len(fresh))
	for path, pid := range fresh {
		persisted[path] = pid
	}
	persistedIdle := make(map[string]int, len(wantPaths))
	var lost []string // workspace KEYS whose backend was lost this tick
	for path := range wantPaths {
		newPID, present := fresh[path]
		oldPID, hadPrior := prior[path]
		deadNow := present && deadBackend[path]
		idleNow := idleStopped[path]
		if !present && serenaTaskHasActiveIdleStop(pathToTaskName[path], now) {
			idleNow = true
		}
		if !hadPrior {
			// First observation of this workspace normally establishes a
			// baseline. Two PID-0 cases are exceptions: a port-stale Restarting
			// row carries no baseline forward, and a present-but-dead row is
			// immediate backend loss because existing sessions are already bound
			// to a non-live backend.
			if present && restarting[path] {
				delete(persisted, path)
				continue
			}
			if idleNow {
				delete(persisted, path)
				persistedIdle[path] = serenaBackendPostIdleGraceTicks
				continue
			}
			// A first observation with NO IPC row is loss, not a harmless
			// baseline miss. A router-bound session exists only after the
			// daemon handshake completed, so the daemon was alive at bind time.
			// The supervisor status producer emits one row for every remaining
			// intent descriptor in every runtime state (running, stopped/idle,
			// backoff/spawning, quarantined; backoff renders as "Restarting").
			// With active idle stops handled above via the resolver task name
			// for absent rows, a genuinely absent row means the descriptor was
			// removed, not that startup/backoff is transiently rowless.
			if !present {
				lost = append(lost, pathToKey[path])
				delete(persisted, path)
				continue
			}
			if deadNow {
				lost = append(lost, pathToKey[path])
				delete(persisted, path)
			}
			continue
		}
		// A port-stale Restarting row (StalePID!=0: alive daemon, real PID parked
		// in stale_pid, current_pid=0) is a transient mid-restart observation,
		// not a confirmed new generation. Do NOT classify it as loss, and carry
		// the PRIOR real PID forward in the persisted snapshot so the eventual
		// recovered PID is compared against the original generation — not against
		// 0. (A real crash/stop has StalePID==0 and is NOT in `restarting`, so it
		// falls through to the loss branch below.)
		if present && restarting[path] {
			persisted[path] = oldPID
			continue
		}
		if idleNow {
			persisted[path] = oldPID
			persistedIdle[path] = serenaBackendPostIdleGraceTicks
			continue
		}
		if deadNow {
			if remaining := priorIdle[path]; remaining > 0 {
				remaining--
				if remaining > 0 {
					persisted[path] = oldPID
					persistedIdle[path] = remaining
					continue
				}
			}
			lost = append(lost, pathToKey[path])
			delete(persisted, path)
			continue
		}
		// A workspace that was present before and is now ABSENT (stop/death), or
		// whose PID changed (restart), is a backend-loss signal. A workspace that
		// is absent in BOTH ticks (oldPID present but daemon never running, e.g.
		// PID 0 both times) is not a change.
		if !present {
			lost = append(lost, pathToKey[path])
			continue
		}
		if newPID != oldPID {
			// An intentional idle wake always respawns the daemon into a new OS
			// PID. If the previous tick classified this path as idle-stopped, the
			// first running PID is the wake generation the still-live router session
			// is now valid against (the request path re-handshakes the daemon
			// session). Refresh the baseline once instead of declaring backend
			// loss; a later crash/stop is no longer idle-marked and tears down.
			if priorIdle[path] > 0 && newPID != 0 {
				persisted[path] = newPID
				continue
			}
			lost = append(lost, pathToKey[path])
		}
	}
	// Persist the snapshot (only the router-relevant workspaces) so the next
	// tick compares against it. Workspaces not in `fresh` (absent now) are
	// dropped — a later reappearance is a first-observation baseline again, not
	// a spurious loss. Transient Restarting rows carry the prior real PID
	// (handled above) rather than their transient 0.
	s.serenaBackendLastPID = persisted
	s.serenaBackendIdlePaths = persistedIdle
	s.serenaBackendPIDMu.Unlock()

	total := 0
	for _, wsKey := range lost {
		if wsKey == "" {
			continue
		}
		n := s.terminateSerenaSessionsForWorkspace(wsKey)
		total += n
		if deps.AuditFn != nil && n > 0 {
			_ = deps.AuditFn("warn", "serena-backend-loss-session-teardown", map[string]any{
				"workspace_key":     wsKey,
				"sessions_torndown": n,
				"trigger":           "ipc-reconcile",
			})
		}
	}
	return total
}
