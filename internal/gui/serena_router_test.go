// internal/gui/serena_router_test.go
//
// Phase C.2 tests for the /serena/mcp path-aware router. The eight
// canonical scenarios in plan v10 test contract are covered here:
//
//  1. TestSerenaRouter_TwoWorkspaces_PathArgRoutesCorrectly
//  2. TestSerenaRouter_WorkspaceNotFound_Returns503
//  3. TestSerenaRouter_UpstreamTimeout_Returns504
//  4. TestSerenaRouter_UpstreamConnectionRefused_Returns502
//  5. TestSerenaRouter_MissingPathArg_FallsThroughToSessionRouter
//  6. TestSerenaRouter_MalformedToolBody_Returns400
//  7. TestSerenaRouter_PreservesMcpSessionIdHeader
//  8. TestSerenaRouter_PreservesContentTypeStreaming
//
// Each test wires a stub workspaceResolver + sessionRouter + an
// httptest upstream mock. The seam injection point is
// serenaRouterTestSeam (package-level var); tests restore it via
// t.Cleanup so ordering does not leak state between cases.
package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/serena_routing"
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

// fakeSerenaDaemon is an httptest handler that models a real
// session-requiring serena / native-http daemon. It implements the MCP
// Streamable HTTP lifecycle the router now exercises (P1): an
// `initialize` POST mints a fresh daemon session id and returns it in
// the Mcp-Session-Id response header; `notifications/initialized` is
// acknowledged with 202; and every OTHER (non-initialize) POST REQUIRES
// the daemon session header — a request missing it (or carrying an
// unknown id) is rejected with HTTP 400 + a JSON-RPC session error,
// exactly as a real serena daemon would reject the router-minted client
// id. This is the fixture the P1 bug description names: "a fake daemon
// that issues a session on initialize + requires it on subsequent POSTs."
type fakeSerenaDaemon struct {
	mu sync.Mutex
	// sessionPrefix differentiates ids minted by distinct daemons in a
	// multi-daemon test; each mint appends an incrementing counter.
	sessionPrefix string
	mintCount     int
	issued        map[string]bool // session ids this daemon has minted

	// tool is invoked for a non-initialize POST AFTER the session check
	// passes. The recorded *http.Request body has already been read into
	// toolHit; tool writes the JSON-RPC response. When nil, a default
	// {"result":{"ok":true}} is written.
	tool func(w http.ResponseWriter, r *http.Request, body []byte)

	// toolHits counts non-initialize POSTs that PASSED the session gate.
	toolHits int
	// lastToolSession is the Mcp-Session-Id observed on the most recent
	// session-gated tool POST.
	lastToolSession string
	// lastToolBody is the body of the most recent session-gated tool POST.
	lastToolBody []byte
	// lastToolHeaders is a clone of the headers on the most recent
	// session-gated tool POST.
	lastToolHeaders http.Header

	// lastInitProtocolVersion is params.protocolVersion observed on the
	// most recent upstream initialize POST (P1 finding 1 — the router must
	// send the client's negotiated version, not a hard-coded one).
	lastInitProtocolVersion string
	// negotiatedVersionOverride, when non-empty, forces the daemon's
	// initialize RESULT to advertise this protocolVersion REGARDLESS of the
	// version the router requested — modelling a daemon that negotiates DOWN
	// to a different supported revision (Finding #8). When empty the daemon
	// echoes the requested version (proper MCP negotiation: a server echoes
	// the client's offered version when it supports it).
	negotiatedVersionOverride string
	// lastInitHeaders / lastInitializedHeaders are clones of the headers on
	// the most recent initialize and notifications/initialized POSTs, so a
	// test can assert the MCP-Protocol-Version header threaded onto the
	// post-initialize request (P1 finding 1).
	lastInitHeaders        http.Header
	lastInitializedHeaders http.Header

	// initializedStatus overrides the HTTP status returned for
	// notifications/initialized. 0 means the default 202. A non-2xx value
	// models a daemon that rejects the initialized notification (bad
	// session/protocol header), which must FAIL the router handshake
	// (P2 finding 2) rather than be cached as a usable session.
	initializedStatus int

	// deleteHits counts DELETE /mcp requests (client-origin session
	// termination, P2 finding 3). lastDeleteSession is the Mcp-Session-Id
	// observed on the most recent DELETE; lastDeleteHeaders is a clone of the
	// headers on it so a test can assert the MCP-Protocol-Version threaded
	// onto the teardown (Finding 2).
	deleteHits        int
	lastDeleteSession string
	lastDeleteHeaders http.Header
	// deleteStatus overrides the HTTP status returned for DELETE. 0 means
	// the default 204; a non-2xx value models a daemon that fails the
	// teardown (the router must still 204 the client — teardown is
	// best-effort).
	deleteStatus int

	// cancelHits counts notifications/cancelled POSTs forwarded by the
	// router (Finding H). lastCancelSession / lastCancelBody / lastCancelHeaders
	// capture the most recent one so a test can assert the router forwarded
	// the DAEMON-issued session id + the verbatim body to the bound daemon.
	cancelHits        int
	lastCancelSession string
	lastCancelBody    []byte
	lastCancelHeaders http.Header
}

func newFakeSerenaDaemon(prefix string) *fakeSerenaDaemon {
	return &fakeSerenaDaemon{sessionPrefix: prefix, issued: map[string]bool{}}
}

func (d *fakeSerenaDaemon) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// DELETE /mcp is client-origin session termination (P2 finding 3).
		// A real serena/native-http daemon acknowledges with 204. Record
		// the session id the router forwarded so a test can assert the
		// router sent the DAEMON-issued id (not the client id).
		if r.Method == http.MethodDelete {
			d.mu.Lock()
			d.deleteHits++
			d.lastDeleteSession = r.Header.Get("Mcp-Session-Id")
			d.lastDeleteHeaders = r.Header.Clone()
			status := d.deleteStatus
			d.mu.Unlock()
			if status == 0 {
				status = http.StatusNoContent
			}
			w.WriteHeader(status)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &probe)

		switch probe.Method {
		case "initialize":
			d.mu.Lock()
			d.mintCount++
			sid := fmt.Sprintf("%s-daemon-session-%d", d.sessionPrefix, d.mintCount)
			d.issued[sid] = true
			d.lastInitProtocolVersion = probe.Params.ProtocolVersion
			d.lastInitHeaders = r.Header.Clone()
			// MCP negotiation: a server echoes the client's offered version
			// when supported. The fixture echoes probe.Params.ProtocolVersion
			// by default so the router forwards subsequent calls under the
			// version it requested. negotiatedVersionOverride forces a
			// DIFFERENT negotiated version to exercise Finding #8 (the router
			// must then use the daemon-negotiated version, not the requested).
			negotiated := probe.Params.ProtocolVersion
			if d.negotiatedVersionOverride != "" {
				negotiated = d.negotiatedVersionOverride
			}
			d.mu.Unlock()
			w.Header().Set("Mcp-Session-Id", sid)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"serverInfo":{"name":"serena","version":"fake"},"capabilities":{"tools":{}}}}`, idOrNull(probe.ID), negotiated)
			return
		case "notifications/initialized":
			// Acknowledge; a real daemon advances its session here. A
			// configured non-2xx initializedStatus models a daemon that
			// rejects the notification (P2 finding 2) so the router
			// handshake must fail rather than cache a phantom session.
			d.mu.Lock()
			d.lastInitializedHeaders = r.Header.Clone()
			status := d.initializedStatus
			d.mu.Unlock()
			if status == 0 {
				status = http.StatusAccepted
			}
			w.WriteHeader(status)
			return
		case "notifications/cancelled":
			// Record the forwarded cancellation (Finding H). A real daemon
			// answers 202 to a notification. Captured separately from
			// toolHits so a test can assert the router forwarded the
			// DAEMON-issued session id and the verbatim body.
			d.mu.Lock()
			d.cancelHits++
			d.lastCancelSession = r.Header.Get("Mcp-Session-Id")
			d.lastCancelBody = append([]byte(nil), body...)
			d.lastCancelHeaders = r.Header.Clone()
			d.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			return
		}

		// Any non-initialize request requires a known daemon session id.
		sid := r.Header.Get("Mcp-Session-Id")
		d.mu.Lock()
		known := sid != "" && d.issued[sid]
		d.mu.Unlock()
		if !known {
			// Mirror a real daemon's rejection of an unknown session.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32600,"message":"missing or unknown Mcp-Session-Id"}}`, idOrNull(probe.ID))
			return
		}

		d.mu.Lock()
		d.toolHits++
		d.lastToolSession = sid
		d.lastToolBody = append([]byte(nil), body...)
		d.lastToolHeaders = r.Header.Clone()
		toolFn := d.tool
		d.mu.Unlock()

		if toolFn != nil {
			toolFn(w, r, body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}
}

// idOrNull renders a JSON-RPC id verbatim, or `null` when absent, for
// echoing in the fake daemon's responses.
func idOrNull(id json.RawMessage) string {
	if len(id) == 0 {
		return "null"
	}
	return string(id)
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

func buildToolCallBodyWithoutName(t *testing.T, arguments any) []byte {
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
			"arguments": json.RawMessage(argsRaw),
		},
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body
}

func makeSerenaWorkspace(t *testing.T, root, name string) string {
	t.Helper()
	wsPath := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(wsPath, ".serena"), 0o755); err != nil {
		t.Fatalf("mkdir serena workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsPath, ".serena", "project.yml"), []byte("project_name: "+name+"\n"), 0o644); err != nil {
		t.Fatalf("write serena marker: %v", err)
	}
	canon, err := api.CanonicalWorkspacePath(wsPath)
	if err != nil {
		t.Fatalf("canonicalize workspace: %v", err)
	}
	return canon
}

func writeWorkspaceFile(t *testing.T, wsPath string, elems ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{wsPath}, elems...)...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir workspace file dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("// fixture\n"), 0o644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}
	return path
}

func testServerPort(t *testing.T, ts *httptest.Server) int {
	t.Helper()
	hostPort := strings.TrimPrefix(ts.URL, "http://")
	_, portText, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("parse test server URL %q: %v", ts.URL, err)
	}
	var port int
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil {
		t.Fatalf("parse test server port %q: %v", portText, err)
	}
	return port
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

func TestSerenaRouter_BindAfterSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "sess-success"})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := deps.Sessions.LookupSession("sess-success"); got == nil || got.WorkspaceKey != "alpha" {
		t.Fatalf("LookupSession(sess-success) = %+v, want alpha binding", got)
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

func TestSerenaRouter_SSEStreamSurvivesPastTimeoutBoundary(t *testing.T) {
	const frameCount = 4
	upstreamTimeout := 150 * time.Millisecond

	frames := make([][]byte, 0, frameCount)
	for i := 1; i <= frameCount; i++ {
		frames = append(frames, []byte(fmt.Sprintf("event: tick\ndata: {\"frame\":%d}\n\n", i)))
	}

	daemon := newFakeSerenaDaemon("alpha")
	daemon.tool = func(w http.ResponseWriter, r *http.Request, body []byte) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream test server does not support Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range frames {
			_, _ = w.Write(frame)
			flusher.Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	resolver := &stubResolver{entries: []*api.WorkspaceEntry{ws}}
	deps := &serenaRouterDeps{
		Resolver:        resolver,
		Sessions:        NewInMemorySessionRouter(),
		UpstreamURLFn:   func(ws *api.WorkspaceEntry) string { return ts.URL },
		UpstreamTimeout: upstreamTimeout,
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
	want := bytes.Join(frames, nil)
	if !bytes.Equal(got, want) {
		t.Fatalf("downstream SSE stream truncated or changed:\n got = %q\nwant = %q", got, want)
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

func TestSerenaRouter_NoBindOnUpstreamConnectionFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	deadURL := fmt.Sprintf("http://%s", ln.Addr().String())
	_ = ln.Close()

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	sessions := NewInMemorySessionRouter()
	if got := sessions.LookupSession("sess-fail"); got != nil {
		t.Fatalf("precondition LookupSession(sess-fail) = %+v, want nil", got)
	}

	deps := &serenaRouterDeps{
		Resolver: &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions: sessions,
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext,
			},
		},
		UpstreamURLFn:   func(ws *api.WorkspaceEntry) string { return deadURL },
		UpstreamTimeout: 500 * time.Millisecond,
		AuditFn:         func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "sess-fail"})

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
	if got := sessions.LookupSession("sess-fail"); got != nil {
		t.Fatalf("LookupSession(sess-fail) after failed upstream = %+v, want nil", got)
	}
}

func TestSerenaRouter_NoBindOnUpstreamHeaderTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	sessions := NewInMemorySessionRouter()
	deps := &serenaRouterDeps{
		Resolver: &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions: sessions,
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext,
				ResponseHeaderTimeout: 25 * time.Millisecond,
			},
		},
		UpstreamURLFn:   func(ws *api.WorkspaceEntry) string { return slow.URL },
		UpstreamTimeout: 50 * time.Millisecond,
		AuditFn:         func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "sess-timeout"})

	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body=%s", rr.Code, rr.Body.String())
	}
	if got := sessions.LookupSession("sess-timeout"); got != nil {
		t.Fatalf("LookupSession(sess-timeout) after upstream timeout = %+v, want nil", got)
	}
}

func TestSerenaRouter_BindOn5xxUpstreamResponse(t *testing.T) {
	// Daemon completes the handshake, then fails the actual tool call
	// with a 500 (per-call failure). The session must still bind: the
	// upstream session exists, the daemon is reachable, only this call
	// failed.
	daemon := newFakeSerenaDaemon("alpha")
	daemon.tool = func(w http.ResponseWriter, r *http.Request, body []byte) {
		http.Error(w, "upstream per-call failure", http.StatusInternalServerError)
	}
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

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "sess-5xx"})

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}
	if got := sessions.LookupSession("sess-5xx"); got == nil || got.WorkspaceKey != "alpha" {
		t.Fatalf("LookupSession(sess-5xx) = %+v, want alpha binding", got)
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

func TestSerenaRouter_MissingParamsName_Returns400(t *testing.T) {
	hit := &upstreamHit{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.record(r)
		w.WriteHeader(http.StatusOK)
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

	body := buildToolCallBodyWithoutName(t, map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if got, want := strings.TrimSpace(rr.Body.String()), `{"error": "missing required field: params.name"}`; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if hit.called {
		t.Fatal("upstream was hit for malformed tool body")
	}
}

// ---------------------------------------------------------------------
// Test 7: TestSerenaRouter_PreservesMcpSessionIdHeader
// ---------------------------------------------------------------------
//
// P1 multiplexing semantics (CHANGED from the pre-completion router):
//   - The upstream daemon must observe the DAEMON-issued session id (the
//     one it minted at the router's handshake), NOT the router-minted
//     client id — the daemon never created the client id and would reject
//     a tool call carrying it.
//   - The client must keep its OWN router-minted session id on the
//     response; the daemon id is an internal router↔daemon detail and is
//     never surfaced downstream (re-asserting it would make the client
//     switch ids and break the router's client→daemon session map).
//   - MCP-Protocol-Version still threads upstream verbatim.
func TestSerenaRouter_PreservesMcpSessionIdHeader(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	daemon.tool = func(w http.ResponseWriter, r *http.Request, body []byte) {
		// A real daemon may re-echo its own session id; the router must
		// NOT leak it downstream.
		w.Header().Set("Mcp-Session-Id", r.Header.Get("Mcp-Session-Id"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}
	ts := httptest.NewServer(daemon.handler())
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
	// Request side: upstream must have observed a DAEMON-issued session id,
	// NOT the router-minted client id.
	gotUpstreamSID := daemon.lastToolSession
	if gotUpstreamSID == clientSessionID {
		t.Errorf("upstream Mcp-Session-Id = client id %q; want a daemon-issued id (router must not pass the client id through)", clientSessionID)
	}
	daemon.mu.Lock()
	known := daemon.issued[gotUpstreamSID]
	daemon.mu.Unlock()
	if !known {
		t.Errorf("upstream Mcp-Session-Id = %q is not a session this daemon minted; want the handshake-issued id", gotUpstreamSID)
	}
	if got := daemon.lastToolHeaders.Get("MCP-Protocol-Version"); got != "2025-11-25" {
		t.Errorf("upstream MCP-Protocol-Version = %q, want 2025-11-25", got)
	}
	// Response side: downstream client must keep its OWN router-minted id,
	// never the daemon id.
	if got := rr.Header().Get("Mcp-Session-Id"); got != clientSessionID {
		t.Errorf("downstream Mcp-Session-Id = %q, want the client's own id %q (daemon id must not leak)", got, clientSessionID)
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

	daemon := newFakeSerenaDaemon("alpha")
	daemon.tool = func(w http.ResponseWriter, r *http.Request, body []byte) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream test server does not support Flusher")
			return
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
	}
	ts := httptest.NewServer(daemon.handler())
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

func TestSerenaRouter_RealResolverIntegration_RoutesPathArgToCorrectWorkspace(t *testing.T) {
	resetSerenaRouterTestSeam(t)
	serenaRouterTestSeam = nil

	root := t.TempDir()
	wsAlpha := makeSerenaWorkspace(t, root, "Alpha")
	wsBeta := makeSerenaWorkspace(t, root, "Beta")
	alphaFile := writeWorkspaceFile(t, wsAlpha, "src", "alpha.go")
	betaFile := writeWorkspaceFile(t, wsBeta, "src", "beta.go")

	// Both upstreams are session-requiring fake daemons; the per-call
	// tool handler answers with the workspace name so we can assert WHICH
	// daemon a path-arg routed to. "Hits" now means session-gated tool
	// calls (the handshake initialize/notifications/initialized are
	// counted separately by the daemon's lifecycle branch).
	daemonAlpha := newFakeSerenaDaemon("alpha")
	daemonAlpha.tool = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"workspace":"alpha"}}`))
	}
	tsAlpha := httptest.NewServer(daemonAlpha.handler())
	t.Cleanup(tsAlpha.Close)

	daemonBeta := newFakeSerenaDaemon("beta")
	daemonBeta.tool = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"workspace":"beta"}}`))
	}
	tsBeta := httptest.NewServer(daemonBeta.handler())
	t.Cleanup(tsBeta.Close)
	alphaToolHits := func() int { daemonAlpha.mu.Lock(); defer daemonAlpha.mu.Unlock(); return daemonAlpha.toolHits }
	betaToolHits := func() int { daemonBeta.mu.Lock(); defer daemonBeta.mu.Unlock(); return daemonBeta.toolHits }

	regPath := filepath.Join(root, "workspaces.yaml")
	reg := api.NewRegistry(regPath)
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  api.WorkspaceKey(wsAlpha),
		WorkspacePath: wsAlpha,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          testServerPort(t, tsAlpha),
		TaskName:      "mcp-local-hub-serena-alpha",
	}); err != nil {
		t.Fatalf("PutSerena alpha: %v", err)
	}
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  api.WorkspaceKey(wsBeta),
		WorkspacePath: wsBeta,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          testServerPort(t, tsBeta),
		TaskName:      "mcp-local-hub-serena-beta",
	}); err != nil {
		t.Fatalf("PutSerena beta: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save registry: %v", err)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("Load registry: %v", err)
	}

	resolver := serena_routing.NewWorkspaceResolver(reg, regPath)
	sessions := serena_routing.NewSessionRouter()
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	s.SetSerenaRouterProduction(resolver, sessions)

	bodyAlpha := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": alphaFile})
	rrAlpha := postSerena(t, s, bodyAlpha, nil)
	if rrAlpha.Code != http.StatusOK {
		t.Fatalf("alpha status = %d, want 200; body=%s", rrAlpha.Code, rrAlpha.Body.String())
	}
	if got := alphaToolHits(); got != 1 {
		t.Fatalf("alpha tool hits = %d, want 1", got)
	}
	if got := betaToolHits(); got != 0 {
		t.Fatalf("beta tool hits after alpha request = %d, want 0", got)
	}

	bodyBeta := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": betaFile})
	rrBeta := postSerena(t, s, bodyBeta, nil)
	if rrBeta.Code != http.StatusOK {
		t.Fatalf("beta status = %d, want 200; body=%s", rrBeta.Code, rrBeta.Body.String())
	}
	if got := alphaToolHits(); got != 1 {
		t.Fatalf("alpha tool hits after beta request = %d, want 1", got)
	}
	if got := betaToolHits(); got != 1 {
		t.Fatalf("beta tool hits = %d, want 1", got)
	}

	unknown := writeWorkspaceFile(t, filepath.Join(root, "Unregistered"), "src", "unknown.go")
	bodyUnknown := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": unknown})
	rrUnknown := postSerena(t, s, bodyUnknown, nil)
	if rrUnknown.Code != http.StatusServiceUnavailable {
		t.Fatalf("unknown status = %d, want 503; body=%s", rrUnknown.Code, rrUnknown.Body.String())
	}
	if got := alphaToolHits(); got != 1 {
		t.Fatalf("alpha tool hits after unknown request = %d, want 1", got)
	}
	if got := betaToolHits(); got != 1 {
		t.Fatalf("beta tool hits after unknown request = %d, want 1", got)
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
	// Finding H: the 405 fallback now advertises both accepted verbs.
	if rr.Header().Get("Allow") != "POST, DELETE" {
		t.Errorf("Allow = %q, want POST, DELETE", rr.Header().Get("Allow"))
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

// ---------------------------------------------------------------------
// Phase 5 Part C: auto-register-on-miss.
//
// When a tool-call resolves to no registered workspace
// (ErrWorkspaceNotFound), the router calls deps.AutoRegisterFn to
// register+spawn the workspace, then forwards the call — instead of the
// pre-Phase-5 immediate 503. These tests inject a fake AutoRegisterFn via
// the deps seam (a fake Resolver returning ErrWorkspaceNotFound is the
// trigger) and assert the four outcome mappings + the unwired back-compat
// path.
// ---------------------------------------------------------------------

// nilEntryResolver returns (nil, nil) on every ResolveByPath — the
// "resolved==nil" not-found shape (distinct from ErrWorkspaceNotFound).
// stubResolver returns ErrWorkspaceNotFound on no-match, so this dedicated
// stub exercises the second auto-register call site.
type nilEntryResolver struct{}

func (nilEntryResolver) ResolveByPath(string) (*api.WorkspaceEntry, error) { return nil, nil }

// TestSerenaRouter_AutoRegister_SuccessForwards: AutoRegisterFn returns
// (entry, nil) → the tool call is FORWARDED to entry's upstream (NOT a 503).
func TestSerenaRouter_AutoRegister_SuccessForwards(t *testing.T) {
	hit := &upstreamHit{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"workspace":"autoreg"}}`))
	}))
	t.Cleanup(ts.Close)

	registered := &api.WorkspaceEntry{WorkspaceKey: "autoreg", WorkspacePath: "/proj/autoreg", Port: 9301, TaskName: `\mcp-local-hub-serena-autoreg`}

	var registerCalls int32
	var registeredPath string
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: nil}, // no match → ErrWorkspaceNotFound
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		AutoRegisterFn: func(ctx context.Context, absPath string) (*api.WorkspaceEntry, error) {
			atomic.AddInt32(&registerCalls, 1)
			registeredPath = absPath
			return registered, nil
		},
	}
	s := newSerenaTestServer(t, deps)

	// Realistic first-call flow: the client initialized at the router first, so its
	// session is LIVE (a tool-call carries a minted session id per MCP). bot PR #253
	// r6 #4: the auto-register MUTATION path requires a live (or empty) session — an
	// absent id is a revoked/unknown session and is rejected before register.
	s.serenaRouterSessions.store("sess-autoreg", "2025-03-26")
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/autoreg/src/main.go"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "sess-autoreg"})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (auto-register then forward); body=%s", rr.Code, rr.Body.String())
	}
	if got := atomic.LoadInt32(&registerCalls); got != 1 {
		t.Errorf("AutoRegisterFn call count = %d, want 1", got)
	}
	if registeredPath != "/proj/autoreg/src/main.go" {
		t.Errorf("AutoRegisterFn received path %q, want the tool-arg path", registeredPath)
	}
	if !hit.called {
		t.Fatal("upstream of the auto-registered workspace was not hit")
	}
	if hit.method != http.MethodPost {
		t.Errorf("upstream method = %s, want POST", hit.method)
	}
	if !bytes.Contains(hit.body, []byte(`"find_symbol"`)) {
		t.Errorf("forwarded upstream body did not carry the tool name; got %s", string(hit.body))
	}
	// The freshly auto-registered workspace binds the sticky session exactly
	// like a pre-registered one.
	if bound := deps.Sessions.LookupSession("sess-autoreg"); bound == nil || bound.WorkspaceKey != "autoreg" {
		t.Errorf("session binding after auto-register = %+v, want autoreg", bound)
	}
}

// TestSerenaRouter_AutoRegister_ResolvedNilForwards: the resolved==nil
// not-found shape (resolver returns (nil, nil)) also drives auto-register.
func TestSerenaRouter_AutoRegister_ResolvedNilForwards(t *testing.T) {
	hit := &upstreamHit{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(ts.Close)

	registered := &api.WorkspaceEntry{WorkspaceKey: "autoreg2", WorkspacePath: "/proj/autoreg2", Port: 9302}
	var registerCalls int32
	deps := &serenaRouterDeps{
		Resolver:      nilEntryResolver{},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		AutoRegisterFn: func(ctx context.Context, absPath string) (*api.WorkspaceEntry, error) {
			atomic.AddInt32(&registerCalls, 1)
			return registered, nil
		},
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/autoreg2/x"})
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := atomic.LoadInt32(&registerCalls); got != 1 {
		t.Errorf("AutoRegisterFn call count = %d, want 1 (resolved==nil branch)", got)
	}
	if !hit.called {
		t.Fatal("upstream was not hit on the resolved==nil auto-register branch")
	}
}

// TestSerenaRouter_CancelAutoRegisterOnSessionDeath_CancelsWhenNotLive: bot PR
// #253 r4 P2 — the watcher cancels the detached auto-register context as soon as
// the router session is no longer live (a DELETE makes it absent, the idle-sweep
// makes it expired), so a terminated session cannot finish a registration. Driven
// here with a never-minted (absent) id so peekVersionState is deterministically
// non-live on the first tick.
func TestSerenaRouter_CancelAutoRegisterOnSessionDeath_CancelsWhenNotLive(t *testing.T) {
	s := newSerenaTestServer(t, &serenaRouterDeps{Resolver: &stubResolver{entries: nil}, Sessions: NewInMemorySessionRouter()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go s.cancelAutoRegisterOnSessionDeath(ctx, cancel, stop, "never-minted-session", NewInMemorySessionRouter())
	select {
	case <-ctx.Done():
		// good — the watcher cancelled because the session is not live
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not cancel the register context for a non-live session")
	}
}

// TestSerenaRouter_AutoRegister_InvalidSession_RejectsBeforeRegister: bot PR #253
// P2 — a path-bearing first call carrying a router session whose negotiated
// version conflicts with the request header must be rejected BEFORE auto-register
// runs, so an invalid/stale client cannot consume a pool port or mutate
// workspaces.yaml / the supervisor intent.
func TestSerenaRouter_AutoRegister_InvalidSession_RejectsBeforeRegister(t *testing.T) {
	var registerCalls int32
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: nil}, // not-found → would auto-register
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return "http://127.0.0.1:0" },
		AutoRegisterFn: func(ctx context.Context, absPath string) (*api.WorkspaceEntry, error) {
			atomic.AddInt32(&registerCalls, 1)
			return &api.WorkspaceEntry{WorkspaceKey: "x", WorkspacePath: "/proj/x", Port: 9303}, nil
		},
	}
	s := newSerenaTestServer(t, deps)
	// Mint a LIVE router session negotiated at one version.
	s.serenaRouterSessions.store("sess-vm", "2025-03-26")

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/x/main.go"})
	// The request carries a CONFLICTING protocol version → reject before register.
	rr := postSerena(t, s, body, map[string]string{
		"Mcp-Session-Id":       "sess-vm",
		"MCP-Protocol-Version": "1999-01-01",
	})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (protocol-version mismatch before register); body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "protocol-version mismatch") {
		t.Errorf("body = %s, want protocol-version mismatch", rr.Body.String())
	}
	if got := atomic.LoadInt32(&registerCalls); got != 0 {
		t.Errorf("AutoRegisterFn called %d times, want 0 (an invalid session must not trigger auto-register)", got)
	}
}

// TestSerenaRouter_AutoRegister_AbsentSession_RejectsBeforeRegister: bot PR #253
// r6 #4 — a NON-EMPTY Mcp-Session-Id the router does not hold live (minted here
// then DELETEd/swept, or one it never minted) is routerSessionAbsent. On the
// registration-MUTATION path it must be REJECTED (400 "session terminated")
// BEFORE auto-register, so a revoked/unknown session cannot allocate a pool port
// or mutate registry+intent. (The main ROUTING handler still treats absent as a
// legacy caller for EXISTING workspaces; this stricter gate is registration-only.)
func TestSerenaRouter_AutoRegister_AbsentSession_RejectsBeforeRegister(t *testing.T) {
	var registerCalls int32
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: nil}, // not-found → would auto-register
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return "http://127.0.0.1:0" },
		AutoRegisterFn: func(ctx context.Context, absPath string) (*api.WorkspaceEntry, error) {
			atomic.AddInt32(&registerCalls, 1)
			return &api.WorkspaceEntry{WorkspaceKey: "x", WorkspacePath: "/proj/x", Port: 9305}, nil
		},
	}
	s := newSerenaTestServer(t, deps)
	// Do NOT mint "sess-gone" at the router: a non-empty id with no live entry →
	// peekVersionState reports routerSessionAbsent.

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/x/main.go"})
	rr := postSerena(t, s, body, map[string]string{
		"Mcp-Session-Id": "sess-gone",
	})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (absent session rejected before register); body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "session terminated") {
		t.Errorf("body = %s, want session terminated", rr.Body.String())
	}
	if got := atomic.LoadInt32(&registerCalls); got != 0 {
		t.Errorf("AutoRegisterFn called %d times, want 0 (an absent session must not trigger auto-register)", got)
	}
}

// TestSerenaRouter_AutoRegister_IdlessNotification_202NoRegister: bot PR #253
// P2 — an id-less tools/call is a JSON-RPC NOTIFICATION, so it must 202 without
// auto-registering (no workspaces.yaml / supervisor-intent mutation, no pool
// port consumed) even for an unregistered marker-bearing path.
func TestSerenaRouter_AutoRegister_IdlessNotification_202NoRegister(t *testing.T) {
	var registerCalls int32
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: nil},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return "http://127.0.0.1:0" },
		AutoRegisterFn: func(ctx context.Context, absPath string) (*api.WorkspaceEntry, error) {
			atomic.AddInt32(&registerCalls, 1)
			return &api.WorkspaceEntry{WorkspaceKey: "x", WorkspacePath: "/proj/x", Port: 9304}, nil
		},
	}
	s := newSerenaTestServer(t, deps)

	// id-less tools/call (no "id" field) for an unregistered marker-bearing path.
	body := []byte(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"find_symbol","arguments":{"relative_path":"/proj/x/main.go"}}}`)
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (id-less notification, no execution); body=%s", rr.Code, rr.Body.String())
	}
	if got := atomic.LoadInt32(&registerCalls); got != 0 {
		t.Errorf("AutoRegisterFn called %d times, want 0 (a notification must not auto-register)", got)
	}
}

// TestSerenaRouter_AutoRegister_NotASerenaProject_Returns503: AutoRegisterFn
// returns api.ErrNotASerenaProject → the canonical workspace-not-found 503
// (the DoS bound — behave exactly like today's not-found, no marker = no
// register).
func TestSerenaRouter_AutoRegister_NotASerenaProject_Returns503(t *testing.T) {
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: nil},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return "http://invalid" },
		AutoRegisterFn: func(ctx context.Context, absPath string) (*api.WorkspaceEntry, error) {
			return nil, api.ErrNotASerenaProject
		},
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/not/a/project/file.go"})
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	var resp notFoundJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode not-found body: %v; raw=%s", err, rr.Body.String())
	}
	if resp.PhaseEStatus != "deferred" {
		t.Errorf("phase_e_status = %q, want %q", resp.PhaseEStatus, "deferred")
	}
	if !strings.Contains(resp.Error, "register workspace") {
		t.Errorf("error = %q does not mention register workspace", resp.Error)
	}
	if resp.ResolvedPath != "/not/a/project/file.go" {
		t.Errorf("resolved_path = %q, want the requested path", resp.ResolvedPath)
	}
}

// TestSerenaRouter_AutoRegister_NoLanguages_Returns422: AutoRegisterFn returns
// api.ErrNoLanguages → JSON-RPC -32602 invalid-params at HTTP 422.
func TestSerenaRouter_AutoRegister_NoLanguages_Returns422(t *testing.T) {
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: nil},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return "http://invalid" },
		AutoRegisterFn: func(ctx context.Context, absPath string) (*api.WorkspaceEntry, error) {
			return nil, api.ErrNoLanguages
		},
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/nolang/file.go"})
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode JSON-RPC error: %v; raw=%s", err, rr.Body.String())
	}
	if resp.Error == nil || resp.Error.Code != jsonrpcInvalidParams {
		t.Fatalf("error code = %+v, want %d (invalid-params)", resp.Error, jsonrpcInvalidParams)
	}
	if !strings.Contains(resp.Error.Message, "declares no languages") {
		t.Errorf("error message %q should name the no-languages condition", resp.Error.Message)
	}
}

// TestSerenaRouter_AutoRegister_GenericError_Returns503: AutoRegisterFn returns
// a generic (install/spawn/readiness) error → JSON-RPC -32603 internal-error at
// HTTP 503.
func TestSerenaRouter_AutoRegister_GenericError_Returns503(t *testing.T) {
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: nil},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return "http://invalid" },
		AutoRegisterFn: func(ctx context.Context, absPath string) (*api.WorkspaceEntry, error) {
			return nil, fmt.Errorf("supervisor reconcile failed: boom")
		},
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/boom/file.go"})
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode JSON-RPC error: %v; raw=%s", err, rr.Body.String())
	}
	if resp.Error == nil || resp.Error.Code != jsonrpcInternalError {
		t.Fatalf("error code = %+v, want %d (internal-error)", resp.Error, jsonrpcInternalError)
	}
	if !strings.Contains(resp.Error.Message, "serena auto-register failed") {
		t.Errorf("error message %q should name the auto-register failure", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "boom") {
		t.Errorf("error message %q should wrap the underlying error", resp.Error.Message)
	}
}

// TestSerenaRouter_AutoRegister_Unwired_Returns503: a nil AutoRegisterFn
// preserves the pre-Phase-5 immediate writeWorkspaceNotFound 503 (back-compat
// for partially-wired routing).
func TestSerenaRouter_AutoRegister_Unwired_Returns503(t *testing.T) {
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: nil},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return "http://invalid" },
		// AutoRegisterFn intentionally nil.
	}
	s := newSerenaTestServer(t, deps)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/unknown/path/file.go"})
	rr := postSerena(t, s, body, nil)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (unwired back-compat); body=%s", rr.Code, rr.Body.String())
	}
	var resp notFoundJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode not-found body: %v; raw=%s", err, rr.Body.String())
	}
	if resp.PhaseEStatus != "deferred" {
		t.Errorf("phase_e_status = %q, want %q", resp.PhaseEStatus, "deferred")
	}
	if resp.ResolvedPath != "/unknown/path/file.go" {
		t.Errorf("resolved_path = %q, want the requested path", resp.ResolvedPath)
	}
}
