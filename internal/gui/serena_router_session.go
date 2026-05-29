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
	"sync"
	"time"
)

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
// Concurrency: the map is shared across concurrent requests; every access
// holds mu.
type routerSessionStore struct {
	mu       sync.Mutex
	bindings map[string]*routerSessionBinding
	clock    func() time.Time // injectable; nil -> time.Now
}

func (st *routerSessionStore) now() time.Time {
	if st.clock != nil {
		return st.clock()
	}
	return time.Now()
}

// store records (clientSessionID -> negotiatedVersion), replacing any
// prior binding for the same client session. An empty client session id
// is ignored so the map never holds a keyless entry.
func (st *routerSessionStore) store(clientSessionID, negotiatedVersion string) {
	if clientSessionID == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.bindings == nil {
		st.bindings = make(map[string]*routerSessionBinding)
	}
	st.bindings[clientSessionID] = &routerSessionBinding{
		negotiatedVersion: negotiatedVersion,
		lastSeen:          st.now(),
	}
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
	if clientSessionID == "" {
		return "", false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	b, ok := st.bindings[clientSessionID]
	if !ok || b == nil {
		return "", false
	}
	if st.now().Sub(b.lastSeen) > daemonSessionTTL {
		delete(st.bindings, clientSessionID)
		return "", false
	}
	return b.negotiatedVersion, true
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
		delete(st.bindings, clientSessionID)
		return false
	}
	b.lastSeen = st.now()
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
	delete(st.bindings, clientSessionID)
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
			delete(st.bindings, id)
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
// Residual (documented, no tombstone machinery — kept simple per the finding):
// a narrow window remains between an ON-READ peek-expiry of a router session
// (peekNegotiatedVersion deletes the entry the moment a request observes it
// past-TTL) and the NEXT periodic SweepSerenaSessions tick. In that window the
// routerSessionStore entry is already gone but the sticky binding is not yet
// swept, so a concurrent path-less call could still route once as legacy. This
// is bounded by the sweep interval (1h in production) and is harmless for the
// idle-disconnect case this guard targets (no concurrent traffic on a
// disconnected session). The tool-call path's own pre-gate peek+touch
// (serena_router.go) already rejects an EXPLICITLY-DELETEd session immediately;
// only the silent idle-expiry has this residual, and closing it cleanly would
// require expiry tombstones across the sticky store's cross-package boundary —
// out of proportion to the bounded, idle-only exposure.
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
		s.serenaDaemonSessions.unbind(id)
		if sticky != nil {
			sticky.UnbindSession(id)
		}
	}

	// Then sweep the daemon store's OWN idle entries (those not already removed
	// by the coordination above). The total dropped count is the expired router
	// sessions plus the daemon store's independently-idle bindings.
	daemonDropped := s.serenaDaemonSessions.cleanup(now, ttl)
	return len(expiredRouter) + daemonDropped
}
