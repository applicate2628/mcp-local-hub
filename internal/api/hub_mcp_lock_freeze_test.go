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
// generation) is covered by TestRefreshStalePortFollowerDoesNotClearNewerRestart.

package api

import (
	"context"
	"net/http"
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

	// Contract preserved #2 (the generation stamp survives releasing state.mu): the
	// MarkPortStale we fired DURING the handshake bumped the port generation. Under
	// the generation-stamped cache the reinit stamps its sid with the FLIGHT's
	// (older) generation, so the cached stamp < the current generation — the newer
	// restart is NOT masked and the port still reads stale. Pre-fix, state.mu was
	// held across the whole handshake, so MarkPortStale could never interleave here.
	if !portStaleForTest(t, sess1, ref) {
		t.Fatalf("newer restart mark (concurrent MarkPortStale during handshake) was masked — the cache was stamped fresh for the superseded generation")
	}
}

// TestRefreshStalePortFollowerDoesNotClearNewerRestart reproduces fable Defect 1
// / Codex #500 P2: two proactive callers coalesce onto ONE singleflight reinit
// flight; a genuine restart lands AFTER the flight started but BEFORE the second
// (follower) caller's snapshot. Pre-fix, the follower's clear token carried its
// OWN newer snapshot generation, so it cleared stale=false even though the
// shared flight's session id predates that restart — wedging every later
// dispatch on a dead daemon session (-32000, no transport failure to trigger
// selfHealRetry). The fix reads the flight's OWN start generation once (shared by
// all joiners) and clears only if no restart landed since; here one did, so
// neither caller may clear.
func TestRefreshStalePortFollowerDoesNotClearNewerRestart(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	const oldSID = "follower-old-sid"
	const flightSID = "follower-flight-sid"
	const proto = "2025-11-25"

	gate := make(chan struct{})
	var gateOnce sync.Once
	releaseGate := func() { gateOnce.Do(func() { close(gate) }) }
	t.Cleanup(releaseGate)

	daemon := newStubDaemon(t, flightSID)
	daemon.onInit = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Mcp-Session-Id", flightSID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"stub","version":"1"}}}`))
	}
	port := daemon.port

	sess := sessionWithParticipants(daemon)
	sess.ClientSessionID = "follower-sess"
	ref := sess.IntendedParticipants[0]
	sess.InitSuccesses[ref] = oldSID
	sess.DaemonProtoVer[ref] = proto
	callRef := canonicalToolRef{Server: ref.Server, Daemon: ref.Daemon, Port: port, RawName: "read"}

	// R1: mark the port stale (generation 0→1).
	if !sess.markStalePort(port) {
		t.Fatalf("setup: port %d not tracked by the session", port)
	}

	// Deterministic join barrier: the seam fires once per caller AFTER it reaches
	// the singleflight DoChan, so we never rely on a wall-clock sleep to know the
	// follower joined (a loaded CI worker could otherwise delay it past the gate
	// release, letting the leader complete and the follower start a second flight).
	joined := make(chan struct{}, 4)
	reinitJoinedFlightHook = func() { joined <- struct{}{} }
	t.Cleanup(func() { reinitJoinedFlightHook = nil })

	// Leader A: starts the flight, blocks in the daemon initialize on `gate`.
	aReturned := make(chan string, 1)
	go func() {
		sid, _ := sess.refreshStalePortBeforeDispatch(context.Background(), callRef, oldSID, proto)
		aReturned <- sid
	}()
	<-joined // leader reached DoChan (flight started)

	// Wait until A's flight actually reached the daemon initialize (in-flight, so
	// the follower joins it rather than starting a new one).
	deadline := time.Now().Add(3 * time.Second)
	for daemon.initCount.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("leader flight never reached the daemon initialize")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// R2: a SECOND genuine restart lands WHILE the flight is still in-flight
	// (generation 1→2). The gate holds the flight open, so this is deterministic.
	if !sess.markStalePort(port) {
		t.Fatal("R2 markStalePort failed")
	}

	// Follower B: joins the SAME in-flight singleflight call. Its snapshot sees
	// generation 2 (post-R2). The gate keeps the leader's flight open, so B
	// provably JOINS rather than starting a new flight.
	bReturned := make(chan string, 1)
	go func() {
		sid, _ := sess.refreshStalePortBeforeDispatch(context.Background(), callRef, oldSID, proto)
		bReturned <- sid
	}()
	<-joined // DETERMINISTIC barrier: B reached the DoChan join (no wall-clock sleep)

	releaseGate()

	select {
	case <-aReturned:
	case <-time.After(PerDaemonInitTimeout + 3*time.Second):
		t.Fatal("leader did not return")
	}
	select {
	case <-bReturned:
	case <-time.After(PerDaemonInitTimeout + 3*time.Second):
		t.Fatal("follower did not return")
	}

	// Coalesced: exactly ONE handshake served both callers (proves B joined the
	// leader's flight; if B had started a new flight it would read gen=2 and this
	// count would be 2, a different scenario).
	if got := daemon.initCount.Load(); got != 1 {
		t.Fatalf("expected 1 coalesced handshake for the leader+follower, got %d", got)
	}

	// THE FIX: the shared flight started at generation 1 (pre-R2); R2 bumped the
	// port to generation 2. The flight stamps its cached sid with its OWN start
	// generation (1) — NOT the follower's later snapshot (2) — so the cached stamp
	// (1) < the current generation (2): the port still reads stale, and the newer
	// restart is not masked. Pre-fix, follower B (its own token generation == 2)
	// cleared the mark, wedging every later dispatch on a dead session id.
	if !portStaleForTest(t, sess, ref) {
		t.Fatal("a singleflight follower masked the newer restart (fable Defect 1): a shared pre-restart flight stamped the cache fresh for the superseded generation")
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

		sid, _ := sess.refreshStalePortBeforeDispatch(context.Background(), callRef, "caller-sid", proto)
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

		sid, _ := sess.refreshStalePortBeforeDispatch(context.Background(), callRef, "caller-sid", proto)
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
