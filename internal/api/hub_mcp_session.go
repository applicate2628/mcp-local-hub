// hub_mcp_session.go — Phase 3 Task 3.3 (G4 unified hub MCP).
//
// hubSession + hubSessionStore: the per-hub session model. One
// hub session per (client_id, Mcp-Session-Id) tuple. Created on
// initialize, terminated on DELETE or idle-sweep expiry.
//
// Layering:
//
//   - hubSessionStore is the package-level map keyed by client_session_id.
//     RWMutex protects the map; insert/delete under Lock, lookup under
//     RLock. Per-client counters + LRU index are sibling maps under the
//     same lock — never acquired separately.
//   - hubSession has its own per-session mu sync.Mutex protecting
//     LastUsedAt + lifecycle. The sweeper holds the STORE Lock to walk
//     the map, then briefly takes each session's mu to read LastUsedAt
//     under a consistent view.
//   - InFlightRequests is a separate map protected by inflightMu, NOT
//     mu. The sweeper checks inFlightCount.Load() == 0 cheaply without
//     touching inflightMu — that's the fast path skip-if-busy check.
//   - inFlightCount is atomic.Int32. INCREMENT before InsertInFlight
//     (under inflightMu); DECREMENT after RemoveInFlight (also under
//     inflightMu) so the two operations look atomic to the sweeper.
//
// Concurrency safety:
//
//   - Sweeper skips a session if inFlightCount.Load() != 0. The cheap
//     check costs one atomic load.
//   - Sweep step DOES NOT call Delete under the same Lock — it
//     collects ids first (under Lock), then iterates Delete (also
//     under Lock). That's a brief Lock-drop-Lock dance which is fine
//     because Delete on a missing id is a no-op.
//   - LRU index: a *list.List protected by store.mu (same Lock as
//     sessions). Push to front on Create / Touch, evict from back on
//     cap-exceeded.
//
// Hard caps:
//
//   - MaxSessionsPerClient = 16 (production default).
//   - MaxSessionsGlobal    = 256 (production default).
//   - IdleTimeout          = 30 min.
//   - SweepInterval        =  1 min.
//
// At global cap, Create evicts the LRU session to make room. At
// per-client cap, Create REJECTS with ErrSessionCapExceeded — the
// HTTP handler surfaces 429 Retry-After: 30 to the caller. (Spec
// §"Concurrency + bounds": "New initialize at cap → 429 with
// Retry-After: 30".)
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Per-hub session model" + §"Concurrency + bounds". Plan: Task 3.3.

package api

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ErrSessionCapExceeded is returned by HubSessionStore.Create when a
// new session cannot be created because the per-client cap is full.
// Global-cap exceedance triggers LRU eviction instead — Create
// SUCCEEDS in that case. The HTTP handler maps this error to
// 429 Too Many Requests with `Retry-After: 30`.
var ErrSessionCapExceeded = errors.New("session cap exceeded")

// Production defaults applied when SessionStoreOpts zero values
// are passed to NewHubSessionStore.
const (
	defaultMaxSessionsPerClient   = 16
	defaultMaxSessionsGlobal      = 256
	defaultSessionIdleTimeout     = 30 * time.Minute
	defaultSessionSweepInterval   = 60 * time.Second
	sessionIDByteLength           = 16 // 128-bit; hex-encodes to 32 chars
)

// SessionStoreOpts configures bound + sweep tuning. Zero values get
// production defaults (16 / 256 / 30min / 60s).
type SessionStoreOpts struct {
	MaxPerClient  int
	MaxGlobal     int
	IdleTimeout   time.Duration
	SweepInterval time.Duration
}

// inflightEntry records the daemon-side state of an in-flight
// tools/call. Stored under hubSession.InFlightRequests, keyed by
// the client-supplied request id (canonicalized via newRequestIDKey).
// On daemon response / error / timeout / cancel, the entry is
// removed and inFlightCount decremented.
type inflightEntry struct {
	DaemonRef       canonicalDaemonRef
	DaemonSessionID string
	DaemonRequestID json.RawMessage
	StartedAt       time.Time
}

// DaemonFailure is one row in the partialFailures surface emitted to
// callers when at least one daemon failed during initialize or
// tools/list. Stage discriminates initialize-time vs list-time vs
// call-time failures so operators can act on the right surface.
type DaemonFailure struct {
	Server string `json:"server"`
	Daemon string `json:"daemon"`
	Stage  string `json:"stage"` // initialize | tools/list | tools/call
	Err    string `json:"err"`
}

// hubSession is the state for one client_session_id. Created on
// initialize, removed on DELETE or sweep. NEVER lifted to a global
// per-hub state — failures from one session never bleed into
// another.
type hubSession struct {
	ClientSessionID      string
	Client               string
	ProtocolVersion      string // captured at initialize for MCP-Protocol-Version validation
	SnapshotAtInit       *ResolverSnapshot
	IntendedParticipants []canonicalDaemonRef
	InitSuccesses        map[canonicalDaemonRef]string // value = daemon Mcp-Session-Id
	InitFailures         []DaemonFailure
	RouteMap             atomic.Pointer[map[string]canonicalToolRef] // session-local; atomic swap
	InFlightRequests     map[requestIDKey]inflightEntry
	inflightMu           sync.Mutex
	inFlightCount        atomic.Int32
	InitAt               time.Time
	LastUsedAt           time.Time
	mu                   sync.Mutex // protects LastUsedAt + lifecycle
}

// IncInFlight / DecInFlight: atomic counter manipulation. Insert/Remove
// pair calls them under inflightMu so the count and map size stay
// consistent. The sweeper reads the count under no lock — fast path.
func (s *hubSession) IncInFlight() { s.inFlightCount.Add(1) }
func (s *hubSession) DecInFlight() { s.inFlightCount.Add(-1) }

// InFlightCount returns the current in-flight count (atomic load).
func (s *hubSession) InFlightCount() int32 { return s.inFlightCount.Load() }

// InsertInFlight stores a per-call entry and increments inFlightCount
// under inflightMu so the sweeper's lock-free count check stays
// consistent with map presence.
func (s *hubSession) InsertInFlight(key requestIDKey, entry inflightEntry) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if _, existed := s.InFlightRequests[key]; existed {
		// Defensive: overwriting an existing entry would leak the
		// count. Callers MUST use unique keys; if a duplicate slips
		// in, replace WITHOUT bumping the count.
		s.InFlightRequests[key] = entry
		return
	}
	s.InFlightRequests[key] = entry
	s.inFlightCount.Add(1)
}

// LookupInFlight returns the entry stored at key. Second return is
// false when no entry is present.
func (s *hubSession) LookupInFlight(key requestIDKey) (inflightEntry, bool) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	e, ok := s.InFlightRequests[key]
	return e, ok
}

// RemoveInFlight deletes the entry and decrements inFlightCount.
// Idempotent — calling on an absent key is a no-op (does NOT
// decrement the count).
func (s *hubSession) RemoveInFlight(key requestIDKey) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if _, ok := s.InFlightRequests[key]; ok {
		delete(s.InFlightRequests, key)
		s.inFlightCount.Add(-1)
	}
}

// HubSessionStore owns every active hub session. Constructed via
// NewHubSessionStore at hub-listener start; closed via Close at
// shutdown (cancels the sweeper goroutine).
type HubSessionStore struct {
	opts SessionStoreOpts

	mu        sync.RWMutex
	sessions  map[string]*hubSession
	perClient map[string]int
	lru       *list.List
	lruIndex  map[string]*list.Element

	now       func() time.Time
	sweepCtx  context.Context
	sweepStop context.CancelFunc
}

// NewHubSessionStore returns a store with the supplied bounds. Zero
// values in opts get production defaults. The store STARTS its sweep
// goroutine immediately; callers MUST Close it at shutdown.
func NewHubSessionStore(opts SessionStoreOpts) *HubSessionStore {
	if opts.MaxPerClient <= 0 {
		opts.MaxPerClient = defaultMaxSessionsPerClient
	}
	if opts.MaxGlobal <= 0 {
		opts.MaxGlobal = defaultMaxSessionsGlobal
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = defaultSessionIdleTimeout
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = defaultSessionSweepInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &HubSessionStore{
		opts:      opts,
		sessions:  make(map[string]*hubSession),
		perClient: make(map[string]int),
		lru:       list.New(),
		lruIndex:  make(map[string]*list.Element),
		now:       time.Now,
		sweepCtx:  ctx,
		sweepStop: cancel,
	}
	go s.sweepLoop()
	return s
}

// Close cancels the sweep goroutine. Idempotent — multiple Close
// calls are safe. Does NOT drain in-flight sessions; the operator
// is responsible for finishing outstanding tools/call work before
// shutdown.
func (s *HubSessionStore) Close() {
	if s.sweepStop != nil {
		s.sweepStop()
	}
}

// Create allocates a new hubSession for the given client. Returns
// ErrSessionCapExceeded when the per-client cap is full; at the
// global cap, the LRU session is evicted and Create succeeds.
func (s *HubSessionStore) Create(client, protoVer string, snap *ResolverSnapshot) (*hubSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perClient[client] >= s.opts.MaxPerClient {
		return nil, fmt.Errorf("per-client cap (%d): %w", s.opts.MaxPerClient, ErrSessionCapExceeded)
	}
	if len(s.sessions) >= s.opts.MaxGlobal {
		s.evictLRULocked()
	}
	id := generateSessionID()
	now := s.now()
	sess := &hubSession{
		ClientSessionID:  id,
		Client:           client,
		ProtocolVersion:  protoVer,
		SnapshotAtInit:   snap,
		InitSuccesses:    map[canonicalDaemonRef]string{},
		InFlightRequests: map[requestIDKey]inflightEntry{},
		InitAt:           now,
		LastUsedAt:       now,
	}
	s.sessions[id] = sess
	s.perClient[client]++
	s.lruIndex[id] = s.lru.PushFront(id)
	return sess, nil
}

// Get returns the session for id, or (nil, false). Read-only — does
// NOT touch LastUsedAt or LRU order. Callers that observed live
// traffic on the session should follow Get with Touch.
func (s *HubSessionStore) Get(id string) (*hubSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

// Touch records activity: updates LastUsedAt + promotes the session
// to the front of the LRU list. Called by the HTTP handler after
// every successful per-session method invocation.
func (s *HubSessionStore) Touch(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return false
	}
	sess.mu.Lock()
	sess.LastUsedAt = s.now()
	sess.mu.Unlock()
	if el, ok := s.lruIndex[id]; ok {
		s.lru.MoveToFront(el)
	}
	return true
}

// Delete removes the session by id. Returns true on success, false if
// the id wasn't present. Idempotent.
func (s *HubSessionStore) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteLocked(id)
}

// deleteLocked is the in-lock half. Caller MUST hold s.mu (Lock).
func (s *HubSessionStore) deleteLocked(id string) bool {
	sess, ok := s.sessions[id]
	if !ok {
		return false
	}
	delete(s.sessions, id)
	s.perClient[sess.Client]--
	if s.perClient[sess.Client] <= 0 {
		delete(s.perClient, sess.Client)
	}
	if el, ok := s.lruIndex[id]; ok {
		s.lru.Remove(el)
		delete(s.lruIndex, id)
	}
	return true
}

// evictLRULocked removes the eldest session from the LRU list to free
// space for a new Create call. Caller MUST hold s.mu (Lock).
func (s *HubSessionStore) evictLRULocked() {
	back := s.lru.Back()
	if back == nil {
		return
	}
	id, _ := back.Value.(string)
	s.deleteLocked(id)
}

// sweepLoop runs forever (until sweepCtx is cancelled). Ticks every
// SweepInterval; sweepOnce does the actual scan.
func (s *HubSessionStore) sweepLoop() {
	ticker := time.NewTicker(s.opts.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.sweepCtx.Done():
			return
		case <-ticker.C:
			s.sweepOnce()
		}
	}
}

// sweepOnce iterates every session and removes those that are both
// idle (LastUsedAt before cutoff) AND have inFlightCount == 0. The
// in-flight check is a lock-free atomic load; only sessions that
// pass both checks get Deleted.
func (s *HubSessionStore) sweepOnce() {
	cutoff := s.now().Add(-s.opts.IdleTimeout)
	s.mu.Lock()
	ids := make([]string, 0, len(s.sessions))
	for id, sess := range s.sessions {
		if sess.inFlightCount.Load() != 0 {
			continue // fast path: in-flight work in progress
		}
		sess.mu.Lock()
		lastUsed := sess.LastUsedAt
		sess.mu.Unlock()
		if lastUsed.Before(cutoff) {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		s.deleteLocked(id)
	}
	s.mu.Unlock()
}

// generateSessionID returns a 128-bit random hex string. Used as the
// Mcp-Session-Id header value handed back to the client at
// initialize.
func generateSessionID() string {
	var buf [sessionIDByteLength]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failing is catastrophic; the hub should not
		// proceed. Return a clearly-bogus value and let the caller
		// surface it. The HTTP handler maps unusable session ids
		// to 500 Internal Server Error.
		return ""
	}
	return hex.EncodeToString(buf[:])
}
