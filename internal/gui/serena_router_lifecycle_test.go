// internal/gui/serena_router_lifecycle_test.go
//
// ROUTER-COMPLETION phase tests: the /serena/mcp router synthesizes the
// non-tool MCP lifecycle (initialize, tools/list, notifications/*, ping)
// workspace-agnostically while leaving tool-call path-routing unchanged.
//
// These complement serena_router_test.go (tool-call routing, session
// bind, SSE, upstream errors) which must keep passing.
package gui

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/serena_routing"
)

// buildLifecycleBody marshals a JSON-RPC envelope for a non-tool method.
// params may be nil (omitted). id is 1 verbatim.
func buildLifecycleBody(t *testing.T, method string, params any) []byte {
	t.Helper()
	env := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
	}
	if params != nil {
		env["params"] = params
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal %s envelope: %v", method, err)
	}
	return body
}

// listerStubResolver embeds the path-routing stubResolver and adds the
// workspaceLister capability so the router can proxy tools/list.
type listerStubResolver struct {
	stubResolver
	list []*api.WorkspaceEntry
}

func (r *listerStubResolver) ListWorkspaces() []*api.WorkspaceEntry { return r.list }

// mintRouterSession drives a real synthesized initialize at the router and
// returns the minted client Mcp-Session-Id. Finding D requires tools/list
// to carry a session id minted by a prior initialize at this router, so
// the tools/list tests establish one through this helper rather than
// inventing a raw id the router never minted.
func mintRouterSession(t *testing.T, s *Server, protocolVersion string) string {
	t.Helper()
	body := buildLifecycleBody(t, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
	})
	rr := postSerena(t, s, body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("mintRouterSession initialize status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	sid := rr.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("mintRouterSession: initialize did not mint a Mcp-Session-Id")
	}
	return sid
}

// ---------------------------------------------------------------------
// initialize — 200 + JSON-RPC result with protocolVersion/serverInfo/
// capabilities + a minted Mcp-Session-Id header.
// ---------------------------------------------------------------------
func TestSerenaRouter_Initialize_SynthesizesResultAndSession(t *testing.T) {
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return "http://unused" },
	}
	s := newSerenaTestServer(t, deps)

	body := buildLifecycleBody(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test-client", "version": "1.0"},
	})
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  *struct {
			ProtocolVersion string            `json:"protocolVersion"`
			Capabilities    map[string]any    `json:"capabilities"`
			ServerInfo      map[string]string `json:"serverInfo"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, rr.Body.String())
	}
	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", resp.JSONRPC)
	}
	if string(resp.ID) != "1" {
		t.Errorf("id = %s, want 1 (verbatim echo)", string(resp.ID))
	}
	if len(resp.Error) > 0 {
		t.Fatalf("expected result, got error: %s", string(resp.Error))
	}
	if resp.Result == nil {
		t.Fatalf("result missing; raw=%s", rr.Body.String())
	}
	if resp.Result.ProtocolVersion != "2025-06-18" {
		t.Errorf("protocolVersion = %q, want echoed 2025-06-18", resp.Result.ProtocolVersion)
	}
	if resp.Result.ServerInfo["name"] != "serena" {
		t.Errorf("serverInfo.name = %q, want serena", resp.Result.ServerInfo["name"])
	}
	if resp.Result.ServerInfo["version"] == "" {
		t.Errorf("serverInfo.version is empty, want non-empty")
	}
	if _, ok := resp.Result.Capabilities["tools"]; !ok {
		t.Errorf("capabilities missing 'tools'; got %v", resp.Result.Capabilities)
	}
	sid := rr.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Errorf("Mcp-Session-Id response header is empty, want a minted id")
	}
	// The minted session is NOT yet bound to a workspace (binding happens
	// on the first path-bearing tools/call). LookupSession must return nil.
	if got := deps.Sessions.LookupSession(sid); got != nil {
		t.Errorf("session %q bound to %+v at initialize; want unbound until first tools/call", sid, got)
	}
}

// P2 finding 5 — initialize with an UNSUPPORTED protocolVersion is
// rejected with -32600 at HTTP 200 (JSON-RPC error in-band) carrying
// error.data.supported + requested, NOT silently negotiated down to the
// router default (which would hide a non-compliant client). Mirrors the
// hub handler's strict-rejection posture
// (internal/api/hub_mcp_handler.go). Replaces the pre-Finding-5
// negotiate-down test.
func TestSerenaRouter_Initialize_UnsupportedProtocolVersionRejected(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &stubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	body := buildLifecycleBody(t, "initialize", map[string]any{
		"protocolVersion": "1999-01-01",
		"capabilities":    map[string]any{},
	})
	rr := postSerena(t, s, body, nil)
	// JSON-RPC errors are carried in-band at HTTP 200 (matches the hub's
	// unsupported-version path).
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (in-band JSON-RPC error); body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int            `json:"code"`
			Message string         `json:"message"`
			Data    map[string]any `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rr.Body.String())
	}
	if len(resp.Result) > 0 {
		t.Errorf("result present on unsupported-version initialize; want error only. result=%s", string(resp.Result))
	}
	if resp.Error == nil {
		t.Fatalf("expected JSON-RPC error for unsupported protocolVersion; raw=%s", rr.Body.String())
	}
	if resp.Error.Code != jsonrpcInvalidRequest {
		t.Errorf("error code = %d, want %d (-32600)", resp.Error.Code, jsonrpcInvalidRequest)
	}
	if resp.Error.Data["requested"] != "1999-01-01" {
		t.Errorf("error.data.requested = %v, want 1999-01-01", resp.Error.Data["requested"])
	}
	if _, ok := resp.Error.Data["supported"]; !ok {
		t.Errorf("error.data missing supported list; got %v", resp.Error.Data)
	}
	// No session must be minted on a rejected initialize.
	if sid := rr.Header().Get("Mcp-Session-Id"); sid != "" {
		t.Errorf("unsupported-version initialize minted Mcp-Session-Id %q; a rejected initialize gets no session", sid)
	}
}

// P2 finding 5 — initialize with ABSENT/empty protocolVersion is a
// -32602 invalid-params error at HTTP 400 (mirrors the hub) carrying
// error.data.supported, and mints no session.
func TestSerenaRouter_Initialize_MissingProtocolVersionRejected(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &stubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	// params present but no protocolVersion field.
	body := buildLifecycleBody(t, "initialize", map[string]any{"capabilities": map[string]any{}})
	rr := postSerena(t, s, body, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int            `json:"code"`
			Data map[string]any `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rr.Body.String())
	}
	if resp.Error == nil || resp.Error.Code != jsonrpcInvalidParams {
		t.Fatalf("expected -32602 invalid-params; got %+v", resp.Error)
	}
	if _, ok := resp.Error.Data["supported"]; !ok {
		t.Errorf("error.data missing supported list; got %v", resp.Error.Data)
	}
	if sid := rr.Header().Get("Mcp-Session-Id"); sid != "" {
		t.Errorf("missing-version initialize minted Mcp-Session-Id %q; want none", sid)
	}
}

// P2 finding 5 — initialize whose params.protocolVersion is the WRONG TYPE
// (e.g. a number) is a -32602 invalid-params error at HTTP 400, not a
// silent default-negotiation.
func TestSerenaRouter_Initialize_TypeMismatchedParamsRejected(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &stubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	// protocolVersion as a JSON number -> unmarshal into the string field
	// fails -> -32602.
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":123,"capabilities":{}}}`)
	rr := postSerena(t, s, body, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rr.Body.String())
	}
	if resp.Error == nil || resp.Error.Code != jsonrpcInvalidParams {
		t.Fatalf("expected -32602 invalid-params for type-mismatched params; got %+v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "invalid initialize params") {
		t.Errorf("error message %q should name invalid initialize params", resp.Error.Message)
	}
}

// P2 finding 1 — initialize carrying an Mcp-Session-Id header is REJECTED
// with -32600 "session-id only valid after initialize" at HTTP 400, and
// mints no session. initialize is what ESTABLISHES the session; echoing a
// caller-supplied id would let a client reinitialize-with-stale-id and
// keep a prior workspace/daemon binding. Mirrors the hub handler
// (internal/api/hub_mcp_handler.go). Replaces the pre-Finding-1
// echo-the-id test.
func TestSerenaRouter_Initialize_RejectsIncomingSessionID(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &stubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	body := buildLifecycleBody(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
	})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "client-supplied-id"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rr.Body.String())
	}
	if len(resp.Result) > 0 {
		t.Errorf("result present on rejected initialize; want error only")
	}
	if resp.Error == nil || resp.Error.Code != jsonrpcInvalidRequest {
		t.Fatalf("expected -32600 Invalid Request; got %+v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "session-id only valid after initialize") {
		t.Errorf("error message = %q, want 'session-id only valid after initialize'", resp.Error.Message)
	}
	// The router must NOT echo or mint a session on the rejected request.
	if got := rr.Header().Get("Mcp-Session-Id"); got != "" {
		t.Errorf("Mcp-Session-Id = %q on a rejected initialize; want none", got)
	}
}

// ---------------------------------------------------------------------
// tools/list — fetch-and-cache from a live daemon, then serve from cache.
// ---------------------------------------------------------------------
func TestSerenaRouter_ToolsList_FetchAndCacheFromLiveDaemon(t *testing.T) {
	// The daemon requires the MCP handshake before answering tools/list
	// (P1 #2). The fake daemon mints a session on initialize and rejects
	// a tools/list that arrives without it; the tool handler asserts the
	// session-gated body carried tools/list AND a daemon session id, then
	// returns the catalog. "toolHits" counts only session-gated tools/list
	// calls — the handshake POSTs are counted separately.
	daemon := newFakeSerenaDaemon("alpha")
	daemon.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		if !strings.Contains(string(b), `"tools/list"`) {
			t.Errorf("upstream body did not carry tools/list method; got %s", string(b))
		}
		if r.Header.Get("Mcp-Session-Id") == "" {
			t.Errorf("tools/list reached the daemon with no Mcp-Session-Id; handshake session not threaded")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"find_symbol"},{"name":"list_dir"}]}}`))
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)
	toolHits := func() int { daemon.mu.Lock(); defer daemon.mu.Unlock(); return daemon.toolHits }

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	// Finding D: tools/list requires a session minted by a prior initialize.
	sid := mintRouterSession(t, s, "2025-11-25")
	hdr := map[string]string{"Mcp-Session-Id": sid}
	body := buildLifecycleBody(t, "tools/list", map[string]any{})

	// First call -> handshake + proxies tools/list to the daemon.
	rr := postSerena(t, s, body, hdr)
	if rr.Code != http.StatusOK {
		t.Fatalf("first tools/list status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	assertToolsListNames(t, rr.Body.Bytes(), []string{"find_symbol", "list_dir"})
	if got := toolHits(); got != 1 {
		t.Fatalf("daemon tool hits after first call = %d, want 1", got)
	}
	// Finding C: the one-shot tools/list session must be torn down upstream.
	if got := daemonDeleteHits(daemon); got != 1 {
		t.Fatalf("daemon DELETE hits after first tools/list = %d, want 1 (one-shot session teardown)", got)
	}

	// Second call -> served from cache, no new daemon hit.
	rr2 := postSerena(t, s, body, hdr)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second tools/list status = %d, want 200", rr2.Code)
	}
	assertToolsListNames(t, rr2.Body.Bytes(), []string{"find_symbol", "list_dir"})
	if got := toolHits(); got != 1 {
		t.Fatalf("daemon tool hits after cached call = %d, want 1 (cache hit expected)", got)
	}
	// A cache hit performs no upstream handshake, so no new DELETE either.
	if got := daemonDeleteHits(daemon); got != 1 {
		t.Fatalf("daemon DELETE hits after cached call = %d, want 1 (cache hit -> no new session)", got)
	}
}

// tools/list accepts a single-event SSE body from the daemon (some MCP
// servers answer Streamable HTTP requests as text/event-stream).
func TestSerenaRouter_ToolsList_AcceptsSSEDaemonResponse(t *testing.T) {
	// The daemon answers the tools/list call as a single-event SSE body;
	// the handshake initialize still answers plain JSON (fake daemon
	// default).
	daemon := newFakeSerenaDaemon("alpha")
	daemon.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[{\"name\":\"read_file\"}]}}\n\n"))
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	assertToolsListNames(t, rr.Body.Bytes(), []string{"read_file"})
}

// tools/list skips a dead daemon and fetches from the next live one.
func TestSerenaRouter_ToolsList_SkipsDeadDaemonAndUsesNext(t *testing.T) {
	// The live daemon completes the handshake, then answers tools/list.
	// The dead candidate is unroutable, so its handshake initialize POST
	// already fails connection — fetchToolsListFromAnyDaemon advances to
	// the live one.
	liveDaemon := newFakeSerenaDaemon("good")
	liveDaemon.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"x"}]}}`))
	}
	live := httptest.NewServer(liveDaemon.handler())
	t.Cleanup(live.Close)

	dead := &api.WorkspaceEntry{WorkspaceKey: "dead", WorkspacePath: "/proj/dead", Port: 1}
	good := &api.WorkspaceEntry{WorkspaceKey: "good", WorkspacePath: "/proj/good", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{list: []*api.WorkspaceEntry{dead, good}},
		Sessions: NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string {
			if ws.WorkspaceKey == "good" {
				return live.URL
			}
			return "http://127.0.0.1:1" // unroutable port -> connection refused
		},
		AuditFn: func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	assertToolsListNames(t, rr.Body.Bytes(), []string{"x"})
}

// ---------------------------------------------------------------------
// tools/list — empty-pool case: no workspace registered -> explicit
// JSON-RPC error (not a fabricated empty list).
// ---------------------------------------------------------------------
func TestSerenaRouter_ToolsList_EmptyPoolReturnsJSONRPCError(t *testing.T) {
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{list: nil}, // empty pool
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return "http://unused" },
	}
	s := newSerenaTestServer(t, deps)

	// Finding D: a known router session is required to even reach the
	// empty-pool branch (otherwise the session gate fires first).
	sid := mintRouterSession(t, s, "2025-11-25")
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	// JSON-RPC errors are carried in-band at HTTP 200.
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (JSON-RPC error in-band); body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rr.Body.String())
	}
	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", resp.JSONRPC)
	}
	if string(resp.ID) != "1" {
		t.Errorf("id = %s, want 1", string(resp.ID))
	}
	if len(resp.Result) > 0 {
		t.Errorf("result present on empty-pool tools/list; want error only. result=%s", string(resp.Result))
	}
	if resp.Error == nil {
		t.Fatalf("expected JSON-RPC error on empty pool; raw=%s", rr.Body.String())
	}
	if resp.Error.Code != serenaNoWorkspaceCode {
		t.Errorf("error code = %d, want %d", resp.Error.Code, serenaNoWorkspaceCode)
	}
	if !strings.Contains(resp.Error.Message, "workspace register") {
		t.Errorf("error message %q should guide the operator to `mcphub workspace register`", resp.Error.Message)
	}
}

// tools/list when the resolver does not implement workspaceLister at all
// -> empty-pool JSON-RPC error (graceful degradation, no panic).
func TestSerenaRouter_ToolsList_ResolverWithoutListerReturnsError(t *testing.T) {
	deps := &serenaRouterDeps{
		Resolver: &stubResolver{}, // no ListWorkspaces method
		Sessions: NewInMemorySessionRouter(),
	}
	s := newSerenaTestServer(t, deps)

	// Finding D: mint a session so the request reaches the no-lister branch.
	sid := mintRouterSession(t, s, "2025-11-25")
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error *jsonrpcError `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != serenaNoWorkspaceCode {
		t.Fatalf("expected serenaNoWorkspaceCode error; got %+v", resp.Error)
	}
}

// ---------------------------------------------------------------------
// ping — JSON-RPC result {}.
// ---------------------------------------------------------------------
func TestSerenaRouter_Ping_ReturnsEmptyResult(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &stubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	rr := postSerena(t, s, buildLifecycleBody(t, "ping", nil), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rr.Body.String())
	}
	if resp.JSONRPC != "2.0" || string(resp.ID) != "1" {
		t.Errorf("envelope mismatch: jsonrpc=%q id=%s", resp.JSONRPC, string(resp.ID))
	}
	if len(resp.Error) > 0 {
		t.Errorf("ping returned error: %s", string(resp.Error))
	}
	if strings.TrimSpace(string(resp.Result)) != "{}" {
		t.Errorf("ping result = %s, want {}", string(resp.Result))
	}
}

// ---------------------------------------------------------------------
// notifications/* — HTTP 202 Accepted, empty body, no workspace needed.
// ---------------------------------------------------------------------
func TestSerenaRouter_NotificationInitialized_Returns202Empty(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &stubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	// notifications carry no id.
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if b := strings.TrimSpace(rr.Body.String()); b != "" {
		t.Errorf("notification response body = %q, want empty", b)
	}
}

func TestSerenaRouter_NotificationCancelled_Returns202(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &stubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/cancelled",
		"params":  map[string]any{"requestId": 7},
	})
	rr := postSerena(t, s, body, nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------
// Regression guard: lifecycle synthesis must NOT touch the tool-call
// path. A tools/call still routes by path-arg to the upstream daemon.
// ---------------------------------------------------------------------
func TestSerenaRouter_ToolCallStillRoutesAfterLifecycleAdded(t *testing.T) {
	hit := &upstreamHit{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}}, list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/src/main.go"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "sess-tc"})
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !hit.called {
		t.Fatal("tool call did not reach the upstream daemon")
	}
	if got := deps.Sessions.LookupSession("sess-tc"); got == nil || got.WorkspaceKey != "alpha" {
		t.Errorf("session binding mismatch after tool call: %+v", got)
	}
}

// ---------------------------------------------------------------------
// toolsListCache unit: TTL expiry forces a re-fetch (per version key).
// ---------------------------------------------------------------------
func TestToolsListCache_TTLExpiry(t *testing.T) {
	c := &toolsListCache{cacheTTL: 50 * time.Millisecond}
	base := time.Now()
	const ver = "2025-11-25"
	c.put(ver, json.RawMessage(`{"tools":[]}`), base)

	if _, ok := c.get(ver, base.Add(10*time.Millisecond)); !ok {
		t.Fatal("expected cache hit within TTL")
	}
	if _, ok := c.get(ver, base.Add(100*time.Millisecond)); ok {
		t.Fatal("expected cache miss after TTL")
	}
}

// P2 finding 5 — the cache is keyed by negotiated protocol version: a
// payload stored under one version is NOT served to a request keyed by a
// different version.
func TestToolsListCache_KeyedByProtocolVersion(t *testing.T) {
	c := &toolsListCache{}
	now := time.Now()
	c.put("2025-11-25", json.RawMessage(`{"tools":[{"name":"newest"}]}`), now)

	// Same version -> hit.
	got, ok := c.get("2025-11-25", now)
	if !ok || !strings.Contains(string(got), "newest") {
		t.Fatalf("get(2025-11-25) = (%s, %v), want the stored payload", string(got), ok)
	}
	// Different version -> miss (must NOT serve the other version's payload).
	if got, ok := c.get("2025-06-18", now); ok {
		t.Fatalf("get(2025-06-18) = (%s, true), want miss (version-keyed cache must not cross versions)", string(got))
	}
	// Storing the other version is independent.
	c.put("2025-06-18", json.RawMessage(`{"tools":[{"name":"older"}]}`), now)
	gotOld, ok := c.get("2025-06-18", now)
	if !ok || !strings.Contains(string(gotOld), "older") {
		t.Fatalf("get(2025-06-18) after put = (%s, %v), want the older payload", string(gotOld), ok)
	}
	// The newest entry is untouched by the older put.
	gotNew, ok := c.get("2025-11-25", now)
	if !ok || !strings.Contains(string(gotNew), "newest") {
		t.Fatalf("get(2025-11-25) after older put = (%s, %v), want the newest payload still cached", string(gotNew), ok)
	}
}

// concurrency smoke: parallel get/put never races (run under -race).
func TestToolsListCache_ConcurrentAccess(t *testing.T) {
	c := &toolsListCache{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			now := time.Now()
			ver := fmt.Sprintf("ver-%d", i%3)
			c.put(ver, json.RawMessage(`{"tools":[{"name":"a"}]}`), now)
			_, _ = c.get(ver, now)
		}(i)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------
// Integration: the Phase-3 reconcile readiness probe shape must now PASS
// against the COMPLETED router.
//
// internal/api's mcpInitializeProbe / defaultRouterReadinessPing are
// unexported, so per the task contract this replicates the EXACT probe
// shape (serena_client_reconcile.go:400-473) and drives it against the
// REAL gui router handler over real HTTP (httptest), through the
// production requireAllowedHost + requireSameOrigin middleware. Before
// this phase the POST initialize step returned 400 (params.name); after
// it must return 200 + a JSON-RPC result, which is precisely what flips
// the reconcile from inert (fail-closed) to functional.
// ---------------------------------------------------------------------
func TestSerenaRouter_ReconcileReadinessProbeShape_PassesAgainstCompletedRouter(t *testing.T) {
	resetSerenaRouterTestSeam(t)
	serenaRouterTestSeam = nil

	// Real (empty-but-valid) resolver + session router so the lifecycle
	// synthesis path is fully real; initialize never reads the registry,
	// so the empty pool is fine for the probe.
	root := t.TempDir()
	regPath := filepath.Join(root, "workspaces.yaml")
	reg := api.NewRegistry(regPath)
	if err := reg.Save(); err != nil {
		t.Fatalf("save empty registry: %v", err)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	resolver := serena_routing.NewWorkspaceResolver(reg, regPath)
	sessions := serena_routing.NewSessionRouter()

	// Build an unstarted httptest server, learn its port, construct the
	// gui Server with that port so requireAllowedHost accepts the
	// 127.0.0.1:<port> Host header the probe will send, then mount the
	// production handler chain and start.
	ts := httptest.NewUnstartedServer(nil)
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	s := NewServer(Config{Port: port, Version: "test", PID: 1})
	s.SetSerenaRouterProduction(resolver, sessions)
	ts.Config.Handler = s.httpHandler()
	ts.Start()
	t.Cleanup(ts.Close)

	const routerPath = "/serena/mcp"

	// --- Step 1: HEAD must answer 405 + Allow: POST (the router signature
	// the probe uses to reject a stale-port reused by another service). ---
	headReq, err := http.NewRequest(http.MethodHead, ts.URL+routerPath, nil)
	if err != nil {
		t.Fatalf("build HEAD: %v", err)
	}
	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	_ = headResp.Body.Close()
	if headResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD status = %d, want 405 (router signature)", headResp.StatusCode)
	}
	if !strings.Contains(strings.ToUpper(headResp.Header.Get("Allow")), "POST") {
		t.Fatalf("HEAD Allow = %q, want POST", headResp.Header.Get("Allow"))
	}

	// --- Step 2: POST initialize must answer 200 + JSON-RPC result. This
	// is the EXACT body mcpInitializeProbe sends. ---
	const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mcphub-reconcile-probe","version":"0"}}}`
	postReq, err := http.NewRequest(http.MethodPost, ts.URL+routerPath, strings.NewReader(initBody))
	if err != nil {
		t.Fatalf("build POST: %v", err)
	}
	postReq.Header.Set("Content-Type", "application/json")
	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("POST initialize: %v", err)
	}
	defer func() { _ = postResp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(postResp.Body, 1<<20))

	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("POST initialize status = %d, want 200; body=%s", postResp.StatusCode, string(raw))
	}
	// Mirror mcpInitializeProbe's accept condition exactly: non-empty
	// result AND no error.
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &rpc); err != nil {
		t.Fatalf("initialize response is not JSON-RPC: %v; raw=%s", err, string(raw))
	}
	if len(rpc.Error) > 0 {
		t.Fatalf("initialize returned a JSON-RPC error (probe would fail closed): %s", string(rpc.Error))
	}
	if len(rpc.Result) == 0 {
		t.Fatalf("initialize returned no result (probe would fail closed); raw=%s", string(raw))
	}
	// The reconcile probe accept condition (status 200 + result + no
	// error) is satisfied -> the reconcile flips from inert to functional.
}

// ---------------------------------------------------------------------
// P1 #1 — initialize → tool-call establishes a REAL upstream daemon
// session and forwards the tool call with the daemon-issued id, NOT the
// router-minted client id. This is the core multiplexing acceptance test.
// ---------------------------------------------------------------------
func TestSerenaRouter_InitializeThenToolCall_ForwardsDaemonSession(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	// Step 1: synthesized initialize at the router -> client session id.
	initRR := postSerena(t, s, buildLifecycleBody(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
	}), nil)
	if initRR.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, want 200; body=%s", initRR.Code, initRR.Body.String())
	}
	clientSID := initRR.Header().Get("Mcp-Session-Id")
	if clientSID == "" {
		t.Fatalf("initialize did not mint a client Mcp-Session-Id")
	}
	// At initialize time NO upstream handshake should have happened yet —
	// the handshake is lazy, on the first tool call (so the reconcile
	// readiness probe stays cheap).
	if mc := daemonMintCount(daemon); mc != 0 {
		t.Fatalf("daemon minted %d sessions at initialize; want 0 (handshake must be lazy)", mc)
	}

	// Step 2: first tool call on that client session -> handshake fires,
	// tool call forwards with the DAEMON session id.
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/src/main.go"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": clientSID})
	if rr.Code != http.StatusOK {
		t.Fatalf("tool call status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if mc := daemonMintCount(daemon); mc != 1 {
		t.Fatalf("daemon minted %d sessions; want exactly 1 handshake", mc)
	}
	// The daemon must have seen its OWN minted session id on the tool
	// call, NOT the router-minted client id.
	daemon.mu.Lock()
	gotSID := daemon.lastToolSession
	known := daemon.issued[gotSID]
	daemon.mu.Unlock()
	if gotSID == clientSID {
		t.Errorf("daemon saw the router-minted client id %q on the tool call; want the daemon-issued id", clientSID)
	}
	if !known {
		t.Errorf("daemon saw session %q it never minted; want the handshake-issued id", gotSID)
	}
	// The client keeps its own id downstream (daemon id never leaks).
	if got := rr.Header().Get("Mcp-Session-Id"); got != clientSID {
		t.Errorf("downstream Mcp-Session-Id = %q, want the client's own id %q", got, clientSID)
	}
}

// P1 #1 — a second tool call on the SAME client session reuses the
// established daemon session: the daemon handshakes exactly once.
func TestSerenaRouter_ToolCall_ReusesDaemonSessionAcrossCalls(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	for i := 0; i < 3; i++ {
		rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "sess-reuse"})
		if rr.Code != http.StatusOK {
			t.Fatalf("call %d status = %d, want 200; body=%s", i, rr.Code, rr.Body.String())
		}
	}
	if mc := daemonMintCount(daemon); mc != 1 {
		t.Errorf("daemon minted %d sessions over 3 calls; want exactly 1 (session reuse)", mc)
	}
	daemon.mu.Lock()
	hits := daemon.toolHits
	daemon.mu.Unlock()
	if hits != 3 {
		t.Errorf("daemon tool hits = %d, want 3", hits)
	}
}

// P1 #1 — when the SAME client session is later routed to a DIFFERENT
// workspace (a new path-arg), the router re-handshakes with the new
// workspace's daemon rather than forwarding the first daemon's session.
func TestSerenaRouter_ToolCall_ReHandshakesOnWorkspaceSwitch(t *testing.T) {
	daemonA := newFakeSerenaDaemon("alpha")
	tsA := httptest.NewServer(daemonA.handler())
	t.Cleanup(tsA.Close)
	daemonB := newFakeSerenaDaemon("beta")
	tsB := httptest.NewServer(daemonB.handler())
	t.Cleanup(tsB.Close)

	wsA := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	wsB := &api.WorkspaceEntry{WorkspaceKey: "beta", WorkspacePath: "/proj/beta", Port: 9202}
	deps := &serenaRouterDeps{
		Resolver: &stubResolver{entries: []*api.WorkspaceEntry{wsA, wsB}},
		Sessions: NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string {
			if ws.WorkspaceKey == "beta" {
				return tsB.URL
			}
			return tsA.URL
		},
	}
	s := newSerenaTestServer(t, deps)

	// Call 1 -> workspace alpha.
	rrA := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{"Mcp-Session-Id": "sess-switch"})
	if rrA.Code != http.StatusOK {
		t.Fatalf("alpha call status = %d; body=%s", rrA.Code, rrA.Body.String())
	}
	// Call 2 (same client session) -> workspace beta. Must re-handshake.
	rrB := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/beta/y"}), map[string]string{"Mcp-Session-Id": "sess-switch"})
	if rrB.Code != http.StatusOK {
		t.Fatalf("beta call status = %d; body=%s", rrB.Code, rrB.Body.String())
	}

	if mc := daemonMintCount(daemonA); mc != 1 {
		t.Errorf("alpha daemon minted %d sessions; want 1", mc)
	}
	if mc := daemonMintCount(daemonB); mc != 1 {
		t.Errorf("beta daemon minted %d sessions; want 1 (re-handshake on workspace switch)", mc)
	}
	daemonB.mu.Lock()
	betaKnown := daemonB.issued[daemonB.lastToolSession]
	daemonB.mu.Unlock()
	if !betaKnown {
		t.Errorf("beta tool call did not carry a beta-issued session id")
	}
}

// ---------------------------------------------------------------------
// P2 #3 — a notifications/* method that carries an id is a malformed
// JSON-RPC request and must get a -32600 error (NOT a 202 acknowledgment
// that leaves the client waiting for a response that never arrives).
// ---------------------------------------------------------------------
func TestSerenaRouter_NotificationWithID_ReturnsInvalidRequest(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &stubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	for _, method := range []string{"notifications/initialized", "notifications/cancelled"} {
		t.Run(method, func(t *testing.T) {
			// id present -> JSON-RPC request shape on a notification method.
			body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method})
			rr := postSerena(t, s, body, nil)
			// JSON-RPC errors are carried in-band at HTTP 200.
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (in-band JSON-RPC error); body=%s", rr.Code, rr.Body.String())
			}
			var resp struct {
				ID    json.RawMessage `json:"id"`
				Error *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v; raw=%s", err, rr.Body.String())
			}
			if resp.Error == nil {
				t.Fatalf("expected a JSON-RPC error for id-bearing %s; raw=%s", method, rr.Body.String())
			}
			if resp.Error.Code != jsonrpcInvalidRequest {
				t.Errorf("error code = %d, want %d (Invalid Request)", resp.Error.Code, jsonrpcInvalidRequest)
			}
			if string(resp.ID) != "7" {
				t.Errorf("error id = %s, want 7 (echoed)", string(resp.ID))
			}
			if len(resp.Result) > 0 {
				t.Errorf("result present on a rejected notification; want error only")
			}
		})
	}
}

// ---------------------------------------------------------------------
// P2 #4 — a request-style UTILITY lifecycle method (tools/list / ping)
// arriving WITHOUT an id is a JSON-RPC notification and must NOT receive a
// response envelope: 202 + empty body, like the hub handler. initialize is
// the EXCEPTION (Finding 1, covered separately by
// TestSerenaRouter_Initialize_IdlessRejected): it is the session-
// establishment request, so an id-less initialize is -32600, never 202.
// ---------------------------------------------------------------------
func TestSerenaRouter_IdlessLifecycle_Returns202NoBody(t *testing.T) {
	// A live daemon is wired so that, if the router WRONGLY proxied an
	// id-less tools/list upstream instead of 202-ing it, the test would
	// still surface the violation via the body assertion below.
	daemon := newFakeSerenaDaemon("alpha")
	daemon.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"x"}]}}`))
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}}, list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	// initialize is deliberately EXCLUDED here — Finding 1 makes id-less
	// initialize a -32600, not a 202 (see TestSerenaRouter_Initialize_IdlessRejected).
	for _, method := range []string{"tools/list", "ping"} {
		t.Run(method, func(t *testing.T) {
			// No "id" field -> notification shape.
			env := map[string]any{"jsonrpc": "2.0", "method": method}
			body, _ := json.Marshal(env)
			rr := postSerena(t, s, body, nil)
			if rr.Code != http.StatusAccepted {
				t.Fatalf("%s (no id) status = %d, want 202; body=%s", method, rr.Code, rr.Body.String())
			}
			if b := strings.TrimSpace(rr.Body.String()); b != "" {
				t.Errorf("%s (no id) response body = %q, want empty (no response envelope for a notification)", method, b)
			}
		})
	}
}

// ---------------------------------------------------------------------
// Finding 1 — an id-less `initialize` is NOT a notification: it is THE
// session-establishment request. The router must reject it -32600 at HTTP
// 400 ("initialize requires a non-null id"), mirroring the hub
// (internal/api/hub_mcp_handler.go:316-319), instead of 202-ing it (which
// would hide the client's protocol bug and let it fail later with a
// missing/unknown session). No session is minted. The present-but-invalid
// id branch is unchanged (also -32600). tools/list + ping id-less stay 202
// (TestSerenaRouter_IdlessLifecycle_Returns202NoBody), and the reconcile
// probe sends id:1 so it is unaffected
// (TestSerenaRouter_Initialize_ReconcileProbeBodyStillSucceeds).
// ---------------------------------------------------------------------
func TestSerenaRouter_Initialize_IdlessRejected(t *testing.T) {
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return "http://unused" },
	}
	s := newSerenaTestServer(t, deps)

	// {"jsonrpc":"2.0","method":"initialize",...} with NO "id" field.
	env := map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}},
	}
	body, _ := json.Marshal(env)
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("id-less initialize status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ID    json.RawMessage `json:"id"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rr.Body.String())
	}
	if resp.Error == nil || resp.Error.Code != jsonrpcInvalidRequest {
		t.Fatalf("expected -32600; got %+v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "initialize requires a non-null id") {
		t.Errorf("error message %q should match the hub wording 'initialize requires a non-null id'", resp.Error.Message)
	}
	// The id could not be determined -> echoed as JSON null (mirrors the hub).
	if strings.TrimSpace(string(resp.ID)) != "null" {
		t.Errorf("id = %s, want null (a request whose id could not be determined echoes null)", string(resp.ID))
	}
	// No session minted for a rejected initialize.
	if sid := rr.Header().Get("Mcp-Session-Id"); sid != "" {
		t.Errorf("rejected id-less initialize minted Mcp-Session-Id %q; want none", sid)
	}
}

// daemonMintCount returns how many upstream sessions a fake daemon has
// minted (one per successful handshake initialize).
func daemonMintCount(d *fakeSerenaDaemon) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mintCount
}

// ---------------------------------------------------------------------
// P1 finding 1 — the upstream daemon handshake uses the client's
// negotiated MCP-Protocol-Version (not the hard-coded router default),
// on BOTH the initialize params.protocolVersion AND the
// notifications/initialized header, AND the routed tool-call forwards
// the same version. A strict daemon binding the header to the session's
// initialized version would otherwise reject the first tool-call.
// ---------------------------------------------------------------------
func TestSerenaRouter_Handshake_UsesClientNegotiatedProtocolVersion(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	// Client negotiated a supported, NON-default revision.
	const negotiated = "2025-06-18"
	if negotiated == defaultProtocolVersion {
		t.Fatalf("test precondition: negotiated version must differ from the router default %q", defaultProtocolVersion)
	}

	// First tool call drives the lazy handshake. The client forwards its
	// negotiated version on the MCP-Protocol-Version header (the value it
	// got from the router-synthesized initialize).
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/src/main.go"})
	rr := postSerena(t, s, body, map[string]string{
		"Mcp-Session-Id":       "sess-pv",
		"MCP-Protocol-Version": negotiated,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("tool call status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	daemon.mu.Lock()
	gotInitPV := daemon.lastInitProtocolVersion
	gotInitializedPV := daemon.lastInitializedHeaders.Get("MCP-Protocol-Version")
	gotToolPV := daemon.lastToolHeaders.Get("MCP-Protocol-Version")
	daemon.mu.Unlock()

	if gotInitPV != negotiated {
		t.Errorf("upstream initialize params.protocolVersion = %q, want client-negotiated %q", gotInitPV, negotiated)
	}
	if gotInitializedPV != negotiated {
		t.Errorf("notifications/initialized MCP-Protocol-Version header = %q, want %q", gotInitializedPV, negotiated)
	}
	if gotToolPV != negotiated {
		t.Errorf("tool-call MCP-Protocol-Version header = %q, want %q", gotToolPV, negotiated)
	}
}

// P1 finding 1 — an older client that omits MCP-Protocol-Version on its
// tool call falls back to the router default handshake version (the
// daemon handshake must still negotiate a concrete version).
func TestSerenaRouter_Handshake_FallsBackToDefaultProtocolVersion(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	// No MCP-Protocol-Version header on the tool call.
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "sess-nopv"})
	if rr.Code != http.StatusOK {
		t.Fatalf("tool call status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	daemon.mu.Lock()
	gotInitPV := daemon.lastInitProtocolVersion
	daemon.mu.Unlock()
	if gotInitPV != handshakeInitializeProtocolVersion {
		t.Errorf("upstream initialize params.protocolVersion = %q, want default fallback %q", gotInitPV, handshakeInitializeProtocolVersion)
	}
}

// ---------------------------------------------------------------------
// P2 finding 5 — the proxied tools/list POST carries the negotiated
// MCP-Protocol-Version header (and the handshake before it uses the
// SAME version). A strict daemon rejects a non-initialize POST whose
// protocol header is missing.
// ---------------------------------------------------------------------
func TestSerenaRouter_ToolsList_SendsProtocolVersionHeader(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	daemon.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"find_symbol"}]}}`))
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	// Finding D + E: the negotiated version comes from the router SESSION
	// (minted at initialize), not the raw request header. Mint the session
	// at 2025-06-18 so the upstream handshake + tools/list POST carry it.
	const negotiated = "2025-06-18"
	sid := mintRouterSession(t, s, negotiated)
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{
		"Mcp-Session-Id":       sid,
		"MCP-Protocol-Version": negotiated,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	assertToolsListNames(t, rr.Body.Bytes(), []string{"find_symbol"})

	daemon.mu.Lock()
	gotToolsListPV := daemon.lastToolHeaders.Get("MCP-Protocol-Version")
	gotHandshakePV := daemon.lastInitProtocolVersion
	daemon.mu.Unlock()
	if gotToolsListPV != negotiated {
		t.Errorf("upstream tools/list MCP-Protocol-Version header = %q, want %q", gotToolsListPV, negotiated)
	}
	if gotHandshakePV != negotiated {
		t.Errorf("handshake initialize params.protocolVersion = %q, want %q (handshake + tools/list must use the same version)", gotHandshakePV, negotiated)
	}
}

// ---------------------------------------------------------------------
// P2 finding 2 — a cursor-bearing tools/list bypasses the first-page
// cache: it reaches a daemon even when a cursorless page is cached, and
// its page-N result does NOT overwrite the first-page cache entry.
// ---------------------------------------------------------------------
func TestSerenaRouter_ToolsList_CursorBypassesCache(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	// The daemon answers page 1 (no cursor) and page 2 (cursor present)
	// distinctly so the test can prove which page the router returned.
	daemon.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if strings.Contains(string(b), `"cursor"`) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"page2_tool"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"page1_tool"}],"nextCursor":"c1"}}`))
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)
	toolHits := func() int { daemon.mu.Lock(); defer daemon.mu.Unlock(); return daemon.toolHits }

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	// Finding D: tools/list requires a session minted by a prior initialize.
	sid := mintRouterSession(t, s, "2025-11-25")
	hdr := map[string]string{"Mcp-Session-Id": sid}

	// Page 1 (cursorless) -> fetches + caches.
	rr1 := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), hdr)
	if rr1.Code != http.StatusOK {
		t.Fatalf("page1 status = %d; body=%s", rr1.Code, rr1.Body.String())
	}
	assertToolsListNames(t, rr1.Body.Bytes(), []string{"page1_tool"})
	if got := toolHits(); got != 1 {
		t.Fatalf("daemon tool hits after page1 = %d, want 1", got)
	}

	// Cursor request -> MUST bypass the cache and reach the daemon again,
	// returning page 2 (not the cached page 1).
	rrCursor := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{"cursor": "c1"}), hdr)
	if rrCursor.Code != http.StatusOK {
		t.Fatalf("cursor status = %d; body=%s", rrCursor.Code, rrCursor.Body.String())
	}
	assertToolsListNames(t, rrCursor.Body.Bytes(), []string{"page2_tool"})
	if got := toolHits(); got != 2 {
		t.Fatalf("daemon tool hits after cursor request = %d, want 2 (cursor must bypass cache)", got)
	}

	// A subsequent cursorless page-1 request still hits the ORIGINAL
	// cache (the cursor request must NOT have overwritten it with page 2).
	rr3 := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), hdr)
	if rr3.Code != http.StatusOK {
		t.Fatalf("page1-again status = %d; body=%s", rr3.Code, rr3.Body.String())
	}
	assertToolsListNames(t, rr3.Body.Bytes(), []string{"page1_tool"})
	if got := toolHits(); got != 2 {
		t.Fatalf("daemon tool hits after cached page1 = %d, want 2 (cache hit; cursor page must not poison the cache)", got)
	}
}

// ---------------------------------------------------------------------
// P2 finding 4 — a lifecycle method (initialize / tools/list / ping)
// whose id is PRESENT but INVALID (null / boolean / array / object) is a
// malformed JSON-RPC request: -32600, no synthesized result, and (for
// initialize) NO minted session. An ABSENT id is a notification (covered
// by TestSerenaRouter_IdlessLifecycle_Returns202NoBody).
// ---------------------------------------------------------------------
func TestSerenaRouter_LifecycleInvalidID_ReturnsInvalidRequest(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &listerStubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	invalidIDs := map[string]string{
		"null":    `null`,
		"boolean": `true`,
		"object":  `{"x":1}`,
		"array":   `[1,2]`,
	}
	for _, method := range []string{"initialize", "tools/list", "ping"} {
		for label, rawID := range invalidIDs {
			t.Run(method+"/"+label, func(t *testing.T) {
				// Build the envelope with an explicit raw id token.
				env := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q`, rawID, method)
				if method == "initialize" {
					env += `,"params":{"protocolVersion":"2025-06-18","capabilities":{}}`
				}
				env += `}`

				rr := postSerena(t, s, []byte(env), nil)
				// JSON-RPC errors are carried in-band at HTTP 200.
				if rr.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200 (in-band JSON-RPC error); body=%s", rr.Code, rr.Body.String())
				}
				var resp struct {
					ID     json.RawMessage `json:"id"`
					Result json.RawMessage `json:"result"`
					Error  *struct {
						Code    int    `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode: %v; raw=%s", err, rr.Body.String())
				}
				if resp.Error == nil {
					t.Fatalf("expected -32600 for %s with %s id; raw=%s", method, label, rr.Body.String())
				}
				if resp.Error.Code != jsonrpcInvalidRequest {
					t.Errorf("error code = %d, want %d (Invalid Request)", resp.Error.Code, jsonrpcInvalidRequest)
				}
				if len(resp.Result) > 0 {
					t.Errorf("result present on a rejected invalid-id request; want error only")
				}
				// A response to a request whose id could not be determined
				// carries a null id (JSON-RPC).
				if strings.TrimSpace(string(resp.ID)) != "null" {
					t.Errorf("error id = %s, want null (invalid request id is not echoed)", string(resp.ID))
				}
				// initialize with an invalid id must NOT mint a session.
				if method == "initialize" {
					if sid := rr.Header().Get("Mcp-Session-Id"); sid != "" {
						t.Errorf("invalid-id initialize minted Mcp-Session-Id %q; a malformed request gets no session", sid)
					}
				}
			})
		}
	}
}

// P2 finding 4 — unit coverage of the id-shape predicate the lifecycle
// gate uses: absent is NOT a valid request id (it is a notification);
// null/bool/array/object are invalid; non-null string/number are valid;
// a number with a leading + or trailing JSON is rejected.
func TestIsValidJSONRPCRequestID(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{``, false},                // absent (len 0) -> not a valid request id
		{`null`, false},            // MCP forbids null
		{`true`, false},            // boolean
		{`false`, false},           // boolean
		{`{}`, false},              // object
		{`[1]`, false},             // array
		{`"abc"`, true},            // string
		{`""`, true},               // empty string is a valid (if odd) id
		{`1`, true},                // number
		{`-3`, true},               // negative number
		{`1.5`, true},              // fractional number
		{`9007199254740993`, true}, // > 2^53 number
		{`+1`, false},              // leading + forbidden by JSON grammar
		{`1]`, false},              // trailing data
		{`"a" "b"`, false},         // two values
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got := isValidJSONRPCRequestID(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Errorf("isValidJSONRPCRequestID(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------
// daemonSessionStore unit tests: workspace-scoped lookup, TTL cleanup,
// and concurrency (run under -race).
// ---------------------------------------------------------------------
func TestDaemonSessionStore_LookupIsWorkspaceScoped(t *testing.T) {
	st := &daemonSessionStore{}
	// First store: no prior binding -> no displacement.
	if _, hadDisplaced := st.store("client-1", "alpha", "daemon-A", "2025-06-18"); hadDisplaced {
		t.Errorf("first store reported a displaced binding; want none")
	}

	// Finding #8: lookup returns the persisted daemon-negotiated version.
	if dsid, dpv, ok := st.lookup("client-1", "alpha"); !ok || dsid != "daemon-A" || dpv != "2025-06-18" {
		t.Fatalf("lookup(client-1, alpha) = (%q, %q, %v), want (daemon-A, 2025-06-18, true)", dsid, dpv, ok)
	}
	// Same client session, DIFFERENT workspace -> miss (the cached
	// daemon session is for another workspace and must be re-established).
	if dsid, _, ok := st.lookup("client-1", "beta"); ok {
		t.Errorf("lookup(client-1, beta) = (%q, true), want miss on workspace mismatch", dsid)
	}
	// Empty client session id -> miss.
	if _, _, ok := st.lookup("", "alpha"); ok {
		t.Errorf("lookup(empty, alpha) = ok, want miss")
	}
	// Finding #2: re-store under a new workspace replaces the binding AND
	// reports the OLD binding as displaced (so the caller tears down the
	// orphaned alpha daemon session), carrying the old daemon-negotiated
	// version (Finding #8).
	old, hadDisplaced := st.store("client-1", "beta", "daemon-B", "2025-11-25")
	if !hadDisplaced {
		t.Fatalf("workspace-switch store did not report a displaced binding; Finding #2 leak")
	}
	if old.workspaceKey != "alpha" || old.daemonSessionID != "daemon-A" || old.daemonProtocolVersion != "2025-06-18" {
		t.Errorf("displaced binding = %+v, want {alpha, daemon-A, 2025-06-18}", old)
	}
	if dsid, dpv, ok := st.lookup("client-1", "beta"); !ok || dsid != "daemon-B" || dpv != "2025-11-25" {
		t.Fatalf("lookup(client-1, beta) after re-store = (%q, %q, %v), want (daemon-B, 2025-11-25, true)", dsid, dpv, ok)
	}
	if _, _, ok := st.lookup("client-1", "alpha"); ok {
		t.Errorf("lookup(client-1, alpha) after re-store = ok, want miss (binding moved to beta)")
	}
	// Finding #2: re-storing the SAME (workspace, daemon session) is NOT a
	// displacement (tearing down the live session would break the next call).
	if _, hadDisplaced := st.store("client-1", "beta", "daemon-B", "2025-11-25"); hadDisplaced {
		t.Errorf("re-store of an UNCHANGED binding reported a displacement; want none")
	}
	// Finding #2 (race correction): re-storing the SAME workspace with a
	// DIFFERENT daemon session id is ALSO NOT a displacement. Two concurrent
	// first handshakes for the same client session + same workspace each mint a
	// distinct daemon session id; when the later one stores, tearing down the
	// earlier same-workspace id would invalidate a still-in-flight request that
	// is forwarding with that id. Only a WORKSPACE change displaces; a
	// superseded same-workspace session is reclaimed by the sweeper/TTL.
	if old, hadDisplaced := st.store("client-1", "beta", "daemon-B2", "2025-11-25"); hadDisplaced {
		t.Errorf("same-workspace re-store with a DIFFERENT daemon session reported a displacement = %+v; want none (Finding #2 race correction)", old)
	}
	// The binding moved to the new daemon session (last store wins) but no
	// teardown was requested for the superseded same-workspace id.
	if dsid, _, ok := st.lookup("client-1", "beta"); !ok || dsid != "daemon-B2" {
		t.Fatalf("lookup(client-1, beta) after same-workspace re-store = (%q, %v), want (daemon-B2, true)", dsid, ok)
	}
	// unbind drops it.
	st.unbind("client-1")
	if _, _, ok := st.lookup("client-1", "beta"); ok {
		t.Errorf("lookup after unbind = ok, want miss")
	}
}

func TestDaemonSessionStore_IgnoresHalfBindings(t *testing.T) {
	st := &daemonSessionStore{}
	st.store("", "alpha", "daemon-A", "2025-11-25") // empty client id
	st.store("client-1", "alpha", "", "2025-11-25") // empty daemon session id
	if _, _, ok := st.lookup("client-1", "alpha"); ok {
		t.Errorf("a half-binding was stored; want no binding")
	}
}

// P2 finding 3 — lookup is expire-on-read: a binding idle past
// daemonSessionTTL is a miss AND is deleted, so the caller re-handshakes
// instead of forwarding a daemon session the upstream already expired.
// This must hold WITHOUT any external cleanup ticker being wired.
func TestDaemonSessionStore_LookupExpiresStaleBinding(t *testing.T) {
	base := time.Now()
	clk := base
	st := &daemonSessionStore{clock: func() time.Time { return clk }}
	st.store("client-1", "alpha", "daemon-A", "2025-11-25")

	// Just inside TTL -> hit (and lastSeen refreshes to the new clock).
	clk = base.Add(daemonSessionTTL - time.Minute)
	if dsid, _, ok := st.lookup("client-1", "alpha"); !ok || dsid != "daemon-A" {
		t.Fatalf("lookup within TTL = (%q, %v), want (daemon-A, true)", dsid, ok)
	}

	// Idle past TTL (measured from the refreshed lastSeen) -> miss, and
	// the stale binding is dropped on read (no cleanup() call in between).
	clk = clk.Add(daemonSessionTTL + time.Minute)
	if dsid, _, ok := st.lookup("client-1", "alpha"); ok {
		t.Fatalf("lookup past TTL = (%q, true), want miss (expire-on-read)", dsid)
	}
	// Prove deletion: a second lookup at the SAME (still-expired) clock is
	// also a miss, but more importantly the map no longer holds the entry,
	// so a re-handshake's store() starts from a clean slot. cleanup at a
	// fresh time must report 0 dropped (the entry is already gone).
	if n := st.cleanup(clk, daemonSessionTTL); n != 0 {
		t.Errorf("cleanup dropped %d bindings; want 0 (lookup already deleted the stale binding)", n)
	}

	// A subsequent store re-establishes a live binding usable immediately.
	st.store("client-1", "alpha", "daemon-B", "2025-11-25")
	if dsid, _, ok := st.lookup("client-1", "alpha"); !ok || dsid != "daemon-B" {
		t.Fatalf("lookup after re-store = (%q, %v), want (daemon-B, true)", dsid, ok)
	}
}

func TestDaemonSessionStore_CleanupExpiresIdle(t *testing.T) {
	base := time.Now()
	clk := base
	st := &daemonSessionStore{clock: func() time.Time { return clk }}
	st.store("client-1", "alpha", "daemon-A", "2025-11-25")

	// Within TTL -> retained, and lookup refreshes lastSeen.
	clk = base.Add(10 * time.Minute)
	if _, _, ok := st.lookup("client-1", "alpha"); !ok {
		t.Fatalf("expected hit within TTL")
	}
	if n := st.cleanup(clk.Add(1*time.Minute), daemonSessionTTL); n != 0 {
		t.Fatalf("cleanup expired %d bindings while still active; want 0", n)
	}
	// Idle past TTL -> swept.
	if n := st.cleanup(clk.Add(daemonSessionTTL+time.Minute), daemonSessionTTL); n != 1 {
		t.Fatalf("cleanup expired %d bindings; want 1", n)
	}
	if _, _, ok := st.lookup("client-1", "alpha"); ok {
		t.Errorf("binding survived cleanup past TTL")
	}
}

// Concurrency smoke: parallel store/lookup/unbind/cleanup never races
// (run under -race).
func TestDaemonSessionStore_ConcurrentAccess(t *testing.T) {
	st := &daemonSessionStore{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("client-%d", i%8)
			st.store(id, "alpha", fmt.Sprintf("daemon-%d", i), "2025-11-25")
			_, _, _ = st.lookup(id, "alpha")
			if i%5 == 0 {
				st.unbind(id)
			}
			if i%7 == 0 {
				_ = st.cleanup(time.Now(), daemonSessionTTL)
			}
		}(i)
	}
	wg.Wait()
}

// --- small helpers ---

// assertToolsListNames decodes a JSON-RPC tools/list success envelope and
// asserts the tool names in order.
func assertToolsListNames(t *testing.T, raw []byte, want []string) {
	t.Helper()
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode tools/list: %v; raw=%s", err, string(raw))
	}
	if len(resp.Error) > 0 {
		t.Fatalf("tools/list returned error: %s", string(resp.Error))
	}
	got := make([]string, 0, len(resp.Result.Tools))
	for _, tool := range resp.Result.Tools {
		got = append(got, tool.Name)
	}
	if len(got) != len(want) {
		t.Fatalf("tool names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tool[%d] = %q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}
}

// daemonDeleteHits returns how many DELETE /mcp requests a fake daemon has
// observed (P2 finding 3 teardown forwarding).
func daemonDeleteHits(d *fakeSerenaDaemon) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deleteHits
}

// daemonCancelHits returns how many notifications/cancelled POSTs a fake
// daemon has observed (Finding H cancel forwarding).
func daemonCancelHits(d *fakeSerenaDaemon) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancelHits
}

// deleteSerena issues a same-origin DELETE /serena/mcp with the supplied
// headers (mirrors postSerena for the client-origin teardown path).
func deleteSerena(t *testing.T, s *Server, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/serena/mcp", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	return rr
}

// ---------------------------------------------------------------------
// P2 finding 5 (probe-compatibility pin) — the EXACT initialize body the
// Phase-3 reconcile readiness probe POSTs
// (internal/api/serena_client_reconcile.go) must still return a
// 200 + JSON-RPC result after the Finding-1/5 validation tightened
// initialize. The probe offers protocolVersion "2024-11-05" (which MUST
// stay in supportedProtocolVersions), a valid id:1, and NO session-id
// header. A future edit that drops "2024-11-05" from the supported set
// would break the cross-package reconcile coupling and fail THIS test
// loudly. The probe lives in another package and is out of scope — this
// is the router-side pin.
func TestSerenaRouter_Initialize_ReconcileProbeBodyStillSucceeds(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &stubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	// Byte-for-byte the reconcile probe body (serena_client_reconcile.go).
	const probeInitBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mcphub-reconcile-probe","version":"0"}}}`
	rr := postSerena(t, s, []byte(probeInitBody), nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// Mirror the probe's accept condition: non-empty result AND no error.
	var rpc struct {
		Result *struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &rpc); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rr.Body.String())
	}
	if len(rpc.Error) > 0 {
		t.Fatalf("reconcile probe body returned an error (probe would fail closed): %s", string(rpc.Error))
	}
	if rpc.Result == nil {
		t.Fatalf("reconcile probe body returned no result; raw=%s", rr.Body.String())
	}
	// A supported version is echoed verbatim (the probe offered a supported one).
	if rpc.Result.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion = %q, want echoed 2024-11-05", rpc.Result.ProtocolVersion)
	}
	if _, ok := supportedProtocolVersions["2024-11-05"]; !ok {
		t.Fatalf("supportedProtocolVersions no longer contains 2024-11-05; the reconcile probe (serena_client_reconcile.go) would fail closed")
	}
	// The probe sends no session-id header, so a session IS minted.
	if rr.Header().Get("Mcp-Session-Id") == "" {
		t.Errorf("reconcile probe initialize minted no Mcp-Session-Id; want one")
	}
}

// supportedProtocolVersionsList unit: returns every supported version,
// sorted newest-first.
func TestSupportedProtocolVersionsList(t *testing.T) {
	got := supportedProtocolVersionsList()
	if len(got) != len(supportedProtocolVersions) {
		t.Fatalf("list length = %d, want %d (one per supported version)", len(got), len(supportedProtocolVersions))
	}
	for _, v := range got {
		if _, ok := supportedProtocolVersions[v]; !ok {
			t.Errorf("list contains %q which is not in supportedProtocolVersions", v)
		}
	}
	// Sorted descending (newest first) -> strictly decreasing string order.
	for i := 1; i < len(got); i++ {
		if got[i-1] <= got[i] {
			t.Errorf("list not sorted newest-first: %v", got)
			break
		}
	}
	// The newest must be defaultProtocolVersion.
	if got[0] != defaultProtocolVersion {
		t.Errorf("first element = %q, want defaultProtocolVersion %q", got[0], defaultProtocolVersion)
	}
}

// ---------------------------------------------------------------------
// P2 finding 2 — when the daemon REJECTS notifications/initialized with a
// non-2xx, the handshake FAILS: the router does not cache a phantom
// session, and a subsequent tool call surfaces the diagnosable
// 502 handshake-failure path instead of an opaque upstream session error.
// ---------------------------------------------------------------------
func TestSerenaRouter_Handshake_FailsWhenInitializedRejected(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	// initialize succeeds (mints a session), but notifications/initialized
	// is rejected -> handshake must fail.
	daemon.initializedStatus = http.StatusBadRequest
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	sessions := NewInMemorySessionRouter()
	var auditEvents []string
	var auditMu sync.Mutex
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions:      sessions,
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		AuditFn: func(level, event string, fields map[string]any) error {
			auditMu.Lock()
			auditEvents = append(auditEvents, event)
			auditMu.Unlock()
			return nil
		},
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "sess-bad-init"})

	// The handshake failure is the same failure class as an unreachable
	// forward -> 502 (the daemon answered initialize but not initialized,
	// which is a non-timeout error).
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (handshake failure); body=%s", rr.Code, rr.Body.String())
	}
	// No phantom session/binding cached.
	if got := sessions.LookupSession("sess-bad-init"); got != nil {
		t.Errorf("LookupSession(sess-bad-init) = %+v, want nil (no phantom binding)", got)
	}
	if _, dsid, _, ok := s.serenaDaemonSessions.bindingFor("sess-bad-init"); ok {
		t.Errorf("serenaDaemonSessions cached %q for sess-bad-init; want no binding on a failed handshake", dsid)
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	sawHandshakeFailure := false
	for _, e := range auditEvents {
		if e == "serena-upstream-unreachable" {
			sawHandshakeFailure = true
		}
	}
	if !sawHandshakeFailure {
		t.Errorf("expected a serena-upstream-unreachable audit on handshake failure; got %v", auditEvents)
	}
}

// ---------------------------------------------------------------------
// P2 finding 3 — a client-origin DELETE /serena/mcp tears down the
// router-owned session state: it forwards a DELETE (carrying the
// DAEMON-issued session id) to the workspace daemon, drops the
// serenaDaemonSessions binding AND the sticky deps.Sessions binding, and
// answers 204.
// ---------------------------------------------------------------------
func TestSerenaRouter_Delete_TearsDownSessionAndForwardsUpstream(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	sessions := NewInMemorySessionRouter()
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions:      sessions,
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	const clientSID = "sess-del"
	// First a tool call establishes the daemon session + sticky binding.
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": clientSID})
	if rr.Code != http.StatusOK {
		t.Fatalf("setup tool call status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// Capture the daemon session id the router established so we can assert
	// the DELETE forwards THAT id (not the client id).
	daemonSID, ok, _ := bindingDaemonSession(s, clientSID)
	if !ok {
		t.Fatalf("precondition: no daemon-session binding for %q after tool call", clientSID)
	}
	if got := sessions.LookupSession(clientSID); got == nil {
		t.Fatalf("precondition: sticky binding for %q missing after tool call", clientSID)
	}

	// Now the DELETE.
	delRR := deleteSerena(t, s, map[string]string{"Mcp-Session-Id": clientSID})
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204; body=%s", delRR.Code, delRR.Body.String())
	}
	// Upstream daemon received a DELETE carrying the DAEMON-issued id.
	if got := daemonDeleteHits(daemon); got != 1 {
		t.Errorf("daemon DELETE hits = %d, want 1 (router must forward the teardown)", got)
	}
	daemon.mu.Lock()
	gotDeleteSID := daemon.lastDeleteSession
	daemon.mu.Unlock()
	if gotDeleteSID != daemonSID {
		t.Errorf("upstream DELETE Mcp-Session-Id = %q, want the daemon-issued id %q (not the client id %q)", gotDeleteSID, daemonSID, clientSID)
	}
	if gotDeleteSID == clientSID {
		t.Errorf("upstream DELETE carried the client id %q; the daemon never minted it", clientSID)
	}
	// Both bindings are gone.
	if _, _, _, ok := s.serenaDaemonSessions.bindingFor(clientSID); ok {
		t.Errorf("serenaDaemonSessions binding for %q survived DELETE; want unbound", clientSID)
	}
	if got := sessions.LookupSession(clientSID); got != nil {
		t.Errorf("sticky binding for %q survived DELETE = %+v; want unbound", clientSID, got)
	}
}

// P2 finding 3 — DELETE with no binding (and DELETE with no session-id
// header) is acknowledged with 204, not 405/5xx; teardown is best-effort.
func TestSerenaRouter_Delete_NoBindingStillAcknowledges(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	// Unknown session id -> 204, no upstream forward (nothing to tear down).
	delRR := deleteSerena(t, s, map[string]string{"Mcp-Session-Id": "never-bound"})
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("DELETE(unknown) status = %d, want 204; body=%s", delRR.Code, delRR.Body.String())
	}
	if got := daemonDeleteHits(daemon); got != 0 {
		t.Errorf("daemon DELETE hits = %d, want 0 for an unbound session", got)
	}

	// No session-id header at all -> still 204.
	delNone := deleteSerena(t, s, nil)
	if delNone.Code != http.StatusNoContent {
		t.Fatalf("DELETE(no sid) status = %d, want 204; body=%s", delNone.Code, delNone.Body.String())
	}
}

// P2 finding 3 — a failed upstream DELETE (daemon returns non-2xx) still
// returns 204 to the client AND still drops the local bindings (teardown
// is best-effort; a shutdown must not 5xx).
func TestSerenaRouter_Delete_UpstreamFailureStill204AndUnbinds(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	daemon.deleteStatus = http.StatusInternalServerError // daemon fails the teardown
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	sessions := NewInMemorySessionRouter()
	var auditEvents []string
	var auditMu sync.Mutex
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions:      sessions,
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		AuditFn: func(level, event string, fields map[string]any) error {
			auditMu.Lock()
			auditEvents = append(auditEvents, event)
			auditMu.Unlock()
			return nil
		},
	}
	s := newSerenaTestServer(t, deps)

	const clientSID = "sess-del-fail"
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	if rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": clientSID}); rr.Code != http.StatusOK {
		t.Fatalf("setup tool call status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	delRR := deleteSerena(t, s, map[string]string{"Mcp-Session-Id": clientSID})
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204 even on upstream failure; body=%s", delRR.Code, delRR.Body.String())
	}
	if _, _, _, ok := s.serenaDaemonSessions.bindingFor(clientSID); ok {
		t.Errorf("serenaDaemonSessions binding survived a failed-upstream DELETE; want unbound")
	}
	if got := sessions.LookupSession(clientSID); got != nil {
		t.Errorf("sticky binding survived a failed-upstream DELETE = %+v; want unbound", got)
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	sawDeleteFailed := false
	for _, e := range auditEvents {
		if e == "serena-upstream-delete-failed" {
			sawDeleteFailed = true
		}
	}
	if !sawDeleteFailed {
		t.Errorf("expected a serena-upstream-delete-failed audit on a non-2xx upstream DELETE; got %v", auditEvents)
	}
}

// GET (and other non-POST, non-DELETE) methods keep their 405 after the
// DELETE branch was added (regression guard for Finding 3 scope).
func TestSerenaRouter_DeleteBranchDoesNotAffectGet405(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &stubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	req := httptest.NewRequest(http.MethodGet, "/serena/mcp", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rr.Code)
	}
	// Finding H: the 405 fallback now advertises both accepted verbs.
	if rr.Header().Get("Allow") != "POST, DELETE" {
		t.Errorf("Allow = %q, want POST, DELETE", rr.Header().Get("Allow"))
	}
}

// bindingDaemonSession reads the daemon-session id the router bound for a
// client session id (test accessor over the unexported store).
func bindingDaemonSession(s *Server, clientSID string) (daemonSID string, ok bool, wsKey string) {
	wsKey, daemonSID, _, ok = s.serenaDaemonSessions.bindingFor(clientSID)
	return daemonSID, ok, wsKey
}

// ---------------------------------------------------------------------
// Finding 4 — readSSEJSONRPCResponse reassembles a JSON-RPC message split
// across MULTIPLE data: lines within one SSE event by joining them with
// "\n" (per the SSE spec), instead of returning only the last fragment.
// (Replaces the pre-fix extractJSONRPCPayload byte-slice flattener; the
// reader now parses an io.Reader incrementally — Finding 4.)
// ---------------------------------------------------------------------
func TestReadSSEJSONRPCResponse_MultiDataLineEvent(t *testing.T) {
	// A pretty-printed JSON-RPC result split across three data: lines in
	// ONE event (blank line terminates). Correct accumulation reconstructs
	// the object; a per-line overwrite would yield only `}` (invalid JSON).
	sse := "event: message\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":1,\n" +
		"data: \"result\":{\"tools\":[{\"name\":\"find_symbol\"}]}\n" +
		"data: }\n" +
		"\n"
	got, err := readSSEJSONRPCResponse(strings.NewReader(sse), 1<<20)
	if err != nil {
		t.Fatalf("readSSEJSONRPCResponse: %v", err)
	}

	var rpc struct {
		JSONRPC string `json:"jsonrpc"`
		Result  struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(got, &rpc); err != nil {
		t.Fatalf("reassembled payload is not valid JSON: %v; got=%q", err, string(got))
	}
	if rpc.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0 (data lines not reassembled)", rpc.JSONRPC)
	}
	if len(rpc.Result.Tools) != 1 || rpc.Result.Tools[0].Name != "find_symbol" {
		t.Errorf("result.tools = %+v, want [find_symbol]", rpc.Result.Tools)
	}
}

// Finding 4 — when a notification event precedes the response event, the
// reader SKIPS the notification (id absent, method present) and returns the
// first JSON-RPC RESPONSE event. This is the behavior change vs the pre-fix
// last-event-wins flattener: the reader stops at the FIRST response so it
// does not wait for stream EOF (a daemon may keep the SSE stream open after
// the response). Mirrors internal/api/hub_mcp_aggregator.go selectJSONRPCResponse.
func TestReadSSEJSONRPCResponse_SkipsNotificationReturnsFirstResponse(t *testing.T) {
	sse := "event: progress\n" +
		"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n" +
		"\n" +
		"event: message\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":1,\n" +
		"data: \"result\":{\"ok\":true}}\n" +
		"\n"
	got, err := readSSEJSONRPCResponse(strings.NewReader(sse), 1<<20)
	if err != nil {
		t.Fatalf("readSSEJSONRPCResponse: %v", err)
	}
	var rpc struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			OK bool `json:"ok"`
		} `json:"result"`
	}
	if err := json.Unmarshal(got, &rpc); err != nil {
		t.Fatalf("payload not valid JSON: %v; got=%q", err, string(got))
	}
	if string(rpc.ID) != "1" || !rpc.Result.OK {
		t.Errorf("returned the wrong event: id=%s result.ok=%v; want the response (notification must be skipped)", string(rpc.ID), rpc.Result.OK)
	}
}

// Finding 4 — readUpstreamJSONRPCResponse: the single-data-line case (the
// common path) works, and a non-SSE content type does a bounded read of the
// raw body verbatim.
func TestReadUpstreamJSONRPCResponse_SingleLineAndNonSSE(t *testing.T) {
	single, err := readUpstreamJSONRPCResponse("text/event-stream", strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"))
	if err != nil {
		t.Fatalf("SSE single line: %v", err)
	}
	if strings.TrimSpace(string(single)) != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Errorf("single data line = %q, want the JSON verbatim", string(single))
	}
	raw := `{"id":1,"result":{"plain":true}}`
	got, err := readUpstreamJSONRPCResponse("application/json", strings.NewReader(raw))
	if err != nil {
		t.Fatalf("non-SSE: %v", err)
	}
	if string(got) != raw {
		t.Errorf("non-SSE body = %q, want raw verbatim %q", string(got), raw)
	}
}

// Finding 4 — readSSEJSONRPCResponse returns at the FIRST JSON-RPC response
// event WITHOUT waiting for the stream to close. The reader is fed a pipe
// whose writer emits the response event and then BLOCKS (modeling a
// Streamable-HTTP daemon that holds the SSE connection open for later
// notifications). A regression that drained to EOF would block forever; the
// test bounds the read with a deadline so a regression FAILS (timeout)
// rather than hanging the suite.
func TestReadSSEJSONRPCResponse_ReturnsBeforeStreamClose(t *testing.T) {
	pr, pw := io.Pipe()
	writerBlocked := make(chan struct{})
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })

	go func() {
		// Emit one complete response event, then BLOCK (never close the
		// writer) — the open-stream condition.
		_, _ = io.WriteString(pw, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n")
		close(writerBlocked)
		<-make(chan struct{}) // block forever; t.Cleanup unblocks via PipeReader close
	}()

	type readResult struct {
		payload []byte
		err     error
	}
	done := make(chan readResult, 1)
	go func() {
		p, err := readSSEJSONRPCResponse(pr, 1<<20)
		done <- readResult{p, err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("readSSEJSONRPCResponse returned error: %v", res.err)
		}
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				OK bool `json:"ok"`
			} `json:"result"`
		}
		if err := json.Unmarshal(res.payload, &rpc); err != nil {
			t.Fatalf("payload not valid JSON: %v; got=%q", err, string(res.payload))
		}
		if string(rpc.ID) != "1" || !rpc.Result.OK {
			t.Errorf("wrong event returned: id=%s ok=%v", string(rpc.ID), rpc.Result.OK)
		}
	case <-time.After(3 * time.Second):
		<-writerBlocked // ensure the writer reached its block (diagnostic ordering)
		t.Fatal("readSSEJSONRPCResponse did not return before the stream closed (Finding 4 regression: read drains to EOF)")
	}
}

// P2 finding 4 (integration) — a daemon that answers tools/list as a
// MULTI-data-line SSE event is parsed correctly by the router (the bug
// surfaced as a tools/list parse failure when the daemon wrapped its JSON).
func TestSerenaRouter_ToolsList_MultiDataLineSSEDaemonResponse(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	daemon.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// One event, JSON split across three data: lines.
		_, _ = w.Write([]byte("event: message\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":1,\n" +
			"data: \"result\":{\"tools\":[{\"name\":\"read_file\"},\n" +
			"data: {\"name\":\"list_dir\"}]}}\n" +
			"\n"))
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	assertToolsListNames(t, rr.Body.Bytes(), []string{"read_file", "list_dir"})
}
