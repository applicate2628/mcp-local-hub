// internal/gui/serena_router_findings_test.go
//
// Code-review-finding tests for the /serena/mcp router (deduplicated to 8):
//
//   A — DELETE revokes local bindings BEFORE the upstream forward.
//   B — DELETE tears down the upstream session even when the sticky
//       binding is missing (resolve-by-key).
//   C — tools/list DELETEs the one-shot upstream session after the proxy.
//   D — tools/list requires a minted/known router session.
//   E — tools/list cache keyed by negotiated protocol version.
//   F — lifecycle dispatch validates jsonrpc == "2.0".
//   G — tool-call persists + enforces the session's negotiated version.
//   H — Allow: POST, DELETE + notifications/cancelled forwarding.
//
// The shared routerSessionStore (serena_router_session.go) backs D + E + G;
// initialize populates it, DELETE clears it. These tests exercise the WHOLE
// set together via the real handler over the test seam.
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
// Finding A — a concurrent POST that arrives AFTER the local revocation
// (but while a slow upstream DELETE is still in flight) must NOT pass
// lookup and run a tool call. We engineer the window deterministically:
// the daemon's DELETE handler blocks on a channel the test controls, so
// the local revocation has definitively run by the time the racing POST
// is issued, regardless of machine speed.
// ---------------------------------------------------------------------
func TestSerenaRouter_Delete_RevokesLocalBindingsBeforeUpstreamForward(t *testing.T) {
	release := make(chan struct{})
	var deleteEntered sync.WaitGroup
	deleteEntered.Add(1)
	var once sync.Once

	daemon := newFakeSerenaDaemon("alpha")
	// Override DELETE so it blocks until the test releases it — this holds
	// the upstream forward open across the racing POST below.
	baseHandler := daemon.handler()
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			once.Do(func() { deleteEntered.Done() })
			<-release // block the upstream DELETE until released
		}
		baseHandler(w, r)
	})
	ts := httptest.NewServer(wrapped)
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	sessions := NewInMemorySessionRouter()
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions:      sessions,
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	const clientSID = "sess-race"
	// Establish the daemon session + sticky binding via a tool call.
	if rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{"Mcp-Session-Id": clientSID}); rr.Code != http.StatusOK {
		t.Fatalf("setup tool call status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if _, _, ok := s.serenaDaemonSessions.bindingFor(clientSID); !ok {
		t.Fatalf("precondition: no daemon binding after tool call")
	}

	// Fire the DELETE in the background; it will block inside the daemon's
	// DELETE handler (after the router has already revoked locally).
	delDone := make(chan int, 1)
	go func() {
		rr := deleteSerena(t, s, map[string]string{"Mcp-Session-Id": clientSID})
		delDone <- rr.Code
	}()

	// Wait until the upstream DELETE is actually in flight. At this point
	// the router MUST have already revoked every local binding (Finding A).
	deleteEntered.Wait()

	// All three local bindings must already be gone — a racing lookup loses.
	if _, _, ok := s.serenaDaemonSessions.bindingFor(clientSID); ok {
		t.Errorf("serenaDaemonSessions binding still present during in-flight DELETE; Finding A requires revoke-first")
	}
	if got := sessions.LookupSession(clientSID); got != nil {
		t.Errorf("sticky binding still present during in-flight DELETE = %+v; want revoked first", got)
	}
	if s.serenaRouterSessions.known(clientSID) {
		t.Errorf("routerSessionStore still knows %q during in-flight DELETE; want revoked first", clientSID)
	}

	// A racing no-path tool call on the same session now fails lookup (the
	// sticky binding is gone) -> 503 missing_session, NOT a forwarded call.
	rrRace := postSerena(t, s, buildToolCallBody(t, "list_memories", map[string]any{}), map[string]string{"Mcp-Session-Id": clientSID})
	if rrRace.Code != http.StatusServiceUnavailable {
		t.Errorf("racing post-revocation tool call status = %d, want 503 (session already revoked)", rrRace.Code)
	}

	close(release) // let the upstream DELETE complete
	if code := <-delDone; code != http.StatusNoContent {
		t.Errorf("DELETE status = %d, want 204", code)
	}
}

// ---------------------------------------------------------------------
// Finding B — when the upstream handshake completed but the tool POST
// failed before sticky BindSession ran, serenaDaemonSessions holds a live
// daemon session while deps.Sessions is nil. DELETE must STILL fire the
// upstream teardown, resolving the workspace by the daemon binding's
// workspaceKey via the resolver (not the absent sticky lookup).
// ---------------------------------------------------------------------
func TestSerenaRouter_Delete_TearsDownUpstreamWhenStickyBindingMissing(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	sessions := NewInMemorySessionRouter()
	// listerStubResolver so the by-key fallback (ListWorkspaces) can resolve
	// the workspace even though the sticky binding is absent.
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}}, list: []*api.WorkspaceEntry{ws}},
		Sessions:      sessions,
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
	}
	s := newSerenaTestServer(t, deps)

	const clientSID = "sess-leak"
	// Seed ONLY the daemon binding (the leak state): a handshake completed
	// and recorded the daemon session, but the sticky BindSession never ran.
	s.serenaDaemonSessions.store(clientSID, ws.WorkspaceKey, "alpha-seeded-daemon-session")
	if got := sessions.LookupSession(clientSID); got != nil {
		t.Fatalf("precondition: sticky binding must be ABSENT for the leak case; got %+v", got)
	}

	delRR := deleteSerena(t, s, map[string]string{"Mcp-Session-Id": clientSID})
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204; body=%s", delRR.Code, delRR.Body.String())
	}
	// The upstream DELETE MUST have fired (resolved by key), carrying the
	// daemon-issued session id — closing the leak.
	if got := daemonDeleteHits(daemon); got != 1 {
		t.Errorf("daemon DELETE hits = %d, want 1 (upstream teardown must fire via resolve-by-key even without sticky binding)", got)
	}
	daemon.mu.Lock()
	gotSID := daemon.lastDeleteSession
	daemon.mu.Unlock()
	if gotSID != "alpha-seeded-daemon-session" {
		t.Errorf("upstream DELETE Mcp-Session-Id = %q, want the seeded daemon id", gotSID)
	}
	// And the daemon binding is cleared.
	if _, _, ok := s.serenaDaemonSessions.bindingFor(clientSID); ok {
		t.Errorf("daemon binding survived DELETE; want unbound")
	}
}

// ---------------------------------------------------------------------
// Finding C — the one-shot tools/list upstream session is DELETEd after
// the proxy, even when the tools/list POST itself fails (post-handshake
// error path). Covered for the success path in
// TestSerenaRouter_ToolsList_FetchAndCacheFromLiveDaemon; here we cover
// the error path: a daemon that handshakes then 500s on tools/list must
// still get its one-shot session torn down.
// ---------------------------------------------------------------------
func TestSerenaRouter_ToolsList_OneShotSessionTornDownEvenOnFetchError(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	daemon.tool = func(w http.ResponseWriter, r *http.Request, body []byte) {
		// Handshake already succeeded; fail the tools/list POST itself.
		http.Error(w, "tools/list upstream failure", http.StatusInternalServerError)
	}
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	// The fetch failed -> the router surfaces an internal JSON-RPC error.
	var resp struct {
		Error *jsonrpcError `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rr.Body.String())
	}
	if resp.Error == nil {
		t.Fatalf("expected a JSON-RPC error when the tools/list fetch fails; raw=%s", rr.Body.String())
	}
	// Even though tools/list failed, the one-shot upstream session that the
	// handshake minted MUST be torn down (Finding C — all post-handshake
	// paths release the session).
	if got := daemonDeleteHits(daemon); got != 1 {
		t.Errorf("daemon DELETE hits = %d, want 1 (one-shot session must be released on the fetch-error path)", got)
	}
}

// ---------------------------------------------------------------------
// Finding D — tools/list requires a session minted by a prior initialize
// at this router. A full initialize(mint) -> tools/list(with the minted
// id) succeeds; a missing or unknown id is rejected -32600 at HTTP 400 and
// reaches NO daemon.
// ---------------------------------------------------------------------
func TestSerenaRouter_ToolsList_RequiresMintedRouterSession(t *testing.T) {
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
	toolHits := func() int { daemon.mu.Lock(); defer daemon.mu.Unlock(); return daemon.toolHits }

	body := buildLifecycleBody(t, "tools/list", map[string]any{})

	// Sub-case A: no Mcp-Session-Id header -> -32600 "session-id required"
	// at HTTP 400, no daemon hit.
	rrNo := postSerena(t, s, body, nil)
	assertToolsListSessionRejected(t, rrNo, "session-id required")
	if got := toolHits(); got != 0 {
		t.Fatalf("daemon tool hits after no-session tools/list = %d, want 0", got)
	}

	// Sub-case B: a random id this router never minted -> -32600
	// "unknown session" at HTTP 400, no daemon hit.
	rrUnknown := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "never-minted-here"})
	assertToolsListSessionRejected(t, rrUnknown, "unknown session")
	if got := toolHits(); got != 0 {
		t.Fatalf("daemon tool hits after unknown-session tools/list = %d, want 0", got)
	}

	// Sub-case C: full initialize(mint) -> tools/list(with the minted id)
	// succeeds and reaches the daemon.
	sid := mintRouterSession(t, s, "2025-11-25")
	rrOK := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sid})
	if rrOK.Code != http.StatusOK {
		t.Fatalf("minted-session tools/list status = %d, want 200; body=%s", rrOK.Code, rrOK.Body.String())
	}
	assertToolsListNames(t, rrOK.Body.Bytes(), []string{"find_symbol"})
	if got := toolHits(); got != 1 {
		t.Fatalf("daemon tool hits after minted-session tools/list = %d, want 1", got)
	}
}

// assertToolsListSessionRejected asserts a tools/list session-gate rejection
// (Finding D): HTTP 400 + JSON-RPC -32600 whose message contains want.
func assertToolsListSessionRejected(t *testing.T, rr *httptest.ResponseRecorder, want string) {
	t.Helper()
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
		t.Errorf("result present on a session-rejected tools/list; want error only")
	}
	if resp.Error == nil || resp.Error.Code != jsonrpcInvalidRequest {
		t.Fatalf("expected -32600 session rejection; got %+v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, want) {
		t.Errorf("error message %q should contain %q", resp.Error.Message, want)
	}
}

// ---------------------------------------------------------------------
// Finding E — the tools/list cache is keyed by the session's negotiated
// protocol version: two clients that initialized with DIFFERENT versions
// each trigger a separate daemon fetch (one cannot be served the other's
// cached payload). The daemon answers a version-tagged tool name so the
// test proves which payload each client received.
// ---------------------------------------------------------------------
func TestSerenaRouter_ToolsList_CacheKeyedByNegotiatedVersion(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	// Echo the negotiated protocol version back in the tool name so the
	// test can prove the cache did not cross versions.
	daemon.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		ver := r.Header.Get("MCP-Protocol-Version")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"tool-` + ver + `"}]}}`))
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

	// Client 1 initialized 2025-11-25 -> fetches + caches under that version.
	sidNew := mintRouterSession(t, s, "2025-11-25")
	rr1 := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sidNew})
	assertToolsListNames(t, rr1.Body.Bytes(), []string{"tool-2025-11-25"})
	if got := toolHits(); got != 1 {
		t.Fatalf("daemon tool hits after client1 = %d, want 1", got)
	}

	// Client 2 initialized 2025-06-18 -> MUST trigger a SEPARATE fetch (not
	// be served client1's 2025-11-25 payload), proving version-keying.
	sidOld := mintRouterSession(t, s, "2025-06-18")
	rr2 := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sidOld})
	assertToolsListNames(t, rr2.Body.Bytes(), []string{"tool-2025-06-18"})
	if got := toolHits(); got != 2 {
		t.Fatalf("daemon tool hits after client2 = %d, want 2 (different version must not hit client1's cache entry)", got)
	}

	// Re-issuing client1's version hits its cache (no new fetch).
	rr3 := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sidNew})
	assertToolsListNames(t, rr3.Body.Bytes(), []string{"tool-2025-11-25"})
	if got := toolHits(); got != 2 {
		t.Fatalf("daemon tool hits after client1 re-issue = %d, want 2 (cache hit for the same version)", got)
	}
}

// ---------------------------------------------------------------------
// Finding F — the lifecycle dispatch validates jsonrpc == "2.0". A body
// with jsonrpc != "2.0" (or omitting it) is rejected -32600 at HTTP 400
// and mints no session, for every lifecycle method. The reconcile probe
// (jsonrpc:"2.0") still passes.
// ---------------------------------------------------------------------
func TestSerenaRouter_Lifecycle_RejectsBadJSONRPCVersion(t *testing.T) {
	deps := &serenaRouterDeps{Resolver: &listerStubResolver{}, Sessions: NewInMemorySessionRouter()}
	s := newSerenaTestServer(t, deps)

	cases := []struct {
		name string
		body string
	}{
		{"initialize jsonrpc 1.0", `{"jsonrpc":"1.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`},
		{"initialize jsonrpc absent", `{"id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`},
		{"ping jsonrpc 1.0", `{"jsonrpc":"1.0","id":1,"method":"ping"}`},
		{"tools/list jsonrpc absent", `{"id":1,"method":"tools/list"}`},
		{"notifications/initialized jsonrpc 1.0", `{"jsonrpc":"1.0","method":"notifications/initialized"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := postSerena(t, s, []byte(tc.body), nil)
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
			if resp.Error == nil || resp.Error.Code != jsonrpcInvalidRequest {
				t.Fatalf("expected -32600; got %+v", resp.Error)
			}
			if !strings.Contains(resp.Error.Message, "jsonrpc must be") {
				t.Errorf("error message %q should name the jsonrpc requirement", resp.Error.Message)
			}
			// A rejected request mints no session.
			if sid := rr.Header().Get("Mcp-Session-Id"); sid != "" {
				t.Errorf("bad-jsonrpc request minted Mcp-Session-Id %q; want none", sid)
			}
		})
	}

	// Sanity: the reconcile probe body (jsonrpc:"2.0") still succeeds.
	const probeInitBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mcphub-reconcile-probe","version":"0"}}}`
	rrProbe := postSerena(t, s, []byte(probeInitBody), nil)
	if rrProbe.Code != http.StatusOK {
		t.Fatalf("reconcile probe status = %d, want 200 after Finding F; body=%s", rrProbe.Code, rrProbe.Body.String())
	}
}

// ---------------------------------------------------------------------
// Finding G — a tool-call header that CONFLICTS with the known session's
// negotiated version is rejected "protocol-version mismatch" (-32600 at
// HTTP 400). A matching header (or an omitted one) is accepted and the
// upstream handshake uses the SESSION's negotiated version.
// ---------------------------------------------------------------------
func TestSerenaRouter_ToolCall_EnforcesSessionNegotiatedVersion(t *testing.T) {
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

	// initialize at the router negotiating 2025-06-18.
	sid := mintRouterSession(t, s, "2025-06-18")
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})

	// Sub-case A: tool-call header CONFLICTS with the session version ->
	// rejected -32600 "protocol-version mismatch", no daemon handshake.
	rrConflict := postSerena(t, s, body, map[string]string{
		"Mcp-Session-Id":       sid,
		"MCP-Protocol-Version": "2025-11-25", // differs from the negotiated 2025-06-18
	})
	if rrConflict.Code != http.StatusBadRequest {
		t.Fatalf("conflicting-version tool call status = %d, want 400; body=%s", rrConflict.Code, rrConflict.Body.String())
	}
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rrConflict.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rrConflict.Body.String())
	}
	if resp.Error == nil || resp.Error.Code != jsonrpcInvalidRequest {
		t.Fatalf("expected -32600 mismatch; got %+v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "protocol-version mismatch") {
		t.Errorf("error message %q should be the protocol-version-mismatch wording", resp.Error.Message)
	}
	if mc := daemonMintCount(daemon); mc != 0 {
		t.Errorf("daemon minted %d sessions on a rejected mismatched tool call; want 0 (rejection precedes handshake)", mc)
	}

	// Sub-case B: tool-call header MATCHES the session version -> accepted,
	// and the upstream handshake uses the negotiated 2025-06-18.
	rrMatch := postSerena(t, s, body, map[string]string{
		"Mcp-Session-Id":       sid,
		"MCP-Protocol-Version": "2025-06-18",
	})
	if rrMatch.Code != http.StatusOK {
		t.Fatalf("matching-version tool call status = %d, want 200; body=%s", rrMatch.Code, rrMatch.Body.String())
	}
	daemon.mu.Lock()
	gotInitPV := daemon.lastInitProtocolVersion
	gotToolPV := daemon.lastToolHeaders.Get("MCP-Protocol-Version")
	daemon.mu.Unlock()
	if gotInitPV != "2025-06-18" {
		t.Errorf("upstream handshake protocolVersion = %q, want the session's 2025-06-18", gotInitPV)
	}
	if gotToolPV != "2025-06-18" {
		t.Errorf("upstream tool-call MCP-Protocol-Version = %q, want 2025-06-18", gotToolPV)
	}
}

// Finding G — a tool call that omits MCP-Protocol-Version on a KNOWN
// session uses the session's negotiated version for the upstream
// handshake (not the router default), proving the session is the source
// of truth even when the request omits the header.
func TestSerenaRouter_ToolCall_OmittedHeaderUsesSessionVersion(t *testing.T) {
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

	const negotiated = "2025-06-18"
	if negotiated == defaultProtocolVersion {
		t.Fatalf("precondition: negotiated must differ from the router default %q", defaultProtocolVersion)
	}
	sid := mintRouterSession(t, s, negotiated)

	// Tool call with NO MCP-Protocol-Version header.
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("tool call status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	daemon.mu.Lock()
	gotInitPV := daemon.lastInitProtocolVersion
	daemon.mu.Unlock()
	if gotInitPV != negotiated {
		t.Errorf("upstream handshake protocolVersion = %q, want the session's %q (not the router default)", gotInitPV, negotiated)
	}
}

// Finding G — a tool call on a session this router NEVER minted keeps
// today's behavior: the raw request header drives the handshake (no
// mismatch rejection), so a legacy direct caller is not regressed.
func TestSerenaRouter_ToolCall_NoRouterSessionUsesRequestHeader(t *testing.T) {
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

	// No initialize at the router; a raw client id + a version header. The
	// router has no session record, so it must NOT reject and must use the
	// request header verbatim (today's behavior).
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, map[string]string{
		"Mcp-Session-Id":       "never-minted-here",
		"MCP-Protocol-Version": "2025-06-18",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("no-router-session tool call status = %d, want 200 (no regression); body=%s", rr.Code, rr.Body.String())
	}
	daemon.mu.Lock()
	gotInitPV := daemon.lastInitProtocolVersion
	daemon.mu.Unlock()
	if gotInitPV != "2025-06-18" {
		t.Errorf("upstream handshake protocolVersion = %q, want the request header 2025-06-18 (no router session)", gotInitPV)
	}
}

// ---------------------------------------------------------------------
// Finding H — notifications/cancelled is FORWARDED to the bound workspace
// daemon (carrying the daemon-issued session id + the verbatim body) so an
// in-flight tool call is actually cancelled. With no bound daemon session
// it keeps the local 202; an id-bearing notifications/cancelled stays a
// -32600 malformed request (covered by
// TestSerenaRouter_NotificationWithID_ReturnsInvalidRequest).
// ---------------------------------------------------------------------
func TestSerenaRouter_NotificationCancelled_ForwardsToBoundDaemon(t *testing.T) {
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

	const clientSID = "sess-cancel"
	// A tool call establishes the daemon session binding (the in-flight
	// state the cancel targets).
	if rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{"Mcp-Session-Id": clientSID}); rr.Code != http.StatusOK {
		t.Fatalf("setup tool call status = %d; body=%s", rr.Code, rr.Body.String())
	}
	wantDaemonSID, ok, _ := bindingDaemonSession(s, clientSID)
	if !ok {
		t.Fatalf("precondition: no daemon binding after tool call")
	}

	// notifications/cancelled (id-less) on that client session.
	cancelBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/cancelled",
		"params":  map[string]any{"requestId": 7},
	})
	rr := postSerena(t, s, cancelBody, map[string]string{"Mcp-Session-Id": clientSID})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("notifications/cancelled status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	// Forwarded to the daemon, carrying the DAEMON-issued session id + the
	// verbatim cancel body.
	if got := daemonCancelHits(daemon); got != 1 {
		t.Fatalf("daemon cancel hits = %d, want 1 (router must forward the cancellation)", got)
	}
	daemon.mu.Lock()
	gotSID := daemon.lastCancelSession
	gotBody := daemon.lastCancelBody
	daemon.mu.Unlock()
	if gotSID != wantDaemonSID {
		t.Errorf("forwarded cancel Mcp-Session-Id = %q, want the daemon-issued id %q (not the client id %q)", gotSID, wantDaemonSID, clientSID)
	}
	if !strings.Contains(string(gotBody), `"requestId"`) {
		t.Errorf("forwarded cancel body did not carry params.requestId; got %s", string(gotBody))
	}
}

// Finding H — a notifications/cancelled with NO bound daemon session keeps
// the local 202 and forwards nothing (nothing is in flight).
func TestSerenaRouter_NotificationCancelled_NoBindingKeeps202(t *testing.T) {
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

	// No prior tool call -> no daemon binding for this session.
	cancelBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/cancelled",
		"params":  map[string]any{"requestId": 7},
	})
	rr := postSerena(t, s, cancelBody, map[string]string{"Mcp-Session-Id": "sess-no-binding"})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if got := daemonCancelHits(daemon); got != 0 {
		t.Errorf("daemon cancel hits = %d, want 0 (nothing in flight to cancel)", got)
	}
}

// ---------------------------------------------------------------------
// Shared store — DELETE clears the routerSessionStore entry (D + E + G
// interplay): after a DELETE, a subsequent tools/list with the SAME id is
// rejected as unknown (the session is no longer minted), and a tool call
// with that id falls back to the no-router-session branch.
// ---------------------------------------------------------------------
func TestSerenaRouter_Delete_ClearsRouterSessionStore(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	daemon.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
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

	sid := mintRouterSession(t, s, "2025-11-25")
	if !s.serenaRouterSessions.known(sid) {
		t.Fatalf("precondition: router session %q should be known after initialize", sid)
	}

	// DELETE the session.
	if rr := deleteSerena(t, s, map[string]string{"Mcp-Session-Id": sid}); rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	// The router-session entry is gone.
	if s.serenaRouterSessions.known(sid) {
		t.Errorf("router session %q still known after DELETE; want cleared", sid)
	}
	// A tools/list with the same id is now rejected as unknown (Finding D
	// gate sees no minted session).
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	assertToolsListSessionRejected(t, rr, "unknown session")
}

// ---------------------------------------------------------------------
// routerSessionStore unit tests: store/lookup, expire-on-read, unbind,
// cleanup, and concurrency (run under -race). Mirrors daemonSessionStore.
// ---------------------------------------------------------------------
func TestRouterSessionStore_StoreLookupUnbind(t *testing.T) {
	st := &routerSessionStore{}
	st.store("c1", "2025-06-18")

	if v, ok := st.negotiatedVersion("c1"); !ok || v != "2025-06-18" {
		t.Fatalf("negotiatedVersion(c1) = (%q, %v), want (2025-06-18, true)", v, ok)
	}
	if !st.known("c1") {
		t.Errorf("known(c1) = false, want true")
	}
	// Empty id -> miss.
	if _, ok := st.negotiatedVersion(""); ok {
		t.Errorf("negotiatedVersion(empty) = ok, want miss")
	}
	if st.known("absent") {
		t.Errorf("known(absent) = true, want false")
	}
	// Re-store replaces the version.
	st.store("c1", "2025-11-25")
	if v, _ := st.negotiatedVersion("c1"); v != "2025-11-25" {
		t.Errorf("negotiatedVersion(c1) after re-store = %q, want 2025-11-25", v)
	}
	// unbind drops it.
	st.unbind("c1")
	if st.known("c1") {
		t.Errorf("known(c1) after unbind = true, want false")
	}
}

func TestRouterSessionStore_ExpireOnRead(t *testing.T) {
	base := time.Now()
	clk := base
	st := &routerSessionStore{clock: func() time.Time { return clk }}
	st.store("c1", "2025-06-18")

	// Inside TTL -> hit (refreshes lastSeen).
	clk = base.Add(daemonSessionTTL - time.Minute)
	if _, ok := st.negotiatedVersion("c1"); !ok {
		t.Fatalf("lookup within TTL = miss, want hit")
	}
	// Past TTL from the refreshed lastSeen -> miss + deleted.
	clk = clk.Add(daemonSessionTTL + time.Minute)
	if _, ok := st.negotiatedVersion("c1"); ok {
		t.Fatalf("lookup past TTL = hit, want miss (expire-on-read)")
	}
	// Already deleted -> cleanup reports 0.
	if n := st.cleanup(clk, daemonSessionTTL); n != 0 {
		t.Errorf("cleanup dropped %d; want 0 (lookup already deleted the stale entry)", n)
	}
}

func TestRouterSessionStore_ConcurrentAccess(t *testing.T) {
	st := &routerSessionStore{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "c" + string(rune('0'+i%8))
			st.store(id, "2025-11-25")
			_, _ = st.negotiatedVersion(id)
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
