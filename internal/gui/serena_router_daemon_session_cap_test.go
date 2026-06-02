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
	if proceed, _ := st.reserveSlot("overflow-reserve"); proceed {
		t.Fatalf("reserveSlot on a fresh id succeeded at cap; want rejection")
	}
	// client-000001 is still a COMPLETED binding (unbound below) → reusable.
	if proceed, inFlight := st.reserveSlot("client-000001"); !proceed || inFlight {
		t.Fatalf("reserveSlot on an existing completed id = (proceed=%v, inFlight=%v); want (true, false) — completed ids stay reusable", proceed, inFlight)
	}

	st.unbind("client-000001")
	if proceed, _ := st.reserveSlot("reserved-new"); !proceed {
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

// TestDaemonSessionStore_InFlightPlaceholderRejectsDuplicate covers bot PR #251 r2
// P1 (cap-bypass via same-id burst): a SECOND concurrent first request for the SAME
// fresh id, while the first's handshake is still in flight (placeholder,
// daemonSessionID==""), is REJECTED with inFlight=true — it must NOT proceed to
// mint a second upstream daemon session for the one local slot. After the first
// completes (store), the id is reusable via lookup.
func TestDaemonSessionStore_InFlightPlaceholderRejectsDuplicate(t *testing.T) {
	st := &daemonSessionStore{}
	const id = "burst-fresh-id"
	if proceed, inFlight := st.reserveSlot(id); !proceed || inFlight {
		t.Fatalf("first reserveSlot = (proceed=%v, inFlight=%v); want (true, false)", proceed, inFlight)
	}
	if proceed, inFlight := st.reserveSlot(id); proceed || !inFlight {
		t.Fatalf("second reserveSlot on an in-flight placeholder = (proceed=%v, inFlight=%v); want (false, true)", proceed, inFlight)
	}
	// Still exactly one placeholder; the rejected duplicate did NOT refcount it.
	if got := len(st.bindings); got != 1 {
		t.Fatalf("bindings = %d, want 1 (the rejected duplicate must not create/refcount a slot)", got)
	}
	if b := st.bindings[id]; b == nil || b.reservations != 1 {
		t.Fatalf("placeholder reservations = %v, want 1 (only the first caller holds it)", b)
	}
	// First caller completes its handshake → the id is now reusable.
	if !st.store(id, "alpha", "daemon-burst", defaultProtocolVersion) {
		t.Fatalf("store for the first caller returned false; want true")
	}
	if dsid, _, ok := st.lookup(id, "alpha"); !ok || dsid != "daemon-burst" {
		t.Fatalf("lookup after store = (%q, %v); want (daemon-burst, true)", dsid, ok)
	}
}

// TestDaemonSessionStore_WorkspaceSwitchSerialized covers bot PR #251 r3 P1: a
// workspace-switch re-handshake on a COMPLETED binding reserves it (the FIRST
// switch proceeds); a CONCURRENT second switch for the same id finds reservations>0
// and is rejected as in-flight — it must NOT mint a second upstream session. After
// the first switch releases, the next switch may proceed.
func TestDaemonSessionStore_WorkspaceSwitchSerialized(t *testing.T) {
	st := &daemonSessionStore{}
	const id = "switch-id"
	if !st.store(id, "alpha", "daemon-alpha", defaultProtocolVersion) {
		t.Fatalf("initial store returned false")
	}
	// First switch (lookup missed on a different wsKey) reserves the completed binding.
	if proceed, inFlight := st.reserveSlot(id); !proceed || inFlight {
		t.Fatalf("first switch reserveSlot = (proceed=%v, inFlight=%v); want (true, false)", proceed, inFlight)
	}
	// A CONCURRENT second switch for the same id is in-flight-suppressed.
	if proceed, inFlight := st.reserveSlot(id); proceed || !inFlight {
		t.Fatalf("concurrent second switch = (proceed=%v, inFlight=%v); want (false, true) — one switch handshake at a time", proceed, inFlight)
	}
	// The first switch fails and releases; the completed binding survives and a
	// fresh switch may now proceed.
	st.releaseReservation(id)
	if dsid, _, ok := st.lookup(id, "alpha"); !ok || dsid != "daemon-alpha" {
		t.Fatalf("completed binding dropped after the in-flight switch released; lookup=(%q,%v)", dsid, ok)
	}
	if proceed, inFlight := st.reserveSlot(id); !proceed || inFlight {
		t.Fatalf("switch after release = (proceed=%v, inFlight=%v); want (true, false) — serialization frees up", proceed, inFlight)
	}
}

// TestDaemonSessionStore_SweepSkipsReservedBinding covers bot PR #251 r3 P2: the
// periodic cleanup sweep must NOT drop a binding with an active reservation (an
// in-flight workspace switch on a near-expired completed binding) even when its
// lastSeen is past the TTL — mirroring reclaimExpiredLocked.
func TestDaemonSessionStore_SweepSkipsReservedBinding(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	clk := base
	st := &daemonSessionStore{clock: func() time.Time { return clk }}
	const id = "near-expired-switch"
	if !st.store(id, "alpha", "daemon-alpha", defaultProtocolVersion) {
		t.Fatalf("store returned false")
	}
	if proceed, _ := st.reserveSlot(id); !proceed { // a workspace-switch handshake reserves it
		t.Fatalf("switch reserveSlot returned false")
	}
	// The hourly sweep runs with the clock past the TTL (the binding's lastSeen is
	// stale because the wsKey miss did not refresh it).
	if swept := st.cleanup(base.Add(daemonSessionTTL+time.Hour), daemonSessionTTL); swept != 0 {
		t.Errorf("sweep dropped %d bindings; want 0 (the reserved in-flight switch must survive)", swept)
	}
	if _, ok := st.bindings[id]; !ok {
		t.Fatalf("the reserved binding was swept; it must survive while a reservation is held")
	}
}

// TestDaemonSessionStore_LookupSkipsReservedExpired covers bot PR #251 r4 P2: a
// concurrent lookup (for the OLD workspace) that hits an idle-expired binding which
// is RESERVED by an in-flight workspace switch must NOT remove it via expire-on-read
// — that would free the reserved slot and delete the serialization state, 429ing the
// switch's store after it already minted an upstream session.
func TestDaemonSessionStore_LookupSkipsReservedExpired(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	clk := base
	st := &daemonSessionStore{clock: func() time.Time { return clk }}
	const id = "switch-near-expiry"
	if !st.store(id, "alpha", "daemon-alpha", defaultProtocolVersion) {
		t.Fatalf("store returned false")
	}
	if proceed, _ := st.reserveSlot(id); !proceed { // a workspace-switch handshake reserves it
		t.Fatalf("switch reserveSlot returned false")
	}
	st.bindings[id].lastSeen = base // pin the binding to the OLD timestamp (near-expiry)

	// Advance past the TTL, then a concurrent request for the OLD workspace looks it
	// up (expire-on-read). The reserved binding must survive as a miss, not be removed.
	clk = base.Add(daemonSessionTTL + time.Hour)
	if _, _, ok := st.lookup(id, "alpha"); ok {
		t.Errorf("lookup returned a hit for an idle-expired binding; want a miss")
	}
	if _, ok := st.bindings[id]; !ok {
		t.Fatalf("lookup removed a RESERVED idle-expired binding; the in-flight switch must keep its slot")
	}
}

// TestDaemonSessionStore_SameIdBurst_OnlyOneProceeds drives the bot PR #251 r2 P1
// cap-bypass scenario under -race: N concurrent first requests for ONE fresh id —
// exactly ONE proceeds (creates the placeholder + would mint an upstream session);
// the rest are rejected with inFlight (no duplicate upstream sessions).
func TestDaemonSessionStore_SameIdBurst_OnlyOneProceeds(t *testing.T) {
	st := &daemonSessionStore{}
	const id = "burst-id"
	const N = 16
	var (
		mu        sync.Mutex
		proceedN  int
		inFlightN int
		otherN    int
	)
	var wg sync.WaitGroup
	wg.Add(N)
	for range [N]struct{}{} {
		go func() {
			defer wg.Done()
			proceed, inFlight := st.reserveSlot(id)
			mu.Lock()
			switch {
			case proceed:
				proceedN++
			case inFlight:
				inFlightN++
			default:
				otherN++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if proceedN != 1 {
		t.Errorf("proceed count = %d, want exactly 1 (only one handshake may mint an upstream session)", proceedN)
	}
	if inFlightN != N-1 {
		t.Errorf("inFlight count = %d, want %d (all duplicates rejected)", inFlightN, N-1)
	}
	if otherN != 0 {
		t.Errorf("other (cap-full) count = %d, want 0 (well below cap)", otherN)
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
	if proceed, _ := st.reserveSlot("fresh-before-expiry"); proceed {
		t.Fatalf("reserveSlot succeeded at cap with no expired entries; want rejection")
	}

	// Advance the clock just past the idle TTL so every filled binding is now
	// idle-expired. A fresh reserveSlot must reclaim them and succeed.
	clk = base.Add(daemonSessionTTL + time.Second)
	if proceed, _ := st.reserveSlot("fresh-after-expiry"); !proceed {
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
	if proceed, _ := st.reserveSlot("in-flight"); !proceed {
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
	if proceed, _ := st.reserveSlot("fresh-at-cap"); !proceed {
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
