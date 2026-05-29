// internal/gui/serena_router_invariant_d_test.go
//
// Tests for the cross-cutting Invariant D (best-effort fire-and-forget
// upstream calls: one-shot teardowns + cancellation forwards) plus the
// router-side leak/version findings closed alongside it:
//
//	D-detach  — a best-effort upstream call is NOT cancelled when the
//	            inbound client request context is cancelled (it is
//	            context.Background()-derived). Asserted at #7 (cancel
//	            forward) and #6 (path-only one-shot teardown).
//	D-bound   — a best-effort upstream call is bounded by a SHORT budget,
//	            not the 60s serenaUpstreamTimeout default. cleanupContext
//	            carries the math; #4's hung-daemon test proves a regression
//	            that hardcoded 60s would be visible.
//	#2        — a workspace switch on one client session tears down the OLD
//	            workspace's daemon session upstream (no per-switch leak).
//	#8        — a daemon that negotiates a DIFFERENT protocolVersion than the
//	            router requested binds subsequent forwards to the
//	            daemon-negotiated version.
//	#3        — a present-but-malformed tools/list cursor bypasses the cache.
package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// postSerenaCtx is postSerena with a caller-supplied request context, so a
// test can cancel the inbound context and assert a detached best-effort
// upstream call still fires (D-detach).
func postSerenaCtx(t *testing.T, s *Server, ctx context.Context, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/serena/mcp", bytes.NewReader(body))
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
// cleanupContext (Invariant D — D-bound math). The detached best-effort
// context is bounded by serenaCleanupTimeout, or the configured
// upstreamTimeout when it is set AND shorter. It is NEVER the 60s
// serenaUpstreamTimeout default. A cancelled PARENT does not propagate (it
// is context.Background()-derived → detached).
// ---------------------------------------------------------------------
func TestCleanupContext_BoundAndDetached(t *testing.T) {
	// Unset upstreamTimeout (<=0) -> the short const, NOT 60s.
	ctx, cancel := cleanupContext(0)
	dl, ok := ctx.Deadline()
	cancel()
	if !ok {
		t.Fatalf("cleanupContext(0) has no deadline; want one bounded by serenaCleanupTimeout")
	}
	if budget := time.Until(dl); budget > serenaCleanupTimeout+time.Second || budget <= 0 {
		t.Errorf("cleanupContext(0) budget = %v, want ~%v (the short cleanup const, NOT %v)", budget, serenaCleanupTimeout, serenaUpstreamTimeout)
	}
	if serenaCleanupTimeout >= serenaUpstreamTimeout {
		t.Fatalf("serenaCleanupTimeout (%v) must be SHORTER than serenaUpstreamTimeout (%v) for Invariant D to mean anything", serenaCleanupTimeout, serenaUpstreamTimeout)
	}

	// A configured timeout SHORTER than the const wins (test determinism).
	ctx2, cancel2 := cleanupContext(200 * time.Millisecond)
	dl2, _ := ctx2.Deadline()
	cancel2()
	if budget := time.Until(dl2); budget > 400*time.Millisecond || budget <= 0 {
		t.Errorf("cleanupContext(200ms) budget = %v, want ~200ms (configured shorter timeout wins)", budget)
	}

	// A configured timeout LONGER than the const is CAPPED at the const.
	ctx3, cancel3 := cleanupContext(serenaUpstreamTimeout) // 60s
	dl3, _ := ctx3.Deadline()
	cancel3()
	if budget := time.Until(dl3); budget > serenaCleanupTimeout+time.Second {
		t.Errorf("cleanupContext(60s) budget = %v, want capped at ~%v", budget, serenaCleanupTimeout)
	}

	// Detached: a cancelled parent does NOT cancel the cleanup context. (We
	// can only observe that cleanupContext takes no parent — it is built from
	// context.Background() internally — so the returned context's Done is
	// driven solely by its own timeout/cancel, never an inbound request.)
	ctx4, cancel4 := cleanupContext(0)
	defer cancel4()
	select {
	case <-ctx4.Done():
		t.Errorf("cleanupContext is already Done; want live until its own timeout/cancel")
	default:
	}
}

// ---------------------------------------------------------------------
// Invariant D-detach (#7) — a notifications/cancelled whose INBOUND request
// context is already cancelled (the client disconnected the instant after
// sending the cancel) STILL forwards the cancel to the bound daemon: the
// forward is detached from r.Context(). A regression that forwarded under
// r.Context() would drop the cancel (the daemon-side in-flight call would
// keep running until its own timeout).
// ---------------------------------------------------------------------
func TestSerenaRouter_NotificationCancelled_DetachedFromInboundContext(t *testing.T) {
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

	const clientSID = "sess-cancel-detach"
	// Establish the daemon binding via a normal (uncancelled) tool call.
	if rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{"Mcp-Session-Id": clientSID}); rr.Code != http.StatusOK {
		t.Fatalf("setup tool call status = %d; body=%s", rr.Code, rr.Body.String())
	}
	wantDaemonSID, ok, _ := bindingDaemonSession(s, clientSID)
	if !ok {
		t.Fatalf("precondition: no daemon binding after tool call")
	}

	// notifications/cancelled with an ALREADY-CANCELLED inbound context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client disconnected before/at send time
	cancelBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/cancelled",
		"params":  map[string]any{"requestId": 7},
	})
	rr := postSerenaCtx(t, s, ctx, cancelBody, map[string]string{"Mcp-Session-Id": clientSID})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("cancel status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	// The forward is detached, so the daemon STILL received the cancel even
	// though the inbound context was cancelled. (A regression forwarding under
	// r.Context() would record 0 hits here.)
	if got := daemonCancelHits(daemon); got != 1 {
		t.Errorf("daemon cancel hits = %d, want 1 (D-detach: a cancelled inbound ctx must NOT abort the forward)", got)
	}
	daemon.mu.Lock()
	gotSID := daemon.lastCancelSession
	daemon.mu.Unlock()
	if gotSID != wantDaemonSID {
		t.Errorf("forwarded cancel Mcp-Session-Id = %q, want the daemon id %q", gotSID, wantDaemonSID)
	}
}

// Note on #6 (path-only one-shot teardown) D-detach coverage: the one-shot
// teardown is a synchronous `defer` that runs INSIDE the handler, while the
// MAIN tool-call forward legitimately uses r.Context(). There is no clean
// injection point between "main forward completed" and "defer runs" to cancel
// the inbound context for ONLY the teardown via the httptest harness. The #6
// teardown's detach + short bound are instead proven by (a)
// TestCleanupContext_BoundAndDetached (the context #6 uses is
// context.Background()-derived + short-bounded) and (b) the existing
// TestSerenaRouter_ToolCall_PathOnlyOneShotSessionTornDown (the teardown
// fires). The #7 test above gives the concrete inbound-cancel detach
// demonstration for the cancel-forward site; the bridge #1 test
// (internal/daemon) gives it for the DELETE-forward site.

// ---------------------------------------------------------------------
// Invariant D-bound (#4) — a tools/list one-shot teardown to a HUNG daemon
// must NOT block the client beyond the short cleanup budget. The PRE-fix #4
// hardcoded serenaUpstreamTimeout (60s) and IGNORED deps.UpstreamTimeout, so
// it would block the client ~60s on a hung teardown; the fix bounds the
// teardown by deps.UpstreamTimeout when set+shorter. We inject a 250ms
// UpstreamTimeout and a teardown daemon whose DELETE hangs, then assert the
// client's tools/list returns well under the 60s a regression would take.
// ---------------------------------------------------------------------
func TestSerenaRouter_ToolsList_OneShotTeardownBoundedNotSixtySeconds(t *testing.T) {
	releaseDelete := make(chan struct{})
	t.Cleanup(func() { close(releaseDelete) }) // unblock any lingering teardown on test exit

	daemon := newFakeSerenaDaemon("alpha")
	daemon.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"find_symbol"}]}}`))
	}
	// Wrap the handler so the one-shot teardown DELETE HANGS until released
	// (or its own short context deadline fires).
	base := daemon.handler()
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			select {
			case <-releaseDelete:
			case <-r.Context().Done(): // the teardown's own short deadline
			}
			return
		}
		base(w, r)
	})
	ts := httptest.NewServer(wrapped)
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:        &listerStubResolver{list: []*api.WorkspaceEntry{ws}},
		Sessions:        NewInMemorySessionRouter(),
		UpstreamURLFn:   func(ws *api.WorkspaceEntry) string { return ts.URL },
		UpstreamTimeout: 250 * time.Millisecond, // PRE-fix #4 ignored this; fix honors it
		AuditFn:         func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	sid := mintRouterSession(t, s, "2025-11-25")

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	}()
	select {
	case rr := <-done:
		if rr.Code != http.StatusOK {
			t.Fatalf("tools/list status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		assertToolsListNames(t, rr.Body.Bytes(), []string{"find_symbol"})
	case <-time.After(10 * time.Second):
		// PRE-fix #4 would block ~60s on the hung teardown -> this fires.
		t.Fatalf("tools/list did not return within 10s; the one-shot teardown is NOT bounded by the short cleanup budget (Invariant D-bound regression: a 60s default would hang here)")
	}
}

// ---------------------------------------------------------------------
// Round-9 (Finding 4) — a workspace switch on the SAME client session does
// NOT eagerly tear down the OLD workspace's daemon session. Rounds 7+8 issued
// a synchronous upstream DELETE of the displaced old session; the reviewer
// found that races a still-in-flight tool call in the old workspace (the
// router tracks no per-daemon in-flight requests), so per the round-9 guidance
// the old upstream session is left to the daemon's idle TTL. This test asserts
// NO eager teardown fires on the switch and that the LOCAL binding is replaced
// with the new workspace's daemon session. (Replaces the rounds 7+8
// "TearsDownOldDaemonSession" test, which is the behavior we intentionally
// reverted.)
// ---------------------------------------------------------------------
func TestSerenaRouter_WorkspaceSwitch_NoEagerDaemonSessionTeardown(t *testing.T) {
	daemonA := newFakeSerenaDaemon("alpha")
	tsA := httptest.NewServer(daemonA.handler())
	t.Cleanup(tsA.Close)
	daemonB := newFakeSerenaDaemon("beta")
	tsB := httptest.NewServer(daemonB.handler())
	t.Cleanup(tsB.Close)

	wsA := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	wsB := &api.WorkspaceEntry{WorkspaceKey: "beta", WorkspacePath: "/proj/beta", Port: 9202}
	deps := &serenaRouterDeps{
		Resolver: &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{wsA, wsB}}, list: []*api.WorkspaceEntry{wsA, wsB}},
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

	const clientSID = "sess-ws-switch"
	// Call 1 -> alpha. Establishes (clientSID -> alpha daemon session).
	if rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{"Mcp-Session-Id": clientSID}); rr.Code != http.StatusOK {
		t.Fatalf("alpha call status = %d; body=%s", rr.Code, rr.Body.String())
	}
	alphaDaemonSID, ok, _ := bindingDaemonSession(s, clientSID)
	if !ok {
		t.Fatalf("precondition: no alpha daemon binding after call 1")
	}
	if got := daemonDeleteHits(daemonA); got != 0 {
		t.Fatalf("alpha DELETE hits before switch = %d, want 0", got)
	}

	// Call 2 (SAME client session) -> beta. The store OVERWRITES the alpha
	// binding locally. Round-9: NO eager upstream DELETE of the alpha daemon
	// session may fire (it would race a still-in-flight alpha tool call); the
	// orphaned alpha upstream session ages out on the daemon's idle clock.
	if rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/beta/y"}), map[string]string{"Mcp-Session-Id": clientSID}); rr.Code != http.StatusOK {
		t.Fatalf("beta call status = %d; body=%s", rr.Code, rr.Body.String())
	}
	// Give any erroneous deferred/eager teardown a chance to fire, then assert
	// none did (the switch must be teardown-free).
	time.Sleep(50 * time.Millisecond)

	// No DELETE was issued to EITHER daemon by the switch (the alpha session
	// is left to TTL; the beta session is live and must not be torn down).
	if got := daemonDeleteHits(daemonA); got != 0 {
		t.Errorf("alpha DELETE hits after switch = %d, want 0 (Round-9: the displaced old session is left to TTL, NOT eagerly torn down)", got)
	}
	if got := daemonDeleteHits(daemonB); got != 0 {
		t.Errorf("beta DELETE hits after switch = %d, want 0 (the new live session must NOT be torn down)", got)
	}

	// The LOCAL binding is now the BETA daemon session (the switch overwrote
	// the alpha binding; the new workspace's session is what's bound).
	betaDaemonSID, present, gotWsKey := bindingDaemonSession(s, clientSID)
	if !present {
		t.Fatalf("client session lost its (beta) binding after the switch; want the new binding live")
	}
	if gotWsKey != "beta" {
		t.Errorf("bound wsKey after switch = %q, want beta (the new workspace's session is bound)", gotWsKey)
	}
	if betaDaemonSID == alphaDaemonSID {
		t.Errorf("bound daemon session after switch = %q (still the alpha id); want the new beta daemon session", betaDaemonSID)
	}
	// Sanity: the beta daemon actually minted the now-bound session.
	daemonB.mu.Lock()
	betaMinted := daemonB.issued[betaDaemonSID]
	daemonB.mu.Unlock()
	if !betaMinted {
		t.Errorf("bound daemon session %q was not minted by the beta daemon; the switch did not rebind to beta", betaDaemonSID)
	}
}

// #2 (negative) — a SECOND tool call on the SAME workspace reuses the binding
// and triggers NO displaced teardown (the binding is unchanged; tearing it
// down would break the reuse).
func TestSerenaRouter_SameWorkspaceReuse_NoDisplacedTeardown(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
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

	const clientSID = "sess-reuse-no-teardown"
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	for i := 0; i < 3; i++ {
		if rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": clientSID}); rr.Code != http.StatusOK {
			t.Fatalf("call %d status = %d; body=%s", i, rr.Code, rr.Body.String())
		}
	}
	// Give any erroneous deferred teardown a chance, then assert none fired.
	time.Sleep(50 * time.Millisecond)
	if got := daemonDeleteHits(daemon); got != 0 {
		t.Errorf("daemon DELETE hits after same-workspace reuse = %d, want 0 (unchanged binding must NOT be torn down)", got)
	}
}

// ---------------------------------------------------------------------
// #8 — when the daemon negotiates a DIFFERENT protocolVersion than the router
// requested, the router binds subsequent forwards (the tool-call POST, the
// teardown DELETE) to the DAEMON-negotiated version, not the requested one.
// The fixture's negotiatedVersionOverride forces the divergence.
// ---------------------------------------------------------------------
func TestSerenaRouter_ToolCall_UsesDaemonNegotiatedVersionForForwards(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	// The client/router will request 2025-11-25 (the session's negotiated
	// version); the daemon negotiates DOWN to 2025-06-18 in its result.
	daemon.negotiatedVersionOverride = "2025-06-18"
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

	const requested = "2025-11-25"
	const daemonNegotiated = "2025-06-18"
	if requested == daemonNegotiated {
		t.Fatalf("precondition: requested and daemon-negotiated must differ")
	}
	sid := mintRouterSession(t, s, requested)

	// Tool call: the router requests `requested` on the handshake, the daemon
	// negotiates `daemonNegotiated`, and the forwarded tool POST must carry
	// the DAEMON-negotiated version.
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, map[string]string{
		"Mcp-Session-Id":       sid,
		"MCP-Protocol-Version": requested,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("tool call status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	daemon.mu.Lock()
	gotInitPV := daemon.lastInitProtocolVersion                     // handshake init params -> requested
	gotToolPV := daemon.lastToolHeaders.Get("MCP-Protocol-Version") // forward -> daemon-negotiated
	gotInitializedPV := daemon.lastInitializedHeaders.Get("MCP-Protocol-Version")
	daemon.mu.Unlock()
	if gotInitPV != requested {
		t.Errorf("handshake initialize params.protocolVersion = %q, want the requested %q", gotInitPV, requested)
	}
	// #8: notifications/initialized + the tool-call forward both carry the
	// DAEMON-negotiated version (the daemon binds its session to that).
	if gotInitializedPV != daemonNegotiated {
		t.Errorf("notifications/initialized MCP-Protocol-Version = %q, want the daemon-negotiated %q (#8)", gotInitializedPV, daemonNegotiated)
	}
	if gotToolPV != daemonNegotiated {
		t.Errorf("forwarded tools/call MCP-Protocol-Version = %q, want the daemon-negotiated %q (#8: not the requested %q)", gotToolPV, daemonNegotiated, requested)
	}

	// The persisted binding carries the daemon-negotiated version, so the
	// client-origin DELETE teardown forwards THAT version too (#8).
	if rr := deleteSerena(t, s, map[string]string{"Mcp-Session-Id": sid, "MCP-Protocol-Version": requested}); rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if got := daemonDeleteHits(daemon); got != 1 {
		t.Fatalf("daemon DELETE hits = %d, want 1", got)
	}
	daemon.mu.Lock()
	gotDelPV := daemon.lastDeleteHeaders.Get("MCP-Protocol-Version")
	daemon.mu.Unlock()
	if gotDelPV != daemonNegotiated {
		t.Errorf("teardown DELETE MCP-Protocol-Version = %q, want the daemon-negotiated %q (#8: persisted on the binding)", gotDelPV, daemonNegotiated)
	}
}

// #8 — the one-shot tools/list path also forwards under the daemon-negotiated
// version (its tools/list POST header and one-shot teardown DELETE).
func TestSerenaRouter_ToolsList_UsesDaemonNegotiatedVersion(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	daemon.negotiatedVersionOverride = "2025-06-18"
	daemon.tool = func(w http.ResponseWriter, r *http.Request, b []byte) {
		// Echo the version the tools/list POST carried so the test can prove
		// the daemon-negotiated version (not the requested) was used.
		ver := r.Header.Get("MCP-Protocol-Version")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"tool-` + ver + `"}]}}`))
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

	// Router session negotiated 2025-11-25; daemon will negotiate 2025-06-18.
	sid := mintRouterSession(t, s, "2025-11-25")
	rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// The tools/list POST carried the daemon-negotiated version (echoed in the
	// tool name), proving #8 on the tools/list leg.
	assertToolsListNames(t, rr.Body.Bytes(), []string{"tool-2025-06-18"})
	// The one-shot teardown DELETE carried the daemon-negotiated version.
	waitForDaemonDeleteHits(t, daemon, 1)
	daemon.mu.Lock()
	gotDelPV := daemon.lastDeleteHeaders.Get("MCP-Protocol-Version")
	daemon.mu.Unlock()
	if gotDelPV != "2025-06-18" {
		t.Errorf("one-shot teardown DELETE MCP-Protocol-Version = %q, want the daemon-negotiated 2025-06-18 (#8)", gotDelPV)
	}
}

// #8 (fallback) — a daemon that OMITS protocolVersion from its initialize
// result falls back to the requested version for subsequent forwards (so the
// forward still carries a non-empty version a strict daemon would accept).
func TestSerenaRouter_ToolCall_DaemonOmitsVersionFallsBackToRequested(t *testing.T) {
	// A bespoke upstream: mints a session on initialize but its result has NO
	// protocolVersion field; the tool POST is session-gated and echoes the
	// version it carried.
	var mu sync.Mutex
	issued := map[string]bool{}
	mintN := 0
	var lastToolPV string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &probe)
		switch probe.Method {
		case "initialize":
			mu.Lock()
			mintN++
			sid := "noverdaemon-" + strconv.Itoa(mintN)
			issued[sid] = true
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", sid)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// result WITHOUT protocolVersion.
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"x","version":"1"},"capabilities":{}}}`))
			return
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		}
		sid := r.Header.Get("Mcp-Session-Id")
		mu.Lock()
		known := issued[sid]
		if known {
			lastToolPV = r.Header.Get("MCP-Protocol-Version")
		}
		mu.Unlock()
		if !known {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	t.Cleanup(upstream.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return upstream.URL },
	}
	s := newSerenaTestServer(t, deps)

	const requested = "2025-06-18"
	sid := mintRouterSession(t, s, requested)
	rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{
		"Mcp-Session-Id":       sid,
		"MCP-Protocol-Version": requested,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("tool call status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	mu.Lock()
	got := lastToolPV
	mu.Unlock()
	if got != requested {
		t.Errorf("forwarded tools/call MCP-Protocol-Version = %q, want the requested %q (#8 fallback when daemon omits its version)", got, requested)
	}
}

// ---------------------------------------------------------------------
// #3 — a present-but-malformed tools/list cursor (number / array / object)
// BYPASSES the first-page cache (proxies fresh so the daemon validates),
// instead of being masked as a cursorless request served the cached page one.
// ---------------------------------------------------------------------
func TestToolsListIsCursorRequest_PresenceAndType(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"cursor absent", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, false},
		{"params empty object", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, false},
		{"params null", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":null}`, false},
		{"cursor empty string", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":""}}`, false},
		{"cursor non-empty string", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":"page2"}}`, true},
		{"cursor number (malformed)", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":42}}`, true},
		{"cursor array (malformed)", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":[1,2]}}`, true},
		{"cursor object (malformed)", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":{"x":1}}}`, true},
		{"cursor null (malformed)", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":null}}`, true},
		// Round-10 finding #2: a PRESENT but NON-OBJECT params is malformed for
		// tools/list — bypass so the daemon validates/rejects it, instead of
		// being masked as cursorless and served the cached page-one.
		{"params number (non-object)", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":1}`, true},
		{"params array (non-object)", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[]}`, true},
		{"params string (non-object)", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":"x"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolsListIsCursorRequest([]byte(tc.body)); got != tc.want {
				t.Errorf("toolsListIsCursorRequest(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// #3 (end-to-end) — a malformed cursor on tools/list bypasses the cache and
// hits the daemon FRESH (it is NOT served the cached first page). We seed the
// cache via a cursorless call, then a malformed-cursor call must trigger a
// SECOND daemon proxy (proving the bypass), whereas a repeat cursorless call
// is served from cache (no new proxy).
func TestSerenaRouter_ToolsList_MalformedCursorBypassesCache(t *testing.T) {
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
		Resolver:      &listerStubResolver{list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)
	sid := mintRouterSession(t, s, "2025-11-25")
	hdr := map[string]string{"Mcp-Session-Id": sid}

	// Seed the first-page cache with a cursorless call.
	if rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), hdr); rr.Code != http.StatusOK {
		t.Fatalf("seed tools/list status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if got := toolHits(); got != 1 {
		t.Fatalf("daemon tool hits after seed = %d, want 1", got)
	}

	// A repeat cursorless call is served from cache -> NO new daemon hit.
	if rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), hdr); rr.Code != http.StatusOK {
		t.Fatalf("cached tools/list status = %d", rr.Code)
	}
	if got := toolHits(); got != 1 {
		t.Fatalf("daemon tool hits after cursorless repeat = %d, want 1 (cache hit)", got)
	}

	// A MALFORMED cursor (number) MUST bypass the cache and proxy FRESH ->
	// a SECOND daemon hit. Pre-fix it was treated as cursorless and served the
	// cached page one (no new hit).
	malformed := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"cursor":42}}`)
	if rr := postSerena(t, s, malformed, hdr); rr.Code != http.StatusOK {
		t.Fatalf("malformed-cursor tools/list status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if got := toolHits(); got != 2 {
		t.Errorf("daemon tool hits after malformed cursor = %d, want 2 (#3: a malformed cursor must bypass the cache, not be served page one)", got)
	}
}

// ---------------------------------------------------------------------
// Round-10 finding #2 — a tools/list whose `params` is PRESENT but NOT an
// object (`"params":[]`, `"params":1`, `"params":"x"`) must NOT be served the
// cached cursorless first page as success.
//
// CONFLICT NOTE (reported to the orchestrator): the finding's stated premise
// was that such a request reaches the cache-decision and is SERVED a cached
// page-one. Empirically it does not: serenaRouterHandler decodes the body into
// `toolBody` (whose `Params` is a struct) BEFORE method dispatch, so a
// non-object `params` fails that decode and the request is rejected with HTTP
// 400 "malformed JSON body" — it never reaches handleToolsList /
// toolsListIsCursorRequest. So the user-facing symptom (stale cache served as
// success) does NOT occur today; the latent defect was in
// toolsListIsCursorRequest itself, which would misclassify a non-object params
// as cursorless IF it were ever reached. The helper is now fixed
// (defense-in-depth + matching the finding's Fix instruction), and this test
// locks in the end-to-end guarantee: seed the first-page cache, then a
// non-object-params tools/list is rejected (NOT served the cached page-one),
// and the daemon is NOT hit a second time. Loosening the decode gate (out of
// scope here) would re-expose the helper path, which the unit-level
// TestToolsListIsCursorRequest_PresenceAndType now covers.
// ---------------------------------------------------------------------
func TestSerenaRouter_ToolsList_NonObjectParamsNotServedStaleCache(t *testing.T) {
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
		Resolver:      &listerStubResolver{list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)
	sid := mintRouterSession(t, s, "2025-11-25")
	hdr := map[string]string{"Mcp-Session-Id": sid}

	// Seed the first-page cache with a cursorless call.
	if rr := postSerena(t, s, buildLifecycleBody(t, "tools/list", map[string]any{}), hdr); rr.Code != http.StatusOK {
		t.Fatalf("seed tools/list status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if got := toolHits(); got != 1 {
		t.Fatalf("daemon tool hits after seed = %d, want 1", got)
	}

	// A non-object params (array / number) must NOT be served the cached page
	// one. With today's decode gate it is rejected 400 "malformed JSON body"
	// BEFORE the cache decision; the load-bearing assertion is that it is NOT a
	// 200 carrying the cached result.
	for _, body := range [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":[]}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":1}`),
	} {
		rr := postSerena(t, s, body, hdr)
		if rr.Code == http.StatusOK {
			t.Errorf("non-object params %s returned 200 (cached page-one served); want a fail-loud rejection, not a stale-cache hit; body=%s", body, rr.Body.String())
		}
	}
	// The daemon must NOT have been hit again — a non-object params request is
	// neither served from cache (asserted above) nor proxied fresh today.
	if got := toolHits(); got != 1 {
		t.Errorf("daemon tool hits after non-object params requests = %d, want 1 (no new proxy)", got)
	}
}

// ---------------------------------------------------------------------
// Finding #3 (initialized-fail leak) — when the daemon 200s `initialize`
// (minting a session) but then 4xxs `notifications/initialized`,
// establishDaemonSession must best-effort upstream-DELETE the just-minted
// session BEFORE returning the handshake error. The handshake still fails
// loud (the tool call surfaces the diagnosable 502 path) AND no upstream
// daemon session leaks. Pre-fix the session id was dropped on the floor,
// leaking one upstream daemon session per failed initialized notification
// until the daemon's idle expiry. The DELETE is detached + short-bounded
// (Invariant D), issued via bestEffortDeleteDaemonSession.
// ---------------------------------------------------------------------
func TestSerenaRouter_Handshake_InitializedFailReleasesDaemonSession(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	// initialize succeeds (mints + records the session id), but
	// notifications/initialized is rejected -> the handshake must fail AND the
	// minted session must be torn down upstream.
	daemon.initializedStatus = http.StatusBadRequest
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

	// A path-bearing tool call drives the lazy handshake; the daemon mints a
	// session on initialize then rejects notifications/initialized.
	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"})
	rr := postSerena(t, s, body, map[string]string{"Mcp-Session-Id": "sess-init-fail"})

	// The handshake still fails LOUD (same class as an unreachable forward).
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("tool call status = %d, want 502 (handshake fails on initialized rejection); body=%s", rr.Code, rr.Body.String())
	}
	// And the just-minted daemon session was released upstream. The DELETE is
	// issued synchronously inside establishDaemonSession (defer cancel + direct
	// Do), so it has completed by the time the 502 returns; poll briefly as a
	// belt-and-braces guard.
	waitForDaemonDeleteHits(t, daemon, 1)
	daemon.mu.Lock()
	gotDelSID := daemon.lastDeleteSession
	releasedAMintedSession := gotDelSID != "" && daemon.issued[gotDelSID]
	daemon.mu.Unlock()
	if !releasedAMintedSession {
		t.Errorf("initialized-fail teardown DELETE Mcp-Session-Id = %q is not a session this daemon minted; Finding #3 must DELETE the just-created session id", gotDelSID)
	}
	// No phantom binding cached (the handshake failed, so nothing is reusable).
	if _, dsid, _, ok := s.serenaDaemonSessions.bindingFor("sess-init-fail"); ok {
		t.Errorf("serenaDaemonSessions cached %q on a failed handshake; want no binding", dsid)
	}
	if got := sessions.LookupSession("sess-init-fail"); got != nil {
		t.Errorf("sticky binding cached on a failed handshake = %+v; want nil", got)
	}
}
