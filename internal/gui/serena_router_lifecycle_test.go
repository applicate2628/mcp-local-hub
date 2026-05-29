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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	var daemonHits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		daemonHits.Add(1)
		// Assert the router forwarded a tools/list, not something else.
		b, _ := readAllBody(r)
		if !strings.Contains(string(b), `"tools/list"`) {
			t.Errorf("upstream body did not carry tools/list method; got %s", string(b))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"find_symbol"},{"name":"list_dir"}]}}`))
	}))
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	body := buildLifecycleBody(t, "tools/list", map[string]any{})

	// First call -> proxies to the daemon.
	rr := postSerena(t, s, body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("first tools/list status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	assertToolsListNames(t, rr.Body.Bytes(), []string{"find_symbol", "list_dir"})
	if got := daemonHits.Load(); got != 1 {
		t.Fatalf("daemon hits after first call = %d, want 1", got)
	}

	// Second call -> served from cache, no new daemon hit.
	rr2 := postSerena(t, s, body, nil)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second tools/list status = %d, want 200", rr2.Code)
	}
	assertToolsListNames(t, rr2.Body.Bytes(), []string{"find_symbol", "list_dir"})
	if got := daemonHits.Load(); got != 1 {
		t.Fatalf("daemon hits after cached call = %d, want 1 (cache hit expected)", got)
	}
}

// tools/list accepts a single-event SSE body from the daemon (some MCP
// servers answer Streamable HTTP requests as text/event-stream).
func TestSerenaRouter_ToolsList_AcceptsSSEDaemonResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[{\"name\":\"read_file\"}]}}\n\n"))
	}))
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
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"x"}]}}`))
	}))
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

// --- small helpers ---

func readAllBody(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}

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
