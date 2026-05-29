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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// toolsListCache caches the workspace-agnostic tools/list result (the raw
// JSON-RPC `result` object) fetched from any live serena daemon. The
// cache is process-local and lives on the Server so it survives across
// requests. It is workspace-agnostic by construction (design §2.1 /
// O1(a): one daemon per workspace, identical MCP surface), so a single
// cached entry serves every client regardless of which workspace daemon
// answered the fetch.
type toolsListCache struct {
	mu       sync.Mutex
	result   json.RawMessage
	fetched  time.Time
	cacheTTL time.Duration // 0 -> toolsListCacheTTL; overridable in tests
}

// get returns the cached result and true when a non-expired entry
// exists.
func (c *toolsListCache) get(now time.Time) (json.RawMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ttl := c.cacheTTL
	if ttl <= 0 {
		ttl = toolsListCacheTTL
	}
	if len(c.result) == 0 {
		return nil, false
	}
	if now.Sub(c.fetched) > ttl {
		return nil, false
	}
	out := make(json.RawMessage, len(c.result))
	copy(out, c.result)
	return out, true
}

// put stores result as the current cached tools/list payload.
func (c *toolsListCache) put(result json.RawMessage, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.result = append(json.RawMessage(nil), result...)
	c.fetched = now
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

// negotiateProtocolVersion echoes the client's requested protocolVersion
// when the router supports it, else returns defaultProtocolVersion.
func negotiateProtocolVersion(requested string) string {
	if requested == "" {
		return defaultProtocolVersion
	}
	if _, ok := supportedProtocolVersions[requested]; ok {
		return requested
	}
	return defaultProtocolVersion
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

// requestedProtocolVersion pulls params.protocolVersion out of a raw MCP
// initialize body without disturbing the toolBody decode used for tool
// routing. Best-effort: a malformed/absent field yields "".
func requestedProtocolVersion(body []byte) string {
	var probe struct {
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Params.ProtocolVersion
}

// handleInitialize synthesizes the InitializeResult and mints a session
// id. Workspace-agnostic: no daemon is contacted (design §5) — every
// serena daemon exposes the same lifecycle surface, and the session is
// bound to a concrete workspace only on the first path-bearing
// tools/call.
func (s *Server) handleInitialize(w http.ResponseWriter, body []byte, tb *toolBody, incomingSessionID string) {
	sessionID := incomingSessionID
	if sessionID == "" {
		sessionID = newMcpSessionID()
	}
	result := initializeResult{
		ProtocolVersion: negotiateProtocolVersion(requestedProtocolVersion(body)),
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

// handleToolsList answers a workspace-agnostic tools/list. It serves a
// cached result when fresh; otherwise it proxies one tools/list to a live
// serena daemon (any registered workspace — the surface is identical),
// caches the daemon's `result`, and returns it.
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
	now := time.Now()
	if cached, ok := s.serenaToolsListCache.get(now); ok {
		writeJSONRPCResult(w, tb.ID, json.RawMessage(cached), nil)
		return
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
	// whole tool surface.
	result, ferr := s.fetchToolsListFromAnyDaemon(r.Context(), entries, body, httpClient, upstreamURLFn, auditFn)
	if ferr != nil {
		_ = auditFn("warn", "serena-tools-list-fetch-failed", map[string]any{
			"err":             ferr.Error(),
			"workspace_count": len(entries),
		})
		writeJSONRPCError(w, tb.ID, jsonrpcInternalError,
			fmt.Sprintf("serena tools/list: no registered workspace daemon answered (%v); the daemons may still be starting — retry shortly", ferr))
		return
	}

	s.serenaToolsListCache.put(result, now)
	writeJSONRPCResult(w, tb.ID, json.RawMessage(result), nil)
}

// fetchToolsListFromAnyDaemon forwards the verbatim tools/list body to
// each candidate workspace daemon in turn, returning the first daemon's
// JSON-RPC `result`. It validates that the daemon answered HTTP 200 with
// a JSON-RPC result (not an error); a non-result answer advances to the
// next candidate. Returns the last error when every candidate fails.
func (s *Server) fetchToolsListFromAnyDaemon(
	ctx context.Context,
	entries []*api.WorkspaceEntry,
	body []byte,
	httpClient *http.Client,
	upstreamURLFn func(ws *api.WorkspaceEntry) string,
	auditFn func(level, event string, fields map[string]any) error,
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
		result, err := proxyToolsListOnce(ctx, httpClient, upstreamURL, body)
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
func proxyToolsListOnce(ctx context.Context, httpClient *http.Client, upstreamURL string, body []byte) (json.RawMessage, error) {
	daemonSessionID, hsErr := establishDaemonSession(ctx, httpClient, upstreamURL)
	if hsErr != nil {
		return nil, fmt.Errorf("upstream handshake: %w", hsErr)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Accept both shapes; daemons that only emit SSE need the stream
	// Accept, plain-JSON daemons ignore the extra type.
	req.Header.Set("Accept", "application/json, text/event-stream")
	if daemonSessionID != "" {
		req.Header.Set("Mcp-Session-Id", daemonSessionID)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream tools/list -> status %d", resp.StatusCode)
	}
	payload := extractJSONRPCPayload(resp.Header.Get("Content-Type"), raw)
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &rpc); err != nil {
		return nil, fmt.Errorf("upstream tools/list non-JSON-RPC body: %w", err)
	}
	if len(rpc.Error) > 0 {
		return nil, fmt.Errorf("upstream tools/list returned JSON-RPC error: %s", string(rpc.Error))
	}
	if len(rpc.Result) == 0 {
		return nil, fmt.Errorf("upstream tools/list returned no result")
	}
	return rpc.Result, nil
}

// extractJSONRPCPayload returns the JSON object to parse from an upstream
// response. For a text/event-stream body it pulls the last non-empty
// `data:` line's JSON (the terminal MCP message of a single-event
// stream); for any other content type it returns raw verbatim.
func extractJSONRPCPayload(contentType string, raw []byte) []byte {
	if !bytesContainsFold(contentType, "text/event-stream") {
		return raw
	}
	var last []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		const dataPrefix = "data:"
		if !bytes.HasPrefix(line, []byte(dataPrefix)) {
			continue
		}
		payload := bytes.TrimSpace(line[len(dataPrefix):])
		if len(payload) > 0 {
			last = payload
		}
	}
	if len(last) == 0 {
		return raw
	}
	return last
}

// bytesContainsFold is a tiny case-insensitive substring check for
// content-type matching (avoids importing strings just for ToLower in
// this file; the value is already lowercased by most servers but we
// fold defensively).
func bytesContainsFold(haystack, needle string) bool {
	return bytes.Contains(bytes.ToLower([]byte(haystack)), bytes.ToLower([]byte(needle)))
}
