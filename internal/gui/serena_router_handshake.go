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
//
// Finding #8 (daemon-negotiated version): daemonProtocolVersion is the
// protocolVersion the DAEMON returned in its initialize result — which can
// DIFFER from the version the router requested (a daemon may negotiate down
// to a revision it supports). A strict daemon binds its session to the
// version IT negotiated, so every subsequent forward to this daemon session
// (tool-call POST, notifications/cancelled, the teardown DELETE) MUST carry
// the daemon-negotiated version on MCP-Protocol-Version, NOT the
// router-requested one — otherwise the daemon rejects the first forward as a
// version mismatch. Empty only when the daemon omitted protocolVersion from
// its result (then the requested version is the fallback).
type daemonSessionBinding struct {
	workspaceKey          string // == api.WorkspaceEntry.WorkspaceKey
	daemonSessionID       string
	daemonProtocolVersion string
	lastSeen              time.Time
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
// Finding #8: lookup also returns the daemon-negotiated protocolVersion
// stored on the binding, so a cache hit forwards under the SAME version the
// session was established with (not the per-request header).
func (st *daemonSessionStore) lookup(clientSessionID string, wsKey string) (daemonSessionID string, daemonProtocolVersion string, ok bool) {
	if clientSessionID == "" {
		return "", "", false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	b, present := st.bindings[clientSessionID]
	if !present || b == nil {
		return "", "", false
	}
	// Expire-on-read: a binding past its idle TTL is stale (the daemon
	// likely expired its side already). Drop it so the caller
	// re-handshakes.
	if st.now().Sub(b.lastSeen) > daemonSessionTTL {
		delete(st.bindings, clientSessionID)
		return "", "", false
	}
	if b.workspaceKey != wsKey || b.daemonSessionID == "" {
		return "", "", false
	}
	b.lastSeen = st.now()
	return b.daemonSessionID, b.daemonProtocolVersion, true
}

// store records (clientSessionID -> workspace, daemonSessionID),
// replacing any prior binding for the same client session. A nil/empty
// argument is ignored so the map never holds a half-binding.
//
// Round-9 (Finding 4 — revert the eager displaced-session teardown):
// store now simply OVERWRITES the binding. It used to return the displaced
// old binding (workspaceKey, daemonSessionID, daemonProtocolVersion) so the
// caller could best-effort upstream-DELETE a workspace-switch's orphaned old
// daemon session. The reviewer's round-9 guidance is to "leave it to the
// daemon/session TTL instead of deleting it synchronously on workspace
// switch": the router does not track per-daemon in-flight requests, so a
// client with a long-running tool call in workspace A that then starts a
// path-bearing call in workspace B would displace A and immediately DELETE
// A's daemon session while A's request may still be streaming — an upstream
// unknown/terminated-session failure. The eager teardown (rounds 7+8) traded
// a bounded-by-TTL leak for an in-flight race; reverting to TTL-based reclaim
// is race-safe. The LOCAL binding is overwritten here immediately (no local
// leak); the orphaned UPSTREAM daemon session is reclaimed by the daemon's
// own idle expiry, and the local store's idle entries by SweepSerenaSessions.
func (st *daemonSessionStore) store(clientSessionID string, wsKey string, daemonSessionID string, daemonProtocolVersion string) {
	if clientSessionID == "" || daemonSessionID == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.bindings == nil {
		st.bindings = make(map[string]*daemonSessionBinding)
	}
	st.bindings[clientSessionID] = &daemonSessionBinding{
		workspaceKey:          wsKey,
		daemonSessionID:       daemonSessionID,
		daemonProtocolVersion: daemonProtocolVersion,
		lastSeen:              st.now(),
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

// bindingFor returns the (workspaceKey, daemonSessionID, daemonProtocolVersion)
// bound to clientSessionID, or ok=false when there is none. Unlike lookup it
// does NOT refresh lastSeen, expire-on-read, or filter by workspace — it is
// the read a client-origin DELETE / cancel uses to find the upstream daemon
// session to tear down or forward to (Finding 3 / Finding H). An idle-expired
// binding is still returned so the DELETE fans out best-effort to the daemon
// that may still hold the session; the caller unbinds it afterwards regardless.
//
// Finding #8: the persisted daemon-negotiated protocolVersion is returned so
// the teardown DELETE / cancel forward carries the version the daemon session
// was established under (not the per-request header).
func (st *daemonSessionStore) bindingFor(clientSessionID string) (wsKey string, daemonSessionID string, daemonProtocolVersion string, ok bool) {
	if clientSessionID == "" {
		return "", "", "", false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	b, present := st.bindings[clientSessionID]
	if !present || b == nil || b.daemonSessionID == "" {
		return "", "", "", false
	}
	return b.workspaceKey, b.daemonSessionID, b.daemonProtocolVersion, true
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
//
// Finding #8: it ALSO returns the protocolVersion the DAEMON negotiated in
// its initialize result. The daemon may negotiate a DIFFERENT revision than
// the router requested; a strict daemon then binds its session to ITS
// version, so notifications/initialized AND every later forward to this
// daemon session must carry the daemon-negotiated version. When the daemon
// omits result.protocolVersion the requested version is the fallback (so the
// returned version is never empty for a session-bearing daemon).
func establishDaemonSession(ctx context.Context, httpClient *http.Client, upstreamURL string, clientProtocolVersion string) (daemonSessionID string, daemonProtocolVersion string, err error) {
	requestedVersion := effectiveHandshakeProtocolVersion(clientProtocolVersion)
	initBody := buildHandshakeInitializeBody(requestedVersion)
	daemonSessionID, negotiated, err := postHandshakeInitialize(ctx, httpClient, upstreamURL, initBody)
	if err != nil {
		return "", "", err
	}
	if daemonSessionID == "" {
		// Daemon did not issue a session id: it does not require session
		// affinity. Skip notifications/initialized (there is no session to
		// advance) and signal "no upstream session" to the caller.
		return "", "", nil
	}
	// Finding #8: the daemon session is bound to the version the DAEMON
	// negotiated; fall back to the requested version only when the daemon
	// omitted the field. notifications/initialized + later forwards use it.
	daemonProtocolVersion = negotiated
	if daemonProtocolVersion == "" {
		daemonProtocolVersion = requestedVersion
	}
	if err := postHandshakeInitialized(ctx, httpClient, upstreamURL, daemonSessionID, daemonProtocolVersion); err != nil {
		// Finding #3 (initialized-fail leak): initialize ALREADY succeeded and
		// the daemon minted daemonSessionID, but notifications/initialized then
		// errored / timed out / returned non-2xx (the Round-3 fail-handshake
		// path). Returning here WITHOUT releasing that session leaks one
		// upstream daemon session per failed initialized notification (tool-call
		// + tools/list handshakes both hit this) until the daemon's idle expiry.
		// Best-effort upstream-DELETE the just-created session BEFORE returning
		// the (still-loud) handshake error. Invariant D: the teardown uses a
		// DETACHED + short-bounded context (cleanupContext), NOT the handshake
		// ctx — that ctx is the inbound request context, which a disconnecting
		// client may have already cancelled, which would then drop the very
		// cleanup this finding adds. The handshake still fails loud (the wrapped
		// error is returned); it just no longer leaks the partial session.
		bestEffortDeleteDaemonSession(httpClient, upstreamURL, daemonSessionID, daemonProtocolVersion)
		return "", "", fmt.Errorf("notifications/initialized: %w", err)
	}
	return daemonSessionID, daemonProtocolVersion, nil
}

// bestEffortDeleteDaemonSession issues a fire-and-forget DELETE to a daemon's
// /mcp carrying daemonSessionID + protocolVersion, so the daemon releases an
// upstream session the router established but cannot keep (Finding #3: the
// initialize succeeded and minted the session, but notifications/initialized
// then failed). It is the establishDaemonSession-local counterpart to the
// Server.forwardSerenaDeleteUpstream teardown — establishDaemonSession is a
// free function with no *Server / no ws / no auditFn, so this issues the DELETE
// directly. Invariant D: the request context is DETACHED (cleanupContext is
// context.Background()-derived, so an already-cancelled inbound request does
// not abort the cleanup) and SHORT-bounded (serenaCleanupTimeout, never the
// 60s default). The result is fully discarded — the handshake error the caller
// returns is the loud signal; a teardown transport failure / non-2xx is itself
// best-effort and not separately surfaced here (the session would then age out
// on the daemon's own idle clock). An empty daemonSessionID is a no-op.
func bestEffortDeleteDaemonSession(httpClient *http.Client, upstreamURL, daemonSessionID, protocolVersion string) {
	if daemonSessionID == "" {
		return
	}
	ctx, cancel := cleanupContext(0)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, upstreamURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", daemonSessionID)
	if protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
	}
	resp, doErr := httpClient.Do(req)
	if doErr != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
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
// returns the daemon's Mcp-Session-Id response header PLUS the
// protocolVersion the daemon negotiated in its initialize result (Finding
// #8). It requires HTTP 200 + a JSON-RPC result (rejecting a JSON-RPC error
// or a non-200 transport status); the session-id header may legitimately be
// empty (sessionless daemon), so an empty header is NOT an error. The
// returned daemon protocolVersion is empty when the daemon omits the field
// (the caller then falls back to the requested version).
func postHandshakeInitialize(ctx context.Context, httpClient *http.Client, upstreamURL string, body []byte) (sessionID string, daemonProtocolVersion string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("upstream initialize -> status %d", resp.StatusCode)
	}
	// Finding 4: read the response INCREMENTALLY. A Streamable-HTTP daemon
	// that answers initialize with text/event-stream may keep the stream
	// open after emitting the response event; io.ReadAll(resp.Body) would
	// then block until the client timeout (60s) on every first handshake.
	// readUpstreamJSONRPCResponse returns at the first JSON-RPC response
	// event for SSE, or does a bounded read for application/json. (Mirrors
	// internal/api/hub_mcp_aggregator.go's doDaemonPost SSE branch.)
	payload, rerr := readUpstreamJSONRPCResponse(resp.Header.Get("Content-Type"), resp.Body)
	if rerr != nil {
		return "", "", fmt.Errorf("read upstream initialize: %w", rerr)
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if uerr := json.Unmarshal(payload, &rpc); uerr != nil {
		return "", "", fmt.Errorf("upstream initialize non-JSON-RPC body: %w", uerr)
	}
	if len(rpc.Error) > 0 {
		return "", "", fmt.Errorf("upstream initialize returned JSON-RPC error: %s", string(rpc.Error))
	}
	if len(rpc.Result) == 0 {
		return "", "", fmt.Errorf("upstream initialize returned no result")
	}
	// Finding #8: pull result.protocolVersion so the caller can bind this
	// daemon session to the version the DAEMON actually negotiated (it may
	// differ from the version the router requested). A missing/malformed
	// field yields "" — the caller falls back to the requested version.
	var resultFields struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(rpc.Result, &resultFields)
	return resp.Header.Get("Mcp-Session-Id"), resultFields.ProtocolVersion, nil
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
//
// Round-9 (Finding 4): the workspace-switch displaced-session teardown is
// REMOVED. rounds 7+8 invoked a teardownDisplaced callback here to
// best-effort upstream-DELETE the old workspace's daemon session when a
// client session rebound to a new workspace. The reviewer found that still
// races — the router does not track per-daemon in-flight requests, so a
// long-running tool call in the OLD workspace can still be streaming when
// the switch DELETEs its daemon session. Per the reviewer's accepted
// resolution, we revert to TTL-based reclaim: store() simply overwrites the
// LOCAL binding (no local leak), and the orphaned UPSTREAM daemon session
// ages out on the daemon's own idle clock (the local store's idle entries via
// SweepSerenaSessions). The DELETE-driven teardown (handleSerenaDelete) and
// the partial-handshake cleanup (establishDaemonSession's
// bestEffortDeleteDaemonSession on a notifications/initialized failure) are
// NOT racy and are kept — only the switch-time teardown is gone.
//
// Finding #8: it returns the daemon-negotiated protocolVersion alongside the
// session id (from a fresh handshake, or persisted on a cache hit) so the
// caller forwards subsequent requests to this daemon session under the
// version the daemon bound the session to.
func (st *daemonSessionStore) resolveDaemonSession(
	ctx context.Context,
	httpClient *http.Client,
	upstreamURL string,
	clientSessionID string,
	ws *api.WorkspaceEntry,
	clientProtocolVersion string,
) (daemonSessionID string, daemonProtocolVersion string, err error) {
	if ws == nil {
		return "", "", fmt.Errorf("resolveDaemonSession: nil workspace")
	}
	if clientSessionID != "" {
		if dsid, dpv, ok := st.lookup(clientSessionID, ws.WorkspaceKey); ok {
			return dsid, dpv, nil
		}
	}
	dsid, dpv, err := establishDaemonSession(ctx, httpClient, upstreamURL, clientProtocolVersion)
	if err != nil {
		return "", "", err
	}
	// Only record a mapping when BOTH a client session id and a daemon
	// session id exist. A sessionless daemon (empty dsid) needs no
	// mapping; a path-only call with no client session id cannot be
	// looked up later anyway. A workspace switch on the same client session
	// just overwrites the prior binding locally (Round-9 / Finding 4 — no
	// eager upstream teardown; the old upstream session ages out on the
	// daemon's idle clock).
	if clientSessionID != "" && dsid != "" {
		st.store(clientSessionID, ws.WorkspaceKey, dsid, dpv)
	}
	return dsid, dpv, nil
}
