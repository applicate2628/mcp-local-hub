// internal/gui/serena_router_handshake.go
//
// Upstream MCP session handshake + the router-owned client→daemon
// session map for the /serena/mcp router (ROUTER-COMPLETION phase,
// design docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md).
//
// Why this exists (P1): the router synthesizes `initialize` for the
// CLIENT and mints a client-facing Mcp-Session-Id (serena_router_lifecycle.go),
// but it never told the workspace daemon about that session. A serena /
// native-http daemon creates a SEPARATE session per upstream initialize
// and returns its own Mcp-Session-Id; non-initialize POSTs require THAT
// id (internal/daemon/http_host.go forwards every native-http initialize
// so the upstream can mint a fresh session). Passing the router-minted
// client id straight upstream means the daemon has never seen that
// session and rejects the first real tool call.
//
// Fix: when a client session first binds to a workspace W (the first
// path-bearing tools/call, or a tools/list fetch), the router performs
// the real MCP handshake WITH W's daemon — POST initialize → capture the
// daemon's Mcp-Session-Id D → POST notifications/initialized with D — and
// records client-session → (workspace W, daemon-session D). Subsequent
// tool calls for that client session forward upstream with D, NOT the
// router-minted client id.
package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

// daemonSessionTTL bounds how long an idle client→daemon session
// mapping survives before it is swept. It mirrors
// serena_routing.DefaultSessionTTL (24h) so the router-owned daemon
// session map and the sticky-routing SessionRouter age on the same
// idle clock. A successful reuse refreshes lastSeen, so an active
// session never expires.
const daemonSessionTTL = 24 * time.Hour

// handshakeInitializeProtocolVersion is the protocolVersion the router
// offers on its synthesized upstream handshake when the client did NOT
// supply one (an older client that omits the MCP-Protocol-Version
// header on its tool-call / tools/list). It uses the router's default
// (latest published) revision; serena daemons accept it and echo a
// supported version back.
//
// When the client DID negotiate a version (P1 finding 1), the router
// threads THAT version into the handshake instead so the daemon
// session's initialized version equals the version the client's
// subsequent tool-calls advertise on their MCP-Protocol-Version header.
// A strict daemon binds the header to the session's initialized version
// (see internal/api/hub_mcp_handler.go gate 7), so a fixed handshake
// version would make the first tool-call fail as a protocol-version
// mismatch whenever the client negotiated a different supported
// revision (e.g. 2025-06-18).
const handshakeInitializeProtocolVersion = defaultProtocolVersion

// effectiveHandshakeProtocolVersion picks the protocolVersion to send on
// the upstream handshake: the client's negotiated version when present,
// else the router default. clientProtocolVersion is the value the client
// put on the incoming request's MCP-Protocol-Version header (empty for an
// older client that omits it).
func effectiveHandshakeProtocolVersion(clientProtocolVersion string) string {
	if clientProtocolVersion != "" {
		return clientProtocolVersion
	}
	return handshakeInitializeProtocolVersion
}

// daemonSessionBinding records the upstream daemon session a client
// session is multiplexed onto: the workspace it bound to and the
// daemon-issued Mcp-Session-Id to send upstream. lastSeen drives idle
// expiry.
type daemonSessionBinding struct {
	workspaceKey    string // == api.WorkspaceEntry.WorkspaceKey
	daemonSessionID string
	lastSeen        time.Time
}

// daemonSessionStore maps a client-facing Mcp-Session-Id to the real
// upstream daemon session it is multiplexed onto. It is router-owned
// state (NOT the sticky-routing sessionRouter, which lives in
// internal/api/serena_routing and maps client-session → workspace
// only): the daemon session id is a NEW concern introduced by the
// router's synthesize-initialize-at-the-router architecture, and it
// must be reachable from the forward path regardless of which
// sessionRouter implementation (in-memory or serena_routing) is wired.
// Keeping it here also avoids changing the cross-package sessionRouter
// interface.
//
// Concurrency: the map is shared across concurrent requests; every
// access holds mu.
type daemonSessionStore struct {
	mu       sync.Mutex
	bindings map[string]*daemonSessionBinding
	clock    func() time.Time // injectable; nil -> time.Now
}

func (st *daemonSessionStore) now() time.Time {
	if st.clock != nil {
		return st.clock()
	}
	return time.Now()
}

// lookup returns the daemon session id bound to clientSessionID for the
// given workspace, refreshing lastSeen on a hit. It returns ("", false)
// when there is no binding OR the binding is for a DIFFERENT workspace
// (the client session was re-routed to another workspace by a later
// path-arg, so the cached daemon session is stale and must be
// re-established).
//
// lookup is expire-on-read (P2 finding 3): a binding idle longer than
// daemonSessionTTL is treated as a miss AND deleted, so the caller
// re-handshakes and stores a fresh daemon session. This is self-
// contained — it does NOT depend on an external cleanup ticker being
// wired (the production sweep ticker only covers the cross-package
// serena_routing.SessionRouter, not this router-owned store). Without
// expire-on-read a long-idle binding would keep refreshing lastSeen on
// every reuse and forward a daemon session id the upstream daemon has
// already expired on its own idle clock, leaving the client stuck on
// upstream "unknown session" errors with no re-handshake. An expired
// binding is deleted regardless of the workspace it was bound to
// (it is stale either way); the `cleanup` method remains for symmetry
// and an optional periodic sweep.
func (st *daemonSessionStore) lookup(clientSessionID string, wsKey string) (string, bool) {
	if clientSessionID == "" {
		return "", false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	b, ok := st.bindings[clientSessionID]
	if !ok || b == nil {
		return "", false
	}
	// Expire-on-read: a binding past its idle TTL is stale (the daemon
	// likely expired its side already). Drop it so the caller
	// re-handshakes.
	if st.now().Sub(b.lastSeen) > daemonSessionTTL {
		delete(st.bindings, clientSessionID)
		return "", false
	}
	if b.workspaceKey != wsKey || b.daemonSessionID == "" {
		return "", false
	}
	b.lastSeen = st.now()
	return b.daemonSessionID, true
}

// store records (clientSessionID -> workspace, daemonSessionID),
// replacing any prior binding for the same client session. A nil/empty
// argument is ignored so the map never holds a half-binding.
func (st *daemonSessionStore) store(clientSessionID string, wsKey string, daemonSessionID string) {
	if clientSessionID == "" || daemonSessionID == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.bindings == nil {
		st.bindings = make(map[string]*daemonSessionBinding)
	}
	st.bindings[clientSessionID] = &daemonSessionBinding{
		workspaceKey:    wsKey,
		daemonSessionID: daemonSessionID,
		lastSeen:        st.now(),
	}
}

// unbind drops the mapping for clientSessionID (no-op if absent).
func (st *daemonSessionStore) unbind(clientSessionID string) {
	if clientSessionID == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.bindings, clientSessionID)
}

// bindingFor returns the (workspaceKey, daemonSessionID) bound to
// clientSessionID, or ok=false when there is none. Unlike lookup it does
// NOT refresh lastSeen, expire-on-read, or filter by workspace — it is the
// read a client-origin DELETE uses to find the upstream daemon session to
// tear down (Finding 3). An idle-expired binding is still returned so the
// DELETE fans out best-effort to the daemon that may still hold the
// session; the caller unbinds it afterwards regardless.
func (st *daemonSessionStore) bindingFor(clientSessionID string) (wsKey string, daemonSessionID string, ok bool) {
	if clientSessionID == "" {
		return "", "", false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	b, present := st.bindings[clientSessionID]
	if !present || b == nil || b.daemonSessionID == "" {
		return "", "", false
	}
	return b.workspaceKey, b.daemonSessionID, true
}

// cleanup drops bindings idle longer than ttl before now and returns
// the count dropped. Bounded growth is already tied to live client
// sessions; cleanup exists for symmetry with the sticky-routing
// SessionRouter and for an optional periodic sweep.
func (st *daemonSessionStore) cleanup(now time.Time, ttl time.Duration) int {
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

// establishDaemonSession performs the upstream MCP handshake with the
// workspace daemon at upstreamURL and returns the daemon-issued
// Mcp-Session-Id. Two steps per the MCP Streamable HTTP lifecycle:
//
//  1. POST initialize. The daemon (internal/daemon/http_host.go) forwards
//     it upstream and returns a fresh Mcp-Session-Id response header. We
//     require HTTP 200 + a JSON-RPC result (not an error) AND a non-empty
//     session-id header; a daemon that issues no id is treated as one
//     that does not require session affinity and yields ("", nil) — the
//     caller then forwards subsequent calls with no upstream session id
//     (back-compat with a sessionless daemon).
//  2. POST notifications/initialized with the captured session id, so the
//     daemon's upstream session reaches the operational state. The daemon
//     answers 2xx (202). A non-2xx is a handshake FAILURE (Finding 2): it
//     means the daemon did not advance the session, so the mapping must
//     NOT be cached (a later tools/list / tools/call would fail opaquely
//     against a half-initialized session). Both a transport error and a
//     non-2xx status fail the handshake so a dead OR rejecting daemon
//     fails loud rather than minting a phantom mapping.
//
// A 1 MiB read cap bounds the initialize response.
//
// clientProtocolVersion (P1 finding 1) is the version the client
// negotiated in the router-synthesized initialize, surfaced on the
// incoming request's MCP-Protocol-Version header. It is sent as the
// upstream initialize's params.protocolVersion AND on the
// notifications/initialized header so the daemon session's initialized
// version matches the version the client's later tool-calls advertise.
// Empty (older client) falls back to handshakeInitializeProtocolVersion.
func establishDaemonSession(ctx context.Context, httpClient *http.Client, upstreamURL string, clientProtocolVersion string) (string, error) {
	protocolVersion := effectiveHandshakeProtocolVersion(clientProtocolVersion)
	initBody := buildHandshakeInitializeBody(protocolVersion)
	daemonSessionID, err := postHandshakeInitialize(ctx, httpClient, upstreamURL, initBody)
	if err != nil {
		return "", err
	}
	if daemonSessionID == "" {
		// Daemon did not issue a session id: it does not require session
		// affinity. Skip notifications/initialized (there is no session to
		// advance) and signal "no upstream session" to the caller.
		return "", nil
	}
	if err := postHandshakeInitialized(ctx, httpClient, upstreamURL, daemonSessionID, protocolVersion); err != nil {
		return "", fmt.Errorf("notifications/initialized: %w", err)
	}
	return daemonSessionID, nil
}

// buildHandshakeInitializeBody returns the JSON-RPC initialize envelope
// the router sends to a workspace daemon to mint an upstream session.
// id is a fixed sentinel (the response id is not threaded anywhere);
// clientInfo identifies the router as the originator. protocolVersion is
// the negotiated version the caller resolved via
// effectiveHandshakeProtocolVersion (the client's version, or the router
// default for an older client) — P1 finding 1.
func buildHandshakeInitializeBody(protocolVersion string) []byte {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "mcphub-router-handshake",
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "mcphub-serena-router",
				"version": serenaServerInfoVersion,
			},
		},
	})
	return body
}

// postHandshakeInitialize POSTs the initialize body to the daemon and
// returns the daemon's Mcp-Session-Id response header. It requires
// HTTP 200 + a JSON-RPC result (rejecting a JSON-RPC error or a non-200
// transport status); the session-id header may legitimately be empty
// (sessionless daemon), so an empty header is NOT an error.
func postHandshakeInitialize(ctx context.Context, httpClient *http.Client, upstreamURL string, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upstream initialize -> status %d", resp.StatusCode)
	}
	payload := extractJSONRPCPayload(resp.Header.Get("Content-Type"), raw)
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if uerr := json.Unmarshal(payload, &rpc); uerr != nil {
		return "", fmt.Errorf("upstream initialize non-JSON-RPC body: %w", uerr)
	}
	if len(rpc.Error) > 0 {
		return "", fmt.Errorf("upstream initialize returned JSON-RPC error: %s", string(rpc.Error))
	}
	if len(rpc.Result) == 0 {
		return "", fmt.Errorf("upstream initialize returned no result")
	}
	return resp.Header.Get("Mcp-Session-Id"), nil
}

// postHandshakeInitialized POSTs notifications/initialized to the daemon
// carrying the daemon session id, advancing the upstream session to the
// operational state. The daemon answers 2xx (202); a non-2xx is a
// handshake FAILURE (Finding 2) — it means the daemon did not advance the
// session, so caching it would make subsequent tools/list / tools/call
// fail opaquely. Both a transport error and a non-2xx status are surfaced
// so a dead OR rejecting daemon fails the handshake loud.
//
// protocolVersion is set on the MCP-Protocol-Version header (P1 finding
// 1): notifications/initialized is a POST that follows initialize, so a
// strict daemon binding the header to the session's initialized version
// requires it. It is the same version sent on the initialize params.
func postHandshakeInitialized(ctx context.Context, httpClient *http.Client, upstreamURL, daemonSessionID, protocolVersion string) error {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", daemonSessionID)
	if protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	// Finding 2: a non-2xx on notifications/initialized means the daemon
	// did NOT advance the session to operational (a bad session/protocol
	// header is the usual cause). Caching that half-initialized session
	// would make the NEXT tools/list / tools/call fail opaquely with an
	// upstream "unknown/!operational session" rejection. Fail the
	// handshake instead — establishDaemonSession wraps this, so
	// resolveDaemonSession does NOT store the binding and the caller
	// surfaces the diagnosable 502/504 handshake-failure path. (Mirrors
	// the hub treating a post-initialize notification failure as an init
	// failure — internal/api/hub_mcp_handler.go.)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notifications/initialized -> status %d", resp.StatusCode)
	}
	return nil
}

// resolveDaemonSession returns the upstream daemon session id to forward
// for clientSessionID against workspace ws, performing the MCP handshake
// once if no live mapping exists. The returned id may be empty when the
// daemon does not require session affinity (a sessionless daemon); the
// caller then forwards with no upstream Mcp-Session-Id.
//
// clientProtocolVersion (P1 finding 1) is threaded into the handshake so
// the daemon session's initialized version matches the version the
// client's subsequent tool-calls advertise; the caller passes the
// incoming request's MCP-Protocol-Version header (empty for an older
// client). It is used ONLY when a handshake actually runs — a cache hit
// reuses the session established under the version negotiated on the
// first call.
//
// The handshake is performed lazily, OUTSIDE the store lock, so a slow
// daemon does not serialize unrelated client sessions. Two concurrent
// first calls for the same client session may each handshake; the last
// store() wins and both calls forward a valid daemon session — serena
// daemons mint independent sessions per initialize, and an orphaned
// extra session ages out on the daemon's own idle sweep. This is the
// same "re-bind drops the previous binding without ceremony" tradeoff
// the sticky-routing SessionRouter already documents.
func (st *daemonSessionStore) resolveDaemonSession(
	ctx context.Context,
	httpClient *http.Client,
	upstreamURL string,
	clientSessionID string,
	ws *api.WorkspaceEntry,
	clientProtocolVersion string,
) (string, error) {
	if ws == nil {
		return "", fmt.Errorf("resolveDaemonSession: nil workspace")
	}
	if clientSessionID != "" {
		if dsid, ok := st.lookup(clientSessionID, ws.WorkspaceKey); ok {
			return dsid, nil
		}
	}
	dsid, err := establishDaemonSession(ctx, httpClient, upstreamURL, clientProtocolVersion)
	if err != nil {
		return "", err
	}
	// Only record a mapping when BOTH a client session id and a daemon
	// session id exist. A sessionless daemon (empty dsid) needs no
	// mapping; a path-only call with no client session id cannot be
	// looked up later anyway.
	if clientSessionID != "" && dsid != "" {
		st.store(clientSessionID, ws.WorkspaceKey, dsid)
	}
	return dsid, nil
}
