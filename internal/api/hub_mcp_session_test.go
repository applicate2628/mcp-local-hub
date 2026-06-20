// hub_mcp_session_test.go — Phase 3 Task 3.3 (G4 unified hub MCP).
//
// Tests for hubSessionStore + hubSession. Covers:
//
//   - Create returns ErrSessionCapExceeded at per-client cap (16 in
//     production; 2 in test for speed).
//   - LRU eviction at global cap pushes the eldest session out,
//     freeing room for a new initialize.
//   - Idle sweeper respects inFlightCount: a session with inFlight
//     work survives a sweep even if LastUsedAt is past the idle
//     timeout.
//   - InsertInFlight / LookupInFlight / RemoveInFlight round-trip on
//     a single session; inFlightCount stays consistent.
//   - Concurrent in-flight insert/remove pairs (race-detector
//     coverage; inflightMu must serialize map mutations).
//
// Spec: §"Per-hub session model" + §"Concurrency + bounds". Plan: Task 3.3.

package api

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// incInFlightForTest / decInFlightForTest synthesize an "in-flight"
// state without going through InsertInFlight/RemoveInFlight. Tests
// that exercise the sweeper's inFlightCount fast-path use this; the
// production code path is Insert/Remove only.
//
// Defined in _test.go so the production binary does NOT expose
// atomic-counter manipulation outside the insert/remove pair.
func (s *hubSession) incInFlightForTest() { s.inFlightCount.Add(1) }
func (s *hubSession) decInFlightForTest() { s.inFlightCount.Add(-1) }

func TestCreateSessionRejectsAtPerClientCap(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{
		MaxPerClient:  2,
		MaxGlobal:     100,
		IdleTimeout:   30 * time.Minute,
		SweepInterval: 60 * time.Second,
	})
	defer store.Close()
	if _, err := store.Create("claude-code", "2025-11-25", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("claude-code", "2025-11-25", nil); err != nil {
		t.Fatal(err)
	}
	_, err := store.Create("claude-code", "2025-11-25", nil)
	if !errors.Is(err, ErrSessionCapExceeded) {
		t.Errorf("got %v, want ErrSessionCapExceeded", err)
	}
}

func TestCreateSessionPerClientCapDoesNotAffectOtherClients(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{MaxPerClient: 1, MaxGlobal: 100})
	defer store.Close()
	if _, err := store.Create("claude-code", "2025-11-25", nil); err != nil {
		t.Fatal(err)
	}
	// codex-cli starts fresh — should succeed even though claude-code is full.
	if _, err := store.Create("codex-cli", "2025-11-25", nil); err != nil {
		t.Errorf("codex-cli rejected: %v", err)
	}
}

// Global-cap LRU eviction: at the cap, the eldest session is evicted
// to make room for a new initialize. The displaced session id no
// longer resolves via Get.
func TestCreateSessionEvictsLRUAtGlobalCap(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{
		MaxPerClient: 100,
		MaxGlobal:    2,
	})
	defer store.Close()
	s1, err := store.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := store.Create("codex-cli", "2025-11-25", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 3rd Create at MaxGlobal=2 → evict s1 (eldest), accept new.
	s3, err := store.Create("cursor", "2025-11-25", nil)
	if err != nil {
		t.Fatalf("3rd create at cap should evict + succeed, got %v", err)
	}
	if _, ok := store.Get(s1.ClientSessionID); ok {
		t.Errorf("eldest session s1 should have been evicted")
	}
	if _, ok := store.Get(s2.ClientSessionID); !ok {
		t.Errorf("middle session s2 should still resolve")
	}
	if _, ok := store.Get(s3.ClientSessionID); !ok {
		t.Errorf("newest session s3 should resolve")
	}
}

// Idle sweeper must NOT remove a session with inFlightCount > 0
// even if LastUsedAt is past the idle timeout. The in-flight call
// continues; sweeper retries on the next tick after Decrement.
func TestIdleSweeperRespectsInFlightCount(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{
		MaxPerClient:  16,
		MaxGlobal:     256,
		IdleTimeout:   30 * time.Minute,
		SweepInterval: 60 * time.Second,
	})
	defer store.Close()
	// Pin clock to t=0 for deterministic sweep math.
	store.now = func() time.Time { return time.Unix(0, 0) }

	s, err := store.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.incInFlightForTest() // simulate an outstanding tools/call

	// Advance clock 31 min. LastUsedAt is still time.Unix(0,0).
	store.now = func() time.Time { return time.Unix(31*60, 0) }
	store.sweepOnce()
	if _, ok := store.Get(s.ClientSessionID); !ok {
		t.Errorf("idle sweeper removed session with non-zero inFlightCount")
	}

	// Now drain in-flight and re-sweep — session should disappear.
	s.decInFlightForTest()
	store.sweepOnce()
	if _, ok := store.Get(s.ClientSessionID); ok {
		t.Errorf("sweep should have removed idle, inFlight=0 session")
	}
}

// Idle sweeper leaves recently-used sessions alone.
func TestIdleSweeperLeavesRecentSessions(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{
		MaxPerClient:  16,
		MaxGlobal:     256,
		IdleTimeout:   30 * time.Minute,
		SweepInterval: 60 * time.Second,
	})
	defer store.Close()
	store.now = func() time.Time { return time.Unix(0, 0) }
	s, _ := store.Create("claude-code", "2025-11-25", nil)
	// Advance only 5 min (within IdleTimeout).
	store.now = func() time.Time { return time.Unix(5*60, 0) }
	store.sweepOnce()
	if _, ok := store.Get(s.ClientSessionID); !ok {
		t.Errorf("sweeper removed a recent session")
	}
}

// InsertInFlight + LookupInFlight + RemoveInFlight round-trip on a
// freshly-created session.
func TestSessionInFlightInsertLookupRemove(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{MaxPerClient: 16, MaxGlobal: 256})
	defer store.Close()
	s, err := store.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := newRequestIDKey(json.RawMessage(`42`))
	if err != nil {
		t.Fatal(err)
	}
	entry := inflightEntry{
		DaemonRef:       canonicalDaemonRef{Server: "fs", Daemon: "claude-code", Port: 9200},
		DaemonSessionID: "sid-1",
		DaemonRequestID: json.RawMessage(`"hub-7"`),
		StartedAt:       time.Unix(100, 0),
	}
	s.InsertInFlight(key, entry)
	if got := s.InFlightCount(); got != 1 {
		t.Errorf("inFlightCount after insert = %d want 1", got)
	}
	got, ok := s.LookupInFlight(key)
	if !ok {
		t.Fatal("LookupInFlight missed")
	}
	if got.DaemonSessionID != "sid-1" || got.DaemonRef.Server != "fs" {
		t.Errorf("LookupInFlight wrong entry: %+v", got)
	}
	s.RemoveInFlight(key)
	if _, ok := s.LookupInFlight(key); ok {
		t.Errorf("RemoveInFlight did not remove")
	}
	if got := s.InFlightCount(); got != 0 {
		t.Errorf("inFlightCount after remove = %d want 0", got)
	}
}

// Removing a non-existent key is a no-op (idempotent cleanup paths).
func TestSessionRemoveInFlightIdempotent(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{MaxPerClient: 16, MaxGlobal: 256})
	defer store.Close()
	s, _ := store.Create("claude-code", "2025-11-25", nil)
	key, _ := newRequestIDKey(json.RawMessage(`1`))
	s.RemoveInFlight(key) // never inserted; must not panic, must not negate count
	if got := s.InFlightCount(); got != 0 {
		t.Errorf("inFlightCount after no-op remove = %d want 0", got)
	}
}

// Concurrent Insert/Remove pairs from N goroutines: race-detector
// coverage for inflightMu. inFlightCount lands back at zero.
func TestSessionInFlightConcurrentRace(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{MaxPerClient: 16, MaxGlobal: 256})
	defer store.Close()
	s, _ := store.Create("claude-code", "2025-11-25", nil)

	const workers = 8
	const opsPerWorker = 1000
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				// Each worker uses string-typed keys with its own
				// suffix so insert/remove pairs collide cleanly.
				raw := json.RawMessage(`"w-` + string(rune('A'+workerID)) + `-` + string(rune('0'+(i%10))) + `"`)
				key, err := newRequestIDKey(raw)
				if err != nil {
					t.Errorf("key err: %v", err)
					return
				}
				s.InsertInFlight(key, inflightEntry{DaemonSessionID: "sid"})
				s.RemoveInFlight(key)
			}
		}(w)
	}
	wg.Wait()
	if got := s.InFlightCount(); got != 0 {
		t.Errorf("inFlightCount after concurrent insert/remove = %d want 0", got)
	}
}

// Delete returns true for a present session, false for an unknown id.
func TestStoreDeleteReturnTrueFalse(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{MaxPerClient: 16, MaxGlobal: 256})
	defer store.Close()
	s, _ := store.Create("claude-code", "2025-11-25", nil)
	if !store.Delete(s.ClientSessionID) {
		t.Error("Delete(present) returned false")
	}
	if store.Delete(s.ClientSessionID) {
		t.Error("Delete(absent) returned true")
	}
	if store.Delete("does-not-exist") {
		t.Error("Delete(never-existed) returned true")
	}
}

// Touch updates LastUsedAt (records activity so the sweeper extends
// the idle window). Also moves the session to the front of the LRU
// list so it's NOT the next eviction candidate.
func TestStoreTouchUpdatesLastUsedAndPromotesLRU(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{MaxPerClient: 16, MaxGlobal: 2})
	defer store.Close()
	store.now = func() time.Time { return time.Unix(0, 0) }
	s1, _ := store.Create("claude-code", "2025-11-25", nil)
	store.now = func() time.Time { return time.Unix(10, 0) }
	s2, _ := store.Create("codex-cli", "2025-11-25", nil)

	// Touch s1 — now s2 is the eldest by LRU.
	store.now = func() time.Time { return time.Unix(20, 0) }
	store.Touch(s1.ClientSessionID)

	// Add a 3rd; LRU evicts s2 instead of s1.
	store.now = func() time.Time { return time.Unix(30, 0) }
	s3, err := store.Create("cursor", "2025-11-25", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get(s2.ClientSessionID); ok {
		t.Errorf("s2 should have been LRU-evicted (s1 was touched, so s2 is eldest)")
	}
	if _, ok := store.Get(s1.ClientSessionID); !ok {
		t.Errorf("s1 should survive (touched recently)")
	}
	if _, ok := store.Get(s3.ClientSessionID); !ok {
		t.Errorf("s3 should resolve")
	}
}

func TestMarkPortStaleIgnoresPortsOutsideSession(t *testing.T) {
	ref := canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: 1111}
	sess := &hubSession{
		ScopeKey:      "claude-code",
		InitSuccesses: map[canonicalDaemonRef]string{ref: "sid-1"},
	}

	sess.markStalePort(2222)
	var count int
	sess.staleDaemonPorts.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != 0 {
		t.Fatalf("unrelated stale port created %d staleDaemonPorts entries; want 0", count)
	}

	sess.markStalePort(1111)
	if _, ok := sess.stalePortStateFor(1111); !ok {
		t.Fatalf("tracked participant port was not marked stale")
	}
}

// SessionStoreOpts zero values get filled with production defaults.
func TestNewHubSessionStoreDefaults(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{})
	defer store.Close()
	if store.opts.MaxPerClient != 16 {
		t.Errorf("default MaxPerClient=%d want 16", store.opts.MaxPerClient)
	}
	if store.opts.MaxGlobal != 256 {
		t.Errorf("default MaxGlobal=%d want 256", store.opts.MaxGlobal)
	}
	if store.opts.IdleTimeout != 30*time.Minute {
		t.Errorf("default IdleTimeout=%v want 30m", store.opts.IdleTimeout)
	}
	if store.opts.SweepInterval != 60*time.Second {
		t.Errorf("default SweepInterval=%v want 60s", store.opts.SweepInterval)
	}
}

// Generated session IDs are unique across a batch and have the
// expected hex shape.
func TestGenerateSessionIDUniqueAndShape(t *testing.T) {
	seen := map[string]bool{}
	const n = 100
	for i := 0; i < n; i++ {
		id, err := generateSessionID()
		if err != nil {
			t.Fatalf("generateSessionID: %v", err)
		}
		if len(id) != 32 {
			t.Errorf("session id %q has length %d, want 32 (128-bit hex)", id, len(id))
		}
		for j := 0; j < len(id); j++ {
			c := id[j]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Errorf("session id %q has non-hex char at %d", id, j)
				break
			}
		}
		if seen[id] {
			t.Errorf("collision: %q seen twice in %d generations", id, n)
		}
		seen[id] = true
	}
}

// TestInsertInFlightPreservesOriginalOnDuplicate pins the codex bot
// r2 P1 closure: duplicate-key InsertInFlight must NOT overwrite the
// existing entry. Earlier code silently replaced the entry while
// returning false; a cancellation arriving for the duplicate-id
// would then route to the SECOND caller's daemon ids and the first
// call's daemon-side request would be untrackable.
func TestInsertInFlightPreservesOriginalOnDuplicate(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{})
	sess, err := store.Create("claude-code", "2025-06-18", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	key, err := newRequestIDKey(json.RawMessage(`"req-1"`))
	if err != nil {
		t.Fatalf("newRequestIDKey: %v", err)
	}
	original := inflightEntry{
		DaemonRef:       canonicalDaemonRef{Server: "first", Daemon: "claude-code", Port: 9101},
		DaemonRequestID: json.RawMessage(`"hub-original"`),
	}
	if !sess.InsertInFlight(key, original) {
		t.Fatal("first InsertInFlight returned false unexpectedly")
	}
	dup := inflightEntry{
		DaemonRef:       canonicalDaemonRef{Server: "second", Daemon: "claude-code", Port: 9102},
		DaemonRequestID: json.RawMessage(`"hub-impostor"`),
	}
	if sess.InsertInFlight(key, dup) {
		t.Fatal("duplicate InsertInFlight returned true")
	}
	got, ok := sess.LookupInFlight(key)
	if !ok {
		t.Fatal("LookupInFlight after duplicate: entry vanished")
	}
	if got.DaemonRef.Server != "first" {
		t.Errorf("DaemonRef.Server overwritten: got %q want %q", got.DaemonRef.Server, "first")
	}
	if string(got.DaemonRequestID) != `"hub-original"` {
		t.Errorf("DaemonRequestID overwritten: got %s want \"hub-original\"", got.DaemonRequestID)
	}
}

// TestCreateAtGlobalCapSkipsInFlightSessions pins the codex bot r3
// P1 closure: when the global cap is reached, Create must evict the
// OLDEST IDLE session — not an in-flight one. If all sessions are
// in-flight, Create returns ErrSessionCapExceeded.
func TestCreateAtGlobalCapSkipsInFlightSessions(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{MaxPerClient: 8, MaxGlobal: 3})
	// Fill the store to capacity.
	s1, err := store.Create("claude-code", "2025-06-18", nil)
	if err != nil {
		t.Fatalf("s1 Create: %v", err)
	}
	s2, err := store.Create("codex-cli", "2025-06-18", nil)
	if err != nil {
		t.Fatalf("s2 Create: %v", err)
	}
	s3, err := store.Create("cursor", "2025-06-18", nil)
	if err != nil {
		t.Fatalf("s3 Create: %v", err)
	}
	// Mark s1 (the LRU) as in-flight. Eviction should walk to the
	// next-oldest (s2) which is idle.
	s1.incInFlightForTest()
	s4, err := store.Create("vscode", "2025-06-18", nil)
	if err != nil {
		t.Fatalf("s4 Create at cap with idle s2: %v", err)
	}
	if s4 == nil {
		t.Fatal("expected s4 non-nil")
	}
	// s1 must STILL be present (in-flight, protected).
	if _, ok := store.Get(s1.ClientSessionID); !ok {
		t.Errorf("in-flight s1 was evicted")
	}
	// s2 (idle LRU) must be gone.
	if _, ok := store.Get(s2.ClientSessionID); ok {
		t.Errorf("idle s2 must have been evicted")
	}
	_ = s3
	// Now mark every remaining session in-flight. Next Create must
	// refuse with ErrSessionCapExceeded.
	s3.incInFlightForTest()
	s4.incInFlightForTest()
	_, err = store.Create("gemini-cli", "2025-06-18", nil)
	if err == nil {
		t.Fatal("expected ErrSessionCapExceeded when all sessions in-flight")
	}
	if !errors.Is(err, ErrSessionCapExceeded) {
		t.Errorf("want ErrSessionCapExceeded, got %v", err)
	}
}
