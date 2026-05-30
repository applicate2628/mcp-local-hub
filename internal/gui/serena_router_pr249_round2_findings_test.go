// internal/gui/serena_router_pr249_round2_findings_test.go
//
// Codex PR #249 round-2 review findings (serena /serena/mcp router). Two of
// these (#2 + #4) complete the EXPIRED/removed-router-session reanimation class
// the prior rounds fixed for some paths but not all:
//
//	Finding 1 (P2) — handleInitialize NEGOTIATES a well-formed-but-unsupported
//	            protocolVersion (responds 200 + defaultProtocolVersion) instead
//	            of rejecting it; the router fronts HOMOGENEOUS serena daemons so
//	            it can/should negotiate down, unlike the heterogeneous hub. The
//	            negotiate + malformed-boundary assertions live in
//	            serena_router_lifecycle_test.go (TestSerenaRouter_Initialize_
//	            UnsupportedProtocolVersionNegotiated / _MalformedVersionNotNegotiated).
//	Finding 2 (P2) — the routerSessionStore LRU eviction COORDINATES the
//	            downstream sticky + daemon unbind for the evicted client id (it
//	            no longer removes only the routerSessionStore entry). An evicted
//	            router session that had path-bearing-call bindings is fully torn
//	            down, so a later pathless call with the evicted id is NOT routed
//	            as legacy.
//	Finding 3 (P2) — proxyToolsListOnce preserves an upstream JSON-RPC error on
//	            a NON-2xx tools/list: a daemon answering HTTP 400 + a JSON-RPC
//	            -32602 is forwarded/classified like the 200 path (client error →
//	            no other daemons), while a 502 + no JSON body stays a transport
//	            failure (next daemon tried).
//	Finding 4 (P2) — the PATH-BEARING tool-call branch distinguishes an EXPIRED
//	            router session (terminate -32600 + coordinated unbind) from a
//	            TRUE-legacy caller (fresh handshake), the same distinction the
//	            pathless branch already makes — an expired router session can no
//	            longer continue simply by including a path argument.
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// ---------------------------------------------------------------------
// Finding 2 (P2) — an LRU eviction of a router session that holds sticky +
// daemon bindings ALSO unbinds those downstream bindings, so the evicted
// session is fully terminated and a later pathless call with the evicted id is
// NOT routed as legacy.
//
// The store cap (maxRouterSessions = 4096) is too large to fill with real
// initialize round-trips in a unit test. Instead we drive the eviction through
// the SAME code path the production caller uses — store() returning the evicted
// id — by directly invoking the handler's coordination on a forced eviction:
// we seed the store to exactly cap-1 via the raw store(), mint one real router
// session with bindings (so it occupies an LRU slot AND has sticky + daemon
// bindings), then mint ONE more real session via initialize, which fills the
// last slot. Finally a real initialize evicts the least-recently-seen entry.
// We arrange the victim to be the bound session and assert its downstream
// bindings are gone.
//
// To keep the victim deterministic without depending on map iteration order, we
// drive the store clock: the bound session is the OLDEST (lastSeen = base), the
// padding sessions are newer, and the bound session is never touched after, so
// it is the LRU back and evicts first once the cap is exceeded.
// ---------------------------------------------------------------------
func TestSerenaRouter_PR249R2_F2_EvictionCoordinatesDownstreamUnbind(t *testing.T) {
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

	// Drive the router-session clock so the bound session is the LRU back
	// (oldest lastSeen) and thus the eviction victim.
	base := time.Now()
	clk := base
	s.serenaRouterSessions.clock = func() time.Time { return clk }

	const negotiated = "2025-06-18"
	// Mint the VICTIM router session at base, then a path-bearing tool call so it
	// gets BOTH the sticky binding (post-response BindSession) and the daemon
	// binding. This is the session whose downstream bindings must be coordinated
	// away when it is later evicted.
	victim := mintRouterSession(t, s, negotiated)
	if rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{
		"Mcp-Session-Id":       victim,
		"MCP-Protocol-Version": negotiated,
	}); rr.Code != http.StatusOK {
		t.Fatalf("setup path tool call status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if got := sessions.LookupSession(victim); got == nil {
		t.Fatalf("precondition: victim sticky binding missing after the path tool call")
	}
	if _, ok, _ := bindingDaemonSession(s, victim); !ok {
		t.Fatalf("precondition: victim daemon binding missing after the path tool call")
	}

	// Pad the router-session store up to the cap with raw store() entries, each
	// NEWER than the victim (advancing the clock) so the victim stays the LRU
	// back. Padding via the raw store() (not real initialize) keeps the test fast
	// and is exactly the same map/LRU the eviction walks; only the COORDINATION
	// of the victim's downstream bindings is what we are exercising, and that runs
	// in handleInitialize regardless of how the other slots were filled.
	//
	// The victim already occupies 1 slot, so add maxRouterSessions-1 padding
	// entries to reach the cap. (len now == maxRouterSessions.)
	for i := 0; i < maxRouterSessions-1; i++ {
		clk = base.Add(time.Duration(i+1) * time.Millisecond)
		s.serenaRouterSessions.store(fmt.Sprintf("pad-%06d", i), negotiated)
	}
	if got := routerSessionCount(s); got != maxRouterSessions {
		t.Fatalf("store size after padding = %d, want exactly the cap %d", got, maxRouterSessions)
	}
	if !s.serenaRouterSessions.known(victim) {
		t.Fatalf("precondition: victim must still be in the store at cap (it is the LRU back, not yet evicted)")
	}

	// Now a REAL initialize mints a fresh session: the store is at cap, so it
	// evicts the LRU back (the victim) AND handleInitialize coordinates the
	// victim's downstream sticky + daemon unbind. Advance the clock so the new
	// session is the newest.
	clk = base.Add(time.Duration(maxRouterSessions+1) * time.Millisecond)
	fresh := mintRouterSession(t, s, negotiated)
	if fresh == victim {
		t.Fatalf("fresh session id collided with the victim id")
	}

	// The victim's router-session entry is gone (evicted).
	if s.serenaRouterSessions.known(victim) {
		t.Errorf("victim router session still known after eviction; want evicted")
	}
	// Finding 2: the coordinated unbind dropped BOTH downstream bindings.
	if got := sessions.LookupSession(victim); got != nil {
		t.Errorf("victim sticky binding %+v survived eviction; want unbound (Finding 2 coordinated unbind)", got)
	}
	if _, ok, _ := bindingDaemonSession(s, victim); ok {
		t.Errorf("victim daemon binding survived eviction; want unbound (Finding 2 coordinated unbind)")
	}

	// And the proof of the reanimation fix: a PATHLESS call with the evicted id is
	// NOT routed as legacy — with the sticky binding gone it falls to the
	// no-binding path (503 missing_session), NOT a forward to a daemon.
	hitsBefore := func() int { daemon.mu.Lock(); defer daemon.mu.Unlock(); return daemon.toolHits }()
	rr := postSerena(t, s, buildToolCallBody(t, "list_memories", map[string]any{}), map[string]string{"Mcp-Session-Id": victim})
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("pathless call with the evicted id status = %d, want 503 (not routed as legacy after coordinated unbind); body=%s", rr.Code, rr.Body.String())
	}
	if got := func() int { daemon.mu.Lock(); defer daemon.mu.Unlock(); return daemon.toolHits }(); got != hitsBefore {
		t.Errorf("pathless call with the evicted id reached the daemon (tool hits %d -> %d); want NOT routed", hitsBefore, got)
	}
}

// Finding 2 (store-level unit) — store() RETURNS the evicted client id when the
// cap forces an eviction, and "" otherwise (re-store of an existing id, or a
// fresh insert below the cap). This is the seam the handler uses to coordinate
// the downstream unbind; pin it directly so a regression that drops the return
// is caught at the store level too.
func TestRouterSessionStore_StoreReturnsEvictedID(t *testing.T) {
	base := time.Now()
	clk := base
	st := &routerSessionStore{clock: func() time.Time { return clk }}

	id := func(i int) string { return fmt.Sprintf("sess-%05d", i) }
	// Fill to cap; every insert is below/at the cap so none evicts -> "".
	for i := 0; i < maxRouterSessions; i++ {
		clk = base.Add(time.Duration(i) * time.Millisecond)
		if ev := st.store(id(i), "2025-06-18"); ev != "" {
			t.Fatalf("store(%q) at fill iteration %d evicted %q; want no eviction below the cap", id(i), i, ev)
		}
	}
	// Re-store an EXISTING id: promotes in place, consumes no slot -> "".
	clk = base.Add(time.Duration(maxRouterSessions) * time.Millisecond)
	if ev := st.store(id(maxRouterSessions-1), "2025-11-25"); ev != "" {
		t.Errorf("re-store of an existing id evicted %q; want \"\" (no slot consumed)", ev)
	}
	// A fresh insert at cap evicts the LRU back. id(0) is the oldest never-touched
	// entry, so it is the victim and its id is returned.
	clk = base.Add(time.Duration(maxRouterSessions+1) * time.Millisecond)
	ev := st.store("overflow", "2025-06-18")
	if ev != id(0) {
		t.Errorf("store at cap evicted %q; want the LRU-back id %q (returned for downstream coordination)", ev, id(0))
	}
	// Empty id is ignored and never evicts.
	if ev := st.store("", "2025-06-18"); ev != "" {
		t.Errorf("store(\"\") evicted %q; want \"\" (empty id ignored)", ev)
	}
}

// ---------------------------------------------------------------------
// Finding 3 (P2) — a daemon that rejects tools/list with HTTP 400 + a JSON-RPC
// -32602 (CLIENT error) is forwarded to the client UNCHANGED and other daemons
// are NOT tried (pre-fix the non-200 guard returned a transport error before
// reading the body, so it tried other daemons and collapsed to -32603).
// ---------------------------------------------------------------------
func TestSerenaRouter_PR249R2_F3_Non200ClientErrorForwardedNoSecondDaemon(t *testing.T) {
	// alpha: handshakes normally, then answers tools/list with HTTP 400 + a
	// JSON-RPC -32602 (the SAME status the router uses for its own -32602).
	daemonA := newFakeSerenaDaemon("alpha")
	daemonA.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest) // non-2xx WITH a JSON-RPC error body
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
		t.Fatalf("tools/list status = %d, want 200 (in-band JSON-RPC error forwarded); body=%s", rr.Code, rr.Body.String())
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
	// The daemon's -32602 is forwarded UNCHANGED — NOT collapsed to the router's
	// generic -32603 transport error.
	if resp.Error == nil || resp.Error.Code != jsonrpcInvalidParams {
		t.Fatalf("error = %+v, want the daemon's -32602 forwarded (not -32603)", resp.Error)
	}
	if resp.Error.Message != "invalid cursor" {
		t.Errorf("error.message = %q, want the daemon's verbatim 'invalid cursor'", resp.Error.Message)
	}
	// The SECOND daemon was never contacted (a client error short-circuits).
	if mc := daemonMintCount(daemonB); mc != 0 {
		t.Errorf("beta daemon minted %d sessions; want 0 (a non-200 CLIENT error must NOT advance to other daemons)", mc)
	}
}

// Finding 3 — a daemon that answers tools/list with HTTP 502 + NO JSON body is a
// genuine transport failure: the loop falls through to the next daemon (which
// answers the catalog). A non-2xx WITHOUT a parseable JSON-RPC error must stay a
// transport failure (the carefully-preserved case).
func TestSerenaRouter_PR249R2_F3_Non200NoJSONBodyIsTransportFailureNextDaemon(t *testing.T) {
	// alpha: handshakes normally, then answers tools/list with HTTP 502 + a
	// plain (non-JSON) body — a genuine transport/HTTP failure.
	daemonA := newFakeSerenaDaemon("alpha")
	daemonA.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("bad gateway, no JSON here"))
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
		// alpha FIRST so it is the candidate that returns the transport failure.
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
		t.Fatalf("tools/list status = %d, want 200 (beta answered after alpha's transport failure); body=%s", rr.Code, rr.Body.String())
	}
	// beta's result is returned (alpha's 502+no-body did NOT short-circuit).
	assertToolsListNames(t, rr.Body.Bytes(), []string{"find_symbol"})
	// Both daemons were handshaked: alpha (transport failure) then beta (success).
	if mc := daemonMintCount(daemonA); mc != 1 {
		t.Errorf("alpha daemon minted %d sessions; want 1 (it was the first candidate)", mc)
	}
	if mc := daemonMintCount(daemonB); mc != 1 {
		t.Errorf("beta daemon minted %d sessions; want 1 (alpha's 502+no-body must fall through to beta)", mc)
	}
}

// ---------------------------------------------------------------------
// Finding 4 (P2) — a PATH-BEARING tool call whose router session just exceeded
// the idle TTL is TERMINATED (-32600 "session terminated") + its sticky + daemon
// bindings unbound, NOT a fresh legacy handshake/re-bind. The path-bearing
// branch must make the SAME expired-vs-absent distinction the pathless branch
// already does; pre-fix it used peekNegotiatedVersion (known=false on expiry) and
// treated the stale id as legacy, so an expired router session could continue
// simply by including a path argument.
//
// The expiry window is engineered deterministically via the injectable
// router-session clock (advance past the idle TTL), not a real sleep.
// ---------------------------------------------------------------------
func TestSerenaRouter_PR249R2_F4_PathBearing_ExpiredRouterSessionTerminatedAndUnbound(t *testing.T) {
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

	base := time.Now()
	clk := base
	s.serenaRouterSessions.clock = func() time.Time { return clk }

	const negotiated = "2025-06-18"
	// Mint a router session at base, then a path-bearing tool call to establish
	// BOTH the sticky binding and the daemon binding.
	sid := mintRouterSession(t, s, negotiated)
	if rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{
		"Mcp-Session-Id":       sid,
		"MCP-Protocol-Version": negotiated,
	}); rr.Code != http.StatusOK {
		t.Fatalf("setup path tool call status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if got := sessions.LookupSession(sid); got == nil {
		t.Fatalf("precondition: sticky binding missing after the path tool call")
	}
	if _, ok, _ := bindingDaemonSession(s, sid); !ok {
		t.Fatalf("precondition: daemon binding missing after the path tool call")
	}
	mintsBefore := daemonMintCount(daemon)

	// Advance the router-session clock PAST the idle TTL so the next read sees the
	// router session as expired-on-read.
	clk = base.Add(daemonSessionTTL + time.Minute)

	// A PATH-BEARING tool call with the (now-idle-expired) router session id must
	// be TERMINATED -32600, NOT a fresh legacy handshake: the daemon must NOT be
	// re-handshaked, and the sticky + daemon bindings must be unbound.
	rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/y"}), map[string]string{
		"Mcp-Session-Id":       sid,
		"MCP-Protocol-Version": negotiated,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expired-router path-bearing call status = %d, want 400 (session terminated, NOT a legacy handshake); body=%s", rr.Code, rr.Body.String())
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
		t.Errorf("result present on an expired-router path-bearing call; want -32600 error only")
	}
	if resp.Error == nil || resp.Error.Code != jsonrpcInvalidRequest {
		t.Fatalf("error = %+v, want -32600 session terminated", resp.Error)
	}
	if resp.Error.Message != "session terminated" {
		t.Errorf("error message = %q, want \"session terminated\"", resp.Error.Message)
	}
	// The daemon was NOT re-handshaked (no fresh legacy session minted): the
	// expired router session did not reanimate via the path argument.
	if got := daemonMintCount(daemon); got != mintsBefore {
		t.Errorf("daemon mint count %d -> %d on the expired path-bearing call; want NO new handshake (terminated, not reanimated)", mintsBefore, got)
	}
	// The coordinated unbind dropped BOTH downstream bindings.
	if got := sessions.LookupSession(sid); got != nil {
		t.Errorf("sticky binding %+v survived the expired path-bearing call; want unbound", got)
	}
	if _, ok, _ := bindingDaemonSession(s, sid); ok {
		t.Errorf("daemon binding survived the expired path-bearing call; want unbound")
	}
	// The router-session entry itself is gone (expire-on-read deleted it).
	if s.serenaRouterSessions.known(sid) {
		t.Errorf("router session still known after the expired path-bearing call; want expired+deleted")
	}
}

// Finding 4 (legacy-compat preserved) — a TRUE-legacy path-bearing caller (one
// that never minted a router session here — no routerSessionStore entry) still
// handshakes normally and routes. This pins that the expired-vs-absent
// distinction on the path-bearing branch terminates ONLY expired router
// sessions, never a never-minted-here direct caller.
func TestSerenaRouter_PR249R2_F4_PathBearing_TrueLegacyStillHandshakes(t *testing.T) {
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

	// A TRUE-legacy caller: a session id this router NEVER minted (no router
	// session entry), carried on a PATH-bearing tool call. peekVersionState must
	// classify it routerSessionAbsent so the call handshakes + routes normally.
	const legacySID = "sess-true-legacy-path"
	if s.serenaRouterSessions.known(legacySID) {
		t.Fatalf("precondition: a true-legacy session must NOT have a router-session entry")
	}

	rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{"Mcp-Session-Id": legacySID})
	if rr.Code != http.StatusOK {
		t.Fatalf("true-legacy path-bearing call status = %d, want 200 (handshakes + routes); body=%s", rr.Code, rr.Body.String())
	}
	// The daemon was actually contacted (a real handshake + forward, not a
	// terminated abort).
	if mc := daemonMintCount(daemon); mc != 1 {
		t.Errorf("daemon mint count = %d, want 1 (a true-legacy path-bearing call must handshake + forward)", mc)
	}
	// And the path-bearing call bound the sticky session (today's behavior).
	if got := sessions.LookupSession(legacySID); got == nil {
		t.Errorf("true-legacy path-bearing call did not bind the sticky session; want bound")
	}
}

// routerSessionCount returns the current number of router-session bindings.
// Reads len under the store mutex so it is race-free even when this is called
// from a test that also drives concurrent requests (none here, but the lock
// keeps it honest).
func routerSessionCount(s *Server) int {
	s.serenaRouterSessions.mu.Lock()
	defer s.serenaRouterSessions.mu.Unlock()
	return len(s.serenaRouterSessions.bindings)
}
