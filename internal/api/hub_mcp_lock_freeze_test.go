// hub_mcp_lock_freeze_test.go — regression guard for the "lock held across
// network I/O freezes the whole gate-ON hub session store" concurrency defect.
//
// Defect (pre-fix): refreshStalePortBeforeDispatch held the per-session
// per-port stalePortState.mu across the blocking MCP initialize handshake
// (reinitDaemonSession). HubSessionStore.MarkPortStale acquires the store
// RLock and, per session, locks that SAME per-port mutex (markStalePort). So a
// MarkPortStale landing during an in-flight proactive reinit blocked on the
// per-port mutex WHILE holding the store RLock; every hot-path store writer
// (Touch/GetAndTouch/Create/sweep) then queued behind that stuck RLock, and
// Go's RWMutex starves subsequent RLock-ers once a writer queues — freezing the
// ENTIRE store (all clients, all aggregated MCP traffic) for the handshake's
// duration (~5-10s).
//
// Fix: (1) refreshStalePortBeforeDispatch never holds state.mu across the
// handshake — it decides reuse-vs-reinit with a monotone generation compare
// (cached InitSuccessGen stamp vs the port's current generation), and the reinit
// runs singleflight-deduped WITHOUT any lock held; (2) MarkPortStale snapshots
// the session pointers under the store RLock and RELEASES it before calling
// markStalePort, so the store lock is never held behind a per-port lock. See
// work-items/decisions/2026-07-04-generation-stamped-hub-cache.md.
//
// This test reproduces the freeze CLASS: it holds a proactive reinit handshake
// open for a bounded window and asserts a concurrent MarkPortStale AND a
// concurrent Touch on an UNRELATED session both complete far inside that window
// (they no longer wait for the handshake).
//
// Negative control precision: reverting fix #1 (holding state.mu across the
// handshake again) makes the concurrent MarkPortStale block on that per-port
// mutex, so the overall deadline fires via `gotMark==false` (with the store
// itself — hence Touch — still free, because fix #2 already decoupled the store
// RLock). Reverting BOTH fixes reproduces the full store freeze (Touch blocks
// too). The singleflight-follower lost-restart (a restart landing mid-flight, so
// a coalesced follower must not stamp the cache fresh for the superseded
// generation) is covered by TestRefreshStalePortLoopReinitsPastMidFlightRestart.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRefreshStalePortDoesNotFreezeStoreDuringReinit(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	const oldSID = "freeze-old-sid"
	const freshSID = "freeze-fresh-sid"
	const proto = "2025-11-25"

	// gate blocks the daemon's initialize so the proactive reinit handshake is
	// held open for a bounded window. Released exactly once.
	gate := make(chan struct{})
	var gateOnce sync.Once
	releaseGate := func() { gateOnce.Do(func() { close(gate) }) }
	t.Cleanup(releaseGate)

	blockingDaemon := newStubDaemon(t, freshSID)
	blockingDaemon.onInit = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
			return // server teardown / request-ctx cancel: don't wedge Close
		}
		w.Header().Set("Mcp-Session-Id", freshSID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"stub","version":"1"}}}`))
	}

	port := blockingDaemon.port

	// A large sweep interval keeps the background sweeper out of the picture for
	// this sub-second test.
	store := NewHubSessionStore(SessionStoreOpts{MaxPerClient: 16, MaxGlobal: 256, SweepInterval: time.Hour})
	t.Cleanup(store.Close)

	// sess1: the session whose stale port triggers the (blocked) proactive reinit.
	sess1 := sessionWithParticipants(blockingDaemon)
	sess1.ClientSessionID = "freeze-sess-A"
	ref := sess1.IntendedParticipants[0] // {srv1, claude-code, port}
	sess1.InitSuccesses[ref] = oldSID
	sess1.DaemonProtoVer[ref] = proto
	if !sess1.markStalePort(port) {
		t.Fatalf("setup: daemon port %d was not tracked as stale on sess1", port)
	}

	// sess2: an UNRELATED session on a different client. Its Touch must not
	// block behind sess1's per-port reinit lock.
	sess2 := &hubSession{
		ClientSessionID:  "freeze-sess-B",
		ScopeKey:         "codex-cli",
		ProtocolVersion:  proto,
		InitSuccesses:    map[canonicalDaemonRef]string{},
		DaemonProtoVer:   map[canonicalDaemonRef]string{},
		InFlightRequests: map[requestIDKey]inflightEntry{},
		InitAt:           time.Now(),
		LastUsedAt:       time.Now(),
	}

	store.mu.Lock()
	for _, s := range []*hubSession{sess1, sess2} {
		store.sessions[s.ClientSessionID] = s
		store.perClient[s.ScopeKey]++
		store.lruIndex[s.ClientSessionID] = store.lru.PushFront(s.ClientSessionID)
	}
	store.mu.Unlock()

	// A: trigger the proactive refresh. It blocks inside the daemon initialize
	// (on `gate`) with the fix in place having ALREADY released state.mu.
	callRef := canonicalToolRef{Server: ref.Server, Daemon: ref.Daemon, Port: port, RawName: "read"}
	refreshReturned := make(chan struct{})
	go func() {
		defer close(refreshReturned)
		sess1.refreshStalePortBeforeDispatch(context.Background(), callRef, oldSID, proto)
	}()

	// Barrier: wait until the handshake actually reached the daemon initialize,
	// so the proactive reinit is genuinely in flight before we probe the store.
	startDeadline := time.Now().Add(3 * time.Second)
	for blockingDaemon.initCount.Load() == 0 {
		if time.Now().After(startDeadline) {
			t.Fatal("proactive reinit never reached the daemon initialize")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Fire the two hot-path store operations concurrently with the in-flight
	// (blocked) handshake. Neither must wait for the handshake.
	const opDeadline = 2 * time.Second // << PerDaemonInitTimeout (5s), so a freeze fails the deadline
	markDone := make(chan time.Duration, 1)
	go func() {
		t0 := time.Now()
		store.MarkPortStale(port)
		markDone <- time.Since(t0)
	}()
	touchDone := make(chan bool, 1)
	go func() {
		touchDone <- store.Touch(sess2.ClientSessionID)
	}()

	overall := time.After(opDeadline)
	var markDur time.Duration
	gotMark, gotTouch := false, false
	for !gotMark || !gotTouch {
		select {
		case markDur = <-markDone:
			gotMark = true
		case ok := <-touchDone:
			if !ok {
				t.Fatalf("Touch(sess2) returned false — unrelated session vanished")
			}
			gotTouch = true
		case <-overall:
			// The store froze: at least one hot-path op is still waiting behind
			// the in-flight reinit handshake. This is exactly the reverted-fix
			// (negative-control) behavior.
			t.Fatalf("store froze during in-flight proactive reinit: MarkPortStale done=%v, Touch done=%v within %s", gotMark, gotTouch, opDeadline)
		}
	}
	if markDur >= opDeadline {
		t.Fatalf("MarkPortStale took %s (>= %s) — blocked behind the reinit handshake", markDur, opDeadline)
	}

	// Positive path: release the handshake and confirm the proactive refresh
	// completes cleanly.
	releaseGate()
	select {
	case <-refreshReturned:
	case <-time.After(PerDaemonInitTimeout + 3*time.Second):
		t.Fatal("refreshStalePortBeforeDispatch did not return after gate release")
	}

	// Contract preserved #1: the stale port got a FRESH binding published to the
	// session cache. This is the whole point of the proactive path — a tools/call
	// that finds a stale port must dispatch against the re-initialized session.
	if cached, ok := sess1.cachedDaemonInitState(ref); !ok || cached.SessionID != freshSID {
		t.Fatalf("proactive reinit did not cache the fresh session id: cached=%+v ok=%v want %q", cached, ok, freshSID)
	}

	// Contract preserved #2: the MarkPortStale we fired DURING the handshake bumped
	// the port generation. The first reinit stamps the FLIGHT's (older) generation,
	// so refreshStalePortBeforeDispatch's loop detects the cached sid is stale for
	// the newer generation and reinits AGAIN — ending with a sid FRESH for the
	// current generation (the caller never dispatches a stale sid). Releasing
	// state.mu is what let the concurrent MarkPortStale interleave here at all.
	if portStaleForTest(t, sess1, ref) {
		t.Fatalf("the loop did not reinit past the concurrent MarkPortStale — the port is still stale for the current generation")
	}
}

// TestRefreshStalePortLoopReinitsPastMidFlightRestart covers the reuse/reinit
// loop's re-validation (Codex #500 P2): a restart landing DURING a reinit flight
// makes that flight's sid stale for the newer daemon incarnation. The flight
// stamps its OWN start generation (the durable fable Defect 1 fix), so the loop
// detects stampGen < curGen and reinits AGAIN, dispatching a sid FRESH for the
// current generation instead of returning the stale one (which the restarted
// daemon would reject with a non-retryable -32000). This subsumes the old
// singleflight-follower scenario: whether one caller or a coalesced follower, the
// flight stamps its own generation and the loop converges to the current one.
func TestRefreshStalePortLoopReinitsPastMidFlightRestart(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	const oldSID = "loop-old-sid"
	const proto = "2025-11-25"

	// gate blocks ONLY the first flight so a restart can land while it is in flight.
	gate := make(chan struct{})
	var gateOnce sync.Once
	releaseGate := func() { gateOnce.Do(func() { close(gate) }) }
	t.Cleanup(releaseGate)

	daemon := newStubDaemon(t, "unused")
	daemon.onInit = func(w http.ResponseWriter, r *http.Request) {
		if daemon.initCount.Load() == 1 { // only the FIRST flight blocks
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		// Distinct sid per flight so the returned sid identifies WHICH flight served
		// it: the pre-restart flight (loop-sid-1, stale) or the post-restart one (2).
		sid := fmt.Sprintf("loop-sid-%d", daemon.initCount.Load())
		w.Header().Set("Mcp-Session-Id", sid)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"stub","version":"1"}}}`))
	}
	port := daemon.port

	sess := sessionWithParticipants(daemon)
	ref := sess.IntendedParticipants[0]
	sess.InitSuccesses[ref] = oldSID
	sess.DaemonProtoVer[ref] = proto
	callRef := canonicalToolRef{Server: ref.Server, Daemon: ref.Daemon, Port: port, RawName: "read"}

	if !sess.markStalePort(port) { // R1: generation 0->1
		t.Fatalf("setup: port %d not tracked", port)
	}

	returned := make(chan string, 1)
	go func() {
		sid, _, _ := sess.refreshStalePortBeforeDispatch(context.Background(), callRef, oldSID, proto)
		returned <- sid
	}()

	// Wait until the FIRST flight is genuinely in-flight (blocked on the gate).
	deadline := time.Now().Add(3 * time.Second)
	for daemon.initCount.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first flight never reached the daemon initialize")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// R2: a restart lands WHILE the first flight is in flight (generation 1->2).
	if !sess.markStalePort(port) {
		t.Fatal("R2 markStalePort failed")
	}
	releaseGate()

	var sid string
	select {
	case sid = <-returned:
	case <-time.After(PerDaemonInitTimeout + 3*time.Second):
		t.Fatal("refreshStalePortBeforeDispatch did not return")
	}

	// The first flight (stamped generation 1) is stale for R2, so the loop reinited
	// a SECOND time (generation 2) and returned THAT sid - never the stale one.
	if sid != "loop-sid-2" {
		t.Fatalf("loop returned %q; want the post-restart flight sid %q (a pre-restart sid would dispatch to a superseded daemon -> -32000)", sid, "loop-sid-2")
	}
	if got := daemon.initCount.Load(); got != 2 {
		t.Fatalf("expected 2 flights (pre- and post-restart), got %d", got)
	}
	if portStaleForTest(t, sess, ref) {
		t.Fatal("port still stale after the loop - the reinit did not converge to the current generation")
	}
}

// TestRefreshStalePortGenerationStampPredicate directly pins the core decision of
// the generation-stamped cache: reuse the cached sid iff its InitSuccessGen stamp
// is >= the port's current restart generation; otherwise reinit. This is the one
// predicate that replaced the stale-bool + the three cross-lock re-checks.
func TestRefreshStalePortGenerationStampPredicate(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })
	const cachedSID = "stamp-cached-sid"
	const reinitSID = "stamp-reinit-sid"
	const proto = "2025-11-25"

	t.Run("fresh_stamp_reuses_no_dial", func(t *testing.T) {
		daemon := newStubDaemon(t, reinitSID)
		sess := sessionWithParticipants(daemon)
		ref := sess.IntendedParticipants[0]
		callRef := canonicalToolRef{Server: ref.Server, Daemon: ref.Daemon, Port: daemon.port, RawName: "read"}
		sess.InitSuccesses[ref] = cachedSID
		sess.DaemonProtoVer[ref] = proto
		if !sess.markStalePort(daemon.port) { // gen 1
			t.Fatalf("setup: port %d not tracked", daemon.port)
		}
		sess.mu.Lock()
		sess.InitSuccessGen = map[canonicalDaemonRef]uint64{ref: 1} // stamp == curGen
		sess.mu.Unlock()

		sid, _, _ := sess.refreshStalePortBeforeDispatch(context.Background(), callRef, "caller-sid", proto)
		if sid != cachedSID {
			t.Fatalf("fresh stamp (>= curGen) must REUSE the cached sid; got %q want %q", sid, cachedSID)
		}
		if got := daemon.initCount.Load(); got != 0 {
			t.Fatalf("fresh stamp must NOT dial a reinit; initCount=%d", got)
		}
	})

	t.Run("stale_stamp_reinits_once", func(t *testing.T) {
		daemon := newStubDaemon(t, reinitSID)
		sess := sessionWithParticipants(daemon)
		ref := sess.IntendedParticipants[0]
		callRef := canonicalToolRef{Server: ref.Server, Daemon: ref.Daemon, Port: daemon.port, RawName: "read"}
		sess.InitSuccesses[ref] = cachedSID
		sess.DaemonProtoVer[ref] = proto
		if !sess.markStalePort(daemon.port) { // gen 1
			t.Fatalf("setup: port %d not tracked", daemon.port)
		}
		sess.markStalePort(daemon.port) // gen 2
		sess.mu.Lock()
		sess.InitSuccessGen = map[canonicalDaemonRef]uint64{ref: 1} // stamp 1 < curGen 2
		sess.mu.Unlock()

		sid, _, _ := sess.refreshStalePortBeforeDispatch(context.Background(), callRef, "caller-sid", proto)
		if sid != reinitSID {
			t.Fatalf("stale stamp (< curGen) must REINIT; got %q want fresh %q", sid, reinitSID)
		}
		if got := daemon.initCount.Load(); got != 1 {
			t.Fatalf("stale stamp must dial exactly one reinit; initCount=%d", got)
		}
		// The reinit re-stamps the cache with the flight generation (2 == curGen) →
		// the port now reads fresh.
		if portStaleForTest(t, sess, ref) {
			t.Fatalf("after a reinit at the current generation the port must read fresh")
		}
	})
}

// TestRefreshStalePortExhaustionReturnsFreshestCached covers fable-review Defect 1
// (the off-by-one): under a restart storm (a restart landing during EACH reinit)
// the loop exhausts, but it must return the FRESHEST CACHED sid (the last reinit's
// result) — NOT the caller's pre-all-restarts snapshot, which would be dispatched
// to a superseded daemon and rejected with a non-retryable -32000.
func TestRefreshStalePortExhaustionReturnsFreshestCached(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	const callerSID = "exhaust-caller-sid"
	const proto = "2025-11-25"

	daemon := newStubDaemon(t, "unused")
	var sess *hubSession
	daemon.onInit = func(w http.ResponseWriter, r *http.Request) {
		n := daemon.initCount.Load()
		// A restart lands during EVERY reinit: bump the generation so the flight's
		// stamp is always < the current generation → the loop never converges and
		// exhausts.
		sess.markStalePort(daemon.port)
		sid := fmt.Sprintf("exhaust-sid-%d", n)
		w.Header().Set("Mcp-Session-Id", sid)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"stub","version":"1"}}}`))
	}
	port := daemon.port

	sess = sessionWithParticipants(daemon)
	ref := sess.IntendedParticipants[0]
	sess.InitSuccesses[ref] = callerSID
	sess.DaemonProtoVer[ref] = proto
	if !sess.markStalePort(port) { // R1
		t.Fatalf("setup: port %d not tracked", port)
	}
	callRef := canonicalToolRef{Server: ref.Server, Daemon: ref.Daemon, Port: port, RawName: "read"}

	sid, _, _ := sess.refreshStalePortBeforeDispatch(context.Background(), callRef, callerSID, proto)

	// Exhausted after maxProactiveReinitAttempts reinits. The returned sid must be a
	// reinit RESULT (the freshest cached), never the caller's original snapshot.
	if sid == callerSID {
		t.Fatalf("exhaustion returned the caller's pre-restart snapshot %q (fable Defect 1 off-by-one) — a fresh reinit sid was discarded", callerSID)
	}
	if got := daemon.initCount.Load(); got != maxProactiveReinitAttempts {
		t.Fatalf("expected exactly %d reinits before exhaustion, got %d", maxProactiveReinitAttempts, got)
	}
	// The last reinit's sid is the freshest cached one.
	want := fmt.Sprintf("exhaust-sid-%d", int32(maxProactiveReinitAttempts))
	if sid != want {
		t.Fatalf("exhaustion returned %q; want the freshest cached (last reinit) sid %q", sid, want)
	}
}

// TestRefreshStalePortConvergesWhenDaemonMovedPort covers fable-review Defect 2: a
// dynamic-pool daemon (serena) comes back on a NEW port. The flight caches under
// the RESOLVED port and the same-daemon-identity sweep deletes the old-port entry;
// if the loop kept re-reading the stale ref.Port it would never see the fresh
// entry and reinit forever (extra handshakes + orphaned sessions). The loop
// re-resolves the port each pass, so it converges in one reinit.
func TestRefreshStalePortConvergesWhenDaemonMovedPort(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	const oldPort = 49150
	const freshSID = "moved-fresh-sid"
	const oldSID = "moved-old-sid"
	const proto = "2025-11-25"

	daemon := newStubDaemon(t, freshSID) // runs at the NEW (moved-to) port
	newPort := daemon.port

	sess := sessionWithParticipants() // build the (Server,Daemon) binding manually
	sess.ScopeKey = "claude-code"
	oldRef := canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: oldPort}
	sess.InitSuccesses[oldRef] = oldSID
	sess.DaemonProtoVer[oldRef] = proto
	if !sess.markStalePort(oldPort) { // the OLD port was marked stale (gen 1)
		t.Fatalf("setup: old port %d not tracked", oldPort)
	}
	// Live resolver snapshot: the daemon moved (srv1,claude-code) → newPort.
	resolverSnapshot.Store(&ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{sess.ScopeKey: {{Server: "srv1", Daemon: "claude-code", Port: newPort}}},
	})

	callRef := canonicalToolRef{Server: "srv1", Daemon: "claude-code", Port: oldPort, RawName: "read"}
	sid, _, resolvedPort := sess.refreshStalePortBeforeDispatch(context.Background(), callRef, oldSID, proto)

	if sid != freshSID {
		t.Fatalf("moved-port reinit did not converge to the fresh sid; got %q want %q", sid, freshSID)
	}
	// The RESOLVED port must be returned to the caller so dispatchToolsCall posts
	// to the daemon the fresh sid was minted for (newPort), not the stale ref.Port
	// (oldPort) — Codex #500 r6 P2. A fresh sid dispatched to oldPort would target
	// a dead/absent daemon.
	if resolvedPort != newPort {
		t.Fatalf("refresh returned port %d; want the moved-to port %d so dispatch targets the fresh daemon (a stale %d would orphan the call)", resolvedPort, newPort, oldPort)
	}
	if got := daemon.initCount.Load(); got != 1 {
		t.Fatalf("moved-port must reinit exactly ONCE (no loop-forever on the deleted old-port key); got %d handshakes", got)
	}
}

// TestDispatchToolsCallPostsToResolvedMovedPort is the END-TO-END guard for Codex
// #500 r6 P2: it drives the full dispatchToolsCall path (not just the refresh
// helper) for a daemon that moved P1→P2 and proves the tools/call HTTP POST lands
// on the RESOLVED port (P2), not the stale ref.Port (P1). Before the fix,
// refreshStalePortBeforeDispatch cached the fresh sid under P2 but returned only
// (sid, proto); dispatch then posted that fresh sid to P1 → a dead/absent daemon.
func TestDispatchToolsCallPostsToResolvedMovedPort(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	const oldPort = 49148
	const freshSID = "e2e-moved-sid"
	const oldSID = "e2e-old-sid"
	const proto = "2025-11-25"

	newDaemon := newStubDaemon(t, freshSID) // the moved-to daemon: serves reinit + tools/call
	newPort := newDaemon.port

	sess := sessionWithParticipants()
	sess.ScopeKey = "claude-code"
	sess.ProtocolVersion = proto
	oldKey := canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: oldPort}
	sess.InitSuccesses[oldKey] = oldSID
	sess.DaemonProtoVer[oldKey] = proto
	if !sess.markStalePort(oldPort) { // old port marked stale (gen 1)
		t.Fatalf("setup: old port %d not tracked", oldPort)
	}
	// Live resolver: (srv1,claude-code) now bound to newPort.
	resolverSnapshot.Store(&ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{sess.ScopeKey: {{Server: "srv1", Daemon: "claude-code", Port: newPort}}},
	})

	ref := canonicalToolRef{Server: "srv1", Daemon: "claude-code", Port: oldPort, RawName: "read"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	params := json.RawMessage(`{"name":"srv1__read","arguments":{}}`)

	body, err := dispatchToolsCall(ctx, sess, json.RawMessage(`1`), params, ref, oldSID, proto)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(string(body), "called=read") {
		t.Fatalf("dispatch body missing called=read: %s", body)
	}
	// The tools/call MUST have hit the RESOLVED (moved-to) port exactly once.
	if got := newDaemon.callCount.Load(); got != 1 {
		t.Fatalf("moved-to daemon callCount=%d want 1 — dispatch must POST to the RESOLVED port, not the stale ref.Port", got)
	}
	// initCount == 1 is the load-bearing assertion (fable-review, r7): callCount
	// and body alone are VACUOUS because the reactive selfHealRetry layer silently
	// repairs the pre-fix wiring — posting the fresh sid to the dead oldPort fails
	// with a retriable transport error, selfHealRetry re-resolves to newPort and
	// reinits a SECOND time, so callCount still lands at 1. The fix does exactly
	// ONE proactive handshake (dispatch posts straight to newPort → success → no
	// selfHeal); the pre-fix path does TWO (proactive + reactive). Asserting the
	// handshake COUNT — the mechanism, not the converged outcome — is what
	// distinguishes fixed (1) from unfixed (2).
	if got := newDaemon.initCount.Load(); got != 1 {
		t.Fatalf("moved-to daemon initCount=%d want 1 — exactly one proactive handshake; 2 means dispatch posted to the stale port and the reactive selfHealRetry silently re-handshook (the fix was not applied)", got)
	}
}

// TestRefreshStalePortReinitFailReturnsOriginalPort guards Codex #500 r7 P2 (the
// pairing invariant): when the daemon moved P1→P2 but the proactive reinit at the
// resolved port FAILS, the helper returns the caller's ORIGINAL sid — which was
// minted for ref.Port — and it MUST be paired with ref.Port, not the resolved
// port. Pairing the old sid with the moved-to daemon (which never issued it) would
// hard-fail non-retriably; pairing it with the (now-dead) original port lets the
// dial-refused transport failure reach selfHealRetry for recovery.
func TestRefreshStalePortReinitFailReturnsOriginalPort(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	const oldPort = 49147
	const oldSID = "reinitfail-old-sid"
	const proto = "2025-11-25"

	movedDaemon := newStubDaemon(t, "unused") // moved-to daemon whose initialize FAILS
	movedDaemon.onInit = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // reinit at the resolved port fails
	}
	newPort := movedDaemon.port

	sess := sessionWithParticipants()
	sess.ScopeKey = "claude-code"
	sess.ProtocolVersion = proto
	oldKey := canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: oldPort}
	sess.InitSuccesses[oldKey] = oldSID
	sess.DaemonProtoVer[oldKey] = proto
	if !sess.markStalePort(oldPort) {
		t.Fatalf("setup: old port %d not tracked", oldPort)
	}
	resolverSnapshot.Store(&ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{sess.ScopeKey: {{Server: "srv1", Daemon: "claude-code", Port: newPort}}},
	})

	callRef := canonicalToolRef{Server: "srv1", Daemon: "claude-code", Port: oldPort, RawName: "read"}
	sid, _, resolvedPort := sess.refreshStalePortBeforeDispatch(context.Background(), callRef, oldSID, proto)

	if sid != oldSID {
		t.Fatalf("reinit-fail must return the caller's original sid; got %q want %q", sid, oldSID)
	}
	if resolvedPort != oldPort {
		t.Fatalf("reinit-fail returned port %d; want the original ref.Port %d (the old sid's minting port), not the resolved %d — posting the old sid to the moved-to daemon would hard-fail non-retriably", resolvedPort, oldPort, newPort)
	}
	if got := movedDaemon.initCount.Load(); got != 1 {
		t.Fatalf("expected exactly one (failed) reinit attempt at the resolved port; got %d", got)
	}
}
