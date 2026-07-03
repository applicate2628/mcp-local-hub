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
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/serena_routing"
	"mcp-local-hub/internal/clients"
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
	// AutoRegisterFn is the Phase-5 auto-register-on-miss seam. When a
	// /serena/mcp tool call resolves to no registered workspace
	// (ErrWorkspaceNotFound / resolved==nil) the handler calls this to
	// register+spawn the workspace at runtime, then forwards the call to
	// the freshly-spawned daemon. Production wires it to
	// api.(*API).AutoRegisterSerenaWorkspace (SetSerenaRouterProduction).
	// A nil AutoRegisterFn preserves the pre-Phase-5 immediate-503
	// behavior (back-compat for partially-wired routing).
	AutoRegisterFn func(ctx context.Context, absPath string) (*api.WorkspaceEntry, error)

	// WakeIdleFn is the v0.6 idle-shutdown (#6, spec §6) next-request wake. On
	// a /serena/mcp tool-call for a resolved workspace, the handler calls it
	// BEFORE forwarding so an idle-stopped daemon is woken first. Production
	// wires it to api.(*API).WakeIdleSerenaDaemon (SetSerenaRouterProduction).
	// Contract: returns nil when the daemon is up OR was successfully woken
	// (idle stop cleared + respawn ready); returns api.ErrWakeRefusedOperatorStop
	// when the daemon has an OPERATOR stop (the wake must NOT resurrect it — the
	// forward then proceeds and fails loud, honoring the operator stop); returns
	// any other error when the wake/respawn did not become ready in time (router
	// → 503, client retries). A nil WakeIdleFn disables idle-wake (back-compat
	// for partially-wired routing); the forward proceeds unchanged.
	WakeIdleFn func(ctx context.Context, taskName string, port int, who string) error
}

// serenaRouterTestSeam lets tests inject a fully-mocked deps bundle.
var serenaRouterTestSeam func() *serenaRouterDeps

// serenaUpstreamTimeout caps the per-forward connect + first-byte
// budget. Matches the 60s ceiling HTTPHost's httpClient uses for
// tool-call traffic. Tests override via UpstreamTimeout.
const serenaUpstreamTimeout = 60 * time.Second

// serenaCleanupTimeout caps best-effort fire-and-forget upstream calls
// (Invariant D): one-shot session teardown DELETEs and cancellation
// forwards. These are NOT the primary thing the client is waiting for, so
// they must be bounded by a SHORT independent budget rather than the 60s
// serenaUpstreamTimeout default — a hung daemon then delays the client by
// at most this budget instead of a full minute. 5s mirrors the hub's
// per-daemon best-effort fan-out budget (internal/api/hub_mcp_handler.go
// handleDelete: context.WithTimeout(context.Background(), 5*time.Second)).
const serenaCleanupTimeout = 5 * time.Second

// serenaDeleteTimeout caps the MAIN client-origin DELETE upstream forward in
// handleSerenaDelete (sonnet post-PASS finding 1). Unlike the Invariant-D
// fire-and-forget sites it is the thing the client's DELETE is awaiting (the
// 204 fires after it), so it is detached-from-r.Context() (it must survive a
// client disconnect) but NOT routed through cleanupContext. It used to use the
// 60s serenaUpstreamTimeout default, which blocked the client's teardown 204
// for up to a minute AND held the handler goroutine 60s after a client
// disconnect on a hung/slow daemon. Local revocation already ran FIRST (Finding
// A), so a shortened forward does not weaken correctness — it only bounds the
// 204 ack. 5s matches the hub's per-daemon teardown budget
// (internal/api/hub_mcp_handler.go handleDelete: a 5s per-daemon fan-out
// budget) and internal/daemon/http_host.go's httpHostCleanupTimeout (5s).
const serenaDeleteTimeout = 5 * time.Second

// cleanupContext returns a context for an Invariant-D best-effort upstream
// call: DETACHED from the client request (context.Background()-derived, so
// the client closing its connection does not cancel the teardown/forward —
// which would leak the upstream session / drop the cancel) and BOUNDED by a
// SHORT independent budget. The budget is serenaCleanupTimeout, except when
// the caller's configured upstreamTimeout is set and shorter — then that
// shorter value is used (keeps tests that inject a sub-second timeout
// deterministic and fast). The returned cancel MUST be called by the caller
// (typically `defer cancel()` for the cancel/DELETE call site). All three
// router-side D sites (#4 tools/list teardown, #6 path-only teardown,
// #7 cancel forward) route through this so detach + bound are consistent.
func cleanupContext(upstreamTimeout time.Duration) (context.Context, context.CancelFunc) {
	budget := serenaCleanupTimeout
	if upstreamTimeout > 0 && upstreamTimeout < budget {
		budget = upstreamTimeout
	}
	return context.WithTimeout(context.Background(), budget)
}

// upstreamReadContext bounds an upstream MCP request the client is AWAITING
// (the synthesized handshake + the tools/list proxy) by the full upstream
// timeout (Finding 3 — round-10). Unlike cleanupContext it stays ATTACHED to
// the parent request context (the client is blocked on this call, so a client
// disconnect SHOULD cancel it) and uses the FULL upstreamTimeout rather than
// the short fire-and-forget budget.
//
// Why it is needed: the production http.Client has Timeout==0 (so a long-lived
// tool-call SSE stream is not killed mid-flight) and the transport's
// ResponseHeaderTimeout covers only response HEADERS. A daemon that returns
// text/event-stream HEADERS then never emits a complete JSON-RPC response event
// (only heartbeats/comments, or nothing) makes readSSEJSONRPCResponse's scanner
// block INDEFINITELY while r.Context() stays live — hanging the first
// tool-call / tools/list forever and preventing fetchToolsListFromAnyDaemon
// from trying the next workspace. Deriving a context.WithTimeout here makes the
// transport abort the in-flight body read when the deadline fires (the scanner
// then returns a context-cancellation error), so the caller surfaces a 502/504
// and the tools/list loop advances. The returned cancel MUST be called by the
// caller (typically `defer cancel()`). A non-positive upstreamTimeout falls
// back to serenaUpstreamTimeout so the deadline is always present.
func upstreamReadContext(parent context.Context, upstreamTimeout time.Duration) (context.Context, context.CancelFunc) {
	budget := upstreamTimeout
	if budget <= 0 {
		budget = serenaUpstreamTimeout
	}
	return context.WithTimeout(parent, budget)
}

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
	// F3 (bot r3): the LSP forward path (nil HTTPClient + lspForwardUpstreamTimeout)
	// must reuse the shared LSP client — this function runs PER REQUEST, so the
	// fall-through below would build a fresh http.Transport per /lsp forward
	// (connection-pool loss + lingering idle conns). Mirror of the serena case.
	if upstreamTimeout == lspForwardUpstreamTimeout {
		return defaultLSPForwardClient
	}
	// Non-default timeout (tests overriding UpstreamTimeout): a fresh client is
	// acceptable — production deps only ever reach the two shared cases above.
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
	wakeAPI := api.NewAPI()
	s.SetSerenaRouterDeps(&serenaRouterDeps{
		Resolver: resolver,
		Sessions: sessions,
		// Phase 5: auto-register-on-miss. api.NewAPI() per call is the
		// established pattern (migrate_serena.go does the same) — the API
		// struct is a thin façade over the live registry/state dir, so a
		// fresh instance shares the same on-disk truth.
		AutoRegisterFn: func(ctx context.Context, absPath string) (*api.WorkspaceEntry, error) {
			return api.NewAPI().AutoRegisterSerenaWorkspace(ctx, absPath)
		},
		// v0.6 idle-shutdown (#6): the next-request wake for an idle-stopped
		// serena daemon. Reuse one API handle so its in-flight wake registry
		// collapses concurrent requests for the same starting daemon.
		WakeIdleFn: func(ctx context.Context, taskName string, port int, who string) error {
			return wakeAPI.WakeIdleSerenaDaemon(ctx, taskName, port, who)
		},
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
//
// Finding 1 (round-10 conflict now DEMANDED): Params is a json.RawMessage,
// NOT a typed struct. A typed-struct Params (the pre-fix shape) failed the
// WHOLE-body json.Unmarshal when params was valid JSON but NOT an object
// (`"params":[]`, `"params":1`, `"params":"x"`), so serenaRouterHandler
// rejected it with a plain HTTP 400 "malformed JSON body" — a transport-level
// failure the per-method -32602 validation never reached, even though an MCP
// client can parse the body as JSON-RPC and SHOULD get a JSON-RPC error
// envelope. Decoding params as a RawMessage never fails on a non-object value,
// so the envelope decodes, dispatch proceeds, and each consumer parses Params
// for what it needs: the tool-call path via toolCallParams (params.name +
// params.arguments), the lifecycle methods via lifecycleParamsObjectOrNull
// (a present-but-non-object params → -32602, mirroring
// internal/api/hub_mcp_handler.go handleInitialize). initialize's
// protocolVersion and tools/list's cursor are parsed from the verbatim body
// (initializeRawParams / toolsListIsCursorRequest), independent of this field.
type toolBody struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	ID      json.RawMessage `json:"id"`
	Params  json.RawMessage `json:"params"`
}

// toolCallParams parses a tools/call envelope's `params` (now a RawMessage,
// Finding 1) into the name + arguments the path-routing needs. A params that
// is absent, JSON null, or not an object yields ("", nil) — the caller then
// hits the existing "missing required field: params.name" path-arg error,
// preserving the pre-Finding-1 tool-call behavior for a malformed params that
// cannot yield a name (the dispatch only reaches here for non-lifecycle
// methods, so a tool-call with a bad params is the path-arg error, not a
// lifecycle -32602).
func toolCallParams(params json.RawMessage) (name string, arguments json.RawMessage) {
	trimmed := bytes.TrimSpace(params)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", nil
	}
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(trimmed, &p); err != nil {
		return "", nil
	}
	return p.Name, p.Arguments
}

// lifecycleParamsObjectOrNull validates the `params` shape for a request-style
// lifecycle method (initialize / tools/list / ping). It returns true when
// params is ABSENT, JSON null, or a JSON object — the shapes the lifecycle
// accepts. A PRESENT but non-object params (`[]`, `1`, `"x"`) returns false:
// the caller then rejects the request with JSON-RPC -32602 "invalid params"
// at HTTP 400, mirroring the hub's handleInitialize type-mismatch rejection
// (internal/api/hub_mcp_handler.go: a non-object initialize params → -32602).
// MCP request params, when present, MUST be a structured object
// (modelcontextprotocol.io basic/lifecycle); a non-object value is a malformed
// request the client should see as a JSON-RPC error, not a transport 400.
func lifecycleParamsObjectOrNull(params json.RawMessage) bool {
	trimmed := bytes.TrimSpace(params)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	return trimmed[0] == '{'
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
//   - project       : activate_project ONLY (the workspace-ROOT path the
//     editor's lead-off call carries). It is the LAST key checked so it
//     is lowest precedence and changes no other tool's routing; among
//     serena's tool set only activate_project declares a `project` arg
//     (serena schema: activate_project's sole arg is `project`, "the name
//     of a registered project OR a path to a project directory"). Without
//     it activate_project resolved as pathless → never bound a fresh
//     session → every subsequent call 503'd missing_session, making
//     per-project serena unusable for editors that lead with
//     activate_project. The `project` key is routed ONLY when its value
//     is an ABSOLUTE path: a bare registered-NAME is skipped here so it
//     falls through to the documented 503 rather than being silently
//     mis-bound by resolveRelative (which would join the name onto each
//     registered WorkspacePath and return the first that Lstat-exists —
//     e.g. project="docs" coincidentally binding /proj/aaa when
//     /proj/aaa/docs exists). The dynamic-pool migrate-configured client
//     always sends the absolute PATH, so the registered-NAME case is a
//     documented follow-up (name resolution), not a regression.
func extractPathArg(arguments json.RawMessage) (string, bool) {
	if len(arguments) == 0 {
		return "", false
	}
	var args map[string]json.RawMessage
	if uerr := json.Unmarshal(arguments, &args); uerr != nil {
		return "", false
	}
	for _, key := range []string{"relative_path", "file_path", "name_path", "path", "project"} {
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
		// project carries a workspace ROOT; route it only when absolute.
		// A bare registered-NAME must fall to the documented 503 (name
		// resolution is a follow-up), not a coincidental resolveRelative match.
		if key == "project" && !filepath.IsAbs(v) {
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
	//   - id ABSENT  -> for tools/list / ping, a JSON-RPC notification:
	//     MUST NOT receive a response envelope; return 202 + empty body.
	//     Without this, handleToolsList would synthesize a result with
	//     id:null and a strict client would treat the unexpected response
	//     as a protocol error. initialize is the EXCEPTION: it is the
	//     session-establishment request, never a notification, so an absent
	//     id there is -32600 (see the initialize case below + the hub at
	//     internal/api/hub_mcp_handler.go:316-319), not 202.
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
		// initialize is the session-ESTABLISHMENT request, NOT a
		// fire-and-forget notification: an ABSENT id must NOT 202 like the
		// other request-style lifecycle methods below. The hub rejects the
		// same id-less shape with -32600 at HTTP 400 BEFORE creating a session
		// (internal/api/hub_mcp_handler.go:316-319, "initialize requires a
		// non-null id"); accepting it here would 202 silently, hide the
		// client's protocol bug, and let the client fail later with a
		// missing/unknown session. Mirror the hub byte-for-byte: BOTH an
		// absent id (this branch) AND a present-but-invalid id (next branch)
		// → -32600; only a valid String/Number id proceeds to handleInitialize.
		// The reconcile readiness probe sends id:1, so it is unaffected.
		if isJSONRPCNotificationID(tb.ID) {
			writeJSONRPCErrorStatus(w, json.RawMessage("null"), http.StatusBadRequest, jsonrpcInvalidRequest,
				"invalid request: initialize requires a non-null id", nil)
			return
		}
		if !isValidJSONRPCRequestID(tb.ID) {
			writeJSONRPCError(w, nil, jsonrpcInvalidRequest,
				"invalid request: id must be a non-null string or number")
			return
		}
		// Finding 1: a PRESENT but non-object params is -32602 (mirrors
		// internal/api/hub_mcp_handler.go handleInitialize, which -32602s a
		// non-object initialize params). handleInitialize's own
		// initializeRawParams path also rejects a non-object params; this gate
		// makes the rejection uniform across the three lifecycle methods and
		// keeps the error shape identical. An object params (incl. one with a
		// wrong-typed protocolVersion) still flows into handleInitialize for the
		// more specific protocolVersion validation.
		if !lifecycleParamsObjectOrNull(tb.Params) {
			writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidParams,
				"invalid params: params must be an object", nil)
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
		// Finding 1: a PRESENT but non-object params on tools/list is -32602
		// (the request itself is malformed; mirror the hub's params type
		// validation). The pre-Finding-1 toolBody struct decode rejected this
		// with a plain HTTP 400 before dispatch; now the body decodes and this
		// gate returns the JSON-RPC error an MCP client expects.
		if !lifecycleParamsObjectOrNull(tb.Params) {
			writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidParams,
				"invalid params: params must be an object", nil)
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
		// Finding 1: a PRESENT but non-object params on ping is -32602, uniform
		// with initialize / tools/list above.
		if !lifecycleParamsObjectOrNull(tb.Params) {
			writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidParams,
				"invalid params: params must be an object", nil)
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

	// Finding 1: params is now a json.RawMessage; parse the tool-call shape
	// (name + arguments) here. A malformed/non-object params yields an empty
	// name and the existing "missing required field: params.name" path-arg
	// error — the pre-Finding-1 tool-call behavior is preserved.
	// bot PR #253 P2/P3: an id-less tools/call is a JSON-RPC NOTIFICATION → 202
	// with NO execution. This MUST precede BOTH the params parse AND the
	// resolve/auto-register branch (mirroring the hub's tools/call gate at
	// internal/api/hub_mcp_handler.go and the tools/list / ping branches above):
	// a fire-and-forget notification — even one with missing/malformed params —
	// must be classified as a notification (202), NOT receive a "missing
	// params.name" error, and must never mutate workspaces.yaml + the supervisor
	// intent or consume a pool port via auto-register.
	if isJSONRPCNotificationID(tb.ID) {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !isValidJSONRPCRequestID(tb.ID) {
		writeJSONRPCError(w, nil, jsonrpcInvalidRequest,
			"invalid request: id must be a non-null string or number")
		return
	}

	toolName, toolArguments := toolCallParams(tb.Params)
	if toolName == "" {
		writeRequiredFieldError(w, "params.name")
		return
	}

	pathArg, hasPath := extractPathArg(toolArguments)

	var ws *api.WorkspaceEntry
	bindSessionAfterUpstream := false
	workspaceResolvedByPath := false
	if hasPath {
		resolved, resolveErr := deps.Resolver.ResolveByPath(pathArg)
		if resolveErr != nil {
			// Phase 5: on workspace-not-found, attempt auto-register-on-miss
			// (register+spawn the serena workspace at this path, then forward)
			// instead of the immediate 503. attemptSerenaAutoRegister returns a
			// non-nil entry to forward to, or nil AFTER writing the HTTP
			// response (so the caller MUST return on nil). A nil AutoRegisterFn
			// preserves today's writeWorkspaceNotFound 503.
			if errors.Is(resolveErr, ErrWorkspaceNotFound) {
				ws = s.attemptSerenaAutoRegister(w, r, deps, tb.ID, sessionID, pathArg)
				if ws == nil {
					return
				}
			} else {
				http.Error(w, "resolve workspace: "+resolveErr.Error(), http.StatusInternalServerError)
				return
			}
		} else if resolved == nil {
			// resolved==nil is the other not-found shape (resolver returned a
			// nil entry with a nil error). Same auto-register-on-miss path.
			ws = s.attemptSerenaAutoRegister(w, r, deps, tb.ID, sessionID, pathArg)
			if ws == nil {
				return
			}
		} else {
			ws = resolved
			workspaceResolvedByPath = true
		}
		// A resolved OR freshly auto-registered workspace binds the sticky
		// session after the upstream forward, exactly as before — auto-register
		// yields a real workspace indistinguishable from a pre-registered one.
		if sessionID != "" && deps.Sessions != nil {
			bindSessionAfterUpstream = true
		}
	} else {
		if sessionID == "" || deps.Sessions == nil {
			writeWorkspaceNotFound(w, "", true)
			return
		}
		// Round-13 (consultant finding — close the pathless reanimation): check
		// the ROUTER session's liveness BEFORE the sticky LookupSession refresh.
		// peekVersionState distinguishes three cases (and is expire-on-read, so an
		// idle-expired router session is deleted from routerSessionStore HERE):
		//
		//   - routerSessionExpired: the id WAS minted here but idle-expired on this
		//     read. The pre-fix code did the sticky LookupSession FIRST, which
		//     REFRESHED the sticky binding's lastSeen and kept the session routable
		//     as a "legacy" caller indefinitely (every pathless call re-refreshed
		//     it), defeating the on-read expiry. We instead coordinate the unbind of
		//     the sticky deps.Sessions + serenaDaemonSessions for the id (mirroring
		//     the round-12 SweepSerenaSessions per-id coordination) and treat the
		//     call as a terminated session: return the router's "session terminated"
		//     -32600 (consistent with the round-10 post-gate-touch abort and the
		//     resolveDaemonSession recheck abort) WITHOUT refreshing the sticky or
		//     routing. The orphaned UPSTREAM daemon session ages out on the daemon's
		//     own idle clock — the same TTL-reclaim posture round-9 / the round-12
		//     sweep already take for an expired router session (no eager upstream
		//     DELETE here, matching the named referent).
		//   - routerSessionAbsent: the id was NEVER minted at this router → a TRUE
		//     legacy caller (older direct client that never initialized here). Keep
		//     today's sticky-routing behavior (route via the sticky binding below).
		//   - routerSessionLive: today's behavior (route via sticky; the version
		//     gate + post-gate touch below still run for the known router session).
		if _, state := s.serenaRouterSessions.peekVersionState(sessionID); state == routerSessionExpired {
			s.coordinateExpiredRouterSessionUnbind(sessionID, deps.Sessions)
			writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
				"session terminated", nil)
			return
		}
		ws = deps.Sessions.LookupSession(sessionID)
		if ws == nil {
			writeWorkspaceNotFound(w, "", true)
			return
		}
	}

	resolverCanList := false
	if _, ok := deps.Resolver.(workspaceLister); ok {
		resolverCanList = true
	}
	gateCtx, gateCancel := upstreamReadContext(r.Context(), upstreamTimeout)
	defer gateCancel()
	resolveToolCallWorkspace := func(gateKey string) *api.WorkspaceEntry {
		var refreshed *api.WorkspaceEntry
		if hasPath {
			if workspaceResolvedByPath {
				resolved, resolveErr := deps.Resolver.ResolveByPath(pathArg)
				if resolveErr != nil {
					if errors.Is(resolveErr, ErrWorkspaceNotFound) {
						refreshed = s.attemptSerenaAutoRegister(w, r, deps, tb.ID, sessionID, pathArg)
						if refreshed == nil {
							return nil
						}
						workspaceResolvedByPath = false
					} else {
						http.Error(w, "resolve workspace: "+resolveErr.Error(), http.StatusInternalServerError)
						return nil
					}
				} else if resolved == nil {
					refreshed = s.attemptSerenaAutoRegister(w, r, deps, tb.ID, sessionID, pathArg)
					if refreshed == nil {
						return nil
					}
					workspaceResolvedByPath = false
				} else {
					refreshed = resolved
					workspaceResolvedByPath = true
				}
			} else {
				refreshed = ws
			}
			if sessionID != "" && deps.Sessions != nil {
				bindSessionAfterUpstream = true
			}
		} else {
			if resolverCanList {
				refreshed = s.resolveWorkspaceByKey(deps, gateKey)
				if refreshed == nil {
					writeWorkspaceNotFound(w, "", true)
					return nil
				}
			} else {
				refreshed = ws
			}
		}
		return refreshed
	}
	recordAndWakeToolCall := func(out *serenaWorkspaceGateOutcome) bool {
		if out == nil || out.ws == nil {
			return true
		}
		if !hasPath && !resolverCanList && (out.gate.phaseActive || out.gate.waitedThroughPrune) {
			writeWorkspaceNotFound(w, "", true)
			return true
		}
		// v0.6 idle-shutdown (#6, spec §6) — record this tool-call as activity for
		// the resolved workspace BEFORE the wake/forward. Stamping last-activity
		// first guarantees a daemon currently servicing this call has a fresh
		// timestamp, so the 60s sweeper can never idle a daemon mid-call (the
		// falsification test). A freshly auto-registered workspace (above) records
		// here too, so its first call already counts as activity.
		s.recordSerenaActivity(out.ws.WorkspaceKey, time.Now())

		// v0.6 idle-shutdown wake — if this daemon was idle-stopped, clear the idle
		// stop and trigger a respawn before forwarding. WakeIdleFn is a fast no-op
		// when the daemon is already up (the steady-state hot path). On an OPERATOR
		// stop (user-stop/user-disabled) the wake REFUSES
		// (api.ErrWakeRefusedOperatorStop), which is terminal for this request: do
		// not forward to a port that may now be rebound by a foreign process. Any
		// other wake error (respawn not ready in time) → 503 so the client retries
		// while the supervisor brings the daemon up (the SAME retry the router
		// already uses for not-yet-ready daemons). A nil WakeIdleFn
		// (partially-wired routing) skips the wake entirely.
		if deps.WakeIdleFn != nil && out.ws.TaskName != "" {
			hadActiveIdleStop := serenaTaskHasActiveIdleStop(out.ws.TaskName, time.Now())
			// Detach from r.Context() cancellation (a client disconnect must not
			// abort the supervisor nudge mid-flight, mirroring the auto-register
			// posture) but BOUND the whole wake so a wedged respawn cannot hang the
			// handler. 30s covers clear + reconcile-nudge + the ~20s readiness probe.
			wakeCtx, wakeCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
			wakeErr := deps.WakeIdleFn(wakeCtx, out.ws.TaskName, out.ws.Port, "serena-router-wake")
			if wakeErr == nil && hadActiveIdleStop {
				s.reseedSerenaBackendPIDAfterConfirmedWake(wakeCtx, out.ws)
			}
			wakeCancel()
			if wakeErr != nil {
				if errors.Is(wakeErr, api.ErrWakeRefusedOperatorStop) {
					_ = auditFn("info", "serena-idle-wake-operator-stopped", map[string]any{
						"workspace_key": out.ws.WorkspaceKey,
						"task_name":     out.ws.TaskName,
						"port":          out.ws.Port,
						"err":           wakeErr.Error(),
					})
					writeJSONRPCErrorStatus(w, tb.ID, http.StatusServiceUnavailable, jsonrpcInternalError,
						"serena daemon stopped by operator stop; request not forwarded", nil)
					return true
				}
				_ = auditFn("warn", "serena-idle-wake-not-ready", map[string]any{
					"workspace_key": out.ws.WorkspaceKey,
					"task_name":     out.ws.TaskName,
					"port":          out.ws.Port,
					"err":           wakeErr.Error(),
				})
				writeJSONRPCErrorStatus(w, tb.ID, http.StatusServiceUnavailable, jsonrpcInternalError,
					"serena daemon waking from idle; retry: "+wakeErr.Error(), nil)
				return true
			}
		}
		out.rewoke = true
		return false
	}
	enteredGate, abortedGate, gateCallErr := s.withSerenaWorkspaceGate(
		gateCtx,
		ws.WorkspaceKey,
		serenaWorkspaceGatePolicyBlock,
		resolveToolCallWorkspace,
		upstreamURLFn,
		recordAndWakeToolCall,
		func(out *serenaWorkspaceGateOutcome) (err error) {
			ws = out.ws
			upstreamURL := out.upstreamURL
			if !hasPath && !resolverCanList && (out.gate.phaseActive || out.gate.waitedThroughPrune) {
				writeWorkspaceNotFound(w, "", true)
				return
			}
			if upstreamURL == "" {
				http.Error(w, "upstream URL resolution failed", http.StatusInternalServerError)
				return
			}
			if !out.rewoke && recordAndWakeToolCall(out) {
				return
			}
			restampSerenaForwardOnExit := false
			upstreamReached := false
			defer func() {
				if restampSerenaForwardOnExit {
					// ORDER MATTERS: re-stamp activity BEFORE dropping the in-flight
					// protection. The sweeper reads counter-then-activity unsynchronized;
					// exit-first would open a window where counter==0 while last-activity
					// is still the stale request-START stamp — a sweep tick landing there
					// idle-stops a daemon that finished a >threshold call nanoseconds ago
					// (focused re-review P3, both lenses).
					now := time.Now()
					s.recordSerenaActivity(ws.WorkspaceKey, now)
					// Only make the activity restart-durable after the HTTP request reached
					// the daemon. Wake refusals, request-build failures, and transport errors
					// must not refresh durable idle-prune state or imply healthy daemon
					// activity in status views.
					if upstreamReached {
						s.maybePersistSerenaActivity(ws.WorkspaceKey, now)
					}
				}
			}()

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
			// Finding 4 (this round — store-vs-DELETE TOCTOU): routerSessionKnown records
			// whether this id was minted by a prior initialize at THIS router. It is used
			// below to decide whether to install the post-handshake liveness recheck: only
			// a router-minted session has a router-session entry a DELETE/sweep could
			// terminate mid-handshake. A legacy/path-only caller (no router session) keeps
			// today's behavior (no recheck).
			routerSessionKnown := false
			// Finding 4 (round-10): PEEK the session's negotiated version (no lastSeen
			// refresh) for this PRE-gate validation — a request rejected for a version
			// mismatch below must NOT keep the session alive. lastSeen is refreshed via
			// touch only AFTER the gate passes.
			//
			// Finding 4 (P2, Codex PR #249 round-2 — distinguish EXPIRED from absent on
			// the PATH-BEARING branch, mirroring the pathless branch above): use the
			// tri-state peekVersionState, NOT the boolean peekNegotiatedVersion (which
			// collapses an expired session into known=false and so would treat a stale id
			// as TRUE-legacy — fresh handshake + re-bind, letting an expired router
			// session continue simply by including a path argument). The pathless branch
			// (Round-13) already does this; the path-bearing branch did not, leaving the
			// expired-session reanimation class open here. On routerSessionExpired (the
			// id WAS minted here but idle-expired on this read; peekVersionState deleted
			// the routerSessionStore entry), coordinate the sticky + daemon unbind for the
			// id and abort with the router's "session terminated" -32600 — consistent with
			// the pathless branch and the round-10 post-gate-touch abort. On
			// routerSessionAbsent (never minted here → a TRUE-legacy / path-only caller),
			// keep today's behavior: routerSessionKnown stays false, the raw header drives
			// a fresh handshake. On routerSessionLive, today's behavior (the version gate
			// + post-gate touch below run for the known session).
			sessionVersion, sessionState := s.serenaRouterSessions.peekVersionState(sessionID)
			if sessionState == routerSessionExpired {
				s.coordinateExpiredRouterSessionUnbind(sessionID, deps.Sessions)
				writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
					"session terminated", nil)
				return
			}
			if sessionState == routerSessionLive {
				routerSessionKnown = true
				if clientProtocolVersion != "" && clientProtocolVersion != sessionVersion {
					writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
						"protocol-version mismatch", nil)
					return
				}
				// Round-10 (Finding 1 — re-check liveness AFTER the gate, mirroring the
				// hub's post-gate Touch, internal/api/hub_mcp_handler.go:402-409). The
				// peek above was PRE-gate; the cleanup ticker or a client DELETE can
				// sweep/terminate the session between that peek and now. touch refreshes
				// lastSeen ONLY for a still-live binding and reports whether it did. A
				// false return means the session was swept/terminated mid-flight, so
				// ABORT here — return the router's "session terminated" -32600 and do NOT
				// proxy upstream or let resolveDaemonSession RECREATE a daemon/sticky
				// binding for a dead session (which would defeat immediate-revocation +
				// idle-sweep).
				if !s.serenaRouterSessions.touch(sessionID) {
					writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
						"session terminated", nil)
					return
				}
				// Use the session's negotiated version for the upstream handshake so
				// the daemon session is established under the consistent version even
				// when this request omitted the header.
				clientProtocolVersion = sessionVersion
			}
			// Round-9 (Finding 4): a workspace switch on this client session no
			// longer eagerly tears down the OLD workspace's daemon session. The
			// rounds 7+8 displaced-teardown raced a still-in-flight tool call in the
			// old workspace (the router tracks no per-daemon in-flight requests), so
			// per the reviewer's round-9 guidance we revert to TTL-based reclaim:
			// resolveDaemonSession overwrites the LOCAL binding immediately (no local
			// leak) and the orphaned UPSTREAM session ages out on the daemon's idle
			// clock. The explicit client-origin DELETE (handleSerenaDelete) and the
			// partial-handshake cleanup remain — only the switch-time teardown is gone.
			//
			// Finding 4 (this round — store-vs-DELETE TOCTOU): pass a liveness recheck
			// so resolveDaemonSession does NOT store a daemon binding for a router
			// session that a concurrent DELETE/sweep terminated DURING the slow
			// handshake. The callback is installed ONLY for a router-minted session
			// (routerSessionKnown); a sessionless/path-only call has no router session
			// to check, so it passes nil and keeps today's behavior. known() is a peek
			// (no lastSeen refresh): the post-gate touch above already refreshed a live
			// session, so this reports false ONLY when the entry was actually removed.
			var sessionLive func() bool
			if routerSessionKnown {
				sid := sessionID
				sessionLive = func() bool { return s.serenaRouterSessions.known(sid) }
			}
			daemonSessionID, daemonProtocolVersion, hsErr := s.serenaDaemonSessions.resolveDaemonSession(r.Context(), httpClient, upstreamURL, sessionID, ws, clientProtocolVersion, upstreamTimeout, sessionLive)
			if hsErr != nil {
				// Finding 4: the router session was DELETEd/swept during the handshake.
				// resolveDaemonSession already best-effort-released the just-minted daemon
				// session and stored NO binding; abort with the router's "session
				// terminated" -32600 (consistent with the round-10 post-gate touch abort,
				// above) BEFORE the post-response sticky BindSession runs — so a later
				// pathless call with this id is NOT routed. This is checked before the
				// transport-error branches because it is NOT a 502/504 (the handshake
				// itself succeeded).
				if errors.Is(hsErr, errRouterSessionTerminated) {
					writeJSONRPCErrorStatus(w, tb.ID, http.StatusBadRequest, jsonrpcInvalidRequest,
						"session terminated", nil)
					return
				}
				if errors.Is(hsErr, errDaemonSessionStoreFull) {
					writeJSONRPCErrorStatus(w, tb.ID, http.StatusTooManyRequests, jsonrpcInvalidRequest,
						"too many serena daemon sessions", nil)
					return
				}
				if errors.Is(hsErr, errDaemonSessionHandshakeInFlight) {
					// bot PR #251 r2 P1: a concurrent first handshake for this session id is in
					// flight; reject this duplicate (retry-able) so it does not mint a second
					// upstream daemon session. The client's retry hits the completed binding.
					writeJSONRPCErrorStatus(w, tb.ID, http.StatusServiceUnavailable, jsonrpcInvalidRequest,
						"serena daemon session handshake already in progress for this session id; retry", nil)
					return
				}
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
				// §3 fail-loud (always-on floor): a CONNECTION failure on the handshake
				// (dial refused / dead backend) — as opposed to a timeout on a slow-but-
				// live daemon, or a protocol-level handshake error on a live daemon — is
				// a backend-loss signal. Tear every session bound to this workspace out
				// of all three stores so the next request fails loud (-32600 / 503) and
				// the client re-initializes, instead of a zombie.
				if isConnectionLossErr(hsErr) {
					s.handleSerenaBackendLossOnForwardFailure(ws.WorkspaceKey, sessionID, hsErr, auditFn)
				}
				http.Error(w, fmt.Sprintf("upstream serena daemon at port %d MCP handshake failed: %s", ws.Port, hsErr.Error()), http.StatusBadGateway)
				return
			}

			// §3 fail-loud: the handshake (fresh or cache-hit-reuse) succeeded, so this
			// client session is genuinely routed to ws. Record the workspace→session
			// edge in the router store's reverse index so a later backend-loss event
			// for this workspace can enumerate + tear down every bound session. Keyed
			// on a known router session only (bindWorkspace no-ops an unknown id), so a
			// legacy/path-only caller with no router session is not indexed.
			if sessionID != "" {
				// Bind BEFORE seeding the PID baseline: bindWorkspace indexes
				// ws.WorkspaceKey into knownWorkspaceKeys, and the 30s reconcile tick
				// only builds wantPaths from bound workspaces. Seeding the baseline
				// first left a window where a tick firing between seed and bind would
				// rebuild serenaBackendLastPID without this (not-yet-bound) workspace,
				// dropping the just-seeded baseline (PR #288 r5 adversarial review).
				//
				s.serenaRouterSessions.bindWorkspace(sessionID, ws.WorkspaceKey)
				s.seedSerenaBackendPIDBaseline(r.Context(), ws)
			}

			// Finding 5 (S — one-shot teardown): a path-bearing tool-call with NO
			// client Mcp-Session-Id handshakes a daemon session that
			// resolveDaemonSession could NOT persist (it stores only when the client
			// session id is non-empty), and no later DELETE would ever follow — the
			// session leaks until the daemon's idle expiry. Best-effort DELETE it
			// upstream after the forwarded response completes, exactly like the
			// tools/list one-shot path (fetchToolsListFromAnyDaemon → Finding C). The
			// DELETE is deferred so it fires on EVERY path after the handshake (SSE
			// completion, plain-body completion, AND the post-handshake error returns
			// below), and context-detached so a finished/cancelled request context
			// does not abort the teardown.
			//
			// Guard: only when sessionID == "" (the unpersisted one-shot case). A
			// non-empty sessionID means resolveDaemonSession stored the daemon
			// session against the client session; it is REUSED on later calls and the
			// client-origin DELETE (handleSerenaDelete) tears it down — tearing it
			// down here would break the next tool-call on the same client session.
			if sessionID == "" && daemonSessionID != "" {
				oneShotDaemonSessionID := daemonSessionID
				oneShotURL := upstreamURL
				oneShotWS := ws
				// Finding #8: the one-shot session was established under the
				// daemon-negotiated version; the teardown DELETE carries THAT version
				// (effectiveHandshakeProtocolVersion fills the router default only if
				// the daemon somehow returned an empty version).
				oneShotVersion := effectiveHandshakeProtocolVersion(daemonProtocolVersion)
				defer func() {
					// Invariant D (#6): bound the one-shot teardown by the SHORT
					// cleanup budget, NOT upstreamTimeout (60s default). The tool
					// result is already copied to the client; this teardown must not
					// hold the handler for up to a minute on a hung daemon. Detached
					// from r.Context() (via cleanupContext) so a finished/cancelled
					// request context does not abort the teardown.
					delCtx, delCancel := cleanupContext(upstreamTimeout)
					s.forwardSerenaDeleteUpstream(delCtx, httpClient, oneShotURL, oneShotDaemonSessionID, oneShotVersion, oneShotWS.WorkspaceKey, oneShotWS, auditFn)
					delCancel()
				}()
			}

			// The request entered the per-workspace stop/forward gate before wake and
			// daemon-session resolution. From here through the upstream POST/SSE copy,
			// the completion path also re-stamps last-activity so the just-finished call
			// resets the daemon's idle clock (recordSerenaActivity was stamped ONCE at
			// request start; a long call would otherwise leave a stale timestamp, and the
			// next sweep right after a long call could idle a daemon that was busy until
			// seconds ago).
			restampSerenaForwardOnExit = true

			upstreamReq, ureqErr := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
			if ureqErr != nil {
				http.Error(w, "build upstream request: "+ureqErr.Error(), http.StatusInternalServerError)
				return
			}
			// Thread Content-Type/Accept through verbatim, but NOT the client's
			// Mcp-Session-Id: the daemon does not know the router-minted client id.
			// Set the daemon-issued session id instead (omit it entirely when the
			// daemon is sessionless and issued none).
			for _, h := range []string{"Content-Type", "Accept"} {
				if v := r.Header.Get(h); v != "" {
					upstreamReq.Header.Set(h, v)
				}
			}
			// Finding 1 + Finding #8 (V-forward): forward the version the DAEMON
			// SESSION was established under, not the raw r.Header and not merely the
			// requested version. The tools/call POST is a non-initialize request, so a
			// strict daemon binds the MCP-Protocol-Version header to the version IT
			// negotiated at initialize (internal/api/hub_mcp_handler.go gate 7);
			// daemonProtocolVersion (from resolveDaemonSession) is exactly that version
			// — it may differ from clientProtocolVersion when the daemon negotiated a
			// different supported revision (Finding #8).
			//
			// Round-9 (Finding 1 — presence gate fix): the PRESENCE gate is the
			// RESOLVED forwardVersion, NOT the ORIGINAL clientProtocolVersion. A
			// path-only / older caller that omits MCP-Protocol-Version still drives a
			// daemon handshake (resolveDaemonSession handshakes under the router
			// default when the client header is empty), so daemonProtocolVersion — and
			// hence forwardVersion — is non-empty even though clientProtocolVersion is
			// empty. Gating on clientProtocolVersion != "" suppressed the header in
			// exactly that case, and a strict daemon then 400s the non-initialize
			// tool-call as "session present, no version header". Gating on
			// forwardVersion != "" forwards the resolved version whenever one exists.
			// A truly sessionless daemon issues no session id AND no negotiated version
			// (daemonProtocolVersion == ""), so forwardVersion stays "" when the client
			// also omitted the header → still NO header (the raw-header back-compat for
			// a sessionless/direct caller is preserved).
			forwardVersion := clientProtocolVersion
			if daemonProtocolVersion != "" {
				forwardVersion = daemonProtocolVersion
			}
			if forwardVersion != "" {
				upstreamReq.Header.Set("MCP-Protocol-Version", forwardVersion)
			}
			if daemonSessionID != "" {
				upstreamReq.Header.Set("Mcp-Session-Id", daemonSessionID)
			}
			if upstreamReq.Header.Get("Content-Type") == "" {
				upstreamReq.Header.Set("Content-Type", "application/json")
			}

			upstreamResp, doErr := httpClient.Do(upstreamReq)
			if doErr == nil {
				upstreamReached = true
			}
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
				// §3 fail-loud (always-on floor): the tool-call forward to this
				// workspace's daemon got a CONNECTION error (dead backend / dial
				// refused). This is the zombie path — the daemon binding cache-hit
				// above means a prior call established the session, and the daemon then
				// died. Tear down every session bound to this workspace so the next
				// request fails loud instead of re-forwarding to a dead daemon. Gate on
				// a genuine connection loss so a non-connection transport error does not
				// over-eagerly evict a workspace's sessions.
				if isConnectionLossErr(doErr) {
					s.handleSerenaBackendLossOnForwardFailure(ws.WorkspaceKey, sessionID, doErr, auditFn)
				}
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
				// Finding 4 (store-vs-DELETE TOCTOU — the post-response sticky bind site):
				// the tool-call forward above is a SECOND slow upstream op; a client DELETE
				// (or the sweeper) can terminate the router session WHILE it was in flight.
				// Re-binding the sticky session for a now-terminated id would let a later
				// pathless call route as a legacy sticky session, defeating the DELETE — the
				// same class the resolveDaemonSession recheck closes for the store. So, for a
				// router-minted session, recheck liveness (a peek — known()) before the
				// sticky bind: if the router session is gone, SKIP BindSession and
				// best-effort tear down the daemon binding + the upstream daemon session
				// (which a coordinated DELETE/sweep already removes locally; this also covers
				// the residual store-after-snapshot micro-window where the daemon binding
				// outlived the DELETE). The response is already committed (200 written
				// above), so this site cannot -32600 like the pre-forward store site; it
				// converges the session state instead. A non-router-minted (legacy)
				// session keeps today's unconditional bind.
				if routerSessionKnown && !s.serenaRouterSessions.known(sessionID) {
					if wsKey, daemonSID, daemonPV, ok := s.serenaDaemonSessions.bindingFor(sessionID); ok {
						s.serenaDaemonSessions.unbind(sessionID)
						delCtx, delCancel := cleanupContext(upstreamTimeout)
						s.forwardSerenaDeleteUpstream(delCtx, httpClient, upstreamURL, daemonSID, effectiveHandshakeProtocolVersion(daemonPV), wsKey, ws, auditFn)
						delCancel()
					}
				} else {
					deps.Sessions.BindSession(sessionID, ws)
				}
			}

			if isSSE {
				streamSSE(w, upstreamResp.Body)
				return
			}

			_, _ = io.Copy(w, upstreamResp.Body)
			return
		},
	)
	if gateCallErr != nil {
		http.Error(w, "serena stop gate: "+gateCallErr.Error(), http.StatusInternalServerError)
		return
	}
	if !enteredGate {
		writeJSONRPCErrorStatus(w, tb.ID, http.StatusServiceUnavailable, jsonrpcInternalError,
			"serena daemon stop gate wait exceeded; retry", nil)
		return
	}
	if abortedGate {
		return
	}
}

// attemptSerenaAutoRegister runs the Phase-5 auto-register-on-miss path.
// It is called from the tool-call handler's not-found branch (the resolver
// returned ErrWorkspaceNotFound or a nil entry). It returns the registered
// *api.WorkspaceEntry to forward to, OR nil AFTER it has already written the
// HTTP response — so the caller MUST return when this returns nil.
//
// Outcome mapping (mirrors the api.AutoRegisterSerenaWorkspace contract):
//   - AutoRegisterFn unwired (nil) → writeWorkspaceNotFound 503 (back-compat:
//     identical to the pre-Phase-5 immediate-not-found when routing is only
//     partially wired); return nil.
//   - success → return the entry (caller falls through to the upstream
//     forward exactly as if the workspace had been registered all along).
//   - api.ErrNotASerenaProject → writeWorkspaceNotFound 503 (no `.serena`
//     marker → NOT auto-registrable; this is the load-bearing DoS bound, so
//     behave exactly like today's not-found); return nil.
//   - api.ErrNoLanguages → JSON-RPC -32602 invalid-params at HTTP 422 (the
//     marker exists but declares no languages → unprocessable); return nil.
//   - any other error → JSON-RPC -32603 internal-error at HTTP 503 (install /
//     spawn / readiness failure; the helper rolls back its registry row so no
//     half-registered workspace is left behind); return nil.
//
// The register call runs under a DETACHED + BOUNDED context: detached from
// r.Context() via context.WithoutCancel so a client that disconnects
// mid-register does NOT abort the spawn / leave a half-registered workspace
// (the D-invariant the router's cleanupContext applies to best-effort upstream
// ops), and bounded at 45s to cover install + reconcile + readiness. Unlike
// cleanupContext (which detaches via context.Background() and drops
// request-scoped values), WithoutCancel preserves request values while
// severing only cancellation — the exact posture an important best-effort
// register needs.
func (s *Server) attemptSerenaAutoRegister(w http.ResponseWriter, r *http.Request, deps *serenaRouterDeps, id json.RawMessage, sessionID, pathArg string) *api.WorkspaceEntry {
	if deps.AutoRegisterFn == nil {
		writeWorkspaceNotFound(w, pathArg, false)
		return nil
	}

	// bot PR #253 P2/r6: validate the router session/version BEFORE auto-registering,
	// so an expired/terminated/unknown or protocol-version-mismatched client cannot
	// trigger registration (pool-port allocation + workspaces.yaml / supervisor-intent
	// mutation). This mirrors the authoritative post-branch version gate; for a LIVE
	// session peekVersionState is non-mutating, so that gate still re-validates and
	// touches normally after the forward. A NON-EMPTY id that is EXPIRED (idle-swept on
	// this read) OR ABSENT (minted here then DELETEd/swept — the entry is removed, so a
	// re-read reads absent — or one this router never minted) is rejected here: a
	// revoked/unknown session must not durably mutate the registry/intent. A genuine
	// first-call uses an EMPTY id (skips this block) or a live minted session.
	watchSession := false
	if sessionID != "" {
		sessionVersion, state := s.serenaRouterSessions.peekVersionState(sessionID)
		switch state {
		case routerSessionExpired:
			// Minted here, idle-swept on THIS read (expire-on-read): coordinate the
			// cross-store unbind, then reject.
			s.coordinateExpiredRouterSessionUnbind(sessionID, deps.Sessions)
			writeJSONRPCErrorStatus(w, id, http.StatusBadRequest, jsonrpcInvalidRequest, "session terminated", nil)
			return nil
		case routerSessionAbsent:
			// Minted-then-DELETEd (entry removed → re-read absent) OR never minted at
			// this router. On the registration-MUTATION path, refuse — do NOT allocate a
			// pool port / mutate registry+intent for a revoked/unknown session. (The main
			// ROUTING handler still treats absent as a legacy/path-only caller for
			// EXISTING workspaces; that leniency is for routing, NOT for registration.
			// The asymmetry is intentional — do NOT harmonize the two gates; see
			// docs/serena-lifecycle-invariants.md §3.)
			writeJSONRPCErrorStatus(w, id, http.StatusBadRequest, jsonrpcInvalidRequest, "session terminated", nil)
			return nil
		case routerSessionLive:
			if cv := r.Header.Get("MCP-Protocol-Version"); cv != "" && cv != sessionVersion {
				writeJSONRPCErrorStatus(w, id, http.StatusBadRequest, jsonrpcInvalidRequest, "protocol-version mismatch", nil)
				return nil
			}
			// A live, minted router session: watch it for the duration of the
			// (detached) register and abort if it is terminated mid-flight.
			watchSession = true
		}
	}

	regCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
	defer cancel()

	// bot PR #253 P2: the register is DETACHED from r.Context() so a client
	// disconnect does not abort it — but a logical session TERMINATION (client
	// DELETE or idle-sweep) DURING the up-to-45s register MUST still abort it, so a
	// terminated session cannot finish allocating a pool port + committing a
	// supervisor-intent daemon. Watch the live session and cancel regCtx on death;
	// the helper re-checks ctx.Err() at its registry-Save and install-commit
	// mutation boundaries. The watcher stops when the register returns.
	if watchSession {
		stop := make(chan struct{})
		defer close(stop)
		go s.cancelAutoRegisterOnSessionDeath(regCtx, cancel, stop, sessionID, deps.Sessions)
	}

	entry, err := deps.AutoRegisterFn(regCtx, pathArg)
	if err == nil {
		if deps.AuditFn != nil {
			port := 0
			if entry != nil {
				port = entry.Port
			}
			_ = deps.AuditFn("info", "serena-workspace-auto-registered", map[string]any{
				"path": pathArg,
				"port": port,
			})
		}
		return entry
	}

	switch {
	case errors.Is(err, api.ErrNotASerenaProject):
		// No `.serena/project.yml` marker → not auto-registrable. This is the
		// DoS bound: behave exactly like today's workspace-not-found 503 so an
		// attacker cannot register an arbitrary path they do not own.
		writeWorkspaceNotFound(w, pathArg, false)
		return nil
	case errors.Is(err, api.ErrNoLanguages):
		writeJSONRPCErrorStatus(w, id, http.StatusUnprocessableEntity, jsonrpcInvalidParams,
			"serena project at "+pathArg+" declares no languages", nil)
		return nil
	case errors.Is(err, api.ErrSerenaRootNotTrusted):
		// AREA-5 TRUST GATE refusal. The marker exists (it is a serena project)
		// but its root is not contained by any operator-trusted root, so
		// auto-register was refused BEFORE any state mutation (fail-closed). Map
		// to 503/-32002 with an actionable message AND a machine-readable
		// `code:"NEEDS_TRUST"` + sanitized candidate path in `data` (gap-a option
		// B — future-proofs one-click trust; the path is C0/C1/ESC-stripped via
		// sanitizeRefusalPath so an attacker-controlled tool-argument path can
		// never inject terminal-control sequences into the client/UI/logs).
		//
		// area-5 r2 (codex P2): surface the CANONICAL RESOLVED root the gate
		// actually checked — NOT the raw tool-arg `pathArg`, which may be a FILE
		// or SUBDIRECTORY inside the project. `mcphub trust <subpath>` would trust
		// the WRONG path and still leave the project unauthorized. The resolved
		// root rides on the typed *api.SerenaRootNotTrustedError; extract it via
		// errors.As. Defensive fallback to pathArg only if the resolved root is
		// somehow empty (never on a real refusal).
		refusedPath := pathArg
		var notTrusted *api.SerenaRootNotTrustedError
		if errors.As(err, &notTrusted) && notTrusted.ResolvedRoot != "" {
			refusedPath = notTrusted.ResolvedRoot
		}
		safePath := sanitizeRefusalPath(refusedPath)
		writeJSONRPCErrorStatus(w, id, http.StatusServiceUnavailable, serenaRootNotTrustedCode,
			"workspace "+safePath+" is not a trusted folder; run `mcphub trust "+safePath+"` or add it in GUI Settings → Trusted Roots, then retry",
			map[string]any{
				"code": "NEEDS_TRUST",
				"path": safePath,
			})
		return nil
	default:
		writeJSONRPCErrorStatus(w, id, http.StatusServiceUnavailable, jsonrpcInternalError,
			"serena auto-register failed: "+err.Error(), nil)
		return nil
	}
}

// cancelAutoRegisterOnSessionDeath polls the router session for sessionID and
// cancels the detached auto-register context the instant the session stops being
// live — a client DELETE makes it routerSessionAbsent, the idle-sweep makes it
// routerSessionExpired; either terminates the in-flight register (bot PR #253
// P2). It returns when the register completes (stop is closed) or the context is
// already done. peekVersionState does NOT refresh lastSeen, so polling here never
// keeps a session artificially alive. The 500 ms cadence bounds the post-death
// window without busy-spinning over the up-to-45 s register.
func (s *Server) cancelAutoRegisterOnSessionDeath(ctx context.Context, cancel context.CancelFunc, stop <-chan struct{}, sessionID string, sessions sessionRouter) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, state := s.serenaRouterSessions.peekVersionState(sessionID)
			if state == routerSessionLive {
				continue
			}
			// bot PR #253 r5 P2: peekVersionState above is expire-on-read — for an
			// idle-expired session it just DELETED the router-session entry. Mirror the
			// request paths and coordinate the sticky + daemon unbind so a later
			// pathless call with this id is not routed through a stale sticky binding
			// (it would otherwise see routerSessionAbsent and be treated as a legacy
			// caller). For a client DELETE (routerSessionAbsent) handleSerenaDelete
			// already coordinated the unbind, so this is a harmless no-op there.
			if state == routerSessionExpired {
				s.coordinateExpiredRouterSessionUnbind(sessionID, sessions)
			}
			cancel()
			return
		}
	}
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

	// Finding 3 (V-validate): for a KNOWN router session, reject a conflicting
	// MCP-Protocol-Version BEFORE any snapshot/unbind so a mismatched DELETE
	// leaves the session intact. Missing/matching header (or an unknown
	// session) falls through to the best-effort teardown below.
	//
	// Finding 2 (V-forward): capture the version to forward on the upstream
	// DELETE HERE, before the unbind below clears the router-session record.
	// A known session forwards its negotiated version (the version the
	// daemon session was initialized under); an unknown session falls back to
	// the raw request header (today's behavior for a legacy caller).
	deleteProtocolVersion := r.Header.Get("MCP-Protocol-Version")
	// Finding 4: PEEK (no lastSeen refresh). A DELETE rejected for a version
	// mismatch leaves the session intact and must NOT extend its idle life; a
	// matching/missing-header DELETE proceeds to tear the session DOWN below
	// (so there is nothing to touch — the unbind removes the binding entirely).
	if sessionVersion, known := s.serenaRouterSessions.peekNegotiatedVersion(sessionID); known {
		if clientProtocolVersion := r.Header.Get("MCP-Protocol-Version"); clientProtocolVersion != "" && clientProtocolVersion != sessionVersion {
			writeJSONRPCErrorStatus(w, json.RawMessage("null"), http.StatusBadRequest, jsonrpcInvalidRequest,
				"protocol-version mismatch", nil)
			return
		}
		deleteProtocolVersion = sessionVersion
	}

	deps := s.serenaRouterDepsProd()
	// Snapshot the daemon binding up front so the upstream DELETE still has
	// the daemon-issued session id AFTER we revoke the local mappings below.
	// Finding #8: also capture the daemon-negotiated version the session was
	// established under — the teardown DELETE forwards THAT version (a strict
	// daemon binds the header on the non-initialize DELETE to its session's
	// initialized version), not the router-client session version.
	wsKey, daemonSessionID, daemonProtocolVersion, hasBinding := s.serenaDaemonSessions.bindingFor(sessionID)

	// Finding B (cheap pre-unbind snapshot ONLY): capture the sticky binding's
	// workspace BEFORE the revocation below clears the sticky map. This is a
	// cheap in-memory map lookup (serena_routing.SessionRouter.LookupSession /
	// InMemorySessionRouter), NOT the blockable registry scan — it is the
	// fallback the upstream DELETE needs for Finding B's leak case (handshake
	// completed but the tool POST failed before BindSession ran, so the daemon
	// binding has an EMPTY wsKey while the sticky binding still resolves the
	// workspace). It must be read before the unbind because UnbindSession would
	// otherwise drop it. The by-key resolution (the potentially-blocking part)
	// is deferred to AFTER the unbind below.
	var stickyWS *api.WorkspaceEntry
	if hasBinding && deps != nil && deps.Sessions != nil {
		stickyWS = deps.Sessions.LookupSession(sessionID)
	}

	// Finding A + P2 (Codex PR #249) — revoke every local binding FIRST, BEFORE
	// the (potentially-blocking) workspace resolution below, so a concurrent
	// POST with the same Mcp-Session-Id cannot pass the router/sticky/daemon
	// lookups and start another tool call during the revocation window. The
	// PRIOR order resolved the teardown workspace via resolveDeleteWorkspace ->
	// ListWorkspaces() — which can BLOCK when the registry refresh holds the
	// cross-process lock (internal/api/serena_routing/resolver.go refresh) —
	// BEFORE unbinding the local state, so a concurrent POST could route during
	// that blocking window and contradict the immediate-revocation guarantee
	// the DELETE path provides (Codex round-7 #A). Unbinding here (cheap,
	// in-memory) revokes the session immediately; the cheap snapshots above
	// (daemon binding via bindingFor + the sticky workspace) mean unbinding
	// first cannot lose the data the forward needs.
	s.serenaDaemonSessions.unbind(sessionID)
	s.serenaRouterSessions.unbind(sessionID)
	if deps != nil && deps.Sessions != nil {
		deps.Sessions.UnbindSession(sessionID)
	}

	// Findings A + B (workspace resolution moved to AFTER the unbind): now that
	// the session is revoked, resolve the workspace for the best-effort upstream
	// DELETE using the SNAPSHOTTED wsKey. The by-key path (resolveWorkspaceByKey
	// -> ListWorkspaces) is the authoritative source and is the
	// potentially-blocking call Finding 3 requires to run after revocation; the
	// pre-snapshotted sticky workspace is the Finding B fallback when the daemon
	// binding had an empty/unresolvable wsKey. A missing workspace just skips the
	// forward — the local revocation already ran so the router does not leak the
	// mapping.
	var delWS *api.WorkspaceEntry
	if hasBinding && deps != nil {
		if ws := s.resolveWorkspaceByKey(deps, wsKey); ws != nil {
			delWS = ws
		} else {
			delWS = stickyWS
		}
	}

	// Best-effort upstream DELETE using the post-unbind workspace resolution
	// above. A missing workspace (or nil URL) just skips the forward — the
	// local revocation already ran so the router does not leak the mapping.
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

		// Detach from r.Context() (mirror the hub's handleDelete,
		// internal/api/hub_mcp_handler.go): a client that disconnects
		// right after sending DELETE would otherwise cancel r.Context()
		// immediately and short-circuit the daemon-side teardown —
		// leaking the upstream session this finding exists to release.
		//
		// Bound: this is handleSerenaDelete's MAIN teardown — the thing the
		// client's DELETE is awaiting (the 204 fires after it), NOT a
		// fire-and-forget call. sonnet post-PASS finding 1: it used to use the
		// 60s upstreamTimeout default, which blocked the client's 204 for up to
		// a minute AND held this handler goroutine 60s after a client
		// disconnect (the context is detached from r.Context()) on a hung/slow
		// daemon. The local revocation already ran above (Finding A), so a slow
		// daemon cannot delay revocation — only the 204 ack. Cap it at the SHORT
		// serenaDeleteTimeout (5s, matching the hub's per-daemon teardown budget
		// and internal/daemon/http_host.go's httpHostCleanupTimeout) so the
		// client's teardown 204 lands within ~5s regardless of daemon health.
		// Still detached from r.Context() (correct — survives client
		// disconnect), just bounded to 5s instead of 60s. (This is distinct from
		// the Invariant-D fire-and-forget cleanupContext sites #4/#6/#7 + the
		// displaced-rebind teardown, which use serenaCleanupTimeout/min budget;
		// the DELETE forward is awaited so it gets its own equal-5s constant.)
		//
		// Finding #8 + Finding 2 (V-forward): forward the version the daemon
		// session was established under. Prefer the persisted
		// daemonProtocolVersion (the daemon-negotiated version on the
		// binding); fall back to the router-client session version
		// (deleteProtocolVersion) for a legacy binding stored before #8 or
		// an unknown router session. effectiveHandshakeProtocolVersion fills
		// the router default if both are empty, so the teardown always
		// carries a non-empty MCP-Protocol-Version and a strict daemon does
		// not 400 the DELETE.
		teardownVersion := daemonProtocolVersion
		if teardownVersion == "" {
			teardownVersion = deleteProtocolVersion
		}
		func() {
			gateKey := delWS.WorkspaceKey
			if gateKey == "" {
				gateKey = wsKey
			}
			fwdCtx, fwdCancel := context.WithTimeout(context.Background(), serenaDeleteTimeout)
			defer fwdCancel()
			resolvedForwardByKey := false
			entered, aborted, err := s.withSerenaWorkspaceGate(
				fwdCtx,
				gateKey,
				serenaWorkspaceGatePolicyBlock,
				func(key string) *api.WorkspaceEntry {
					if resolved := s.resolveWorkspaceByKey(deps, key); resolved != nil {
						resolvedForwardByKey = true
						return resolved
					}
					resolvedForwardByKey = false
					if key == gateKey {
						return delWS
					}
					return nil
				},
				upstreamURLFn,
				nil,
				func(out *serenaWorkspaceGateOutcome) (err error) {
					if out.gate.waitedThroughPrune && !resolvedForwardByKey {
						return
					}
					if out.ws == nil || out.upstreamURL == "" {
						return
					}
					s.forwardSerenaDeleteUpstream(fwdCtx, httpClient, out.upstreamURL, daemonSessionID, effectiveHandshakeProtocolVersion(teardownVersion), wsKey, out.ws, auditFn)
					return
				},
			)
			if !entered || aborted || err != nil {
				return
			}
		}()
	}

	w.WriteHeader(http.StatusNoContent)
}

// resolveDeleteWorkspace resolves the workspace for a DELETE/cancel forward
// from a client session that carries no path-arg (Findings B + H). The DELETE
// / cancel must reach the daemon that actually holds the daemon session being
// torn down / cancelled, so the daemon binding's wsKey is the AUTHORITATIVE
// key — the daemon session id and its workspace travel together.
//
// Round-9 (Finding 2 — prefer the daemon binding's wsKey over the stale
// sticky binding): the by-key resolution is tried FIRST when wsKey is
// non-empty. During a workspace switch the daemon binding (serenaDaemonSessions)
// is updated to the NEW workspace BEFORE the sticky deps.Sessions binding is
// refreshed (sticky BindSession runs only AFTER the upstream tool response).
// In that window a concurrent DELETE / notifications/cancelled passes the NEW
// wsKey (read from the daemon binding by both callers) while the sticky
// deps.Sessions.LookupSession still returns the OLD workspace. The pre-fix
// order tried the sticky lookup FIRST and so resolved the OLD workspace,
// forwarding the (new) daemon session id to the WRONG daemon — failing to
// actually tear down / cancel. Resolving by the daemon binding's wsKey first
// keeps the daemon session id and its workspace consistent. The sticky lookup
// remains the fallback for Finding B's leak case (a daemon binding with an
// EMPTY wsKey, or a key no longer registered, while the sticky binding still
// resolves). Returns nil when neither path resolves it.
func (s *Server) resolveDeleteWorkspace(deps *serenaRouterDeps, sessionID, wsKey string) *api.WorkspaceEntry {
	if deps == nil {
		return nil
	}
	// Prefer the daemon binding's wsKey (the workspace the daemon session
	// actually belongs to) so a mid-switch stale sticky binding cannot
	// misroute the teardown/cancel to the old daemon (Finding 2).
	if ws := s.resolveWorkspaceByKey(deps, wsKey); ws != nil {
		return ws
	}
	// Fallback: the daemon binding had no resolvable wsKey (empty, or a key
	// no longer registered) but the sticky binding still resolves (Finding B).
	if deps.Sessions != nil && sessionID != "" {
		if ws := deps.Sessions.LookupSession(sessionID); ws != nil {
			return ws
		}
	}
	return nil
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
//
// Finding 2 (V-forward): protocolVersion is set on the MCP-Protocol-Version
// header. DELETE is a non-initialize request, so a strict daemon that binds
// the header to the session's initialized version
// (internal/api/hub_mcp_handler.go gate 7 / handleDelete gate 7) 400s a
// teardown that omits or mismatches it — leaking the upstream session while
// the client gets its 204. Callers pass the negotiated version
// (effectiveHandshakeProtocolVersion-resolved). An empty value omits the
// header (sessionless daemon / legacy caller).
func (s *Server) forwardSerenaDeleteUpstream(ctx context.Context, httpClient *http.Client, upstreamURL, daemonSessionID, protocolVersion, wsKey string, ws *api.WorkspaceEntry, auditFn func(level, event string, fields map[string]any) error) {
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
	if protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
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
//
// Finding 3 (V-validate): for a KNOWN router session whose incoming
// MCP-Protocol-Version CONFLICTS with the negotiated version, the cancel is
// NOT forwarded — it is acknowledged 202 with no upstream forward. This
// mirrors the hub's gate-7 handling of a version-mismatched notification
// (internal/api/hub_mcp_handler.go handlePost: a notification on a
// mismatched version returns 202 and is silently dropped, never dispatched).
// Forwarding a cross-version cancel would let a stray/buggy client cancel an
// in-flight daemon request it never legitimately negotiated. A matching or
// missing header (or an UNKNOWN session — a legacy direct caller) forwards
// as before with the resolved version (V-forward).
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

	// Finding 3 (V-validate) + V-forward: resolve the forward version and
	// decide whether to forward at all. For a KNOWN session a raw header that
	// conflicts with the negotiated version suppresses the forward (silent
	// 202, mirroring the hub); a matching/missing header forwards with the
	// negotiated version. For an UNKNOWN session the raw header drives the
	// forward (today's legacy behavior). protocolVersion is the value sent on
	// the forwarded cancel (resolved below); forward stays false on mismatch.
	rawProtocolVersion := r.Header.Get("MCP-Protocol-Version")
	protocolVersion := rawProtocolVersion
	forward := true
	// Finding 4: PEEK (no lastSeen refresh) for the pre-gate validation. A
	// cross-version cancel that is suppressed below must NOT keep the session
	// alive; lastSeen is refreshed via touch only on the ACCEPTED (forwarded)
	// path.
	if sessionVersion, known := s.serenaRouterSessions.peekNegotiatedVersion(sessionID); known {
		if rawProtocolVersion != "" && rawProtocolVersion != sessionVersion {
			// Cross-version cancel on a known session: do NOT forward.
			forward = false
		} else if !s.serenaRouterSessions.touch(sessionID) {
			// Round-10 (Finding 1 — re-check liveness AFTER the gate, mirroring
			// the hub's post-gate Touch, internal/api/hub_mcp_handler.go:402-409).
			// The peek above was PRE-gate; the cleanup ticker or a client DELETE
			// can sweep/terminate the session between that peek and now. touch
			// refreshes lastSeen ONLY for a still-live binding and reports whether
			// it did. A false return means the session was swept/terminated
			// mid-flight, so do NOT forward the cancel to the (now-released)
			// daemon session — the local 202 is kept (notification semantics: a
			// notifications/cancelled gets no JSON-RPC response, the same posture
			// the hub takes for a notification on a swept session,
			// internal/api/hub_mcp_handler.go:403-405).
			forward = false
		}
		// Gate passed AND touch refreshed a live binding (forward stays true): a
		// forwarded cancel is legitimate session activity. A suppressed
		// cross-version OR swept-session cancel skipped the touch.
		protocolVersion = sessionVersion
	}

	if forward && sessionID != "" {
		if wsKey, daemonSessionID, daemonProtocolVersion, ok := s.serenaDaemonSessions.bindingFor(sessionID); ok {
			if ws := s.resolveDeleteWorkspace(deps, sessionID, wsKey); ws != nil {
				if upstreamURL := upstreamURLFn(ws); upstreamURL != "" {
					// Finding #8 + V-forward: send the version the daemon SESSION
					// was established under (persisted daemonProtocolVersion);
					// fall back to the router-client session version
					// (protocolVersion) for a legacy binding stored before #8.
					// A strict daemon binds the MCP-Protocol-Version header on a
					// non-initialize POST to its session's initialized version.
					forwardVersion := daemonProtocolVersion
					if forwardVersion == "" {
						forwardVersion = protocolVersion
					}
					// sonnet post-PASS finding 2: forward the cancel ASYNCHRONOUSLY
					// and write the 202 immediately (below). This handler's own
					// contract is "the client's 202 must be effectively immediate" —
					// a notifications/cancelled carries NO response body, so a
					// strict/pipelined client can observe up-to-cleanup-budget (≤5s)
					// latency before the 202 if the forward blocks it. The pre-fix
					// code forwarded SYNCHRONOUSLY (bounded by cleanupContext ≤5s)
					// and wrote the 202 only after, contradicting that comment. The
					// fix is semantically identical — still best-effort, still
					// DETACHED from r.Context() (a client disconnect right after the
					// cancel must NOT abort the daemon-side forward) and still bounded
					// by cleanupContext (Invariant D #7) — just non-blocking. The
					// forward/no-forward decision (the version gate + the round-10
					// swept-session-touch abort above) already ran SYNCHRONOUSLY, so
					// launching here cannot resurrect a suppressed/swept cancel.
					//
					// Capture-by-value (CAUTION): the goroutine outlives this
					// request, so it must NOT reference request-scoped state that may
					// be reused. daemonSessionID / forwardVersion / wsKey / upstreamURL
					// are block-local copies already; ws / httpClient / auditFn are
					// stable for the request; body is COPIED into a fresh slice so a
					// later reuse of the inbound buffer cannot race the forward's
					// bytes.NewReader. fwdCancel's defer moves INTO the goroutine.
					cancelBody := append([]byte(nil), body...)
					fwdWS := ws
					fwdURL := upstreamURL
					fwdDaemonSID := daemonSessionID
					fwdVersion := effectiveHandshakeProtocolVersion(forwardVersion)
					fwdWSKey := wsKey
					fwdGateKey := wsKey
					if fwdGateKey == "" {
						fwdGateKey = ws.WorkspaceKey
					}
					fwdClient := httpClient
					fwdAudit := auditFn
					fwdTimeout := deps.UpstreamTimeout
					fwdDeps := deps
					go func() {
						fwdCtx, fwdCancel := cleanupContext(fwdTimeout)
						defer fwdCancel()
						resolvedForwardByKey := false
						_, _, _ = s.withSerenaWorkspaceGate(
							fwdCtx,
							fwdGateKey,
							serenaWorkspaceGatePolicyBlock,
							func(key string) *api.WorkspaceEntry {
								if resolved := s.resolveWorkspaceByKey(fwdDeps, key); resolved != nil {
									resolvedForwardByKey = true
									return resolved
								}
								resolvedForwardByKey = false
								if key == fwdGateKey {
									return fwdWS
								}
								return nil
							},
							upstreamURLFn,
							nil,
							func(out *serenaWorkspaceGateOutcome) (err error) {
								if out.gate.waitedThroughPrune && !resolvedForwardByKey {
									return
								}
								forwardURL := out.upstreamURL
								if forwardURL == "" && out.ws == fwdWS {
									forwardURL = fwdURL
								}
								if out.ws == nil || forwardURL == "" {
									return
								}
								s.forwardSerenaCancelledUpstream(fwdCtx, fwdClient, forwardURL, fwdDaemonSID, fwdVersion, fwdWSKey, out.ws, cancelBody, fwdAudit)
								return
							},
						)
					}()
				}
			}
		}
	}

	// notifications/* (genuine notification) gets no JSON-RPC response body, and
	// the upstream cancel forward (if any) was launched detached above — the 202
	// is written immediately, not after the forward completes (finding 2).
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
// plan: http://127.0.0.1:<workspace.Port>/mcp.
func defaultUpstreamURL(ws *api.WorkspaceEntry) string {
	if ws == nil || ws.Port <= 0 {
		return ""
	}
	return clients.HubLoopbackURL(ws.Port, "/mcp")
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

// isConnectionLossErr reports whether err is a genuine TRANSPORT/CONNECTION
// failure to the upstream serena daemon — a dial refused (dead backend), a
// connection reset, or a connection dropped mid-request — as opposed to a
// timeout (a slow-but-live daemon) or a protocol-level handshake error (a live
// daemon that returned a malformed/error initialize result). It is the §3
// fail-loud floor's trigger gate: ONLY a real connection loss is a backend-loss
// signal worth tearing down a workspace's sessions for. A timeout is excluded
// (the caller checks isTimeoutErr first, and Timeout() net errors are not
// matched here either); a non-net error (e.g. "upstream initialize returned no
// result") returns false so a transient protocol glitch on a live daemon does
// NOT trigger a session teardown.
func isConnectionLossErr(err error) bool {
	if err == nil {
		return false
	}
	// A client-side cancel (MCP client interrupts an in-flight tool call /
	// disconnects mid-request — common with Claude Code) or a context deadline
	// is NOT a backend-loss signal: the upstream daemon may be perfectly alive.
	// The forward sites carry r.Context() (defaultSerenaClient uses Timeout:0),
	// so a client disconnect surfaces as a *url.Error wrapping context.Canceled,
	// which (a) satisfies net.Error with Timeout()==false and (b) would otherwise
	// fall into the net.Error branch below and be MISCLASSIFIED as a connection
	// loss → false-positive workspace-wide session teardown. Exclude both context
	// errors up front so only a genuine transport-layer connection loss qualifies.
	// (DeadlineExceeded is already filtered by isTimeoutErr at the call sites;
	// excluding it here makes the predicate self-contained for any caller.)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A net.Error that is NOT a timeout (dial refused, reset, broken pipe) is a
	// connection loss. http.Client.Do wraps the dial/transport error in a
	// *url.Error whose inner err is the *net.OpError; errors.As unwraps it.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return !netErr.Timeout()
	}
	// Direct syscall-level connection errors (refused / reset / aborted) in case
	// a path surfaces the raw errno without a net.Error wrapper.
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) {
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
