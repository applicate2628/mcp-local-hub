// internal/gui/serena_router_lifecycle.go
//
// MCP session-lifecycle synthesis for the /serena/mcp router
// (ROUTER-COMPLETION phase; design
// docs/superpowers/specs/2026-05-29-serena-migrate-redesign-descriptor-proxy.md
// §5 / O1(a)).
//
// The non-tool MCP lifecycle (initialize, tools/list, notifications/*,
// ping) is workspace-agnostic: every serena daemon exposes the same MCP
// surface, and the router itself has NO per-client identity (one
// same-origin route, no client branch — design §9 O1(a)). So the router
// synthesizes these methods directly instead of forwarding them to a
// workspace daemon (it cannot — initialize/tools/list carry no path-arg
// and no bound session). Tool calls (params.name + path-arg, or a bound
// session) keep their existing path-routing + upstream-forward unchanged
// in serenaRouterHandler.
package gui

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

// supportedProtocolVersions is the set of MCP protocol versions the
// synthesized initialize will echo back verbatim. Negotiation rule (MCP
// spec, basic/lifecycle): if the client's requested protocolVersion is
// one the server supports, the server MUST respond with the same
// version; otherwise it responds with a version it does support. The
// router is a thin lifecycle front for serena daemons, so it accepts the
// well-known published MCP revisions and falls back to
// defaultProtocolVersion for anything it does not recognize.
var supportedProtocolVersions = map[string]struct{}{
	"2024-11-05": {},
	"2025-03-26": {},
	"2025-06-18": {},
	"2025-11-25": {},
}

// defaultProtocolVersion is returned when the client requests a version
// the router does not recognize (or omits the field). It is the latest
// revision in supportedProtocolVersions.
const defaultProtocolVersion = "2025-11-25"

// supportedProtocolVersionsList returns the supported MCP protocol
// versions as a sorted slice, for the error.data.supported field on an
// initialize rejection (P2 findings 1 + 5). Mirrors the hub handler's
// hubSupportedVersionsList() (internal/api/hub_mcp_handler.go) but derives
// the slice from supportedProtocolVersions directly so a future edit to
// that map cannot leave the advertised list stale. Sorted descending so
// the newest revision the router speaks is listed first (matches the hub's
// newest-first ordering).
func supportedProtocolVersionsList() []string {
	out := make([]string, 0, len(supportedProtocolVersions))
	for v := range supportedProtocolVersions {
		out = append(out, v)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// serenaServerInfoName / serenaServerInfoVersion identify the synthesized
// MCP server. Name matches the manifest server name ("serena") so a
// client's serverInfo display is consistent with the daemon it will
// eventually reach; version marks this as the mcphub router front rather
// than a specific serena build (the real serena version is observable on
// a tool call's upstream response).
const (
	serenaServerInfoName    = "serena"
	serenaServerInfoVersion = "mcphub-router"
)

// jsonrpcInvalidParams / jsonrpcInternalError are the JSON-RPC 2.0
// reserved error codes used by the synthesized lifecycle. The empty-pool
// tools/list uses serenaNoWorkspaceCode, a server-defined code in the
// implementation-defined range (-32000..-32099).
const (
	jsonrpcInvalidRequest = -32600
	jsonrpcInvalidParams  = -32602
	jsonrpcInternalError  = -32603
	serenaNoWorkspaceCode = -32001
)

// workspaceLister is the OPTIONAL capability the router uses to pick a
// live serena daemon to proxy a workspace-agnostic tools/list to. The
// production *serena_routing.WorkspaceResolver implements it; a resolver
// that does not is handled gracefully (tools/list then reports the
// empty-pool error). Kept separate from workspaceResolver so the
// path-routing seam stays minimal and a fake resolver in a tool-call
// test need not implement enumeration.
type workspaceLister interface {
	ListWorkspaces() []*api.WorkspaceEntry
}

// toolsListCacheTTL bounds how long a fetched tools/list result is served
// from cache before the router re-proxies to a live daemon. The serena
// tool surface is effectively static for a given serena version, so a
// short TTL keeps repeated client tools/list calls cheap while still
// picking up a serena upgrade within the window.
const toolsListCacheTTL = 5 * time.Minute

// toolsListCacheEntry is one cached cursorless tools/list result plus the
// time it was fetched (for TTL).
type toolsListCacheEntry struct {
	result  json.RawMessage
	fetched time.Time
}

// toolsListCache caches the workspace-agnostic tools/list result (the raw
// JSON-RPC `result` object) fetched from any live serena daemon. The
// cache is process-local and lives on the Server so it survives across
// requests. It is workspace-agnostic by construction (design §2.1 /
// O1(a): one daemon per workspace, identical MCP surface), so a single
// cached entry per protocol version serves every client regardless of
// which workspace daemon answered the fetch.
//
// Finding E: the cache is KEYED BY the session's negotiated protocol
// version. The serena tool surface a daemon advertises can differ across
// protocol revisions, so a payload fetched under 2025-11-25 must not be
// served to a client that initialized with 2025-06-18. Version-keying is
// preferred over a global single entry (which the pre-fix cache used) so
// each negotiated revision gets its own TTL-bounded entry.
type toolsListCache struct {
	mu       sync.Mutex
	byVer    map[string]toolsListCacheEntry
	cacheTTL time.Duration // 0 -> toolsListCacheTTL; overridable in tests
}

// get returns the cached result for protocolVersion and true when a
// non-expired entry exists for that version.
func (c *toolsListCache) get(protocolVersion string, now time.Time) (json.RawMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ttl := c.cacheTTL
	if ttl <= 0 {
		ttl = toolsListCacheTTL
	}
	entry, ok := c.byVer[protocolVersion]
	if !ok || len(entry.result) == 0 {
		return nil, false
	}
	if now.Sub(entry.fetched) > ttl {
		return nil, false
	}
	out := make(json.RawMessage, len(entry.result))
	copy(out, entry.result)
	return out, true
}

// put stores result as the cached tools/list payload for protocolVersion.
func (c *toolsListCache) put(protocolVersion string, result json.RawMessage, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byVer == nil {
		c.byVer = make(map[string]toolsListCacheEntry)
	}
	c.byVer[protocolVersion] = toolsListCacheEntry{
		result:  append(json.RawMessage(nil), result...),
		fetched: now,
	}
}

// writeJSONRPCResult writes a JSON-RPC 2.0 success envelope carrying id
// and result at HTTP 200. id is threaded verbatim from the request (MCP
// requires the response id to match the request id); a nil/absent id is
// emitted as JSON null per JSON-RPC.
func writeJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any, extraHeaders map[string]string) {
	for k, v := range extraHeaders {
		if v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(jsonrpcEnvelope{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Result:  result,
	})
}

// writeJSONRPCError writes a JSON-RPC 2.0 error envelope at HTTP 200
// (JSON-RPC carries errors in-band; the transport status stays 200 so a
// compliant client parses the error rather than a transport failure).
func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(jsonrpcEnvelope{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Error:   &jsonrpcError{Code: code, Message: message},
	})
}

// writeJSONRPCErrorStatus writes a JSON-RPC 2.0 error envelope at an
// EXPLICIT HTTP status, with an optional error.data field. It mirrors the
// hub handler's writeJSONRPCErrorStatus
// (internal/api/hub_mcp_handler.go) so the router's initialize validation
// (P2 findings 1 + 5) can return the same status/code/data shapes the hub
// returns. writeJSONRPCError (HTTP 200, no data) remains for the in-band
// error path that does not need a custom status — initialize rejections
// that the spec maps to a transport error (e.g. session-id present →
// HTTP 400) need this status-carrying variant. A nil data is omitted.
func writeJSONRPCErrorStatus(w http.ResponseWriter, id json.RawMessage, httpStatus, code int, message string, data any) {
	errObj := map[string]any{
		"code":    code,
		"message": message,
	}
	if data != nil {
		errObj["data"] = data
	}
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      normalizeID(id),
		"error":   errObj,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(httpStatus)
	_, _ = w.Write(payload)
}

// normalizeID returns a JSON null RawMessage when id is empty so the
// emitted envelope always carries an explicit id field (JSON-RPC allows
// a null id on a response to a request whose id could not be determined).
func normalizeID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// isJSONRPCNotificationID reports whether a JSON-RPC envelope is a
// notification (no response permitted). It returns true ONLY when the
// "id" field is absent from the body entirely (len == 0). An explicit
// `"id": null` token is NOT a notification — MCP §1.5 requires id to be
// a non-null String/Number, so `id:null` is a malformed request, not a
// fire-and-forget event. This mirrors the hub handler's
// isJSONRPCNotificationID byte-for-byte
// (internal/api/hub_mcp_handler.go) so the router and the hub classify
// the same wire shape identically.
func isJSONRPCNotificationID(id json.RawMessage) bool {
	return len(id) == 0
}

// isValidJSONRPCRequestID reports whether a PRESENT JSON-RPC id is a
// valid request id: a non-null JSON String or Number. MCP §1.5 narrows
// the JSON-RPC base spec to FORBID null ids, and JSON-RPC's grammar
// already forbids booleans, arrays, and objects as ids. So `id:null`,
// `id:true`, `id:{}`, and `id:[1]` are malformed requests, not
// notifications and not synthesizable requests — the caller rejects
// them with -32600 Invalid Request.
//
// PRE: id is PRESENT (len > 0); an absent id is a notification, handled
// by isJSONRPCNotificationID before this runs. The router only needs to
// VALIDATE the id shape (it echoes the id verbatim via normalizeID and
// never keys a map on it), so this is a focused String/Number predicate
// rather than the full numeric canonicalizer in
// internal/api/hub_mcp_request_id.go (which exists only because the hub
// uses the id as an in-flight map key). The rejection rules match that
// validator: null/bool/array/object out, string/number in, leading `+`
// and trailing JSON data on a number rejected.
func isValidJSONRPCRequestID(id json.RawMessage) bool {
	trimmed := bytes.TrimSpace(id)
	if len(trimmed) == 0 {
		return false
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	// JSON string: starts with a quote. json.Valid confirms the escapes
	// are well-formed (a bare `"foo` would otherwise pass the prefix
	// check yet be invalid JSON).
	if trimmed[0] == '"' {
		return json.Valid(trimmed)
	}
	// Anything else must be a JSON number. UseNumber keeps the raw
	// decimal string (no float64 demotion) and Decode rejects booleans,
	// arrays, and objects. Demand a single value with nothing trailing.
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return false
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		// A second value, or any non-EOF error (e.g. `1]`), means
		// leftover input — the id is malformed.
		return false
	}
	num, ok := v.(json.Number)
	if !ok {
		// bool / array / object.
		return false
	}
	// json.Number admits a leading `+` the JSON grammar forbids; reject
	// it defensively so the router never echoes a non-spec id.
	return len(num) == 0 || num[0] != '+'
}

type jsonrpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// initializeResult is the synthesized InitializeResult payload (MCP
// basic/lifecycle). capabilities advertises tools (the router fronts
// serena's tool surface); listChanged stays false because the router
// does not push tool-list-changed notifications.
type initializeResult struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    map[string]any    `json:"capabilities"`
	ServerInfo      map[string]string `json:"serverInfo"`
}

// newMcpSessionID mints a fresh opaque session id (32 hex chars / 16
// random bytes) for a synthesized initialize. The id is returned to the
// client in the Mcp-Session-Id response header; it binds to a workspace
// later, on the first path-bearing tools/call, via the existing
// BindSession path in serenaRouterHandler. crypto/rand failure is
// vanishingly unlikely; on error we fall back to a time-seeded value so
// initialize never fails for a transient RNG hiccup (the id only needs
// process-local uniqueness for the in-memory session map).
func newMcpSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("sess-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// initializeRawParams pulls the raw `params` object out of an MCP
// initialize body without disturbing the toolBody decode used for tool
// routing. Unlike a best-effort string probe, returning the raw bytes lets
// handleInitialize tell a PRESENT-but-type-mismatched params object (which
// must be a -32602 error, P2 finding 5) apart from an absent/empty one. A
// malformed/absent params field yields a nil RawMessage; the caller then
// treats protocolVersion as absent (also a -32602 error).
func initializeRawParams(body []byte) json.RawMessage {
	var probe struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil
	}
	return probe.Params
}

// requestedToolsListCursor pulls params.cursor out of a raw MCP
// tools/list body. A non-empty cursor means the client is paging (MCP
// basic/utilities/pagination), and the workspace-agnostic tools/list
// cache MUST be bypassed for it — the cache holds only the first page,
// so serving it for any cursor would loop the client on the same
// nextCursor (P2 finding 2). Best-effort: a malformed/absent field
// yields "".
func requestedToolsListCursor(body []byte) string {
	var probe struct {
		Params struct {
			Cursor string `json:"cursor"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Params.Cursor
}

// handleInitialize synthesizes the InitializeResult and mints a session
// id. Workspace-agnostic: no daemon is contacted (design §5) — every
// serena daemon exposes the same lifecycle surface, and the session is
// bound to a concrete workspace only on the first path-bearing
// tools/call.
//
// incomingSessionID is the request's Mcp-Session-Id header. The MCP
// lifecycle forbids a session id on initialize (initialize is what
// ESTABLISHES the session), and the synthesized body + the rawParams
// type/version validation below mirror the hub handler's handleInitialize
// (internal/api/hub_mcp_handler.go) so the router and hub reject the same
// non-compliant clients identically.
func (s *Server) handleInitialize(w http.ResponseWriter, body []byte, tb *toolBody, incomingSessionID string) {
	// Finding 1 (mirror hub handleInitialize): a session id on initialize
	// is invalid — initialize is what mints the session. Reject it instead
	// of echoing the caller-supplied id (which would let a client
	// reinitialize-with-stale-id and keep a prior workspace/daemon
	// binding). The reconcile readiness probe sends NO session-id header,
	// so it is unaffected.
	if incomingSessionID != "" {
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
			"session-id only valid after initialize", nil)
		return
	}

	// Finding 5 (mirror hub handleInitialize): validate params.protocolVersion
	// synchronously BEFORE minting a session. initializeRawParams returns the
	// raw params bytes so a present-but-type-mismatched params object is told
	// apart from an absent/empty protocolVersion, and the router requires a
	// SUPPORTED version rather than silently negotiating the default (which
	// hides a non-compliant client).
	rawParams := initializeRawParams(body)
	var initParams struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(rawParams) > 0 && string(rawParams) != "null" {
		if uerr := json.Unmarshal(rawParams, &initParams); uerr != nil {
			writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidParams,
				"invalid initialize params: "+uerr.Error(), nil)
			return
		}
	}
	if initParams.ProtocolVersion == "" {
		// Missing/empty protocolVersion is an invalid-params error
		// (-32602), not a "default to router-preferred" path.
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidParams,
			"invalid initialize params: protocolVersion required", map[string]any{
				"supported": supportedProtocolVersionsList(),
			})
		return
	}
	if _, ok := supportedProtocolVersions[initParams.ProtocolVersion]; !ok {
		// Unsupported version: reject with -32600 at HTTP 200 (JSON-RPC
		// carries the error in-band) + error.data.supported so the client
		// can re-issue initialize with a version the router speaks. The
		// router fronts per-workspace daemons that all share one MCP
		// surface, so silently downgrading to defaultProtocolVersion would
		// hide a real client/server mismatch — same posture as the hub.
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusOK, jsonrpcInvalidRequest,
			"unsupported protocolVersion", map[string]any{
				"supported": supportedProtocolVersionsList(),
				"requested": initParams.ProtocolVersion,
			})
		return
	}
	negotiatedVersion := initParams.ProtocolVersion

	sessionID := newMcpSessionID()
	// Record the minted session + its negotiated version in the
	// authoritative router-session registry (P2 findings 4 + 5 + 7). This
	// is the only place a router session is minted; the tools/list gate,
	// the version-keyed tools/list cache, and the tool-call version
	// enforcement all read from here, and the DELETE teardown unbinds it.
	s.serenaRouterSessions.store(sessionID, negotiatedVersion)
	result := initializeResult{
		ProtocolVersion: negotiatedVersion,
		Capabilities: map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		ServerInfo: map[string]string{
			"name":    serenaServerInfoName,
			"version": serenaServerInfoVersion,
		},
	}
	writeJSONRPCResult(w, tb.ID, result, map[string]string{"Mcp-Session-Id": sessionID})
}

// handleNotificationOrPing answers MCP utility/lifecycle methods that
// need no workspace:
//   - notifications/* WITHOUT an id -> HTTP 202 Accepted, empty body
//     (MCP Streamable HTTP: a POST whose payload is only a notification
//     gets 202 if accepted).
//   - notifications/* WITH an id    -> JSON-RPC -32600 error. An id
//     present on a notification method makes it a JSON-RPC request, and
//     the client will block waiting for a response; a notification can
//     never produce one, so we reject the malformed shape loud instead
//     of acknowledging it (mirrors the hub handler's
//     "notifications/* must not include id" rule in
//     internal/api/hub_mcp_handler.go).
//   - ping WITH an id   -> JSON-RPC result {} (MCP basic/utilities/ping).
//   - ping WITHOUT an id -> HTTP 202, empty body (ping-as-notification;
//     writeJSONRPCResult is not used because a notification gets no
//     response envelope — handled by the caller's notification gate).
//
// Returns true when it handled the method.
//
// NOTE: id-less ping is gated to 202 by the caller (serenaRouterHandler)
// before this runs, so the ping branch here only sees id-bearing pings.
func (s *Server) handleNotificationOrPing(w http.ResponseWriter, tb *toolBody) bool {
	switch {
	case tb.Method == "ping":
		// Reaches here only with an id present (the caller's notification
		// gate already 202'd a ping with no id).
		writeJSONRPCResult(w, tb.ID, map[string]any{}, nil)
		return true
	case isNotificationMethod(tb.Method):
		if !isJSONRPCNotificationID(tb.ID) {
			// notifications/* with an id is a JSON-RPC request that can
			// never be answered. Reject -32600 (Invalid Request) so the
			// client sees the protocol error instead of hanging.
			writeJSONRPCError(w, tb.ID, jsonrpcInvalidRequest,
				"invalid request: notifications/* must not include id")
			return true
		}
		// Genuine notification (no id): no JSON-RPC response.
		w.WriteHeader(http.StatusAccepted)
		return true
	}
	return false
}

// isNotificationMethod reports whether method is an MCP notification
// (the "notifications/" namespace). A genuine notification carries no
// id and the server returns 202 with no body; a notifications/* method
// that DOES carry an id is a malformed request (handled by the caller).
func isNotificationMethod(method string) bool {
	const prefix = "notifications/"
	return len(method) >= len(prefix) && method[:len(prefix)] == prefix
}

// handleToolsList answers a workspace-agnostic tools/list. For the
// cursorless first-page request it serves a cached result when fresh;
// otherwise it proxies one tools/list to a live serena daemon (any
// registered workspace — the surface is identical), caches the daemon's
// `result`, and returns it.
//
// Finding D — require a minted/known router session BEFORE any proxy. A
// client that skipped initialize (or sent a random id) must not be able to
// enumerate tools / trigger handshakes / write the cache. The request's
// Mcp-Session-Id must be present AND minted by a prior initialize at this
// router (serenaRouterSessions); missing/unknown → -32600 at HTTP 400
// (mirrors the hub's "session required"/"unknown session" shape). The
// Phase-3 reconcile probe does initialize ONLY (never tools/list), so this
// gate does not affect it.
//
// Finding E — the cache is keyed by the session's negotiated protocol
// version. The negotiated version (from serenaRouterSessions, the same
// source Finding G uses) keys both the cache read and write AND drives the
// upstream fetch, so a client that negotiated 2025-06-18 cannot be served
// a payload fetched under 2025-11-25.
//
// Finding 1 — reject a tools/list whose MCP-Protocol-Version conflicts with
// the session's negotiated version BEFORE the cache read or proxy, mirroring
// the tool-call path's Finding G and the hub's gate 7
// (internal/api/hub_mcp_handler.go). A missing header uses the session
// version (the resolved version keys the cache + drives the fetch). Without
// this gate a session negotiated as 2025-06-18 could enumerate tools under
// 2025-11-25 and succeed while the hub rejects the same non-initialize
// mismatch.
//
// Cursor-bearing requests bypass the cache entirely (P2 finding 2). The
// workspace-agnostic cache holds only the first page, so reading it for
// a `params.cursor` request would return page 1 for every cursor until
// TTL expiry — the client could never page past the first nextCursor and
// might loop on it. A cursor request is proxied straight to a daemon and
// its (page-N) result is NOT written to the first-page cache. Bypass is
// simpler than keying the cache by cursor and matches serena's
// typically-small, mostly-single-page static tool surface.
//
// Empty-pool case (no workspace registered, so no daemon to fetch from):
// returns an explicit JSON-RPC error (serenaNoWorkspaceCode) rather than
// a fabricated empty tool list. Justification: an empty list would be a
// lie a client could cache forever, never seeing tools appear after the
// operator runs `mcphub workspace register`; a fail-loud error is the
// honest, retryable signal consistent with the router's fail-closed
// posture.
func (s *Server) handleToolsList(
	w http.ResponseWriter,
	r *http.Request,
	deps *serenaRouterDeps,
	tb *toolBody,
	body []byte,
	httpClient *http.Client,
	upstreamURLFn func(ws *api.WorkspaceEntry) string,
	auditFn func(level, event string, fields map[string]any) error,
) {
	// Finding D: require a router session minted by a prior initialize. A
	// missing header or an id this router never minted is rejected -32600
	// at HTTP 400 before any daemon proxy / cache write (mirrors the hub's
	// gate-6 session requirement, internal/api/hub_mcp_handler.go). The
	// negotiated version is captured here and used as the Finding E cache
	// key + the upstream fetch version.
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
			"session-id required (initialize first)", nil)
		return
	}
	negotiatedVersion, known := s.serenaRouterSessions.negotiatedVersion(sessionID)
	if !known {
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
			"unknown session (initialize first)", nil)
		return
	}

	// Finding 1 (mirror the tool-call path's Finding G — serena_router.go —
	// and the hub's gate 7, internal/api/hub_mcp_handler.go:382-392): for a
	// KNOWN router session the session's NEGOTIATED version is the source of
	// truth, NOT the raw per-request header. A request MCP-Protocol-Version
	// that conflicts with the negotiated version is a "protocol-version
	// mismatch" (-32600 at HTTP 400, the hub's exact wording). A missing
	// header is fine — negotiatedVersion is used for both the version-keyed
	// cache lookup and the upstream fetch below, so an omitted header stays
	// consistent with the tool-call path. The session is always known here
	// (Finding D's gate above already rejected an unknown/missing session),
	// so there is no raw-header fallthrough as there is on the tool-call path.
	if clientProtocolVersion := r.Header.Get("MCP-Protocol-Version"); clientProtocolVersion != "" && clientProtocolVersion != negotiatedVersion {
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
			"protocol-version mismatch", nil)
		return
	}

	now := time.Now()
	// A paginated (cursor-bearing) tools/list bypasses the first-page
	// cache on BOTH read and write (P2 finding 2). The cache read/write are
	// keyed by the session's negotiated protocol version (P2 finding 5).
	isCursorRequest := requestedToolsListCursor(body) != ""
	if !isCursorRequest {
		if cached, ok := s.serenaToolsListCache.get(negotiatedVersion, now); ok {
			writeJSONRPCResult(w, tb.ID, json.RawMessage(cached), nil)
			return
		}
	}

	lister, ok := deps.Resolver.(workspaceLister)
	if !ok {
		// Resolver cannot enumerate workspaces — treat as empty pool.
		writeJSONRPCError(w, tb.ID, serenaNoWorkspaceCode,
			"serena tools/list unavailable: workspace enumeration not supported by this router build; register a workspace and retry")
		return
	}

	entries := lister.ListWorkspaces()
	if len(entries) == 0 {
		writeJSONRPCError(w, tb.ID, serenaNoWorkspaceCode,
			"no serena workspace registered: cannot enumerate tools until at least one workspace is registered (run `mcphub workspace register <path>`), then retry tools/list")
		return
	}

	// Proxy one tools/list to the first live daemon. On a per-daemon
	// failure, try the next so a single dead daemon does not blank the
	// whole tool surface. The handshake + the upstream tools/list POST use
	// the SESSION's negotiated protocol version (P1 findings 1 + 5, and P2
	// finding 5 consistency — not the raw per-request header, which Finding
	// G treats as advisory for a known session). fetchToolsListFromAnyDaemon
	// also DELETEs each one-shot upstream daemon session after the proxy so
	// it does not leak until the daemon's idle expiry (P2 finding C).
	result, ferr := s.fetchToolsListFromAnyDaemon(r.Context(), entries, body, httpClient, upstreamURLFn, auditFn, negotiatedVersion)
	if ferr != nil {
		_ = auditFn("warn", "serena-tools-list-fetch-failed", map[string]any{
			"err":             ferr.Error(),
			"workspace_count": len(entries),
		})
		writeJSONRPCError(w, tb.ID, jsonrpcInternalError,
			fmt.Sprintf("serena tools/list: no registered workspace daemon answered (%v); the daemons may still be starting — retry shortly", ferr))
		return
	}

	// Only the cursorless first page is cacheable, keyed by the negotiated
	// protocol version (P2 finding 5). A cursor request's page-N result
	// must NOT overwrite the first-page cache entry.
	if !isCursorRequest {
		s.serenaToolsListCache.put(negotiatedVersion, result, now)
	}
	writeJSONRPCResult(w, tb.ID, json.RawMessage(result), nil)
}

// fetchToolsListFromAnyDaemon forwards the verbatim tools/list body to
// each candidate workspace daemon in turn, returning the first daemon's
// JSON-RPC `result`. It validates that the daemon answered HTTP 200 with
// a JSON-RPC result (not an error); a non-result answer advances to the
// next candidate. Returns the last error when every candidate fails.
//
// clientProtocolVersion is threaded to each proxyToolsListOnce so the
// handshake + tools/list POST carry the version the client negotiated
// (P1 findings 1 + 5).
//
// Finding C: the tools/list handshake mints a fresh ONE-SHOT upstream
// daemon session (the router does not reuse it for later tool calls — those
// re-handshake via serenaDaemonSessions). proxyToolsListOnce returns the
// daemon-issued session id so this loop can best-effort DELETE it after the
// proxy, on BOTH the success path AND the post-handshake error path
// (otherwise the session leaks until the daemon's idle expiry). The DELETE
// is context-detached (a finished/cancelled tools/list request context must
// not abort the teardown), mirroring the main DELETE path.
func (s *Server) fetchToolsListFromAnyDaemon(
	ctx context.Context,
	entries []*api.WorkspaceEntry,
	body []byte,
	httpClient *http.Client,
	upstreamURLFn func(ws *api.WorkspaceEntry) string,
	auditFn func(level, event string, fields map[string]any) error,
	clientProtocolVersion string,
) (json.RawMessage, error) {
	var lastErr error
	for _, ws := range entries {
		if ws == nil {
			continue
		}
		upstreamURL := upstreamURLFn(ws)
		if upstreamURL == "" {
			lastErr = fmt.Errorf("workspace %q: upstream URL resolution failed", ws.WorkspaceKey)
			continue
		}
		result, daemonSessionID, err := proxyToolsListOnce(ctx, httpClient, upstreamURL, body, clientProtocolVersion)
		// Finding C: release the one-shot upstream session regardless of the
		// proxy outcome, whenever the handshake established one. Detach the
		// context so a done tools/list request context cannot abort it.
		//
		// Finding 2 (V-forward): the teardown DELETE carries the SAME resolved
		// protocol version proxyToolsListOnce used for its handshake
		// (effectiveHandshakeProtocolVersion), so a strict daemon that binds
		// the header to the session's initialized version does not 400 the
		// teardown and leak the one-shot session.
		if daemonSessionID != "" {
			delCtx, delCancel := context.WithTimeout(context.Background(), serenaUpstreamTimeout)
			s.forwardSerenaDeleteUpstream(delCtx, httpClient, upstreamURL, daemonSessionID, effectiveHandshakeProtocolVersion(clientProtocolVersion), ws.WorkspaceKey, ws, auditFn)
			delCancel()
		}
		if err != nil {
			lastErr = fmt.Errorf("workspace %q (port %d): %w", ws.WorkspaceKey, ws.Port, err)
			_ = auditFn("warn", "serena-tools-list-daemon-skip", map[string]any{
				"workspace_key": ws.WorkspaceKey,
				"port":          ws.Port,
				"err":           err.Error(),
			})
			continue
		}
		return result, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no candidate workspace daemon available")
	}
	return nil, lastErr
}

// proxyToolsListOnce POSTs the tools/list body to one daemon's /mcp
// endpoint and extracts the JSON-RPC `result`. It accepts both a plain
// JSON body and a single-event SSE body (serena daemons may answer either
// per the MCP Streamable HTTP transport). A 1 MiB read cap bounds the
// response.
//
// Before the tools/list POST it performs the MCP handshake with the
// daemon (initialize → notifications/initialized) and sends the
// daemon-issued Mcp-Session-Id on the tools/list request (P1). A healthy
// serena / native-http daemon requires a session for every non-initialize
// POST, so a bare tools/list with no preceding initialize would be
// rejected with a 400 / JSON-RPC session error. A handshake failure (dead
// daemon) surfaces as the per-candidate error so fetchToolsListFromAnyDaemon
// advances to the next daemon. A sessionless daemon yields an empty
// session id, in which case tools/list is sent with no Mcp-Session-Id
// (back-compat).
//
// clientProtocolVersion (P1 findings 1 + 5) is the version the client put
// on the tools/list request's MCP-Protocol-Version header (empty for an
// older client). The handshake AND the tools/list POST use the SAME
// resolved version so the daemon's session is initialized at the version
// the tools/list header advertises — a strict daemon binding the header
// to the session's initialized version (see
// internal/api/hub_mcp_handler.go gate 7) rejects a non-initialize POST
// whose header is missing OR mismatched. The header is omitted only when
// the resolved version is empty (cannot happen — the fallback default is
// non-empty).
//
// Finding C: it returns the daemon-issued session id (the second return
// value) so the caller can DELETE the one-shot session afterwards. The id
// is returned on every path AFTER a successful handshake — including the
// post-handshake error paths — so the caller tears it down even when the
// tools/list POST itself fails. A handshake FAILURE returns "" (no upstream
// session was minted, so there is nothing to release); a sessionless daemon
// also returns "" (it issued no id).
func proxyToolsListOnce(ctx context.Context, httpClient *http.Client, upstreamURL string, body []byte, clientProtocolVersion string) (json.RawMessage, string, error) {
	protocolVersion := effectiveHandshakeProtocolVersion(clientProtocolVersion)
	daemonSessionID, hsErr := establishDaemonSession(ctx, httpClient, upstreamURL, protocolVersion)
	if hsErr != nil {
		return nil, "", fmt.Errorf("upstream handshake: %w", hsErr)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, daemonSessionID, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Accept both shapes; daemons that only emit SSE need the stream
	// Accept, plain-JSON daemons ignore the extra type.
	req.Header.Set("Accept", "application/json, text/event-stream")
	// tools/list is a non-initialize POST: send the negotiated protocol
	// version so a strict daemon does not reject the missing header
	// (P1 finding 5). Same version as the handshake above.
	if protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
	}
	if daemonSessionID != "" {
		req.Header.Set("Mcp-Session-Id", daemonSessionID)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, daemonSessionID, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, daemonSessionID, fmt.Errorf("upstream tools/list -> status %d", resp.StatusCode)
	}
	// Finding 4: read INCREMENTALLY (same ReadAll-to-EOF hang as the
	// handshake — a daemon that answers tools/list as a still-open SSE
	// stream would otherwise block until the client timeout).
	// readUpstreamJSONRPCResponse returns at the first JSON-RPC response
	// event for SSE, or a bounded read for application/json.
	payload, rerr := readUpstreamJSONRPCResponse(resp.Header.Get("Content-Type"), resp.Body)
	if rerr != nil {
		return nil, daemonSessionID, fmt.Errorf("read upstream tools/list: %w", rerr)
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &rpc); err != nil {
		return nil, daemonSessionID, fmt.Errorf("upstream tools/list non-JSON-RPC body: %w", err)
	}
	if len(rpc.Error) > 0 {
		return nil, daemonSessionID, fmt.Errorf("upstream tools/list returned JSON-RPC error: %s", string(rpc.Error))
	}
	if len(rpc.Result) == 0 {
		return nil, daemonSessionID, fmt.Errorf("upstream tools/list returned no result")
	}
	return rpc.Result, daemonSessionID, nil
}

// bytesContainsFold is a tiny case-insensitive substring check for
// content-type matching (avoids importing strings just for ToLower in
// this file; the value is already lowercased by most servers but we
// fold defensively).
func bytesContainsFold(haystack, needle string) bool {
	return bytes.Contains(bytes.ToLower([]byte(haystack)), bytes.ToLower([]byte(needle)))
}

// readUpstreamJSONRPCResponse reads a daemon's upstream MCP response body
// and returns the JSON-RPC envelope bytes to parse, bounded by a 1 MiB
// defensive cap. It is the read used by the synthesized handshake
// (postHandshakeInitialize) and the workspace-agnostic tools/list proxy
// (proxyToolsListOnce) — both expect a single JSON-RPC RESPONSE.
//
// Finding 4 — for a text/event-stream body it parses the stream
// INCREMENTALLY and returns as soon as it assembles the FIRST complete
// JSON-RPC response event, WITHOUT waiting for the stream to close. A
// Streamable-HTTP daemon may keep its SSE connection open after emitting
// the response event (to push later notifications); the pre-fix code did
// io.ReadAll(resp.Body) to EOF and then flattened with extractJSONRPCPayload,
// so the handshake blocked until the client timeout (60s) on EVERY first
// tool-call / tools-list against such a daemon. This mirrors the hub
// aggregator's readSSEResponse + selectJSONRPCResponse
// (internal/api/hub_mcp_aggregator.go) — those helpers are unexported in
// package api (the router lives in package gui), so the technique is
// replicated here rather than imported. A non-SSE (application/json)
// body keeps a bounded read (io.ReadAll under the same cap).
func readUpstreamJSONRPCResponse(contentType string, body io.Reader) ([]byte, error) {
	const maxBytes = 1 << 20
	if bytesContainsFold(contentType, "text/event-stream") {
		return readSSEJSONRPCResponse(body, maxBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(body, maxBytes))
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// readSSEJSONRPCResponse scans a text/event-stream body line-by-line and
// returns the data of the FIRST complete event whose payload is a JSON-RPC
// RESPONSE envelope (jsonrpc=="2.0" with a non-empty `id` and no `method`).
// It stops at that event without draining the stream to EOF (Finding 4), so
// a daemon that holds the connection open after the response does not stall
// the handshake. It mirrors internal/api/hub_mcp_aggregator.go's
// readSSEResponse byte-for-byte in technique (bufio.Scanner with a maxBytes
// buffer; blank line dispatches an event; non-data fields and `:` comments
// are skipped; multiple `data:` lines within one event join with "\n"; one
// optional leading space stripped from each value); notification events
// (id absent / method present) are skipped so the caller keeps reading for
// the actual response. maxBytes caps total bytes so a runaway daemon cannot
// OOM the router.
func readSSEJSONRPCResponse(r io.Reader, maxBytes int) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	// Allow a single line up to maxBytes — some daemons emit one large
	// `data:` line carrying the full JSON envelope.
	scanner.Buffer(make([]byte, 64*1024), maxBytes+1)

	var dataLines [][]byte
	totalBytes := 0
	for scanner.Scan() {
		raw := scanner.Bytes()
		totalBytes += len(raw) + 1 // +1 for the LF the scanner consumed
		if totalBytes > maxBytes {
			return nil, fmt.Errorf("SSE response too large (> %d bytes)", maxBytes)
		}
		line := bytes.TrimSuffix(raw, []byte("\r"))

		if len(line) == 0 {
			// Blank line: dispatch the accumulated event (SSE boundary).
			if len(dataLines) > 0 {
				if payload, ok := selectSSEJSONRPCResponse(dataLines); ok {
					return payload, nil
				}
				dataLines = nil
			}
			continue
		}
		if line[0] == ':' {
			// SSE comment line — ignored.
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			// Other SSE field (`event:`, `id:`, `retry:`, unknown).
			continue
		}
		value := line[len("data:"):]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		// Copy: the scanner reuses its buffer on the next Scan, so retaining
		// the slice across iterations would corrupt the accumulated event.
		valueCopy := make([]byte, len(value))
		copy(valueCopy, value)
		dataLines = append(dataLines, valueCopy)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read SSE: %w", err)
	}
	// Stream ended without a trailing blank line — dispatch any final event.
	if len(dataLines) > 0 {
		if payload, ok := selectSSEJSONRPCResponse(dataLines); ok {
			return payload, nil
		}
	}
	return nil, errors.New("SSE stream ended without a JSON-RPC response event")
}

// selectSSEJSONRPCResponse joins the accumulated `data:` lines of one SSE
// event with "\n" and returns the payload IFF it is a JSON-RPC RESPONSE
// envelope (jsonrpc=="2.0", non-empty `id`, no `method`). Notifications
// (id absent, method present) and unrelated events are rejected so the
// caller keeps reading. Mirrors internal/api/hub_mcp_aggregator.go's
// selectJSONRPCResponse.
func selectSSEJSONRPCResponse(dataLines [][]byte) ([]byte, bool) {
	payload := bytes.Join(dataLines, []byte("\n"))
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, false
	}
	if env.JSONRPC != "2.0" {
		return nil, false
	}
	// JSON-RPC responses ALWAYS carry an `id` and never a `method`.
	if len(env.ID) == 0 || env.Method != "" {
		return nil, false
	}
	return payload, true
}
