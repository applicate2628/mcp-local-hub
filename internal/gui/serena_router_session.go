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

// negotiatedVersion returns the protocol version a client session
// negotiated at initialize, refreshing lastSeen on a hit. ok=false means
// the id was never minted at this router (or its binding idle-expired).
//
// Expire-on-read (mirrors daemonSessionStore.lookup, P2 finding 3): a
// binding idle longer than daemonSessionTTL is treated as a miss AND
// deleted, so a long-idle session id is no longer trusted as "minted
// here". This is self-contained — it does not depend on an external
// cleanup ticker.
func (st *routerSessionStore) negotiatedVersion(clientSessionID string) (string, bool) {
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
	b.lastSeen = st.now()
	return b.negotiatedVersion, true
}

// known reports whether clientSessionID was minted at this router and is
// not idle-expired. It is negotiatedVersion without the version return —
// the tools/list gate (Finding 4) only needs the existence answer.
func (st *routerSessionStore) known(clientSessionID string) bool {
	_, ok := st.negotiatedVersion(clientSessionID)
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
