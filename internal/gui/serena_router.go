// internal/gui/serena_router.go
//
// POST /serena/mcp -- path-aware MCP request router for per-workspace
// serena daemons (plan v10 Phase C.2).
package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/serena_routing"
)

// ErrWorkspaceNotFound re-exports the A1 package sentinel so callers
// outside `internal/api/serena_routing` can `errors.Is(err, gui.ErrWorkspaceNotFound)`
// without importing the routing package directly. Both names refer to
// the SAME underlying error value — `errors.Is` works across the
// re-export boundary.
var ErrWorkspaceNotFound = serena_routing.ErrWorkspaceNotFound

// workspaceResolver is the narrow interface the handler uses to map
// an absolute or workspace-relative path to its owning workspace.
type workspaceResolver interface {
	ResolveByPath(path string) (*api.WorkspaceEntry, error)
}

// sessionRouter is the narrow interface for sticky session binding.
type sessionRouter interface {
	BindSession(sessionID string, ws *api.WorkspaceEntry)
	LookupSession(sessionID string) *api.WorkspaceEntry
	UnbindSession(sessionID string)
}

// serenaRouterDeps bundles the test seams so callers swap them
// atomically. Tests inject fakes via serenaRouterTestSeam.
type serenaRouterDeps struct {
	Resolver        workspaceResolver
	Sessions        sessionRouter
	HTTPClient      *http.Client
	UpstreamURLFn   func(ws *api.WorkspaceEntry) string
	AuditFn         func(level, event string, fields map[string]any) error
	UpstreamTimeout time.Duration
}

// serenaRouterTestSeam lets tests inject a fully-mocked deps bundle.
var serenaRouterTestSeam func() *serenaRouterDeps

// serenaUpstreamTimeout caps the per-forward connect + first-byte
// budget. Matches the 60s ceiling HTTPHost's httpClient uses for
// tool-call traffic. Tests override via UpstreamTimeout.
const serenaUpstreamTimeout = 60 * time.Second

func newSerenaTransport(upstreamTimeout time.Duration) *http.Transport {
	if upstreamTimeout <= 0 {
		upstreamTimeout = serenaUpstreamTimeout
	}
	dialTimeout := 5 * time.Second
	if upstreamTimeout < dialTimeout {
		dialTimeout = upstreamTimeout
	}
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: dialTimeout,
		}).DialContext,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: upstreamTimeout,
		DisableCompression:    true,
	}
}

// defaultSerenaClient is the production http.Client. Client.Timeout
// stays zero so a long-lived SSE stream is not killed mid-flight; the
// transport's dial timeout + ResponseHeaderTimeout enforce the
// connect + first-byte budget independently from body streaming.
var defaultSerenaClient = &http.Client{
	Timeout:   0,
	Transport: newSerenaTransport(serenaUpstreamTimeout),
}

func serenaHTTPClient(httpClient *http.Client, upstreamTimeout time.Duration) *http.Client {
	if httpClient != nil {
		return httpClient
	}
	if upstreamTimeout == serenaUpstreamTimeout {
		return defaultSerenaClient
	}
	return &http.Client{
		Timeout:   0,
		Transport: newSerenaTransport(upstreamTimeout),
	}
}

// serenaRouterDepsProd returns the production deps bundle. When the
// test seam is set, returns its output; otherwise reads the Server's
// atomic deps slot. A nil return means the routing layer is not yet
// wired -- the handler then emits 503 with the canonical body.
func (s *Server) serenaRouterDepsProd() *serenaRouterDeps {
	if serenaRouterTestSeam != nil {
		return serenaRouterTestSeam()
	}
	return s.serenaRouterDeps.Load()
}

// SetSerenaRouterProduction wires the production resolver + session
// router from A1's serena_routing package. Callable from internal/cli
// at GUI server construction time (cannot construct the unexported
// serenaRouterDeps from outside this package, so this exported helper
// fills the role).
func (s *Server) SetSerenaRouterProduction(resolver *serena_routing.WorkspaceResolver, sessions *serena_routing.SessionRouter) {
	if s == nil || resolver == nil || sessions == nil {
		return
	}
	s.SetSerenaRouterDeps(&serenaRouterDeps{
		Resolver: resolver,
		Sessions: sessions,
	})
}

// SetSerenaRouterDeps wires the production resolver + session router.
// CLI boot (cmd/mcphub) calls this after constructing Agent A1's
// adapters from the live api.Registry. Calling with nil clears the
// wiring (the route then emits 503).
func (s *Server) SetSerenaRouterDeps(deps *serenaRouterDeps) {
	s.serenaRouterDeps.Store(deps)
}

// NewInMemorySessionRouter returns a process-local sessionRouter for
// production callers who want sticky-session binding without depending
// on Agent A1's serena_routing package directly.
func NewInMemorySessionRouter() *InMemorySessionRouter {
	return newInMemorySessionRouter()
}

func registerSerenaRouterRoutes(s *Server) {
	s.mux.HandleFunc("/serena/mcp", s.requireSameOrigin(s.serenaRouterHandler))
}

// toolBody is the narrow shape we decode from the incoming MCP body.
// We re-encode the raw bytes verbatim when forwarding, so this struct
// only carries the fields we need to make routing decisions.
type toolBody struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	ID      json.RawMessage `json:"id"`
	Params  struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

// extractPathArg scans the tool body's arguments for any of the known
// serena path-arg field names. Returns ("", false) when none is
// present or all are empty strings.
//
// Field set (per plan section C.2 + the serena tool schema):
//   - relative_path : symbol/file ops
//   - file_path     : edit_file, read_file
//   - name_path     : insert_after_symbol, replace_symbol_body
//   - path          : list_dir, search_for_pattern
func extractPathArg(arguments json.RawMessage) (string, bool) {
	if len(arguments) == 0 {
		return "", false
	}
	var args map[string]json.RawMessage
	if uerr := json.Unmarshal(arguments, &args); uerr != nil {
		return "", false
	}
	for _, key := range []string{"relative_path", "file_path", "name_path", "path"} {
		raw, ok := args[key]
		if !ok {
			continue
		}
		var v string
		if uerr := json.Unmarshal(raw, &v); uerr != nil {
			continue
		}
		if v == "" {
			continue
		}
		return v, true
	}
	return "", false
}

// notFoundJSON is the canonical body returned on workspace-not-found.
type notFoundJSON struct {
	Error          string `json:"error"`
	PhaseEStatus   string `json:"phase_e_status"`
	HintCommand    string `json:"hint_command,omitempty"`
	ResolvedPath   string `json:"resolved_path,omitempty"`
	MissingSession bool   `json:"missing_session,omitempty"`
}

// writeWorkspaceNotFound emits the 503 with the canonical body.
func writeWorkspaceNotFound(w http.ResponseWriter, resolvedPath string, missingSession bool) {
	body := notFoundJSON{
		Error:        "register workspace first via mcphub workspace register <path>",
		PhaseEStatus: "deferred",
		HintCommand:  "mcphub workspace register <path>",
	}
	if resolvedPath != "" {
		body.ResolvedPath = resolvedPath
	}
	if missingSession {
		body.MissingSession = true
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(body)
}

func writeRequiredFieldError(w http.ResponseWriter, field string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = fmt.Fprintf(w, `{"error": "missing required field: %s"}`, field)
}

func (s *Server) serenaRouterHandler(w http.ResponseWriter, r *http.Request) {
	// DELETE is the MCP Streamable HTTP client-origin session-termination
	// method (Finding 3). A Streamable HTTP client sends DELETE /serena/mcp
	// on shutdown; without this branch it would 405 and leak the upstream
	// daemon session + the router-owned bindings until idle expiry. GET and
	// every other non-POST method keep their current 405 (Allow: POST).
	if r.Method == http.MethodDelete {
		s.handleSerenaDelete(w, r)
		return
	}
	if r.Method != http.MethodPost {
		// Finding H: DELETE is handled above (client-origin session
		// termination), so the 405 fallback for every OTHER non-POST method
		// advertises both verbs the route accepts (mirrors the hub's
		// `Allow: POST, DELETE`, internal/api/hub_mcp_handler.go).
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	deps := s.serenaRouterDepsProd()
	if deps == nil || deps.Resolver == nil {
		writeWorkspaceNotFound(w, "", false)
		return
	}
	auditFn := deps.AuditFn
	if auditFn == nil {
		auditFn = api.LogHubMcpEvent
	}
	upstreamURLFn := deps.UpstreamURLFn
	if upstreamURLFn == nil {
		upstreamURLFn = defaultUpstreamURL
	}
	upstreamTimeout := deps.UpstreamTimeout
	if upstreamTimeout <= 0 {
		upstreamTimeout = serenaUpstreamTimeout
	}
	httpClient := serenaHTTPClient(deps.HTTPClient, upstreamTimeout)

	const maxBodyBytes = 4 << 20
	body, rerr := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if rerr != nil {
		http.Error(w, "read body: "+rerr.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxBodyBytes {
		http.Error(w, "request body exceeds 4 MiB", http.StatusBadRequest)
		return
	}

	var tb toolBody
	if uerr := json.Unmarshal(body, &tb); uerr != nil {
		http.Error(w, "malformed JSON body: "+uerr.Error(), http.StatusBadRequest)
		return
	}
	if tb.Method == "" {
		http.Error(w, "missing required field: method", http.StatusBadRequest)
		return
	}

	// Finding F (mirror internal/api/hub_mcp_handler.go handlePost): the
	// lifecycle/tool dispatch below switches solely on `method`, so a body
	// with the wrong (or absent) jsonrpc version would still mint a session
	// / synthesize a 2.0 response. Reject jsonrpc != "2.0" up front with
	// -32600 at HTTP 400, exactly as the hub does before its method
	// dispatch. The reconcile readiness probe sends "jsonrpc":"2.0", so it
	// is unaffected. The id is echoed only when it is a valid request id
	// (MCP §1.5: a malformed/absent id is rendered as null, never echoed).
	if tb.JSONRPC != "2.0" {
		echoID := tb.ID
		if !isValidJSONRPCRequestID(echoID) {
			echoID = nil
		}
		writeJSONRPCErrorStatus(w, echoID, http.StatusBadRequest, jsonrpcInvalidRequest,
			"invalid request: jsonrpc must be \"2.0\"", nil)
		return
	}

	sessionID := r.Header.Get("Mcp-Session-Id")

	// MCP session lifecycle (non-tool, workspace-agnostic) is synthesized
	// at the router BEFORE the params.name tool-routing requirement. Every
	// serena daemon exposes the same lifecycle surface and the router has
	// no per-client identity, so initialize/tools/list/notifications/ping
	// are answered here rather than forwarded to a workspace daemon (they
	// carry no path-arg and no bound session). Tool calls fall through to
	// the path-routing + upstream-forward path below, unchanged.
	//
	// JSON-RPC id gate (mirrors internal/api/hub_mcp_handler.go): a
	// request-style lifecycle method (initialize / tools/list / ping)
	// can arrive in three id shapes, and each MUST be handled
	// differently —
	//   - id ABSENT  -> JSON-RPC notification: MUST NOT receive a
	//     response envelope; return 202 + empty body. Without this,
	//     handleInitialize / handleToolsList would synthesize a result
	//     with id:null and a strict client would treat the unexpected
	//     response as a protocol error.
	//   - id PRESENT but INVALID (null / boolean / array / object):
	//     MCP §1.5 requires a request id to be a non-null String/Number,
	//     so this is a malformed request -> -32600 Invalid Request. The
	//     pre-fix path treated ANY present id as valid and would BOTH
	//     synthesize a response AND (for initialize) mint a session for
	//     `{"id":null,...}` — surfacing nothing of the client's protocol
	//     bug. (notifications/* is handled inside handleNotificationOrPing,
	//     which already rejects an id-bearing notifications/* with -32600.)
	//   - id PRESENT and VALID (String/Number): synthesize the response.
	switch {
	case tb.Method == "initialize":
		if isJSONRPCNotificationID(tb.ID) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if !isValidJSONRPCRequestID(tb.ID) {
			writeJSONRPCError(w, nil, jsonrpcInvalidRequest,
				"invalid request: id must be a non-null string or number")
			return
		}
		s.handleInitialize(w, body, &tb, sessionID)
		return
	case tb.Method == "tools/list":
		if isJSONRPCNotificationID(tb.ID) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if !isValidJSONRPCRequestID(tb.ID) {
			writeJSONRPCError(w, nil, jsonrpcInvalidRequest,
				"invalid request: id must be a non-null string or number")
			return
		}
		s.handleToolsList(w, r, deps, &tb, body, httpClient, upstreamURLFn, auditFn)
		return
	case tb.Method == "ping":
		if isJSONRPCNotificationID(tb.ID) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if !isValidJSONRPCRequestID(tb.ID) {
			writeJSONRPCError(w, nil, jsonrpcInvalidRequest,
				"invalid request: id must be a non-null string or number")
			return
		}
		s.handleNotificationOrPing(w, &tb)
		return
	case tb.Method == "notifications/cancelled":
		// Finding H (mirror internal/api/hub_mcp_handler.go's
		// notifications/cancelled fan-out): an id-less notifications/cancelled
		// must reach the workspace daemon so an in-flight serena tool call is
		// actually cancelled. The generic notification branch below would
		// answer a local 202 and never forward it, silently ignoring the
		// cancellation. Route it through the client session's
		// serenaDaemonSessions binding instead. An id-bearing
		// notifications/cancelled stays a -32600 malformed-request (handled
		// inside handleSerenaCancelled), and a cancel with no bound daemon
		// session keeps the local 202.
		s.handleSerenaCancelled(w, r, deps, &tb, body, httpClient, upstreamURLFn, auditFn, sessionID)
		return
	case isNotificationMethod(tb.Method):
		// notifications/* : 202 when id-less, -32600 when id-bearing.
		s.handleNotificationOrPing(w, &tb)
		return
	}

	if tb.Params.Name == "" {
		writeRequiredFieldError(w, "params.name")
		return
	}

	pathArg, hasPath := extractPathArg(tb.Params.Arguments)

	var ws *api.WorkspaceEntry
	bindSessionAfterUpstream := false
	if hasPath {
		resolved, resolveErr := deps.Resolver.ResolveByPath(pathArg)
		if resolveErr != nil {
			if errors.Is(resolveErr, ErrWorkspaceNotFound) {
				writeWorkspaceNotFound(w, pathArg, false)
				return
			}
			http.Error(w, "resolve workspace: "+resolveErr.Error(), http.StatusInternalServerError)
			return
		}
		if resolved == nil {
			writeWorkspaceNotFound(w, pathArg, false)
			return
		}
		ws = resolved
		if sessionID != "" && deps.Sessions != nil {
			bindSessionAfterUpstream = true
		}
	} else {
		if sessionID == "" || deps.Sessions == nil {
			writeWorkspaceNotFound(w, "", true)
			return
		}
		ws = deps.Sessions.LookupSession(sessionID)
		if ws == nil {
			writeWorkspaceNotFound(w, "", true)
			return
		}
	}

	upstreamURL := upstreamURLFn(ws)
	if upstreamURL == "" {
		http.Error(w, "upstream URL resolution failed", http.StatusInternalServerError)
		return
	}

	// Bind the client session to a REAL upstream daemon session (P1).
	// The router synthesized `initialize` for the client and minted the
	// client-facing Mcp-Session-Id; the workspace daemon has never seen
	// that id. Establish (lazily, once per client-session×workspace) an
	// upstream MCP session WITH the daemon — initialize →
	// notifications/initialized — and forward this and subsequent tool
	// calls with the daemon-issued id, NOT the router-minted client id.
	// A handshake transport failure is the same failure class as an
	// unreachable tool-call forward (502/504), audited identically so a
	// dead/slow daemon is diagnosable rather than yielding an opaque
	// "unknown session" rejection.
	//
	// The negotiated MCP-Protocol-Version (P1 finding 1 + Finding G) is
	// threaded into the handshake so the daemon session's initialized
	// version matches the version this same request (and subsequent
	// tool-calls) forward verbatim below — otherwise a strict daemon that
	// binds the header to the session's initialized version rejects the
	// first tool-call as a protocol-version mismatch when the client
	// negotiated a non-default supported revision.
	//
	// Finding G (mirror internal/api/hub_mcp_handler.go gate 7): when the
	// id was minted by a prior initialize at this router, the session's
	// NEGOTIATED version — not the raw per-request header — is the source
	// of truth. A request header that conflicts with the known session's
	// negotiated version is a "protocol-version mismatch" (-32600 at HTTP
	// 400, the hub's exact wording); a missing header is fine (the session
	// version is used). When there is NO router session for the id (a
	// tool-call that never initialized at this router — e.g. an older
	// direct caller), today's behavior is preserved: the raw request header
	// drives the handshake.
	clientProtocolVersion := r.Header.Get("MCP-Protocol-Version")
	if sessionVersion, known := s.serenaRouterSessions.negotiatedVersion(sessionID); known {
		if clientProtocolVersion != "" && clientProtocolVersion != sessionVersion {
			writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
				"protocol-version mismatch", nil)
			return
		}
		// Use the session's negotiated version for the upstream handshake so
		// the daemon session is established under the consistent version even
		// when this request omitted the header.
		clientProtocolVersion = sessionVersion
	}
	daemonSessionID, hsErr := s.serenaDaemonSessions.resolveDaemonSession(r.Context(), httpClient, upstreamURL, sessionID, ws, clientProtocolVersion)
	if hsErr != nil {
		if isTimeoutErr(hsErr) {
			_ = auditFn("warn", "serena-upstream-timeout", map[string]any{
				"workspace_key": ws.WorkspaceKey,
				"port":          ws.Port,
				"upstream_url":  upstreamURL,
				"timeout_secs":  int(upstreamTimeout / time.Second),
				"phase":         "handshake",
				"err":           hsErr.Error(),
			})
			http.Error(w, fmt.Sprintf("upstream serena daemon at port %d did not respond to MCP handshake within %ds", ws.Port, int(upstreamTimeout/time.Second)), http.StatusGatewayTimeout)
			return
		}
		_ = auditFn("warn", "serena-upstream-unreachable", map[string]any{
			"workspace_key": ws.WorkspaceKey,
			"port":          ws.Port,
			"upstream_url":  upstreamURL,
			"phase":         "handshake",
			"err":           hsErr.Error(),
		})
		http.Error(w, fmt.Sprintf("upstream serena daemon at port %d MCP handshake failed: %s", ws.Port, hsErr.Error()), http.StatusBadGateway)
		return
	}

	upstreamReq, ureqErr := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if ureqErr != nil {
		http.Error(w, "build upstream request: "+ureqErr.Error(), http.StatusInternalServerError)
		return
	}
	// Thread Content-Type/Accept/protocol-version through verbatim, but
	// NOT the client's Mcp-Session-Id: the daemon does not know the
	// router-minted client id. Set the daemon-issued session id instead
	// (omit it entirely when the daemon is sessionless and issued none).
	for _, h := range []string{"Content-Type", "Accept", "MCP-Protocol-Version"} {
		if v := r.Header.Get(h); v != "" {
			upstreamReq.Header.Set(h, v)
		}
	}
	if daemonSessionID != "" {
		upstreamReq.Header.Set("Mcp-Session-Id", daemonSessionID)
	}
	if upstreamReq.Header.Get("Content-Type") == "" {
		upstreamReq.Header.Set("Content-Type", "application/json")
	}

	upstreamResp, doErr := httpClient.Do(upstreamReq)
	if doErr != nil {
		if isTimeoutErr(doErr) {
			_ = auditFn("warn", "serena-upstream-timeout", map[string]any{
				"workspace_key": ws.WorkspaceKey,
				"port":          ws.Port,
				"upstream_url":  upstreamURL,
				"timeout_secs":  int(upstreamTimeout / time.Second),
				"err":           doErr.Error(),
			})
			http.Error(w, fmt.Sprintf("upstream serena daemon at port %d did not respond within %ds", ws.Port, int(upstreamTimeout/time.Second)), http.StatusGatewayTimeout)
			return
		}
		_ = auditFn("warn", "serena-upstream-unreachable", map[string]any{
			"workspace_key": ws.WorkspaceKey,
			"port":          ws.Port,
			"upstream_url":  upstreamURL,
			"err":           doErr.Error(),
		})
		http.Error(w, fmt.Sprintf("upstream serena daemon at port %d unreachable: %s", ws.Port, doErr.Error()), http.StatusBadGateway)
		return
	}
	defer upstreamResp.Body.Close()

	copyHeaders(w.Header(), upstreamResp.Header)

	// The daemon's response carries the DAEMON's Mcp-Session-Id, which is
	// an internal router↔daemon detail (P1 multiplexing). The client must
	// keep using its OWN router-minted session id, so we never surface the
	// daemon id downstream: re-assert the client's id when it sent one,
	// else drop the header entirely. Leaking the daemon id would make the
	// client switch session ids mid-stream and break the router's
	// client-session→daemon-session map on the next call.
	if sessionID != "" {
		w.Header().Set("Mcp-Session-Id", sessionID)
	} else {
		w.Header().Del("Mcp-Session-Id")
	}

	contentType := upstreamResp.Header.Get("Content-Type")
	isSSE := strings.Contains(strings.ToLower(contentType), "text/event-stream")

	w.WriteHeader(upstreamResp.StatusCode)
	if bindSessionAfterUpstream {
		deps.Sessions.BindSession(sessionID, ws)
	}

	if isSSE {
		streamSSE(w, upstreamResp.Body)
		return
	}

	_, _ = io.Copy(w, upstreamResp.Body)
}

// handleSerenaDelete tears down the router-owned session state for a
// client-origin DELETE /serena/mcp (Finding 3). It mirrors the hub
// handler's DELETE/session-termination handling
// (internal/api/hub_mcp_handler.go handleDelete): snapshot the daemon
// binding for the client's Mcp-Session-Id, revoke EVERY local binding
// FIRST (immediate revocation), then fan a best-effort DELETE out to the
// workspace daemon's /mcp carrying the daemon-issued session id. DELETE
// carries no path-arg, so the workspace is resolved from the STORED
// binding, never from the request.
//
// Finding A — local revocation must be IMMEDIATE. The upstream DELETE can
// block up to the full upstream timeout on a slow daemon; the hub closes
// the same window by invalidating its session BEFORE the daemon fan-out
// (internal/api/hub_mcp_handler.go handleDelete). The pre-fix order
// forwarded first and unbound after, so during that window a concurrent
// POST with the same Mcp-Session-Id still passed lookup and executed a
// tool call after the client had terminated the session. We snapshot the
// binding up front, unbind serenaDaemonSessions + sticky deps.Sessions +
// the router-session registry FIRST, then forward upstream.
//
// Finding B — tear down the upstream session even when the sticky binding
// is missing. If a path-bearing tool-call completed the upstream handshake
// but the tool POST then failed before sticky BindSession ran,
// serenaDaemonSessions holds a live daemon session while
// deps.Sessions.LookupSession is nil. Resolving the workspace ONLY from the
// sticky lookup would skip the upstream DELETE and leak the daemon session.
// Resolve the workspace from the daemon binding's workspaceKey (sticky
// lookup first, then by-key via the resolver's ListWorkspaces) so the
// upstream DELETE fires whenever serenaDaemonSessions has a binding.
//
// Teardown is best-effort: the response is 204 regardless of the upstream
// DELETE outcome (a shutdown must not 5xx), and a transport failure is
// audited (warn) for diagnosability, consistent with the POST path.
//
// Finding 3 — validate MCP-Protocol-Version BEFORE tearing anything down,
// mirroring the tool-call path's Finding G (lines 466-477) and the hub's
// DELETE gate (internal/api/hub_mcp_handler.go handleDelete gate 7,
// :655-663). For a KNOWN router session (one this router minted at
// initialize) a request header that conflicts with the session's negotiated
// version is a "protocol-version mismatch" (-32600 at HTTP 400) and the
// session is NOT torn down — a stray DELETE carrying the wrong version must
// not revoke a live session. A missing or matching header proceeds. An
// UNKNOWN session (no router-session record — e.g. an older direct caller,
// or a session the router never minted) keeps today's best-effort teardown
// so an unbound id still gets its 204 ack and any leaked daemon binding is
// still released. The router validates against the SESSION version rather
// than rejecting a missing header outright (as the hub does) because the
// router mints its own sessions and uses the session version as the source
// of truth, the same posture Finding G takes on the tool-call path.
func (s *Server) handleSerenaDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		// No client session to tear down. Acknowledge (best-effort
		// shutdown semantic) rather than erroring.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Finding 3: for a KNOWN router session, reject a conflicting
	// MCP-Protocol-Version BEFORE any snapshot/unbind so a mismatched DELETE
	// leaves the session intact. Missing/matching header (or an unknown
	// session) falls through to the best-effort teardown below.
	if sessionVersion, known := s.serenaRouterSessions.negotiatedVersion(sessionID); known {
		if clientProtocolVersion := r.Header.Get("MCP-Protocol-Version"); clientProtocolVersion != "" && clientProtocolVersion != sessionVersion {
			writeJSONRPCErrorStatus(w, json.RawMessage("null"), http.StatusBadRequest, jsonrpcInvalidRequest,
				"protocol-version mismatch", nil)
			return
		}
	}

	deps := s.serenaRouterDepsProd()
	// Snapshot the daemon binding up front so the upstream DELETE still has
	// the daemon-issued session id AFTER we revoke the local mappings below.
	wsKey, daemonSessionID, hasBinding := s.serenaDaemonSessions.bindingFor(sessionID)

	// Findings A + B: resolve the workspace for the upstream DELETE UP FRONT,
	// BEFORE the local revocation clears the sticky map. The sticky binding
	// is the cheap source when it still exists (the common case after a
	// successful tool call); the daemon binding's workspaceKey is the robust
	// fallback for Finding B's leak (handshake completed but the tool POST
	// failed before BindSession ran, so the sticky binding is nil while
	// serenaDaemonSessions still holds a live daemon session). Snapshotting
	// the workspace here is what lets Finding A unbind FIRST without losing
	// the data the forward needs.
	var delWS *api.WorkspaceEntry
	if hasBinding && deps != nil {
		delWS = s.resolveDeleteWorkspace(deps, sessionID, wsKey)
	}

	// Finding A: revoke every local binding FIRST so a concurrent POST with
	// the same Mcp-Session-Id cannot pass lookup and run a tool call during
	// a slow upstream DELETE. The snapshots above mean unbinding here cannot
	// affect the forward below.
	s.serenaDaemonSessions.unbind(sessionID)
	s.serenaRouterSessions.unbind(sessionID)
	if deps != nil && deps.Sessions != nil {
		deps.Sessions.UnbindSession(sessionID)
	}

	// Best-effort upstream DELETE using the up-front workspace snapshot. A
	// missing workspace (or nil URL) just skips the forward — the local
	// revocation already ran so the router does not leak the mapping.
	if hasBinding && deps != nil && delWS != nil {
		auditFn := deps.AuditFn
		if auditFn == nil {
			auditFn = api.LogHubMcpEvent
		}
		upstreamURLFn := deps.UpstreamURLFn
		if upstreamURLFn == nil {
			upstreamURLFn = defaultUpstreamURL
		}
		upstreamTimeout := deps.UpstreamTimeout
		if upstreamTimeout <= 0 {
			upstreamTimeout = serenaUpstreamTimeout
		}
		httpClient := serenaHTTPClient(deps.HTTPClient, upstreamTimeout)

		if upstreamURL := upstreamURLFn(delWS); upstreamURL != "" {
			// Detach from r.Context() (mirror the hub's handleDelete,
			// internal/api/hub_mcp_handler.go): a client that disconnects
			// right after sending DELETE would otherwise cancel r.Context()
			// immediately and short-circuit the daemon-side teardown —
			// leaking the upstream session this finding exists to release.
			// Bound the detached context by the upstream timeout.
			fwdCtx, fwdCancel := context.WithTimeout(context.Background(), upstreamTimeout)
			s.forwardSerenaDeleteUpstream(fwdCtx, httpClient, upstreamURL, daemonSessionID, wsKey, delWS, auditFn)
			fwdCancel()
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// resolveDeleteWorkspace resolves the workspace for a DELETE/cancel forward
// from a client session that carries no path-arg (Findings B + H). It tries
// the sticky deps.Sessions binding FIRST (cheap, a value-copy, present in
// the common case where a tool call already bound the session) and falls
// back to a by-key scan of the resolver's ListWorkspaces — the existing
// workspaceLister capability (serena_router_lifecycle.go) — for the entry
// whose WorkspaceKey == wsKey (Finding B's leak case, where the sticky
// binding never ran). Returns nil when neither path resolves it (unwired
// sessions + a resolver without enumeration, or a key no longer registered).
func (s *Server) resolveDeleteWorkspace(deps *serenaRouterDeps, sessionID, wsKey string) *api.WorkspaceEntry {
	if deps == nil {
		return nil
	}
	if deps.Sessions != nil && sessionID != "" {
		if ws := deps.Sessions.LookupSession(sessionID); ws != nil {
			return ws
		}
	}
	return s.resolveWorkspaceByKey(deps, wsKey)
}

// resolveWorkspaceByKey scans the resolver's ListWorkspaces — the existing
// workspaceLister capability (serena_router_lifecycle.go) — for the entry
// whose WorkspaceKey == wsKey. Returns nil when the resolver cannot
// enumerate workspaces or the key is no longer registered.
func (s *Server) resolveWorkspaceByKey(deps *serenaRouterDeps, wsKey string) *api.WorkspaceEntry {
	if deps == nil || wsKey == "" {
		return nil
	}
	lister, ok := deps.Resolver.(workspaceLister)
	if !ok {
		return nil
	}
	for _, ws := range lister.ListWorkspaces() {
		if ws != nil && ws.WorkspaceKey == wsKey {
			return ws
		}
	}
	return nil
}

// forwardSerenaDeleteUpstream issues a best-effort DELETE to a workspace
// daemon's /mcp carrying the daemon-issued Mcp-Session-Id, so the daemon
// releases the upstream session. A transport failure or a non-2xx status
// is audited (warn) but never propagated — the client-origin DELETE
// teardown is best-effort (Finding 3).
func (s *Server) forwardSerenaDeleteUpstream(ctx context.Context, httpClient *http.Client, upstreamURL, daemonSessionID, wsKey string, ws *api.WorkspaceEntry, auditFn func(level, event string, fields map[string]any) error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, upstreamURL, nil)
	if err != nil {
		_ = auditFn("warn", "serena-upstream-delete-failed", map[string]any{
			"workspace_key": wsKey,
			"port":          ws.Port,
			"upstream_url":  upstreamURL,
			"phase":         "session-teardown",
			"err":           err.Error(),
		})
		return
	}
	if daemonSessionID != "" {
		req.Header.Set("Mcp-Session-Id", daemonSessionID)
	}
	resp, doErr := httpClient.Do(req)
	if doErr != nil {
		_ = auditFn("warn", "serena-upstream-delete-failed", map[string]any{
			"workspace_key": wsKey,
			"port":          ws.Port,
			"upstream_url":  upstreamURL,
			"phase":         "session-teardown",
			"err":           doErr.Error(),
		})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = auditFn("warn", "serena-upstream-delete-failed", map[string]any{
			"workspace_key": wsKey,
			"port":          ws.Port,
			"upstream_url":  upstreamURL,
			"phase":         "session-teardown",
			"status":        resp.StatusCode,
		})
	}
}

// handleSerenaCancelled forwards an id-less notifications/cancelled to the
// workspace daemon the client session is bound to, so an in-flight serena
// tool call is actually cancelled (Finding H). It mirrors the hub's
// notifications/cancelled handling (internal/api/hub_mcp_handler.go
// handlePost case "notifications/cancelled"): an id-bearing
// notifications/cancelled is a malformed request (-32600); an id-less one
// forwards best-effort to the bound daemon and answers 202.
//
// The forward is best-effort and context-detached (like the DELETE
// teardown): a client that disconnects right after sending the cancel must
// not cancel the daemon-side forward. When there is no bound daemon session
// (nothing in flight) the local 202 is kept — there is no daemon to tell.
func (s *Server) handleSerenaCancelled(
	w http.ResponseWriter,
	r *http.Request,
	deps *serenaRouterDeps,
	tb *toolBody,
	body []byte,
	httpClient *http.Client,
	upstreamURLFn func(ws *api.WorkspaceEntry) string,
	auditFn func(level, event string, fields map[string]any) error,
	sessionID string,
) {
	// An id-bearing notifications/* is a JSON-RPC request that can never be
	// answered — reject it -32600, same rule as handleNotificationOrPing
	// (and the hub). Only a genuine (id-less) notification is forwarded.
	if !isJSONRPCNotificationID(tb.ID) {
		writeJSONRPCError(w, tb.ID, jsonrpcInvalidRequest,
			"invalid request: notifications/* must not include id")
		return
	}

	if sessionID != "" {
		if wsKey, daemonSessionID, ok := s.serenaDaemonSessions.bindingFor(sessionID); ok {
			if ws := s.resolveDeleteWorkspace(deps, sessionID, wsKey); ws != nil {
				if upstreamURL := upstreamURLFn(ws); upstreamURL != "" {
					upstreamTimeout := deps.UpstreamTimeout
					if upstreamTimeout <= 0 {
						upstreamTimeout = serenaUpstreamTimeout
					}
					// Thread the session's negotiated version (Finding G
					// consistency): a strict daemon binds the
					// MCP-Protocol-Version header on a non-initialize POST to
					// the session's initialized version. Fall back to the raw
					// request header, then the router default.
					protocolVersion := r.Header.Get("MCP-Protocol-Version")
					if sessionVersion, known := s.serenaRouterSessions.negotiatedVersion(sessionID); known {
						protocolVersion = sessionVersion
					}
					protocolVersion = effectiveHandshakeProtocolVersion(protocolVersion)
					fwdCtx, fwdCancel := context.WithTimeout(context.Background(), upstreamTimeout)
					s.forwardSerenaCancelledUpstream(fwdCtx, httpClient, upstreamURL, daemonSessionID, protocolVersion, wsKey, ws, body, auditFn)
					fwdCancel()
				}
			}
		}
	}

	// notifications/* (genuine notification) gets no JSON-RPC response body.
	w.WriteHeader(http.StatusAccepted)
}

// forwardSerenaCancelledUpstream POSTs the verbatim notifications/cancelled
// body to a workspace daemon's /mcp carrying the daemon-issued
// Mcp-Session-Id + the negotiated MCP-Protocol-Version, so the daemon
// cancels the matching in-flight tool call. Best-effort: a transport
// failure or a non-2xx status is audited (warn) but never propagated (a
// cancellation notification has no response).
func (s *Server) forwardSerenaCancelledUpstream(ctx context.Context, httpClient *http.Client, upstreamURL, daemonSessionID, protocolVersion, wsKey string, ws *api.WorkspaceEntry, body []byte, auditFn func(level, event string, fields map[string]any) error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		_ = auditFn("warn", "serena-upstream-cancel-failed", map[string]any{
			"workspace_key": wsKey,
			"port":          ws.Port,
			"upstream_url":  upstreamURL,
			"phase":         "cancel-forward",
			"err":           err.Error(),
		})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if daemonSessionID != "" {
		req.Header.Set("Mcp-Session-Id", daemonSessionID)
	}
	if protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
	}
	resp, doErr := httpClient.Do(req)
	if doErr != nil {
		_ = auditFn("warn", "serena-upstream-cancel-failed", map[string]any{
			"workspace_key": wsKey,
			"port":          ws.Port,
			"upstream_url":  upstreamURL,
			"phase":         "cancel-forward",
			"err":           doErr.Error(),
		})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = auditFn("warn", "serena-upstream-cancel-failed", map[string]any{
			"workspace_key": wsKey,
			"port":          ws.Port,
			"upstream_url":  upstreamURL,
			"phase":         "cancel-forward",
			"status":        resp.StatusCode,
		})
	}
}

// defaultUpstreamURL points at the workspace's serena daemon. Per the
// plan: http://localhost:<workspace.Port>/mcp.
func defaultUpstreamURL(ws *api.WorkspaceEntry) string {
	if ws == nil || ws.Port <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", ws.Port)
}

// copyHeaders threads upstream -> downstream headers, skipping the
// connection-management headers that must NOT cross a proxy boundary.
// Hop-by-hop list per RFC 7230 section 6.1.
func copyHeaders(dst, src http.Header) {
	hopByHop := map[string]struct{}{
		"connection":          {},
		"keep-alive":          {},
		"proxy-authenticate":  {},
		"proxy-authorization": {},
		"te":                  {},
		"trailer":             {},
		"transfer-encoding":   {},
		"upgrade":             {},
	}
	for k, vv := range src {
		if _, hop := hopByHop[strings.ToLower(k)]; hop {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// streamSSE copies from src to dst with explicit flush after each
// read so event-stream frames reach the client without buffering. On
// a writer that does not support http.Flusher, the function degrades
// to plain io.Copy.
func streamSSE(dst http.ResponseWriter, src io.Reader) {
	flusher, _ := dst.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

// isTimeoutErr returns true when err is a context-deadline or net
// timeout. Used to pick 504 vs 502 on http.Client.Do failures.
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// InMemorySessionRouter is a process-local *api.WorkspaceEntry binding
// map. It exists so test code and production callers can wire a
// working SessionRouter without depending on Agent A1's
// serena_routing package directly; the public surface matches
// sessionRouter exactly.
type InMemorySessionRouter struct {
	mu       sync.RWMutex
	sessions map[string]*api.WorkspaceEntry
}

func newInMemorySessionRouter() *InMemorySessionRouter {
	return &InMemorySessionRouter{sessions: map[string]*api.WorkspaceEntry{}}
}

func (s *InMemorySessionRouter) BindSession(sessionID string, ws *api.WorkspaceEntry) {
	if sessionID == "" || ws == nil {
		return
	}
	s.mu.Lock()
	s.sessions[sessionID] = ws
	s.mu.Unlock()
}

func (s *InMemorySessionRouter) LookupSession(sessionID string) *api.WorkspaceEntry {
	if sessionID == "" {
		return nil
	}
	s.mu.RLock()
	ws := s.sessions[sessionID]
	s.mu.RUnlock()
	return ws
}

func (s *InMemorySessionRouter) UnbindSession(sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

// Compile-time guard.
var _ sessionRouter = (*InMemorySessionRouter)(nil)
