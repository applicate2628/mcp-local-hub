package gui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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

// TestDaemonSessionStore_SharedReservationRefcount covers Codex PR #251 finding
// 1: two concurrent first requests for the SAME fresh Mcp-Session-Id each
// reserveSlot the one shared placeholder. If the first caller's handshake fails
// and it releaseReservation()s while the second caller's handshake is still in
// flight, the shared placeholder must NOT be dropped (the second caller still
// owns a reservation on it); a subsequent store() by the second caller must then
// succeed instead of being spuriously rejected at cap.
func TestDaemonSessionStore_SharedReservationRefcount(t *testing.T) {
	st := &daemonSessionStore{}

	const id = "shared-fresh-id"
	// Request A and request B both take a reservation on the same fresh id.
	if !st.reserveSlot(id) {
		t.Fatalf("reserveSlot A on fresh id returned false; want true")
	}
	if !st.reserveSlot(id) {
		t.Fatalf("reserveSlot B on the same fresh id returned false; want true (shared placeholder)")
	}
	if got := len(st.bindings); got != 1 {
		t.Fatalf("two reservations on the same id created %d bindings; want exactly 1 shared placeholder", got)
	}

	// Request A's handshake fails -> it releases. The placeholder must survive
	// because B still holds a reservation on it.
	st.releaseReservation(id)
	if got := len(st.bindings); got != 1 {
		t.Fatalf("placeholder dropped after one of two reservations released; size=%d, want 1 (B still owns it)", got)
	}

	// Request B's handshake succeeds -> it stores. Must NOT be rejected: the slot
	// was reserved, so store sees the id as existing and completes the binding.
	if !st.store(id, "alpha", "daemon-shared", defaultProtocolVersion) {
		t.Fatalf("store for the surviving reserved id returned false; want true (no spurious cap rejection)")
	}
	gotDS, _, ok := st.lookup(id, "alpha")
	if !ok || gotDS != "daemon-shared" {
		t.Fatalf("lookup after store = (%q, ok=%v); want (\"daemon-shared\", ok=true)", gotDS, ok)
	}
}

// TestDaemonSessionStore_SharedReservationRefcount_Concurrent drives the same
// shared-placeholder race from two goroutines under -race so the data-race
// detector exercises the refcounted reserve/release/store path concurrently.
func TestDaemonSessionStore_SharedReservationRefcount_Concurrent(t *testing.T) {
	st := &daemonSessionStore{}
	const id = "race-fresh-id"

	// Pre-reserve so BOTH goroutines observe the placeholder as already present
	// (the contended path: reserveSlot increments instead of creating). Both then
	// race release (A's failed handshake) vs store (B's success). Whichever order
	// the scheduler picks, the binding must end live (B's store wins) and the
	// store must never be rejected.
	if !st.reserveSlot(id) { // A
		t.Fatalf("pre-reserve A returned false")
	}
	if !st.reserveSlot(id) { // B
		t.Fatalf("pre-reserve B returned false")
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var storeOK bool
	go func() {
		defer wg.Done()
		st.releaseReservation(id) // A's handshake failed
	}()
	go func() {
		defer wg.Done()
		storeOK = st.store(id, "alpha", "daemon-race", defaultProtocolVersion) // B's handshake succeeded
	}()
	wg.Wait()

	if !storeOK {
		t.Fatalf("concurrent store lost the race and was rejected; want it to succeed (reservation held the slot)")
	}
	gotDS, _, ok := st.lookup(id, "alpha")
	if !ok || gotDS != "daemon-race" {
		t.Fatalf("post-race lookup = (%q, ok=%v); want (\"daemon-race\", ok=true)", gotDS, ok)
	}
}

// TestDaemonSessionStore_ReclaimExpiredBeforeCap covers Codex PR #251 finding 2:
// a store sitting at maxRouterSessions with COMPLETED bindings that are all idle
// past daemonSessionTTL must reclaim those expired entries before applying the
// cap check, so a fresh reserveSlot succeeds rather than returning false (429)
// until the next periodic sweep. The clock seam injects the aged lastSeen and
// then advances time past the TTL.
func TestDaemonSessionStore_ReclaimExpiredBeforeCap(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	clk := base
	st := &daemonSessionStore{clock: func() time.Time { return clk }}

	// Fill to cap with COMPLETED bindings, all stamped lastSeen = base.
	for i := 0; i < maxRouterSessions; i++ {
		if !st.store(fmt.Sprintf("client-%06d", i), "alpha", fmt.Sprintf("daemon-%06d", i), defaultProtocolVersion) {
			t.Fatalf("store %d while filling to cap returned false", i)
		}
	}
	if got := len(st.bindings); got != maxRouterSessions {
		t.Fatalf("store size after fill = %d, want %d", got, maxRouterSessions)
	}

	// Sanity: at the SAME clock, a fresh reserveSlot is rejected (nothing expired
	// yet, store is full).
	if st.reserveSlot("fresh-before-expiry") {
		t.Fatalf("reserveSlot succeeded at cap with no expired entries; want rejection")
	}

	// Advance the clock just past the idle TTL so every filled binding is now
	// idle-expired. A fresh reserveSlot must reclaim them and succeed.
	clk = base.Add(daemonSessionTTL + time.Second)
	if !st.reserveSlot("fresh-after-expiry") {
		t.Fatalf("reserveSlot rejected after all entries idle-expired; want success via reclaim-before-cap")
	}
	// After reclaim of all maxRouterSessions expired entries + inserting the one
	// fresh placeholder, exactly one binding remains.
	if got := len(st.bindings); got != 1 {
		t.Fatalf("store size after reclaim = %d, want 1 (all expired reclaimed, one fresh reservation)", got)
	}
	if _, ok := st.bindings["fresh-after-expiry"]; !ok {
		t.Fatalf("fresh reservation missing after reclaim; bindings=%v", st.bindings)
	}
}

// TestDaemonSessionStore_ReclaimSkipsInFlightPlaceholder asserts that when
// reclaim fires under cap pressure it never drops an in-flight placeholder
// (daemonSessionID == "" with a live reservation): those are active handshakes,
// not idle sessions, regardless of how old their lastSeen is. The store is held
// at cap by ONE in-flight placeholder plus (cap-1) COMPLETED idle-expired
// bindings; a fresh reserveSlot must reclaim ONLY the completed-expired entries
// and succeed, leaving the placeholder intact.
func TestDaemonSessionStore_ReclaimSkipsInFlightPlaceholder(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	clk := base
	st := &daemonSessionStore{clock: func() time.Time { return clk }}

	// One in-flight reservation stamped at base (a slow handshake placeholder).
	if !st.reserveSlot("in-flight") {
		t.Fatalf("reserveSlot for in-flight placeholder returned false")
	}
	// Fill the remaining cap-1 slots with COMPLETED bindings, also stamped base.
	for i := 0; i < maxRouterSessions-1; i++ {
		if !st.store(fmt.Sprintf("completed-%06d", i), "alpha", fmt.Sprintf("daemon-%06d", i), defaultProtocolVersion) {
			t.Fatalf("store %d while filling to cap returned false", i)
		}
	}
	if got := len(st.bindings); got != maxRouterSessions {
		t.Fatalf("store size after fill = %d, want %d (1 placeholder + %d completed)", got, maxRouterSessions, maxRouterSessions-1)
	}

	// Advance well past the TTL so the COMPLETED entries are idle-expired but the
	// placeholder (never idle) is not eligible for reclaim. A fresh reserveSlot at
	// cap must reclaim the completed entries and succeed.
	clk = base.Add(daemonSessionTTL * 10)
	if !st.reserveSlot("fresh-at-cap") {
		t.Fatalf("reserveSlot rejected at cap despite cap-1 reclaimable completed entries; want success")
	}
	// The in-flight placeholder must survive the reclaim.
	if b, ok := st.bindings["in-flight"]; !ok || b == nil || b.daemonSessionID != "" {
		t.Fatalf("reclaim dropped (or mutated) the in-flight placeholder; it must be retained untouched (active handshake, not idle)")
	}
	// After reclaiming all cap-1 completed entries and inserting the fresh
	// reservation, exactly 2 bindings remain: the surviving placeholder + the new
	// reservation.
	if got := len(st.bindings); got != 2 {
		t.Fatalf("store size after reclaim = %d, want 2 (surviving in-flight placeholder + fresh reservation)", got)
	}
}

func daemonSessionCount(s *Server) int {
	s.serenaDaemonSessions.mu.Lock()
	defer s.serenaDaemonSessions.mu.Unlock()
	return len(s.serenaDaemonSessions.bindings)
}
