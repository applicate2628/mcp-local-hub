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
// Fix: (1) refreshStalePortBeforeDispatch snapshots the stale flag + generation
// under a brief state.mu, RELEASES it, runs the singleflight-deduped handshake
// WITHOUT the lock, then re-acquires state.mu only to guard-clear; (2)
// MarkPortStale snapshots the session pointers under the store RLock and
// RELEASES it before calling markStalePort, so the store lock is never held
// behind a per-port lock.
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
// too). Fix #2 alone is defense-in-depth: with fix #1 present, state.mu is never
// held across I/O, so markStalePort never stalls under the RLock and this test
// still passes with fix #2 reverted — fix #2 guards a FUTURE long per-port hold,
// not a hold this test can currently exercise. The follower-branch re-check race
// (a restart landing in the not-stale fast-path window) is covered separately by
// TestRefreshStalePortRecheckCatchesInWindowRestart.

package api

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
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

	// Contract preserved #2 (generation guard survives releasing state.mu): the
	// MarkPortStale we fired DURING the handshake bumped state.generation before
	// this caller's guard-clear ran, so the newer restart mark is correctly
	// PRESERVED (stale stays true) rather than being clobbered by a handshake
	// that predates it. Pre-fix, state.mu was held across the whole handshake, so
	// MarkPortStale could never interleave here and this guard was untestable;
	// the fix both removes the freeze AND exercises the guard for real.
	state, ok := sess1.stalePortStateFor(port)
	if !ok {
		t.Fatalf("stale port state for port %d vanished", port)
	}
	state.mu.Lock()
	stillStale := state.stale
	state.mu.Unlock()
	if !stillStale {
		t.Fatalf("newer restart mark (concurrent MarkPortStale during handshake) was clobbered — generation guard lost across the state.mu-release refactor")
	}
}

// TestRefreshStalePortRecheckCatchesInWindowRestart covers the follower
// (not-stale) fast-path window that narrowing the lock scope reopened: a genuine
// daemon restart (MarkPortStale) landing AFTER the not-stale snapshot but BEFORE
// the cache read/return would make the follower return a PRE-restart session id.
// The restarted daemon rejects that id as an HTTP-level error that
// isRetriableTransportFailure does NOT retry, so the client would get a bare
// -32000 — the exact stale-session failure the proactive path exists to prevent.
// The else-branch re-check closes the window; this test drives the window
// deterministically via the refreshStaleRecheckHook seam (not a wall-clock race).
func TestRefreshStalePortRecheckCatchesInWindowRestart(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	const oldCachedSID = "recheck-old-cached-sid"
	const freshSID = "recheck-fresh-sid"
	const proto = "2025-11-25"

	// Stub daemon serves the fresh sid on reinit via the default init handler
	// (returns sd.sessionID). No gate — the reinit completes promptly.
	daemon := newStubDaemon(t, freshSID)
	port := daemon.port

	sess := sessionWithParticipants(daemon)
	sess.ClientSessionID = "recheck-sess"
	ref := sess.IntendedParticipants[0] // {srv1, claude-code, port}
	sess.InitSuccesses[ref] = oldCachedSID
	sess.DaemonProtoVer[ref] = proto

	// Create the per-port stale state, then clear stale so the NOT-STALE fast
	// path (the branch under test) is entered. markStalePort creates the state
	// (stale=true, gen=1); clearing stale leaves the generation at 1.
	if !sess.markStalePort(port) {
		t.Fatalf("setup: port %d not tracked by the session", port)
	}
	st, ok := sess.stalePortStateFor(port)
	if !ok {
		t.Fatalf("setup: stale state for port %d missing", port)
	}
	st.mu.Lock()
	st.stale = false
	st.mu.Unlock()

	// Seam: land a genuine restart (MarkPortStale → generation++, stale=true) in
	// the exact window the else-branch re-check guards — after the fast-path
	// cache read, before the re-check. Fires once.
	var fired int32
	refreshStaleRecheckHook = func() {
		if atomic.CompareAndSwapInt32(&fired, 0, 1) {
			sess.markStalePort(port)
		}
	}
	t.Cleanup(func() { refreshStaleRecheckHook = nil })

	callRef := canonicalToolRef{Server: ref.Server, Daemon: ref.Daemon, Port: port, RawName: "read"}
	sid, gotProto := sess.refreshStalePortBeforeDispatch(context.Background(), callRef, oldCachedSID, proto)

	if atomic.LoadInt32(&fired) != 1 {
		t.Fatal("refreshStaleRecheckHook never fired — the fast-path window was not exercised")
	}
	// Pre-fix (no re-check) this returned the PRE-restart cached sid → -32000.
	// The re-check must instead detect the flip, reinit, and return the FRESH sid.
	if sid != freshSID {
		t.Fatalf("fast-path re-check missed the in-window restart: got sid %q, want fresh %q (pre-fix: the stale cached %q → client -32000)", sid, freshSID, oldCachedSID)
	}
	if gotProto != proto {
		t.Fatalf("proto = %q, want %q", gotProto, proto)
	}
	if daemon.initCount.Load() == 0 {
		t.Fatal("reinit never reached the daemon — the re-check did not trigger a reinit")
	}
}
