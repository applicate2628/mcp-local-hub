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
// offers on its synthesized upstream handshake. It uses the router's
// default (latest published) revision; serena daemons accept it and
// echo a supported version back. The handshake's negotiated version is
// not surfaced to the client (the client negotiated independently in
// the router-synthesized initialize), so a stable value is sufficient.
const handshakeInitializeProtocolVersion = defaultProtocolVersion

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
func (st *daemonSessionStore) lookup(clientSessionID string, wsKey string) (string, bool) {
	if clientSessionID == "" {
		return "", false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	b, ok := st.bindings[clientSessionID]
	if !ok || b == nil || b.workspaceKey != wsKey || b.daemonSessionID == "" {
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
//     daemon's upstream session reaches the operational state. The
//     daemon answers 202; we tolerate any 2xx and do not fail the
//     handshake on a non-2xx here (the session already exists from step 1
//     and the client's first tool call is the authoritative liveness
//     check) — but a transport error IS surfaced so a dead daemon fails
//     loud rather than minting a phantom mapping.
//
// A 1 MiB read cap bounds the initialize response.
func establishDaemonSession(ctx context.Context, httpClient *http.Client, upstreamURL string) (string, error) {
	initBody := buildHandshakeInitializeBody()
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
	if err := postHandshakeInitialized(ctx, httpClient, upstreamURL, daemonSessionID); err != nil {
		return "", fmt.Errorf("notifications/initialized: %w", err)
	}
	return daemonSessionID, nil
}

// buildHandshakeInitializeBody returns the JSON-RPC initialize envelope
// the router sends to a workspace daemon to mint an upstream session.
// id is a fixed sentinel (the response id is not threaded anywhere);
// clientInfo identifies the router as the originator.
func buildHandshakeInitializeBody() []byte {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "mcphub-router-handshake",
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": handshakeInitializeProtocolVersion,
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
// operational state. The daemon answers 202; a non-2xx is tolerated
// (the session already exists from initialize), but a transport error is
// surfaced so a dead daemon fails the handshake.
func postHandshakeInitialized(ctx context.Context, httpClient *http.Client, upstreamURL, daemonSessionID string) error {
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
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	// 2xx expected; we do not hard-fail on non-2xx because the session
	// already exists from the initialize step and the client's first
	// real tool call is the authoritative liveness check.
	return nil
}

// resolveDaemonSession returns the upstream daemon session id to forward
// for clientSessionID against workspace ws, performing the MCP handshake
// once if no live mapping exists. The returned id may be empty when the
// daemon does not require session affinity (a sessionless daemon); the
// caller then forwards with no upstream Mcp-Session-Id.
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
) (string, error) {
	if ws == nil {
		return "", fmt.Errorf("resolveDaemonSession: nil workspace")
	}
	if clientSessionID != "" {
		if dsid, ok := st.lookup(clientSessionID, ws.WorkspaceKey); ok {
			return dsid, nil
		}
	}
	dsid, err := establishDaemonSession(ctx, httpClient, upstreamURL)
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
