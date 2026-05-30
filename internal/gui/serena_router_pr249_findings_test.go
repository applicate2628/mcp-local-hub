// internal/gui/serena_router_pr249_findings_test.go
//
// Codex PR #249 review findings (serena /serena/mcp router):
//
//	Finding 1 (P2) — postHandshakeInitialized must NOT block-drain a 2xx
//	            notifications/initialized body. notifications/initialized is a
//	            NOTIFICATION (no response payload); a daemon that answers 2xx
//	            with an open/never-EOF SSE stream must not make
//	            establishDaemonSession hang until the upstream timeout — the
//	            body is closed, not io.Copy-drained.
//	Finding 2 (P2) — routerSessionStore caps at maxRouterSessions with
//	            LRU-eviction (mirrors the hub session store's GLOBAL-cap
//	            policy). A looping initialize cannot grow the map unbounded;
//	            the least-recently-seen entry evicts first so an active session
//	            is never locked out, and the reconcile probe (initialize-only,
//	            never DELETE) is self-limiting.
//	Finding 3 (P2) — handleSerenaDelete revokes local session state BEFORE the
//	            (blockable) ListWorkspaces-based workspace resolution. A
//	            concurrent POST with the same Mcp-Session-Id during the DELETE's
//	            blocking workspace resolution is rejected (session already
//	            revoked), not routed.
package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// ---------------------------------------------------------------------
// Finding 1 (P2) — a daemon that 2xx's notifications/initialized with an
// OPEN, never-EOF SSE body must NOT block establishDaemonSession. The
// handshake is part of a tool-call the client is awaiting and is bounded by
// upstreamTimeout; pre-fix postHandshakeInitialized did
// io.Copy(io.Discard, resp.Body) on the 2xx path, which blocks until that
// timeout when the body never EOFs, so the handshake "succeeds" only after
// the full upstream delay. The fix closes the body without draining it. We
// set a LARGE upstreamTimeout and assert the call returns far under it.
// ---------------------------------------------------------------------
func TestSerenaRouter_PR249_F1_InitializedNoticeBodyNotDrained(t *testing.T) {
	// Release gate so the daemon's blocked notifications/initialized handler
	// unblocks on test teardown (it would otherwise hold the handler goroutine
	// open past the test).
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	const daemonSID = "f1-daemon-session"
	const negotiated = "2025-06-18"

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &probe)
		switch probe.Method {
		case "initialize":
			// Normal initialize: mint a session id + JSON-RPC result so the
			// handshake advances to notifications/initialized.
			w.Header().Set("Mcp-Session-Id", daemonSID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":"mcphub-router-handshake","result":{"protocolVersion":%q,"serverInfo":{"name":"serena","version":"fake"},"capabilities":{"tools":{}}}}`, negotiated)
		case "notifications/initialized":
			// Answer 2xx headers as text/event-stream, FLUSH them, then BLOCK
			// without ever sending a terminating event or closing the body.
			// A block-draining client would hang here until its context
			// deadline (upstreamTimeout); the fix closes the body promptly.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusAccepted)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	// A LARGE upstream timeout: a regression that block-drains the 2xx body
	// would hang ~30s here; the fix returns in well under a second.
	const upstreamTimeout = 30 * time.Second
	// Plain client (no transport ResponseHeaderTimeout) so ONLY the handshake
	// context bounds a hung body read — proving the fix returns promptly on its
	// own, not because the transport killed the read.
	httpClient := &http.Client{}

	type result struct {
		sid string
		ver string
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		sid, ver, err := establishDaemonSession(context.Background(), httpClient, ts.URL, negotiated, upstreamTimeout)
		done <- result{sid: sid, ver: ver, err: err}
	}()

	select {
	case got := <-done:
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("establishDaemonSession took %v; want prompt (<3s). A 2xx notifications/initialized body must be CLOSED, not block-drained until the %v upstream timeout", elapsed, upstreamTimeout)
		}
		if got.err != nil {
			t.Fatalf("establishDaemonSession err = %v; want nil (2xx initialized is a successful handshake)", got.err)
		}
		if got.sid != daemonSID {
			t.Errorf("daemon session id = %q, want %q", got.sid, daemonSID)
		}
		if got.ver != negotiated {
			t.Errorf("daemon protocol version = %q, want %q", got.ver, negotiated)
		}
	case <-time.After(10 * time.Second):
		// Pre-fix the 2xx io.Copy blocks until the 30s upstreamTimeout, so this
		// 10s guard fires well before the call would have returned.
		t.Fatalf("establishDaemonSession did not return within 10s; the 2xx notifications/initialized body is being block-drained (Finding 1 regression)")
	}
}

// ---------------------------------------------------------------------
// Finding 2 (P2) — routerSessionStore caps at maxRouterSessions with
// LRU-eviction. Fill the store to capacity, mark one entry as
// most-recently-seen via touch, then store one MORE: the size stays <= cap,
// the eldest (least-recently-seen) entry is evicted, the newest entry is
// present, and the touched MRU entry survives (never locked out).
//
// Reads of len(st.bindings) are race-free here: the store is exercised by
// THIS goroutine only (no concurrent access in this unit test).
// ---------------------------------------------------------------------
func TestSerenaRouter_PR249_F2_StoreCapEvictsLRU(t *testing.T) {
	base := time.Now()
	clk := base
	st := &routerSessionStore{clock: func() time.Time { return clk }}

	// Insert IDs in order; advance the clock per insert so insertion order ==
	// LRU order (id-0 oldest, id-(cap-1) newest). The store moves a fresh
	// insert to the FRONT, so the LRU back == id-0.
	id := func(i int) string { return fmt.Sprintf("sess-%05d", i) }
	for i := 0; i < maxRouterSessions; i++ {
		clk = base.Add(time.Duration(i) * time.Millisecond)
		st.store(id(i), "2025-06-18")
	}
	if got := len(st.bindings); got != maxRouterSessions {
		t.Fatalf("store size after filling to cap = %d, want %d", got, maxRouterSessions)
	}

	// Touch the OLDEST entry (id-0) so it becomes most-recently-seen: it must
	// then survive the eviction that the next store triggers (an active session
	// is never the eviction victim).
	clk = base.Add(time.Duration(maxRouterSessions) * time.Millisecond)
	if !st.touch(id(0)) {
		t.Fatalf("touch(id-0) returned false; the entry should be live at cap")
	}

	// Now the LRU back is id-1 (id-0 was promoted). Store one MORE (a new id):
	// the cap forces an eviction of the current LRU back (id-1).
	clk = base.Add(time.Duration(maxRouterSessions+1) * time.Millisecond)
	const newID = "sess-overflow"
	st.store(newID, "2025-06-18")

	// Size stayed at the cap (one in, one out).
	if got := len(st.bindings); got != maxRouterSessions {
		t.Fatalf("store size after overflow store = %d, want %d (cap held)", got, maxRouterSessions)
	}
	// The newest entry is present.
	if !st.known(newID) {
		t.Errorf("newest entry %q absent after overflow store; want present", newID)
	}
	// The touched MRU entry (id-0) survived — not locked out by the churn.
	if !st.known(id(0)) {
		t.Errorf("touched-MRU entry %q was evicted; LRU-eviction must keep an active session", id(0))
	}
	// The least-recently-seen entry (id-1) was evicted.
	if st.known(id(1)) {
		t.Errorf("LRU entry %q survived; the eldest entry should evict at cap", id(1))
	}
}

// Finding 2 (probe-safety) — the Phase-3 reconcile probe initializes every
// cycle and NEVER DELETEs. With LRU-eviction the store stays bounded under a
// looping initialize that far exceeds the cap, and the MOST RECENT entries
// are the survivors (an active client's latest session is retained). This is
// the property that makes the cap probe-safe: the probe / a churning client
// cannot grow the map unbounded NOR push out the freshest sessions.
func TestSerenaRouter_PR249_F2_LoopingInitializeStaysBounded(t *testing.T) {
	base := time.Now()
	clk := base
	st := &routerSessionStore{clock: func() time.Time { return clk }}

	// Loop initialize-style stores well past the cap (no DELETE between), the
	// exact pattern the reconcile probe / a buggy looping client produces.
	const total = maxRouterSessions * 3
	id := func(i int) string { return fmt.Sprintf("loop-%06d", i) }
	for i := 0; i < total; i++ {
		clk = base.Add(time.Duration(i) * time.Millisecond)
		st.store(id(i), "2025-06-18")
		if got := len(st.bindings); got > maxRouterSessions {
			t.Fatalf("store size = %d at iteration %d; cap %d must never be exceeded", got, i, maxRouterSessions)
		}
	}
	if got := len(st.bindings); got != maxRouterSessions {
		t.Fatalf("final store size = %d, want exactly the cap %d", got, maxRouterSessions)
	}
	// The freshest cap-many entries survived; the older ones evicted.
	if !st.known(id(total - 1)) {
		t.Errorf("most-recent entry %q absent; LRU-eviction must keep the freshest sessions", id(total-1))
	}
	if st.known(id(0)) {
		t.Errorf("oldest entry %q survived %d stores; want evicted", id(0), total)
	}
}

// ---------------------------------------------------------------------
// Finding 3 (P2) — handleSerenaDelete revokes local session state BEFORE the
// blockable ListWorkspaces-based workspace resolution. We make ListWorkspaces
// BLOCK; a concurrent POST with the same Mcp-Session-Id issued WHILE the
// DELETE is stuck in that resolution must be REJECTED (503 missing-session —
// the session was already revoked), NOT routed to a daemon. Pre-fix the
// DELETE resolved the workspace (ListWorkspaces, blocking) BEFORE unbinding,
// so the concurrent POST passed the sticky lookup and routed.
// ---------------------------------------------------------------------
func TestSerenaRouter_PR249_F3_DeleteRevokesBeforeBlockingWorkspaceResolve(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)
	toolHits := func() int { daemon.mu.Lock(); defer daemon.mu.Unlock(); return daemon.toolHits }

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	lister := &blockingLister{
		listerStubResolver: listerStubResolver{
			stubResolver: stubResolver{entries: []*api.WorkspaceEntry{ws}},
			list:         []*api.WorkspaceEntry{ws},
		},
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	t.Cleanup(func() {
		// Unblock any lingering ListWorkspaces on teardown.
		select {
		case <-lister.release:
		default:
			close(lister.release)
		}
	})

	deps := &serenaRouterDeps{
		Resolver:      lister,
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	const negotiated = "2025-06-18"
	// Establish the router session + sticky binding + daemon binding via a
	// path-bearing tool call (ResolveByPath, which does NOT block — only
	// ListWorkspaces is armed to block). mintRouterSession returns the
	// server-minted client session id we drive the rest of the test with.
	sid := mintRouterSession(t, s, negotiated)
	if rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{
		"Mcp-Session-Id":       sid,
		"MCP-Protocol-Version": negotiated,
	}); rr.Code != http.StatusOK {
		t.Fatalf("setup path tool call status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if got := deps.Sessions.LookupSession(sid); got == nil {
		t.Fatalf("precondition: sticky binding missing after the path tool call")
	}
	if _, ok, _ := bindingDaemonSession(s, sid); !ok {
		t.Fatalf("precondition: daemon binding missing after the path tool call")
	}

	// Arm ListWorkspaces to block: from now on the DELETE's by-key resolution
	// (resolveWorkspaceByKey -> ListWorkspaces) hangs until released.
	lister.arm()

	// Fire the DELETE in a goroutine; it will unbind FIRST (the fix) then block
	// in ListWorkspaces.
	deleteDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		deleteDone <- deleteSerena(t, s, map[string]string{"Mcp-Session-Id": sid, "MCP-Protocol-Version": negotiated})
	}()

	// Wait until the DELETE has entered the blocking ListWorkspaces — at that
	// point, with the fix, the unbind has ALREADY run.
	select {
	case <-lister.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("DELETE never reached the blocking ListWorkspaces within 5s")
	}

	// Concurrent PATHLESS POST with the SAME session id while the DELETE is
	// blocked in workspace resolution. With revocation-before-resolution it is
	// rejected 503 (no binding remains); a regression that resolved before
	// unbinding would still route it (200) via the live sticky binding.
	hitsBefore := toolHits()
	rr := postSerena(t, s, buildToolCallBody(t, "list_memories", map[string]any{}), map[string]string{"Mcp-Session-Id": sid})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("concurrent POST during blocked DELETE status = %d, want 503 (session already revoked, not routed); body=%s", rr.Code, rr.Body.String())
	}
	// And it did NOT reach the daemon (not routed).
	if got := toolHits(); got != hitsBefore {
		t.Errorf("concurrent POST reached the daemon (tool hits %d -> %d); want NOT routed after revocation", hitsBefore, got)
	}

	// Release ListWorkspaces and let the DELETE complete with its best-effort
	// 204 (teardown is best-effort regardless of the upstream forward outcome).
	close(lister.release)
	select {
	case rrDel := <-deleteDone:
		if rrDel.Code != http.StatusNoContent {
			t.Errorf("DELETE status = %d, want 204 (best-effort teardown); body=%s", rrDel.Code, rrDel.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("DELETE did not complete within 10s after releasing ListWorkspaces")
	}
}

// blockingLister is a listerStubResolver whose ListWorkspaces BLOCKS (after
// signaling entry) once armed, so a test can hold the DELETE inside its
// workspace resolution while it issues a concurrent POST. ResolveByPath is
// inherited unchanged (it does NOT block — the setup path tool call uses it).
type blockingLister struct {
	listerStubResolver
	armed   bool
	mu      sync.Mutex
	entered chan struct{}
	release chan struct{}
}

func (b *blockingLister) arm() {
	b.mu.Lock()
	b.armed = true
	b.mu.Unlock()
}

func (b *blockingLister) ListWorkspaces() []*api.WorkspaceEntry {
	b.mu.Lock()
	armed := b.armed
	b.mu.Unlock()
	if armed {
		select {
		case b.entered <- struct{}{}:
		default:
		}
		<-b.release
	}
	return b.listerStubResolver.ListWorkspaces()
}
