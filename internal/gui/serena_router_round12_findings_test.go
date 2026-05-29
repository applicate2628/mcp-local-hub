// internal/gui/serena_router_round12_findings_test.go
//
// Round-12 code-review-finding tests for the /serena/mcp router:
//
//	Finding 1 — toolBody.Params is a json.RawMessage; a PRESENT but
//	            non-object params on a lifecycle method (initialize /
//	            tools/list) yields a JSON-RPC -32602 envelope (HTTP 400),
//	            NOT a plain-text "malformed JSON body" 400. Whole-body
//	            malformed JSON still gets the plain 400. Valid object params
//	            still work. Tool-call routing still extracts params.name.
//	Finding 3 — fetchToolsListFromAnyDaemon classifies an upstream
//	            tools/list JSON-RPC error: a CLIENT error (-32602) is
//	            forwarded immediately (no other daemon tried); a SERVER error
//	            (-32603 / -32000..-32099) is a candidate failure that falls
//	            through to the next daemon.
//	Finding 4 — a client DELETE that arrives DURING a slow upstream
//	            handshake terminates the router session; the in-flight
//	            tool-call must NOT store a daemon binding or re-bind the
//	            sticky session for the terminated id, must best-effort-DELETE
//	            the just-minted daemon session, and must abort -32600; a
//	            later pathless call with that id is NOT routed.
//	Finding 2 — the partial-handshake (notifications/initialized-fail)
//	            cleanup DELETE honors a SHORT injected UpstreamTimeout rather
//	            than the fixed 5s serenaCleanupTimeout default.
package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// ---------------------------------------------------------------------
// Finding 1 — a lifecycle method (initialize / tools/list) with a PRESENT
// but NON-OBJECT params (`"params":[]`) must now decode (toolBody.Params is
// a json.RawMessage) and be rejected with a JSON-RPC -32602 envelope at HTTP
// 400 — NOT the pre-fix plain-text "malformed JSON body" 400 that a typed
// struct Params produced. The reconcile probe + valid object params still work.
// ---------------------------------------------------------------------
func TestSerenaRouter_Lifecycle_NonObjectParamsReturnsJSONRPC32602(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	daemon.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"find_symbol"}]}}`))
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)
	toolHits := func() int { daemon.mu.Lock(); defer daemon.mu.Unlock(); return daemon.toolHits }

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}}, list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	assertNonObjectParams32602 := func(t *testing.T, rr *httptest.ResponseRecorder) {
		t.Helper()
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
		}
		// MUST be a parseable JSON-RPC -32602 envelope, NOT the plain-text
		// "malformed JSON body" the pre-Finding-1 struct decode produced.
		if strings.Contains(rr.Body.String(), "malformed JSON body") {
			t.Fatalf("got the pre-fix plain-text 'malformed JSON body' 400; want a JSON-RPC -32602 envelope. body=%s", rr.Body.String())
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
			t.Fatalf("body is not a JSON-RPC envelope: %v; raw=%s", err, rr.Body.String())
		}
		if resp.JSONRPC != "2.0" {
			t.Errorf("jsonrpc = %q, want 2.0", resp.JSONRPC)
		}
		if len(resp.Result) > 0 {
			t.Errorf("result present on a -32602 rejection; want error only")
		}
		if resp.Error == nil || resp.Error.Code != jsonrpcInvalidParams {
			t.Fatalf("expected -32602 invalid params; got %+v", resp.Error)
		}
	}

	// initialize with params:[] -> -32602 envelope, no session minted.
	t.Run("initialize", func(t *testing.T) {
		rr := postSerena(t, s, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":[]}`), nil)
		assertNonObjectParams32602(t, rr)
		if sid := rr.Header().Get("Mcp-Session-Id"); sid != "" {
			t.Errorf("non-object-params initialize minted Mcp-Session-Id %q; want none", sid)
		}
	})

	// tools/list with params:[] -> -32602 envelope, no daemon proxy.
	t.Run("tools/list", func(t *testing.T) {
		sid := mintRouterSession(t, s, "2025-11-25")
		before := toolHits()
		rr := postSerena(t, s, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[]}`), map[string]string{"Mcp-Session-Id": sid})
		assertNonObjectParams32602(t, rr)
		if got := toolHits(); got != before {
			t.Errorf("daemon tool hits = %d, want %d (a -32602 non-object-params tools/list must not proxy)", got, before)
		}
	})

	// tools/list with params:1 (number) -> also -32602 (defense-in-depth on the
	// other non-object shapes).
	t.Run("tools/list number params", func(t *testing.T) {
		sid := mintRouterSession(t, s, "2025-11-25")
		rr := postSerena(t, s, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":1}`), map[string]string{"Mcp-Session-Id": sid})
		assertNonObjectParams32602(t, rr)
	})
}

// Finding 1 — a WHOLE-BODY malformed JSON (not even valid JSON) still gets the
// plain HTTP 400 "malformed JSON body" (the envelope decode itself fails, so
// there is no JSON-RPC id to echo). This pins that the RawMessage change did
// NOT swallow truly-malformed bodies into a JSON-RPC envelope.
func TestSerenaRouter_WholeBodyMalformedStillPlain400(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &stubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	rr := postSerena(t, s, []byte("not-json-at-all{"), nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "malformed JSON body") {
		t.Errorf("whole-body-malformed JSON should keep the plain 'malformed JSON body' 400; body=%s", rr.Body.String())
	}
}

// Finding 1 — valid OBJECT params still work for both lifecycle methods and the
// tool-call path-routing still extracts params.name from the RawMessage.
func TestSerenaRouter_Finding1_ValidObjectParamsStillWork(t *testing.T) {
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
		Resolver:      &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}}, list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	// initialize with a valid object params -> 200 + minted session.
	sid := mintRouterSession(t, s, "2025-06-18")

	// tools/list with a valid (empty) object params -> 200 + tools.
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/list (object params) status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	assertToolsListNames(t, rr.Body.Bytes(), []string{"find_symbol"})

	// tool-call with a real params.name -> routed to the daemon by path-arg.
	tcDaemon := newFakeSerenaDaemon("alpha")
	tsTC := httptest.NewServer(tcDaemon.handler())
	t.Cleanup(tsTC.Close)
	deps.UpstreamURLFn = func(ws *api.WorkspaceEntry) string { return tsTC.URL }
	rrTC := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{"Mcp-Session-Id": "sess-tc-f1"})
	if rrTC.Code != http.StatusOK {
		t.Fatalf("tool call (object params) status = %d, want 200; body=%s", rrTC.Code, rrTC.Body.String())
	}
	if got := func() int { tcDaemon.mu.Lock(); defer tcDaemon.mu.Unlock(); return tcDaemon.toolHits }(); got != 1 {
		t.Errorf("tool-call daemon hits = %d, want 1 (params.name must still route from the RawMessage)", got)
	}
}

// ---------------------------------------------------------------------
// Finding 3 (unit) — isClientToolsListError classifies request-level codes as
// client (short-circuit) and server codes (-32603 + the -32000..-32099 range)
// as server (fall through). An unknown/out-of-range code is conservatively
// client.
// ---------------------------------------------------------------------
func TestIsClientToolsListError(t *testing.T) {
	cases := []struct {
		code       int
		wantClient bool
	}{
		{-32700, true},  // parse error
		{-32600, true},  // invalid request
		{-32601, true},  // method not found
		{-32602, true},  // invalid params
		{-32603, false}, // internal error (SERVER)
		{-32000, false}, // server-reserved range start
		{-32050, false}, // server-reserved range middle
		{-32099, false}, // server-reserved range end
		{-31999, true},  // just outside the server range -> conservative client
		{-32100, true},  // just outside the server range -> conservative client
		{100, true},     // application-defined positive -> conservative client
		{0, true},       // unknown -> conservative client
	}
	for _, tc := range cases {
		if got := isClientToolsListError(tc.code); got != tc.wantClient {
			t.Errorf("isClientToolsListError(%d) = %v, want %v", tc.code, got, tc.wantClient)
		}
	}
}

// Finding 3 — a CLIENT error (-32602) from the FIRST daemon is forwarded to the
// client immediately; the SECOND daemon is NEVER contacted (the request is
// invalid for every daemon).
func TestSerenaRouter_ToolsList_ClientErrorShortCircuitsNoSecondDaemon(t *testing.T) {
	daemonA := newFakeSerenaDaemon("alpha")
	daemonA.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid cursor"}}`))
	}
	tsA := httptest.NewServer(daemonA.handler())
	t.Cleanup(tsA.Close)

	daemonB := newFakeSerenaDaemon("beta")
	tsB := httptest.NewServer(daemonB.handler())
	t.Cleanup(tsB.Close)

	wsA := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	wsB := &api.WorkspaceEntry{WorkspaceKey: "beta", WorkspacePath: "/proj/beta", Port: 9202}
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{list: []*api.WorkspaceEntry{wsA, wsB}},
		Sessions: NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string {
			if ws.WorkspaceKey == "beta" {
				return tsB.URL
			}
			return tsA.URL
		},
		AuditFn: func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	// A cursor request bypasses the cache and reaches the (first) daemon.
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{"cursor": "bad"}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200 (in-band JSON-RPC error); body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rr.Body.String())
	}
	if resp.Error == nil || resp.Error.Code != jsonrpcInvalidParams {
		t.Fatalf("expected the -32602 client error forwarded unchanged; got %+v", resp.Error)
	}
	// The SECOND daemon must never have been contacted (no handshake).
	if mc := daemonMintCount(daemonB); mc != 0 {
		t.Errorf("beta daemon minted %d sessions; want 0 (a client error must NOT advance to other daemons)", mc)
	}
}

// Finding 3 — a SERVER error (-32603) from the FIRST daemon must NOT
// short-circuit: the loop falls through to the SECOND daemon, which answers
// successfully, and that result is returned to the client.
func TestSerenaRouter_ToolsList_ServerErrorFallsThroughToNextDaemon(t *testing.T) {
	// alpha: handshakes then answers tools/list with a -32603 SERVER error.
	daemonA := newFakeSerenaDaemon("alpha")
	daemonA.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"daemon internal error"}}`))
	}
	tsA := httptest.NewServer(daemonA.handler())
	t.Cleanup(tsA.Close)

	// beta: healthy, answers the real catalog.
	daemonB := newFakeSerenaDaemon("beta")
	daemonB.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"find_symbol"}]}}`))
	}
	tsB := httptest.NewServer(daemonB.handler())
	t.Cleanup(tsB.Close)

	wsA := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	wsB := &api.WorkspaceEntry{WorkspaceKey: "beta", WorkspacePath: "/proj/beta", Port: 9202}
	deps := &serenaRouterDeps{
		// alpha FIRST so it is the candidate that returns the server error.
		Resolver: &listerStubResolver{list: []*api.WorkspaceEntry{wsA, wsB}},
		Sessions: NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string {
			if ws.WorkspaceKey == "beta" {
				return tsB.URL
			}
			return tsA.URL
		},
		AuditFn: func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// beta's result is returned (the server error on alpha did NOT short-circuit).
	assertToolsListNames(t, rr.Body.Bytes(), []string{"find_symbol"})
	// Both daemons were handshaked: alpha (server error) then beta (success).
	if mc := daemonMintCount(daemonA); mc != 1 {
		t.Errorf("alpha daemon minted %d sessions; want 1 (it was the first candidate)", mc)
	}
	if mc := daemonMintCount(daemonB); mc != 1 {
		t.Errorf("beta daemon minted %d sessions; want 1 (the server error on alpha must fall through to beta)", mc)
	}
}

// Finding 3 — when EVERY daemon fails with a SERVER error, the router forwards
// the LAST server-error envelope to the client (preserving the signal) rather
// than masking it as the generic transport "no daemon answered".
func TestSerenaRouter_ToolsList_AllServerErrorsForwardsLastServerError(t *testing.T) {
	daemonA := newFakeSerenaDaemon("alpha")
	daemonA.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"alpha internal error"}}`))
	}
	tsA := httptest.NewServer(daemonA.handler())
	t.Cleanup(tsA.Close)

	daemonB := newFakeSerenaDaemon("beta")
	daemonB.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// A server-reserved-range code (-32050) is also a SERVER error.
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32050,"message":"beta server error"}}`))
	}
	tsB := httptest.NewServer(daemonB.handler())
	t.Cleanup(tsB.Close)

	wsA := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	wsB := &api.WorkspaceEntry{WorkspaceKey: "beta", WorkspacePath: "/proj/beta", Port: 9202}
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{list: []*api.WorkspaceEntry{wsA, wsB}},
		Sessions: NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string {
			if ws.WorkspaceKey == "beta" {
				return tsB.URL
			}
			return tsA.URL
		},
		AuditFn: func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200; body=%s", rr.Code, rr.Body.String())
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
		t.Errorf("result present when all daemons returned server errors; want error only")
	}
	if resp.Error == nil {
		t.Fatalf("expected a forwarded server error; raw=%s", rr.Body.String())
	}
	// The LAST candidate's server error (beta's -32050) is forwarded, NOT the
	// router's generic -32603 "no daemon answered".
	if resp.Error.Code != -32050 {
		t.Errorf("forwarded error code = %d, want -32050 (the LAST server error, not the generic -32603)", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "beta server error") {
		t.Errorf("forwarded error message = %q, want beta's verbatim message", resp.Error.Message)
	}
}

// ---------------------------------------------------------------------
// Finding 4 — a client DELETE that arrives DURING a slow upstream handshake
// terminates the router session; the in-flight tool-call must (a) NOT store a
// daemon binding for the terminated id, (b) NOT re-bind the sticky session,
// (c) best-effort-DELETE the just-minted daemon session upstream, (d) abort
// with -32600 "session terminated", and (e) a subsequent PATHLESS call with
// that id must NOT be routed.
//
// The window is engineered deterministically: the daemon's initialize handler
// blocks on a channel until the test fires the DELETE (so the DELETE has
// definitively removed the router session by the time the handshake completes
// and resolveDaemonSession's liveness recheck runs), regardless of machine
// speed.
// ---------------------------------------------------------------------
func TestSerenaRouter_ToolCall_DeleteDuringSlowHandshake_NoBindAbortsTerminated(t *testing.T) {
	releaseHandshake := make(chan struct{})
	t.Cleanup(func() {
		// Unblock any lingering handshake on test exit (idempotent close guard).
		select {
		case <-releaseHandshake:
		default:
			close(releaseHandshake)
		}
	})
	var handshakeEntered sync.WaitGroup
	handshakeEntered.Add(1)
	var once sync.Once

	daemon := newFakeSerenaDaemon("alpha")
	base := daemon.handler()
	// Wrap initialize so the FIRST handshake blocks until the DELETE has run.
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// Peek the method without consuming the body the base handler needs.
			// A cheap header-free check: only the handshake initialize is slow;
			// detect it by buffering+restoring is overkill — instead gate on the
			// FIRST POST, which (for this test's single tool-call) is the
			// handshake initialize. notifications/initialized is the 2nd POST and
			// the tool POST never fires (the handshake aborts).
			once.Do(func() {
				handshakeEntered.Done()
				<-releaseHandshake // block the handshake until the DELETE has run
			})
		}
		base(w, r)
	})
	ts := httptest.NewServer(wrapped)
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	sessions := NewInMemorySessionRouter()
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}}, list: []*api.WorkspaceEntry{ws}},
		Sessions:      sessions,
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	// A router-minted session is required for the recheck to install.
	const negotiated = "2025-06-18"
	sid := mintRouterSession(t, s, negotiated)

	// Fire the path-bearing tool-call in the background; it blocks inside the
	// daemon handshake.
	tcDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		tcDone <- postSerena(t, s,
			buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}),
			map[string]string{"Mcp-Session-Id": sid, "MCP-Protocol-Version": negotiated})
	}()

	// Wait until the handshake is actually in flight, then DELETE the session.
	handshakeEntered.Wait()
	if rr := deleteSerena(t, s, map[string]string{"Mcp-Session-Id": sid, "MCP-Protocol-Version": negotiated}); rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	// The router session is now terminated.
	if s.serenaRouterSessions.known(sid) {
		t.Fatalf("router session %q still known after DELETE; precondition for the race not met", sid)
	}

	// Release the handshake; the in-flight tool-call's recheck now sees the dead
	// session and aborts.
	close(releaseHandshake)

	rr := <-tcDone
	// (d) abort -32600 "session terminated" at HTTP 400.
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("tool-call status = %d, want 400 (session terminated); body=%s", rr.Code, rr.Body.String())
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
		t.Errorf("result present on a terminated-session tool-call; want -32600 error only")
	}
	if resp.Error == nil || resp.Error.Code != jsonrpcInvalidRequest {
		t.Fatalf("error = %+v, want -32600 session terminated", resp.Error)
	}
	if resp.Error.Message != "session terminated" {
		t.Errorf("error message = %q, want \"session terminated\"", resp.Error.Message)
	}

	// (a) NO daemon binding was stored for the terminated session.
	if _, _, _, ok := s.serenaDaemonSessions.bindingFor(sid); ok {
		t.Errorf("a daemon binding was stored for a session DELETEd mid-handshake; Finding 4 requires the recheck to skip the store")
	}
	// (b) NO sticky binding for the terminated session.
	if got := sessions.LookupSession(sid); got != nil {
		t.Errorf("a sticky binding %+v was created for a session DELETEd mid-handshake; want none", got)
	}
	// (c) the just-minted daemon session was best-effort-DELETEd upstream.
	waitForDaemonDeleteHits(t, daemon, 1)
	daemon.mu.Lock()
	gotDelSID := daemon.lastDeleteSession
	releasedMinted := gotDelSID != "" && daemon.issued[gotDelSID]
	daemon.mu.Unlock()
	if !releasedMinted {
		t.Errorf("teardown DELETE Mcp-Session-Id = %q is not a session this daemon minted; Finding 4 must release the just-established daemon session", gotDelSID)
	}

	// (e) a subsequent PATHLESS call with the terminated id is NOT routed
	// (the sticky binding never existed -> 503 missing_session, not a forward).
	rrPathless := postSerena(t, s, buildToolCallBody(t, "list_memories", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rrPathless.Code != http.StatusServiceUnavailable {
		t.Errorf("pathless call with the terminated id status = %d, want 503 (NOT routed as a legacy sticky session)", rrPathless.Code)
	}
}

// Finding 4 (negative) — a normal tool-call whose router session stays live
// THROUGH the handshake stores the daemon binding and binds the sticky session
// as before (the recheck is a no-op when the session is not terminated).
func TestSerenaRouter_ToolCall_LiveSessionThroughHandshakeStillBinds(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	sessions := NewInMemorySessionRouter()
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions:      sessions,
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-06-18")
	rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("tool-call status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// Daemon binding stored AND sticky binding present (recheck was a no-op).
	if _, _, _, ok := s.serenaDaemonSessions.bindingFor(sid); !ok {
		t.Errorf("daemon binding missing for a live session; the recheck must not skip the store when the session is live")
	}
	if got := sessions.LookupSession(sid); got == nil || got.WorkspaceKey != "alpha" {
		t.Errorf("sticky binding = %+v, want alpha (the recheck must not skip the bind when the session is live)", got)
	}
}

// ---------------------------------------------------------------------
// Finding 4 (second site — the post-response sticky BindSession) — a client
// DELETE that arrives DURING the slow tool-call FORWARD (AFTER the handshake +
// daemon-binding store already succeeded) terminates the router session; the
// in-flight tool-call must NOT re-bind the sticky session for the terminated
// id at the post-response BindSession site (which would let a later pathless
// call route as a legacy sticky session). This exercises the SECOND
// store-after-slow-work site (the handler's sticky bind), distinct from the
// resolveDaemonSession store site the slow-handshake test above covers.
//
// Window engineered deterministically: the daemon's TOOL handler blocks until
// the test fires the DELETE (so the handshake/store has definitively completed
// and the DELETE has definitively run by the time the forward returns and the
// BindSession recheck fires).
// ---------------------------------------------------------------------
func TestSerenaRouter_ToolCall_DeleteDuringSlowForward_NoStickyRebind(t *testing.T) {
	releaseTool := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseTool:
		default:
			close(releaseTool)
		}
	})
	var toolEntered sync.WaitGroup
	toolEntered.Add(1)
	var once sync.Once

	daemon := newFakeSerenaDaemon("alpha")
	// The tool handler (reached only AFTER the session-gated handshake) blocks
	// until the DELETE has run, then answers 200.
	daemon.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		once.Do(func() {
			toolEntered.Done()
			<-releaseTool
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	sessions := NewInMemorySessionRouter()
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}}, list: []*api.WorkspaceEntry{ws}},
		Sessions:      sessions,
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	const negotiated = "2025-06-18"
	sid := mintRouterSession(t, s, negotiated)

	// Fire the path-bearing tool-call; it completes the handshake (storing the
	// daemon binding), then blocks inside the daemon's tool handler.
	tcDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		tcDone <- postSerena(t, s,
			buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}),
			map[string]string{"Mcp-Session-Id": sid, "MCP-Protocol-Version": negotiated})
	}()

	// Wait until the tool POST is in flight (handshake + store already done).
	toolEntered.Wait()
	// Precondition: the daemon binding WAS stored (the store-site recheck passed
	// because the session was live during the handshake).
	if _, _, _, ok := s.serenaDaemonSessions.bindingFor(sid); !ok {
		t.Fatalf("precondition: daemon binding should exist after the handshake (before the DELETE)")
	}

	// DELETE the session mid-forward.
	if rr := deleteSerena(t, s, map[string]string{"Mcp-Session-Id": sid, "MCP-Protocol-Version": negotiated}); rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if s.serenaRouterSessions.known(sid) {
		t.Fatalf("router session %q still known after DELETE; precondition for the race not met", sid)
	}

	// Release the tool forward; it returns 200 and reaches the BindSession site.
	close(releaseTool)
	rr := <-tcDone
	// The forwarded tool response itself still completes (200) — the DELETE does
	// not retroactively fail the in-flight response.
	if rr.Code != http.StatusOK {
		t.Fatalf("tool-call status = %d, want 200 (the in-flight response completes); body=%s", rr.Code, rr.Body.String())
	}

	// The load-bearing assertion: the sticky session was NOT re-bound for the
	// terminated id (Finding 4 second site). A pre-fix unconditional BindSession
	// would have re-created it.
	if got := sessions.LookupSession(sid); got != nil {
		t.Errorf("sticky binding %+v was re-created for a session DELETEd mid-forward; Finding 4 requires the post-response BindSession recheck to skip it", got)
	}
	// And the daemon binding is gone (the DELETE tore it down; the recheck does
	// not re-create it).
	if _, _, _, ok := s.serenaDaemonSessions.bindingFor(sid); ok {
		t.Errorf("daemon binding survived the mid-forward DELETE; want torn down")
	}
	// A subsequent pathless call with the terminated id is NOT routed.
	rrPathless := postSerena(t, s, buildToolCallBody(t, "list_memories", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rrPathless.Code != http.StatusServiceUnavailable {
		t.Errorf("pathless call with the terminated id status = %d, want 503 (NOT routed as a legacy sticky session)", rrPathless.Code)
	}
}

// ---------------------------------------------------------------------
// Finding 2 — the partial-handshake cleanup (initialize succeeds, then
// notifications/initialized fails) DELETE honors a SHORT injected
// UpstreamTimeout rather than the fixed 5s serenaCleanupTimeout default. We
// inject a 200ms UpstreamTimeout and make the cleanup DELETE HANG; the
// handshake-failing tool-call must return well under the 5s a fixed default
// would impose (the cleanup is fire-and-forget but the test wraps it so a
// regression that ignored the configured timeout would keep the daemon's
// DELETE handler blocked beyond the budget).
// ---------------------------------------------------------------------
func TestSerenaRouter_Handshake_InitializedFailCleanupHonorsShortTimeout(t *testing.T) {
	releaseDelete := make(chan struct{})
	t.Cleanup(func() { close(releaseDelete) })

	// deleteObserved fires when the cleanup DELETE's context is cancelled by its
	// OWN short deadline (proving the budget was the short injected one).
	deleteCtxDone := make(chan struct{}, 1)

	daemon := newFakeSerenaDaemon("alpha")
	// initialize mints a session; notifications/initialized is rejected so the
	// handshake fails and triggers the Finding #3 partial-handshake cleanup.
	daemon.initializedStatus = http.StatusBadRequest
	base := daemon.handler()
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			// HANG the cleanup DELETE until released OR its own (short) context
			// deadline fires. Finding 2: with a 200ms UpstreamTimeout the context
			// deadline fires at ~200ms; a regression using the fixed 5s default
			// would only fire at ~5s.
			select {
			case <-releaseDelete:
			case <-r.Context().Done():
				select {
				case deleteCtxDone <- struct{}{}:
				default:
				}
			}
			return
		}
		base(w, r)
	})
	ts := httptest.NewServer(wrapped)
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		// Inject a plain client with NO transport ResponseHeaderTimeout so that
		// cleanupContext is the SOLE bound on the cleanup DELETE. (The default
		// serenaHTTPClient derives ResponseHeaderTimeout from UpstreamTimeout,
		// which would ITSELF abort the hung DELETE at ~200ms and mask whether
		// cleanupContext honored the configured timeout — making the test pass
		// even with the bug. With this plain client, only cleanupContext bounds
		// the DELETE, so a regression using cleanupContext(0)=5s is observable.)
		HTTPClient:      &http.Client{},
		UpstreamTimeout: 200 * time.Millisecond, // SHORT — the cleanup must honor this, not 5s
		AuditFn:         func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	// A path-bearing tool-call drives the lazy handshake; initialize succeeds,
	// notifications/initialized is rejected, the cleanup DELETE is issued (and
	// hangs in the wrapper). The handshake fails loud (502).
	start := time.Now()
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "sess-f2"})
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("tool-call status = %d, want 502 (handshake fails on initialized rejection); body=%s", rr.Code, rr.Body.String())
	}

	// The cleanup DELETE's own context deadline must fire at ~200ms (the short
	// injected UpstreamTimeout), NOT the 5s serenaCleanupTimeout default. Bound
	// the wait at 2s: a regression ignoring the configured timeout would make the
	// daemon's DELETE handler block ~5s, so deleteCtxDone would NOT arrive within
	// 2s and this fails.
	select {
	case <-deleteCtxDone:
		// good — the cleanup honored the short budget.
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("cleanup DELETE context fired after %v; want ~200ms (the short injected UpstreamTimeout)", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("cleanup DELETE context did NOT fire within 2s; Finding 2 regression — the cleanup ignored the 200ms UpstreamTimeout and used the fixed 5s default")
	}
}

// Finding 2 (unit) — bestEffortDeleteDaemonSession routes through
// cleanupContext(upstreamTimeout), so the cleanup budget is the configured
// short timeout (capped by serenaCleanupTimeout). This is the math
// TestCleanupContext_BoundAndDetached pins; here we assert the call path threads
// the timeout by issuing a cleanup against a hung daemon with a short timeout
// and confirming it returns within the budget rather than the 5s default.
func TestBestEffortDeleteDaemonSession_HonorsShortTimeout(t *testing.T) {
	releaseDelete := make(chan struct{})
	t.Cleanup(func() { close(releaseDelete) })

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hang the DELETE until its own context deadline fires (or release).
		select {
		case <-releaseDelete:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(ts.Close)

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		// A short 150ms timeout: the cleanup must abort the hung DELETE at ~150ms,
		// not the 5s serenaCleanupTimeout default.
		bestEffortDeleteDaemonSession(ts.Client(), ts.URL, "daemon-sid", "2025-11-25", 150*time.Millisecond)
		done <- time.Since(start)
	}()
	select {
	case elapsed := <-done:
		if elapsed > 2*time.Second {
			t.Errorf("bestEffortDeleteDaemonSession returned after %v; want ~150ms (honors the short timeout, not the 5s default)", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("bestEffortDeleteDaemonSession did not return within 3s; Finding 2 regression — it ignored the short timeout")
	}
}
