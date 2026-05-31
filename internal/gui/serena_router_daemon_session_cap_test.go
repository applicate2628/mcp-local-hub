package gui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/api"
)

// A caller that skips initialize and supplies arbitrary unique
// Mcp-Session-Id values used to be treated as a legacy session each time: the
// router would perform a full upstream MCP handshake and store the resulting
// daemon session even though routerSessionStore (the initialize-created, capped
// store) had no entry. The daemon-session store now reserves a capped slot
// before handshaking, so a full store rejects a new legacy id without minting
// another upstream session.
func TestSerenaRouter_LegacySessionID_DaemonSessionCapRejectsBeforeHandshake(t *testing.T) {
	daemon := newFakeSerenaDaemon("alpha")
	ts := httptest.NewServer(daemon.handler())
	t.Cleanup(ts.Close)

	ws := &api.WorkspaceEntry{WorkspaceKey: "alpha", WorkspacePath: "/proj/alpha", Port: 9201}
	deps := &serenaRouterDeps{
		Resolver:      &stubResolver{entries: []*api.WorkspaceEntry{ws}},
		Sessions:      NewInMemorySessionRouter(),
		UpstreamURLFn: func(ws *api.WorkspaceEntry) string { return ts.URL },
		AuditFn:       func(level, event string, fields map[string]any) error { return nil },
	}
	s := newSerenaTestServer(t, deps)

	for i := 0; i < maxRouterSessions; i++ {
		s.serenaDaemonSessions.store(fmt.Sprintf("legacy-%06d", i), ws.WorkspaceKey, fmt.Sprintf("daemon-%06d", i), defaultProtocolVersion)
	}
	if got := daemonSessionCount(s); got != maxRouterSessions {
		t.Fatalf("precondition daemon session store size = %d, want cap %d", got, maxRouterSessions)
	}

	rr := postSerena(t, s, buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": "/proj/alpha/x"}), map[string]string{
		"Mcp-Session-Id": "attacker-fresh-legacy-id",
	})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d; body=%s; want %d", rr.Code, rr.Body.String(), http.StatusTooManyRequests)
	}
	if got := daemonSessionCount(s); got != maxRouterSessions {
		t.Fatalf("daemon session store size after rejected request = %d, want still capped at %d", got, maxRouterSessions)
	}
	daemon.mu.Lock()
	minted := daemon.mintCount
	daemon.mu.Unlock()
	if minted != 0 {
		t.Fatalf("upstream daemon minted %d sessions; want 0 because cap must reject before handshake", minted)
	}
}

func TestDaemonSessionStore_CapAndReservationLifecycle(t *testing.T) {
	st := &daemonSessionStore{}
	for i := 0; i < maxRouterSessions; i++ {
		st.store(fmt.Sprintf("client-%06d", i), "alpha", fmt.Sprintf("daemon-%06d", i), defaultProtocolVersion)
	}
	if got := len(st.bindings); got != maxRouterSessions {
		t.Fatalf("store size after fill = %d, want %d", got, maxRouterSessions)
	}
	st.store("overflow-store", "alpha", "daemon-overflow", defaultProtocolVersion)
	if got := len(st.bindings); got != maxRouterSessions {
		t.Fatalf("store size after overflow store = %d, want capped at %d", got, maxRouterSessions)
	}
	if st.reserveSlot("overflow-reserve") {
		t.Fatalf("reserveSlot on a fresh id succeeded at cap; want rejection")
	}
	if !st.reserveSlot("client-000001") {
		t.Fatalf("reserveSlot on an existing id failed at cap; existing ids must remain reusable")
	}

	st.unbind("client-000001")
	if !st.reserveSlot("reserved-new") {
		t.Fatalf("reserveSlot after freeing one slot failed")
	}
	if got := len(st.bindings); got != maxRouterSessions {
		t.Fatalf("store size after reservation = %d, want %d", got, maxRouterSessions)
	}
	st.releaseReservation("reserved-new")
	if got := len(st.bindings); got != maxRouterSessions-1 {
		t.Fatalf("store size after releasing reservation = %d, want %d", got, maxRouterSessions-1)
	}
}

func daemonSessionCount(s *Server) int {
	s.serenaDaemonSessions.mu.Lock()
	defer s.serenaDaemonSessions.mu.Unlock()
	return len(s.serenaDaemonSessions.bindings)
}
