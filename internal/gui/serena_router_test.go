// internal/gui/serena_router_test.go
//
// Phase C.2 tests for the /serena/mcp path-aware router. The eight
// canonical scenarios in plan v10 test contract are covered here:
//
//   1. TestSerenaRouter_TwoWorkspaces_PathArgRoutesCorrectly
//   2. TestSerenaRouter_WorkspaceNotFound_Returns503
//   3. TestSerenaRouter_UpstreamTimeout_Returns504
//   4. TestSerenaRouter_UpstreamConnectionRefused_Returns502
//   5. TestSerenaRouter_MissingPathArg_FallsThroughToSessionRouter
//   6. TestSerenaRouter_MalformedToolBody_Returns400
//   7. TestSerenaRouter_PreservesMcpSessionIdHeader
//   8. TestSerenaRouter_PreservesContentTypeStreaming
//
// Each test wires a stub workspaceResolver + sessionRouter + an
// httptest upstream mock. The seam injection point is
// serenaRouterTestSeam (package-level var); tests restore it via
// t.Cleanup so ordering does not leak state between cases.
package gui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// resetSerenaRouterTestSeam restores the global seam after the test
// completes so a subsequent test sees a clean default.
func resetSerenaRouterTestSeam(t *testing.T) {
	t.Helper()
	prev := serenaRouterTestSeam
	t.Cleanup(func() { serenaRouterTestSeam = prev })
}

// stubResolver is a minimal workspaceResolver. ResolveByPath returns
// the first WorkspaceEntry whose WorkspacePath is a prefix of the
// query path; on no match it returns (nil, ErrWorkspaceNotFound).
type stubResolver struct {
	mu         sync.Mutex
	entries    []*api.WorkspaceEntry
	resolveErr error // when non-nil, every call returns (nil, err)
	calls      []string
}

func (r *stubResolver) ResolveByPath(path string) (*api.WorkspaceEntry, error) {
	r.mu.Lock()
	r.calls = append(r.calls, path)
	r.mu.Unlock()
	if r.resolveErr != nil {
		return nil, r.resolveErr
	}
	// Longest-prefix match -- the resolver returns the most specific
	// owner. Tests rely on this when two registered workspaces share a
	// parent path.
	var best *api.WorkspaceEntry
	bestLen := -1
	for _, e := range r.entries {
		if strings.HasPrefix(path, e.WorkspacePath) && len(e.WorkspacePath) > bestLen {
			best = e
			bestLen = len(e.WorkspacePath)
		}
	}
	if best == nil {
		return nil, ErrWorkspaceNotFound
	}
	return best, nil
}

// upstreamHit records what the mock upstream observed -- request URL,
// headers, and body. Each upstream mock is wired into a single test
// case and asserts on its captured state at the end.
type upstreamHit struct {
	mu      sync.Mutex
	called  bool
	headers http.Header
	body    []byte
	method  string
	path    string
}

func (h *upstreamHit) record(r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.called = true
	h.headers = r.Header.Clone()
	b, _ := io.ReadAll(r.Body)
	h.body = b
	h.method = r.Method
	h.path = r.URL.Path
}

// newSerenaTestServer constructs a Server with the seam wired to deps.
// Returns the Server plus the deps so tests can inspect the resolver +
// session router after the handler runs.
func newSerenaTestServer(t *testing.T, deps *serenaRouterDeps) *Server {
	t.Helper()
	resetSerenaRouterTestSeam(t)
	serenaRouterTestSeam = func() *serenaRouterDeps { return deps }
	return NewServer(Config{Port: 9125, Version: "test", PID: 1})
}

// buildToolCallBody marshals a JSON-RPC tools/call envelope around the
// given tool name and arguments object. id is "1" verbatim.
func buildToolCallBody(t *testing.T, toolName string, arguments any) []byte {
	t.Helper()
	argsRaw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	env := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": json.RawMessage(argsRaw),
		},
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body
}

// postSerena issues a same-origin POST /serena/mcp with body and the
// supplied headers. Returns the recorder.
func postSerena(t *testing.T, s *Server, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/serena/mcp", bytes.NewReader(body))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	if _, hasCT := headers["Content-Type"]; !hasCT {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	return rr
}

// ---------------------------------------------------------------------
// Test 1: TestSerenaRouter_TwoWorkspaces_PathArgRoutesCorrectly
// ---------------------------------------------------------------------
//
// Happy path: two registered workspaces (alpha, beta). A request with
// a path argument under workspace alpha must reach alpha upstream mock
// and NOT beta.
func TestSerenaRouter_TwoWorkspaces_PathArgRoutesCorrectly(t *testing.T) {
	hitAlpha := &upstreamHit{}
	hitBeta := &upstreamHit{}

	tsAlpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitAlpha.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"workspace":"alpha"}}`))
	}))
	t.Cleanup(tsAlpha.Close)
	tsBeta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitBeta.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tsBeta.Close)

	wsAlpha := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201, TaskName: `\mcp-local-hub-serena-alpha`}
	wsBeta := &api.WorkspaceEntry{WorkspaceKey: "beta", WorkspacePath: "/proj/beta", Port: 9202, TaskName: `\mcp-local-hub-serena-beta`}

	resolver := &stubResolver{entries: []*api.WorkspaceEntry{wsAlpha, wsBeta}}
	deps := &serenaRouterDeps{
		Resolver: resolver,
		Sessions: NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string {
			switch ws.WorkspaceKey {
			case "alpha":
				return tsAlpha.URL
			case "beta":
				return tsBeta.URL
			}
			return ""
		},
	}

	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{
		"relative_path": "/proj/alpha/src/main.go",
		"name_path":     "MyFunc",
	})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "sess-1"})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !hitAlpha.called {
		t.Fatal("alpha upstream was not hit")
	}
	if hitBeta.called {
		t.Fatal("beta upstream was hit (should not have been)")
	}
	if hitAlpha.method != http.MethodPost {
		t.Errorf("upstream method = %s, want POST", hitAlpha.method)
	}
	if !bytes.Contains(hitAlpha.body, []byte(`"find_symbol"`)) {
		t.Errorf("upstream body did not carry tool name; got %s", string(hitAlpha.body))
	}
	bound := deps.Sessions.LookupSession("sess-1")
	if bound == nil || bound.WorkspaceKey != "alpha" {
		t.Errorf("session binding mismatch: got %+v", bound)
	}
}

// ---------------------------------------------------------------------
// Test 2: TestSerenaRouter_WorkspaceNotFound_Returns503
// ---------------------------------------------------------------------
func TestSerenaRouter_WorkspaceNotFound_Returns503(t *testing.T) {
	resolver := &stubResolver{entries: nil}
	deps := &serenaRouterDeps{
		Resolver:      resolver,
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return "http://invalid" },
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{
		"relative_path": "/unknown/path/file.go",
	})
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	var resp notFoundJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, rr.Body.String())
	}
	if resp.PhaseEStatus != "deferred" {
		t.Errorf("phase_e_status = %q, want %q", resp.PhaseEStatus, "deferred")
	}
	if !strings.Contains(resp.Error, "register workspace") {
		t.Errorf("error = %q does not mention register workspace", resp.Error)
	}
	if resp.ResolvedPath != "/unknown/path/file.go" {
		t.Errorf("resolved_path = %q, want the requested path", resp.ResolvedPath)
	}
}

// ---------------------------------------------------------------------
// Test 3: TestSerenaRouter_UpstreamTimeout_Returns504
// ---------------------------------------------------------------------
//
// Upstream never sends headers within the short test timeout. The
// handler must emit 504 + audit serena-upstream-timeout.
func TestSerenaRouter_UpstreamTimeout_Returns504(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	resolver := &stubResolver{entries: []*api.WorkspaceEntry{ws}}

	auditCalls := make([]string, 0, 2)
	var auditMu sync.Mutex

	client := &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext,
			ResponseHeaderTimeout: 100 * time.Millisecond,
		},
	}

	deps := &serenaRouterDeps{
		Resolver:        resolver,
		Sessions:        NewInMemorySessionRouter(),
		HTTPClient:      client,
		UpstreamURLFn:   func(ws *api.WorkspaceEntry) string { return slow.URL },
		UpstreamTimeout: 200 * time.Millisecond,
		AuditFn: func(level, event string, fields map[string]any) error {
			auditMu.Lock()
			auditCalls = append(auditCalls, event)
			auditMu.Unlock()
			return nil
		},
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body=%s", rr.Code, rr.Body.String())
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	found := false
	for _, e := range auditCalls {
		if e == "serena-upstream-timeout" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("audit did not record serena-upstream-timeout; got %v", auditCalls)
	}
}

// ---------------------------------------------------------------------
// Test 4: TestSerenaRouter_UpstreamConnectionRefused_Returns502
// ---------------------------------------------------------------------
func TestSerenaRouter_UpstreamConnectionRefused_Returns502(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	deadURL := fmt.Sprintf("http://%s", ln.Addr().String())
	_ = ln.Close()

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	resolver := &stubResolver{entries: []*api.WorkspaceEntry{ws}}

	auditCalls := make([]string, 0, 2)
	var auditMu sync.Mutex

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		},
	}
	deps := &serenaRouterDeps{
		Resolver:        resolver,
		Sessions:        NewInMemorySessionRouter(),
		HTTPClient:      client,
		UpstreamURLFn:   func(ws *api.WorkspaceEntry) string { return deadURL },
		UpstreamTimeout: 3 * time.Second,
		AuditFn: func(level, event string, fields map[string]any) error {
			auditMu.Lock()
			auditCalls = append(auditCalls, event)
			auditMu.Unlock()
			return nil
		},
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	found := false
	for _, e := range auditCalls {
		if e == "serena-upstream-unreachable" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("audit did not record serena-upstream-unreachable; got %v", auditCalls)
	}
}

// ---------------------------------------------------------------------
// Test 5: TestSerenaRouter_MissingPathArg_FallsThroughToSessionRouter
// ---------------------------------------------------------------------
//
// Tool body without any path-arg field. Behavior depends on whether
// the session-id is bound: bound -> forward to the sticky workspace;
// unbound -> 503.
func TestSerenaRouter_MissingPathArg_FallsThroughToSessionRouter(t *testing.T) {
	hit := &upstreamHit{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.record(r)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	resolver := &stubResolver{entries: []*api.WorkspaceEntry{ws}}
	sessions := NewInMemorySessionRouter()
	sessions.BindSession("sess-bound", ws)

	deps := &serenaRouterDeps{
		Resolver:      resolver,
		Sessions:      sessions,
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	// Sub-case A: bound session, no path-arg -> 200 + upstream hit.
	bodyNoPath := buildToolCallBody(t, "list_memories", map[string]any{})
	rr := postSerena(t, s, bodyNoPath, map[string]string{"Mcp-Session-Id": "sess-bound"})
	if rr.Code != http.StatusOK {
		t.Fatalf("bound session: status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !hit.called {
		t.Fatal("bound session: upstream was not hit")
	}

	// Sub-case B: unbound session, no path-arg -> 503 missing_session.
	hit.called = false
	rrUnbound := postSerena(t, s, bodyNoPath, map[string]string{"Mcp-Session-Id": "sess-unknown"})
	if rrUnbound.Code != http.StatusServiceUnavailable {
		t.Fatalf("unbound session: status = %d, want 503; body=%s", rrUnbound.Code, rrUnbound.Body.String())
	}
	var resp notFoundJSON
	if err := json.Unmarshal(rrUnbound.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !resp.MissingSession {
		t.Errorf("missing_session = false, want true")
	}
	if hit.called {
		t.Error("unbound session: upstream was hit (should not have been)")
	}

	// Sub-case C: no Mcp-Session-Id header at all -> 503 missing_session.
	hit.called = false
	rrNone := postSerena(t, s, bodyNoPath, nil)
	if rrNone.Code != http.StatusServiceUnavailable {
		t.Fatalf("no session id: status = %d, want 503", rrNone.Code)
	}
	if hit.called {
		t.Error("no session id: upstream was hit (should not have been)")
	}
}

// ---------------------------------------------------------------------
// Test 6: TestSerenaRouter_MalformedToolBody_Returns400
// ---------------------------------------------------------------------
func TestSerenaRouter_MalformedToolBody_Returns400(t *testing.T) {
	resolver := &stubResolver{}
	deps := &serenaRouterDeps{
		Resolver:      resolver,
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return "http://invalid" },
	}
	s := newSerenaTestServer(t, deps)

	// Sub-case A: non-JSON garbage.
	rr := postSerena(t, s, []byte("not-json-at-all"), nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("non-JSON: status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}

	// Sub-case B: valid JSON but missing method field.
	noMethod, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"params":  map[string]any{"name": "find_symbol", "arguments": map[string]any{}},
	})
	rr2 := postSerena(t, s, noMethod, nil)
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("missing method: status = %d, want 400; body=%s", rr2.Code, rr2.Body.String())
	}
	if !strings.Contains(rr2.Body.String(), "method") {
		t.Errorf("missing method body does not mention method: %s", rr2.Body.String())
	}
}

// ---------------------------------------------------------------------
// Test 7: TestSerenaRouter_PreservesMcpSessionIdHeader
// ---------------------------------------------------------------------
//
// Mcp-Session-Id must reach upstream verbatim; upstream-set headers
// (Mcp-Session-Id again, or any other) must thread back to the client.
func TestSerenaRouter_PreservesMcpSessionIdHeader(t *testing.T) {
	const upstreamSessionID = "upstream-session-xyz"

	hit := &upstreamHit{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.record(r)
		// Echo a fresh session id back so the client sees it.
		w.Header().Set("Mcp-Session-Id", upstreamSessionID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	resolver := &stubResolver{entries: []*api.WorkspaceEntry{ws}}
	deps := &serenaRouterDeps{
		Resolver:      resolver,
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	const clientSessionID = "client-session-abc"
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, map[string]string{
		"Mcp-Session-Id":       clientSessionID,
		"MCP-Protocol-Version": "2025-11-25",
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// Request side: upstream must have observed the client session id.
	if got := hit.headers.Get("Mcp-Session-Id"); got != clientSessionID {
		t.Errorf("upstream Mcp-Session-Id = %q, want %q", got, clientSessionID)
	}
	if got := hit.headers.Get("MCP-Protocol-Version"); got != "2025-11-25" {
		t.Errorf("upstream MCP-Protocol-Version = %q, want 2025-11-25", got)
	}
	// Response side: downstream client must see the upstream-minted id.
	if got := rr.Header().Get("Mcp-Session-Id"); got != upstreamSessionID {
		t.Errorf("downstream Mcp-Session-Id = %q, want %q", got, upstreamSessionID)
	}
}

// ---------------------------------------------------------------------
// Test 8: TestSerenaRouter_PreservesContentTypeStreaming
// ---------------------------------------------------------------------
//
// Upstream Content-Type: text/event-stream. Three event frames flushed
// at intervals. The handler must thread Content-Type back AND deliver
// each frame as it is flushed -- never coalesce them into one buffered
// blob.
func TestSerenaRouter_PreservesContentTypeStreaming(t *testing.T) {
	frame1 := []byte("event: tool_progress\ndata: {\"step\":1}\n\n")
	frame2 := []byte("event: tool_progress\ndata: {\"step\":2}\n\n")
	frame3 := []byte("event: tool_result\ndata: {\"done\":true}\n\n")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream test server does not support Flusher")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(frame1)
		flusher.Flush()
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write(frame2)
		flusher.Flush()
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write(frame3)
		flusher.Flush()
	}))
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	resolver := &stubResolver{entries: []*api.WorkspaceEntry{ws}}
	deps := &serenaRouterDeps{
		Resolver:      resolver,
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(strings.ToLower(ct), "text/event-stream") {
		t.Errorf("downstream Content-Type = %q, want text/event-stream", ct)
	}
	got := rr.Body.Bytes()
	want := append(append(append([]byte{}, frame1...), frame2...), frame3...)
	if !bytes.Equal(got, want) {
		t.Errorf("downstream body mismatch:\n got = %q\nwant = %q", got, want)
	}
}

// ---------------------------------------------------------------------
// Supplementary: route emits 503 when deps are unwired.
// ---------------------------------------------------------------------
//
// Without SetSerenaRouterDeps + an installed Resolver, the route must
// emit 503 (canonical "register workspace first" body). This proves
// the orchestrator-integration path: until Agent A1 lands, every call
// to /serena/mcp returns the same banner-friendly 503 instead of a
// generic 404 or 500.
func TestSerenaRouter_RoutingLayerNotWired_Returns503(t *testing.T) {
	resetSerenaRouterTestSeam(t)
	serenaRouterTestSeam = func() *serenaRouterDeps { return nil }
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	var resp notFoundJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.PhaseEStatus != "deferred" {
		t.Errorf("phase_e_status = %q", resp.PhaseEStatus)
	}
}

// ---------------------------------------------------------------------
// Supplementary: non-POST methods are 405.
// ---------------------------------------------------------------------
func TestSerenaRouter_NonPostReturns405(t *testing.T) {
	resolver := &stubResolver{}
	deps := &serenaRouterDeps{
		Resolver: resolver,
		Sessions: NewInMemorySessionRouter(),
	}
	s := newSerenaTestServer(t, deps)

	req := httptest.NewRequest(http.MethodGet, "/serena/mcp", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rr.Code)
	}
	if rr.Header().Get("Allow") != "POST" {
		t.Errorf("Allow = %q, want POST", rr.Header().Get("Allow"))
	}
}

// ---------------------------------------------------------------------
// Supplementary: cross-origin POST is rejected by the
// requireSameOrigin middleware before the handler runs.
// ---------------------------------------------------------------------
func TestSerenaRouter_CrossOriginPostRejected(t *testing.T) {
	resolver := &stubResolver{}
	deps := &serenaRouterDeps{
		Resolver: resolver,
		Sessions: NewInMemorySessionRouter(),
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/x"})
	req := httptest.NewRequest(http.MethodPost, "/serena/mcp", bytes.NewReader(body))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "http://attacker.example.com")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("cross-origin status = %d, want 403", rr.Code)
	}
}

// ---------------------------------------------------------------------
// Supplementary: extractPathArg returns the first non-empty field in
// the documented precedence order.
// ---------------------------------------------------------------------
func TestSerenaRouter_ExtractPathArg_PrecedenceAndFallbacks(t *testing.T) {
	cases := []struct {
		name     string
		args     map[string]any
		wantPath string
		wantOk   bool
	}{
		{
			name:     "relative_path wins over file_path",
			args:     map[string]any{"relative_path": "/r", "file_path": "/f"},
			wantPath: "/r",
			wantOk:   true,
		},
		{
			name:     "file_path used when relative_path absent",
			args:     map[string]any{"file_path": "/f"},
			wantPath: "/f",
			wantOk:   true,
		},
		{
			name:     "name_path used when above absent",
			args:     map[string]any{"name_path": "/n"},
			wantPath: "/n",
			wantOk:   true,
		},
		{
			name:     "path used when above absent",
			args:     map[string]any{"path": "/p"},
			wantPath: "/p",
			wantOk:   true,
		},
		{
			name:     "empty string is skipped",
			args:     map[string]any{"relative_path": "", "file_path": "/f"},
			wantPath: "/f",
			wantOk:   true,
		},
		{
			name:     "non-string value is skipped",
			args:     map[string]any{"relative_path": 42, "file_path": "/f"},
			wantPath: "/f",
			wantOk:   true,
		},
		{
			name:     "no path keys at all",
			args:     map[string]any{"foo": "bar", "name_pattern": "Mc.*"},
			wantPath: "",
			wantOk:   false,
		},
		{
			name:     "empty args object",
			args:     map[string]any{},
			wantPath: "",
			wantOk:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.args)
			got, ok := extractPathArg(raw)
			if got != tc.wantPath || ok != tc.wantOk {
				t.Errorf("extractPathArg(%s) = (%q, %v), want (%q, %v)",
					string(raw), got, ok, tc.wantPath, tc.wantOk)
			}
		})
	}
}

// ---------------------------------------------------------------------
// Supplementary: InMemorySessionRouter round-trip and unbind.
// ---------------------------------------------------------------------
func TestInMemorySessionRouter_BindLookupUnbind(t *testing.T) {
	sr := NewInMemorySessionRouter()
	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha"}

	sr.BindSession("", ws)
	if got := sr.LookupSession(""); got != nil {
		t.Errorf("LookupSession(empty) = %+v, want nil", got)
	}

	sr.BindSession("sess-1", nil)
	if got := sr.LookupSession("sess-1"); got != nil {
		t.Errorf("LookupSession after nil bind = %+v, want nil", got)
	}

	sr.BindSession("sess-1", ws)
	if got := sr.LookupSession("sess-1"); got == nil || got.WorkspaceKey != "alpha" {
		t.Errorf("LookupSession after real bind = %+v, want alpha", got)
	}

	sr.UnbindSession("sess-1")
	if got := sr.LookupSession("sess-1"); got != nil {
		t.Errorf("LookupSession after unbind = %+v, want nil", got)
	}

	var wg sync.WaitGroup
	var counter atomic.Int32
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("sess-%d", i)
			sr.BindSession(id, ws)
			if got := sr.LookupSession(id); got != nil {
				counter.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if counter.Load() != 100 {
		t.Errorf("concurrent bindings observed = %d, want 100", counter.Load())
	}
}