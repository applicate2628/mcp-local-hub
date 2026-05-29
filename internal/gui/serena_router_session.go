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
// touch arriving after the idle window does not revive a dead session). A
// no-op when the id is unknown/expired or empty.
func (st *routerSessionStore) touch(clientSessionID string) {
	if clientSessionID == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	b, ok := st.bindings[clientSessionID]
	if !ok || b == nil {
		return
	}
	if st.now().Sub(b.lastSeen) > daemonSessionTTL {
		delete(st.bindings, clientSessionID)
		return
	}
	b.lastSeen = st.now()
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
// optional periodic sweep.
func (st *routerSessionStore) cleanup(now time.Time, ttl time.Duration) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	cutoff := now.Add(-ttl)
	n := 0
	for id, b := range st.bindings {
		if b == nil || !b.lastSeen.After(cutoff) {
			delete(st.bindings, id)
			n++
		}
	}
	return n
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
func (s *Server) SweepSerenaSessions(now time.Time, ttl time.Duration) int {
	return s.serenaRouterSessions.cleanup(now, ttl) +
		s.serenaDaemonSessions.cleanup(now, ttl)
}
