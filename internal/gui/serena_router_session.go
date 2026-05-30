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
	"sync"
	"time"
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
type routerSessionStore struct {
	mu       sync.Mutex
	bindings map[string]*routerSessionBinding
	lru      *list.List               // values are session-id strings; front == most-recently-seen
	lruIndex map[string]*list.Element // session id -> its lru element
	clock    func() time.Time         // injectable; nil -> time.Now
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
