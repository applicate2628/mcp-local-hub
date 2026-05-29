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

// initialize with an unknown protocolVersion negotiates down to the
// router default rather than echoing the unsupported value.
func TestSerenaRouter_Initialize_NegotiatesUnknownProtocolVersion(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &stubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	body := buildLifecycleBody(t, "initialize", map[string]any{
		"protocolVersion": "1999-01-01",
		"capabilities":    map[string]any{},
	})
	rr := postSerena(t, s, body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Result.ProtocolVersion != defaultProtocolVersion {
		t.Errorf("protocolVersion = %q, want default %q", resp.Result.ProtocolVersion, defaultProtocolVersion)
	}
}

// initialize honors an Mcp-Session-Id the client already sent (it does
// not mint a fresh one over an existing session).
func TestSerenaRouter_Initialize_PreservesIncomingSessionID(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &stubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	body := buildLifecycleBody(t, "initialize", map[string]any{"capabilities": map[string]any{}})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "client-supplied-id"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Mcp-Session-Id"); got != "client-supplied-id" {
		t.Errorf("Mcp-Session-Id = %q, want the client-supplied id echoed back", got)
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

	body := buildLifecycleBody(t, "tools/list", map[string]any{})

	// First call -> handshake + proxies tools/list to the daemon.
	rr := postSerena(t, s, body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("first tools/list status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	assertToolsListNames(t, rr.Body.Bytes(), []string{"find_symbol", "list_dir"})
	if got := toolHits(); got != 1 {
		t.Fatalf("daemon tool hits after first call = %d, want 1", got)
	}

	// Second call -> served from cache, no new daemon hit.
	rr2 := postSerena(t, s, body, nil)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second tools/list status = %d, want 200", rr2.Code)
	}
	assertToolsListNames(t, rr2.Body.Bytes(), []string{"find_symbol", "list_dir"})
	if got := toolHits(); got != 1 {
		t.Fatalf("daemon tool hits after cached call = %d, want 1 (cache hit expected)", got)
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

	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), nil)
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

	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), nil)
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

	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), nil)
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

	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), nil)
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
// toolsListCache unit: TTL expiry forces a re-fetch.
// ---------------------------------------------------------------------
func TestToolsListCache_TTLExpiry(t *testing.T) {
	c := &toolsListCache{cacheTTL: 50 * time.Millisecond}
	base := time.Now()
	c.put(json.RawMessage(`{"tools":[]}`), base)

	if _, ok := c.get(base.Add(10 * time.Millisecond)); !ok {
		t.Fatal("expected cache hit within TTL")
	}
	if _, ok := c.get(base.Add(100 * time.Millisecond)); ok {
		t.Fatal("expected cache miss after TTL")
	}
}

// concurrency smoke: parallel get/put never races (run under -race).
func TestToolsListCache_ConcurrentAccess(t *testing.T) {
	c := &toolsListCache{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := time.Now()
			c.put(json.RawMessage(`{"tools":[{"name":"a"}]}`), now)
			_, _ = c.get(now)
		}()
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
// P2 #4 — a request-style lifecycle method (initialize / tools/list /
// ping) arriving WITHOUT an id is a JSON-RPC notification and must NOT
// receive a response envelope: 202 + empty body, like the hub handler.
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

	for _, method := range []string{"initialize", "tools/list", "ping"} {
		t.Run(method, func(t *testing.T) {
			// No "id" field -> notification shape.
			env := map[string]any{"jsonrpc": "2.0", "method": method}
			if method == "initialize" {
				env["params"] = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
			}
			body, _ := json.Marshal(env)
			rr := postSerena(t, s, body, nil)
			if rr.Code != http.StatusAccepted {
				t.Fatalf("%s (no id) status = %d, want 202; body=%s", method, rr.Code, rr.Body.String())
			}
			if b := strings.TrimSpace(rr.Body.String()); b != "" {
				t.Errorf("%s (no id) response body = %q, want empty (no response envelope for a notification)", method, b)
			}
			// And the id-less initialize must NOT mint a session header.
			if method == "initialize" {
				if sid := rr.Header().Get("Mcp-Session-Id"); sid != "" {
					t.Errorf("id-less initialize minted Mcp-Session-Id %q; a notification gets no session", sid)
				}
			}
		})
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
// daemonSessionStore unit tests: workspace-scoped lookup, TTL cleanup,
// and concurrency (run under -race).
// ---------------------------------------------------------------------
func TestDaemonSessionStore_LookupIsWorkspaceScoped(t *testing.T) {
	st := &daemonSessionStore{}
	st.store("client-1", "alpha", "daemon-A")

	if dsid, ok := st.lookup("client-1", "alpha"); !ok || dsid != "daemon-A" {
		t.Fatalf("lookup(client-1, alpha) = (%q, %v), want (daemon-A, true)", dsid, ok)
	}
	// Same client session, DIFFERENT workspace -> miss (the cached
	// daemon session is for another workspace and must be re-established).
	if dsid, ok := st.lookup("client-1", "beta"); ok {
		t.Errorf("lookup(client-1, beta) = (%q, true), want miss on workspace mismatch", dsid)
	}
	// Empty client session id -> miss.
	if _, ok := st.lookup("", "alpha"); ok {
		t.Errorf("lookup(empty, alpha) = ok, want miss")
	}
	// Re-store under a new workspace replaces the binding.
	st.store("client-1", "beta", "daemon-B")
	if dsid, ok := st.lookup("client-1", "beta"); !ok || dsid != "daemon-B" {
		t.Fatalf("lookup(client-1, beta) after re-store = (%q, %v), want (daemon-B, true)", dsid, ok)
	}
	if _, ok := st.lookup("client-1", "alpha"); ok {
		t.Errorf("lookup(client-1, alpha) after re-store = ok, want miss (binding moved to beta)")
	}
	// unbind drops it.
	st.unbind("client-1")
	if _, ok := st.lookup("client-1", "beta"); ok {
		t.Errorf("lookup after unbind = ok, want miss")
	}
}

func TestDaemonSessionStore_IgnoresHalfBindings(t *testing.T) {
	st := &daemonSessionStore{}
	st.store("", "alpha", "daemon-A") // empty client id
	st.store("client-1", "alpha", "") // empty daemon session id
	if _, ok := st.lookup("client-1", "alpha"); ok {
		t.Errorf("a half-binding was stored; want no binding")
	}
}

func TestDaemonSessionStore_CleanupExpiresIdle(t *testing.T) {
	base := time.Now()
	clk := base
	st := &daemonSessionStore{clock: func() time.Time { return clk }}
	st.store("client-1", "alpha", "daemon-A")

	// Within TTL -> retained, and lookup refreshes lastSeen.
	clk = base.Add(10 * time.Minute)
	if _, ok := st.lookup("client-1", "alpha"); !ok {
		t.Fatalf("expected hit within TTL")
	}
	if n := st.cleanup(clk.Add(1*time.Minute), daemonSessionTTL); n != 0 {
		t.Fatalf("cleanup expired %d bindings while still active; want 0", n)
	}
	// Idle past TTL -> swept.
	if n := st.cleanup(clk.Add(daemonSessionTTL+time.Minute), daemonSessionTTL); n != 1 {
		t.Fatalf("cleanup expired %d bindings; want 1", n)
	}
	if _, ok := st.lookup("client-1", "alpha"); ok {
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
			st.store(id, "alpha", fmt.Sprintf("daemon-%d", i))
			_, _ = st.lookup(id, "alpha")
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
