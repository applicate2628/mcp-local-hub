// internal/gui/serena_router_postpass_test.go
//
// Post-PASS deeper-review refinements for the /serena/mcp router (PR #249):
//
//	Finding 1 (sonnet) — the MAIN client-origin DELETE upstream forward is
//	            capped at serenaDeleteTimeout (5s), NOT the 60s
//	            serenaUpstreamTimeout default. A hung-on-DELETE daemon must not
//	            block the client's teardown 204 for up to a minute; local
//	            revocation already ran first, so the cap does not weaken
//	            correctness.
//	Finding 2 (sonnet) — notifications/cancelled forwards the cancel upstream
//	            ASYNCHRONOUSLY and writes the 202 immediately (the forward no
//	            longer blocks the 202). The forward still reaches the daemon
//	            (detached + bounded). [The reach is also covered by the existing
//	            Finding-H / Finding-3 cancel tests, now async-adapted.]
//	Finding 3 (consultant) — a pathless tool call on an idle-EXPIRED router
//	            session is NOT routed (terminated -32600) and its sticky+daemon
//	            bindings are unbound (no reanimation via the sticky lastSeen
//	            refresh); a TRUE-legacy sticky-only session (never minted at this
//	            router) still routes (legacy compat preserved).
package gui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// ---------------------------------------------------------------------
// Finding 1 (sonnet) — handleSerenaDelete's MAIN upstream forward is bounded by
// the SHORT serenaDeleteTimeout (5s), not the 60s serenaUpstreamTimeout. A
// daemon whose DELETE hangs must still let the client's teardown 204 land within
// ~5s. Pre-fix the forward used the 60s default, so a hung daemon blocked the
// 204 (and held the handler goroutine) for up to a minute even after the local
// revocation already ran. We wire a plain http.Client (no transport
// ResponseHeaderTimeout) so serenaDeleteTimeout is the SOLE bound on the hung
// DELETE — a regression using the 60s default would block well past the 10s
// guard here.
// ---------------------------------------------------------------------
func TestSerenaRouter_Delete_MainForwardBoundedNotSixtySeconds(t *testing.T) {
	// Sanity on the constant the fix introduced: it must be SHORT relative to the
	// 60s upstream default for the cap to mean anything.
	if serenaDeleteTimeout >= serenaUpstreamTimeout {
		t.Fatalf("serenaDeleteTimeout (%v) must be SHORTER than serenaUpstreamTimeout (%v)", serenaDeleteTimeout, serenaUpstreamTimeout)
	}

	releaseDelete := make(chan struct{})
	t.Cleanup(func() { close(releaseDelete) }) // unblock any lingering teardown on exit

	daemon := newFakeSerenaDaemon("alpha")
	base := daemon.handler()
	// Wrap so the client-origin teardown DELETE HANGS until released OR its own
	// (short) context deadline fires; initialize / notifications/initialized /
	// tool all behave normally so the prior tool call can establish the binding.
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			select {
			case <-releaseDelete:
			case <-r.Context().Done(): // the teardown's own short deadline (serenaDeleteTimeout)
			}
			return
		}
		base(w, r)
	})
	ts := httptest.NewServer(wrapped)
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &listerStubResolver{stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}}, list: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		// Plain client with NO transport ResponseHeaderTimeout so ONLY the
		// handler's context (serenaDeleteTimeout) bounds the hung DELETE. (The
		// default serenaHTTPClient would derive a ResponseHeaderTimeout from
		// UpstreamTimeout and mask whether the handler honored the short cap.)
		// UpstreamTimeout left at default (60s) so a regression that reused it for
		// the forward context would block ~60s — the bug this test guards.
		HTTPClient: &http.Client{},
		AuditFn:    func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	const sid = "sess-delete-bound"
	const negotiated = "2025-06-18"
	// Establish a daemon binding via a normal tool call (so DELETE has a daemon
	// session to tear down and resolves the workspace).
	if rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{
		"Mcp-Session-Id":       sid,
		"MCP-Protocol-Version": negotiated,
	}); rr.Code != http.StatusOK {
		t.Fatalf("setup tool call status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if _, ok, _ := bindingDaemonSession(s, sid); !ok {
		t.Fatalf("precondition: no daemon binding after the tool call")
	}

	// DELETE on a HUNG daemon: the client's 204 must land within ~5s
	// (serenaDeleteTimeout), well under the 60s a regression would impose.
	done := make(chan *httptest.ResponseRecorder, 1)
	start := time.Now()
	go func() {
		done <- deleteSerena(t, s, map[string]string{"Mcp-Session-Id": sid, "MCP-Protocol-Version": negotiated})
	}()
	select {
	case rr := <-done:
		if rr.Code != http.StatusNoContent {
			t.Fatalf("DELETE status = %d, want 204 (teardown is best-effort); body=%s", rr.Code, rr.Body.String())
		}
		// The hung DELETE forward fires AND aborts at the short cap before the 204
		// returns, so the whole teardown completes in ~serenaDeleteTimeout, not 60s.
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("DELETE 204 took %v; want ~%v (serenaDeleteTimeout), NOT the 60s upstream default", elapsed, serenaDeleteTimeout)
		}
	case <-time.After(20 * time.Second):
		// Pre-fix the forward used the 60s default -> this fires.
		t.Fatalf("DELETE did not return within 20s; the MAIN teardown forward is NOT bounded by serenaDeleteTimeout (a 60s default would hang here)")
	}
}

// ---------------------------------------------------------------------
// Finding 2 (sonnet) — notifications/cancelled writes the 202 IMMEDIATELY; the
// upstream forward runs on a detached goroutine and does NOT block the 202. We
// make the daemon's notifications/cancelled handler BLOCK (until released); the
// client's 202 must still return promptly (well under the block), and the
// forward must reach the daemon once released (async). Pre-fix the forward was
// synchronous, so the 202 would not return until the (blocked) forward's own
// cleanup-budget deadline (~5s) fired.
// ---------------------------------------------------------------------
func TestSerenaRouter_NotificationCancelled_202ImmediateForwardAsync(t *testing.T) {
	releaseCancel := make(chan struct{})
	t.Cleanup(func() {
		// Idempotent: the test body closes this to release the blocked forward;
		// guard so cleanup does not double-close if the body already did (or
		// re-close on an early failure path that skipped the body close).
		select {
		case <-releaseCancel:
		default:
			close(releaseCancel)
		}
	})

	daemon := newFakeSerenaDaemon("alpha")
	base := daemon.handler()
	cancelEntered := make(chan struct{}, 1)
	// Wrap so the notifications/cancelled POST BLOCKS until released (or its own
	// context deadline). initialize / initialized / tool behave normally. The
	// router forwards the cancel as a POST whose BODY carries
	// method:notifications/cancelled (custom client headers are NOT forwarded by
	// forwardSerenaCancelledUpstream), so we peek+restore the body to detect it.
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			peeked, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(peeked))
			var probe struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(peeked, &probe)
			if probe.Method == "notifications/cancelled" {
				select {
				case cancelEntered <- struct{}{}:
				default:
				}
				select {
				case <-releaseCancel:
				case <-r.Context().Done():
				}
			}
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
		// Plain client (no transport header timeout) so the cancel forward, once
		// launched, only ends on release / cleanupContext — proving the 202 does
		// not wait on it.
		HTTPClient: &http.Client{},
		AuditFn:    func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	const sid = "sess-cancel-async"
	// Establish the daemon binding via a normal tool call (X-Test-Cancel absent,
	// so the wrapper does not block the handshake / tool POSTs).
	if rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{"Mcp-Session-Id": sid}); rr.Code != http.StatusOK {
		t.Fatalf("setup tool call status = %d; body=%s", rr.Code, rr.Body.String())
	}
	wantDaemonSID, ok, _ := bindingDaemonSession(s, sid)
	if !ok {
		t.Fatalf("precondition: no daemon binding after the tool call")
	}

	cancelBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/cancelled",
		"params":  map[string]any{"requestId": 7},
	})
	// The 202 must return PROMPTLY even though the daemon's cancel handler blocks.
	start := time.Now()
	rr := postSerena(t, s, cancelBody, map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("cancel status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		// Pre-fix (synchronous forward) the 202 would wait for the blocked
		// forward's own cleanup deadline (~5s); 2s is a comfortable upper bound for
		// the async (immediate) 202.
		t.Errorf("202 returned after %v; want immediate (finding 2 — the forward must not block the 202)", elapsed)
	}

	// The forward IS in flight on the detached goroutine (it entered the daemon's
	// blocked cancel handler), and once released it records the hit carrying the
	// daemon-issued session id.
	select {
	case <-cancelEntered:
	case <-time.After(2 * time.Second):
		t.Fatalf("the async cancel forward never reached the daemon within 2s")
	}
	close(releaseCancel)
	waitForDaemonCancelHits(t, daemon, 1)
	daemon.mu.Lock()
	gotSID := daemon.lastCancelSession
	daemon.mu.Unlock()
	if gotSID != wantDaemonSID {
		t.Errorf("forwarded cancel Mcp-Session-Id = %q, want the daemon id %q", gotSID, wantDaemonSID)
	}
}

// ---------------------------------------------------------------------
// Finding 3 (consultant) — a pathless tool call on an idle-EXPIRED router
// session is NOT routed: it returns -32600 "session terminated" and its sticky +
// daemon bindings are unbound (no reanimation), and a SUBSEQUENT pathless call
// with the same id is also not routed. Pre-fix the pathless branch did the
// sticky LookupSession (refreshing its lastSeen) BEFORE checking router-session
// liveness, so an expired router session was kept routable as "legacy"
// indefinitely. The window is engineered deterministically via the injectable
// router-session clock (advance past the idle TTL), not a real sleep.
// ---------------------------------------------------------------------
func TestSerenaRouter_Pathless_ExpiredRouterSessionNotRoutedAndUnbound(t *testing.T) {
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

	// Drive the router-session store's clock so lastSeen / expiry is deterministic.
	base := time.Now()
	clk := base
	s.serenaRouterSessions.clock = func() time.Time { return clk }

	// Mint a router session at base, then a PATH-bearing tool call to establish
	// BOTH the sticky binding (post-response BindSession) and the daemon binding.
	const negotiated = "2025-06-18"
	sid := mintRouterSession(t, s, negotiated)
	if rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{
		"Mcp-Session-Id":       sid,
		"MCP-Protocol-Version": negotiated,
	}); rr.Code != http.StatusOK {
		t.Fatalf("setup path tool call status = %d; body=%s", rr.Code, rr.Body.String())
	}
	// Preconditions: router session known, sticky bound, daemon bound.
	if !s.serenaRouterSessions.known(sid) {
		t.Fatalf("precondition: router session not known after initialize+tool call")
	}
	if got := sessions.LookupSession(sid); got == nil {
		t.Fatalf("precondition: sticky binding missing after the path tool call")
	}
	if _, ok, _ := bindingDaemonSession(s, sid); !ok {
		t.Fatalf("precondition: daemon binding missing after the path tool call")
	}

	// Advance the router-session clock PAST the idle TTL so the next read sees the
	// router session as expired-on-read.
	clk = base.Add(daemonSessionTTL + time.Minute)

	// A PATHLESS tool call with the (now-idle-expired) router session id must NOT
	// be routed: it is terminated -32600, and the coordinated unbind drops the
	// sticky + daemon bindings (no reanimation via the sticky lastSeen refresh).
	rr := postSerena(t, s, buildToolCallBody(t, "list_memories", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expired-router pathless call status = %d, want 400 (session terminated); body=%s", rr.Code, rr.Body.String())
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
		t.Errorf("result present on an expired-router pathless call; want -32600 error only")
	}
	if resp.Error == nil || resp.Error.Code != jsonrpcInvalidRequest {
		t.Fatalf("error = %+v, want -32600 session terminated", resp.Error)
	}
	if resp.Error.Message != "session terminated" {
		t.Errorf("error message = %q, want \"session terminated\"", resp.Error.Message)
	}

	// The coordinated unbind dropped BOTH downstream bindings.
	if got := sessions.LookupSession(sid); got != nil {
		t.Errorf("sticky binding %+v survived the expired-router pathless call; want unbound (no reanimation)", got)
	}
	if _, ok, _ := bindingDaemonSession(s, sid); ok {
		t.Errorf("daemon binding survived the expired-router pathless call; want unbound")
	}
	// The router-session entry itself is gone (expire-on-read deleted it).
	if s.serenaRouterSessions.known(sid) {
		t.Errorf("router session still known after the expired pathless call; want expired+deleted")
	}

	// A SUBSEQUENT pathless call with the same id is also NOT routed: with the
	// sticky binding now gone, the pathless branch finds nothing -> 503
	// missing_session (NOT a forward, NOT another -32600 — the router session is
	// already absent, so it falls to the no-sticky-binding path).
	rr2 := postSerena(t, s, buildToolCallBody(t, "list_memories", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr2.Code != http.StatusServiceUnavailable {
		t.Errorf("second pathless call status = %d, want 503 (no binding remains; not routed)", rr2.Code)
	}
}

// Finding 3 (legacy-compat preserved) — a TRUE-legacy session (a sticky binding
// that was NEVER minted at this router, i.e. no routerSessionStore entry) + a
// pathless call IS routed via the sticky binding. This pins that the expired-vs-
// legacy distinction is correct: the on-read close terminates only EXPIRED
// router sessions, never a true-legacy direct caller.
func TestSerenaRouter_Pathless_TrueLegacyStickyStillRoutes(t *testing.T) {
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

	// A TRUE-legacy caller: bind the sticky session DIRECTLY (no initialize at
	// this router -> no routerSessionStore entry). peekVersionState must classify
	// it routerSessionAbsent, so the pathless branch keeps routing via sticky.
	const legacySID = "sess-true-legacy"
	sessions.BindSession(legacySID, ws)
	if s.serenaRouterSessions.known(legacySID) {
		t.Fatalf("precondition: a true-legacy session must NOT have a router-session entry")
	}

	// A pathless tool call with the legacy id IS routed (200), proving legacy
	// compat is preserved (the on-read close fired only for expired router
	// sessions, not for this never-minted-here caller).
	rr := postSerena(t, s, buildToolCallBody(t, "list_memories", map[string]any{}), map[string]string{"Mcp-Session-Id": legacySID})
	if rr.Code != http.StatusOK {
		t.Fatalf("true-legacy pathless call status = %d, want 200 (routed via sticky); body=%s", rr.Code, rr.Body.String())
	}
	// The sticky binding survives (a legacy route must not unbind it).
	if got := sessions.LookupSession(legacySID); got == nil {
		t.Errorf("true-legacy sticky binding was unbound by a pathless route; want preserved")
	}
	// And the daemon was actually contacted (a real forward, not a terminated
	// abort): it minted a session for the legacy caller's lazy handshake.
	if mc := daemonMintCount(daemon); mc != 1 {
		t.Errorf("daemon mint count = %d, want 1 (the legacy pathless call must drive a real handshake+forward)", mc)
	}
}

// peekVersionState unit coverage — the tri-state the pathless close relies on.
func TestRouterSessionStore_PeekVersionState_TriState(t *testing.T) {
	st := &routerSessionStore{}
	base := time.Now()
	clk := base
	st.clock = func() time.Time { return clk }

	// Absent: never stored.
	if v, state := st.peekVersionState("nope"); state != routerSessionAbsent || v != "" {
		t.Errorf("absent: got (%q, %v), want (\"\", routerSessionAbsent)", v, state)
	}
	// Empty id is absent too.
	if _, state := st.peekVersionState(""); state != routerSessionAbsent {
		t.Errorf("empty id: state = %v, want routerSessionAbsent", state)
	}

	// Live: stored, within TTL -> returns version, does NOT refresh/expire.
	st.store("live", "2025-06-18")
	if v, state := st.peekVersionState("live"); state != routerSessionLive || v != "2025-06-18" {
		t.Errorf("live: got (%q, %v), want (\"2025-06-18\", routerSessionLive)", v, state)
	}

	// Expired: aged past TTL -> reported expired AND deleted on read.
	clk = base.Add(daemonSessionTTL + time.Minute)
	if v, state := st.peekVersionState("live"); state != routerSessionExpired || v != "" {
		t.Errorf("expired: got (%q, %v), want (\"\", routerSessionExpired)", v, state)
	}
	// After the expire-on-read delete, a re-read is now ABSENT (not expired again).
	if _, state := st.peekVersionState("live"); state != routerSessionAbsent {
		t.Errorf("post-expiry re-read: state = %v, want routerSessionAbsent (entry deleted on the expiring read)", state)
	}
	// And peekNegotiatedVersion (the boolean form) agrees: false for the gone id.
	if _, ok := st.peekNegotiatedVersion("live"); ok {
		t.Errorf("peekNegotiatedVersion after expiry returned ok=true; want false")
	}
}
