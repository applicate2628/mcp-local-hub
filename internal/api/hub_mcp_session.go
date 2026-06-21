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

	"golang.org/x/sync/singleflight"
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
	defaultMaxSessionsPerClient = 16
	defaultMaxSessionsGlobal    = 256
	defaultSessionIdleTimeout   = 30 * time.Minute
	defaultSessionSweepInterval = 60 * time.Second
	sessionIDByteLength         = 16 // 128-bit; hex-encodes to 32 chars
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
	// DaemonProtocol is the daemon's NEGOTIATED MCP protocol version
	// captured at initialize time. Stored on the in-flight row so
	// ForwardCancellation can emit notifications/cancelled with the
	// daemon's own header value instead of the session-level requested
	// version. codex bot r17 P1 closure on PR #157.
	DaemonProtocol  string
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
	ScopeKey             string
	InstanceID           string // hub instance id admitted at initialize; used for stable response metadata
	ProtocolVersion      string // version requested at hub-side initialize (used for daemons that don't return a negotiated version)
	SnapshotAtInit       *ResolverSnapshot
	IntendedParticipants []canonicalDaemonRef
	InitSuccesses        map[canonicalDaemonRef]string // value = daemon Mcp-Session-Id
	// DaemonProtoVer stores each daemon's NEGOTIATED protocol version,
	// parsed from `result.protocolVersion` in the daemon's initialize
	// response. MCP version negotiation allows daemons to reply with a
	// supported version different from what the hub requested; every
	// subsequent header (`notifications/initialized`, `tools/list`,
	// `tools/call`, `notifications/cancelled`) MUST use the
	// daemon-negotiated value or strict daemons reject with 400.
	// Populated by AggregateInitialize alongside InitSuccesses; protected
	// by the same `mu`. codex bot r17 P1 closure on PR #157.
	DaemonProtoVer   map[canonicalDaemonRef]string
	InitFailures     []DaemonFailure
	RouteMap         atomic.Pointer[map[string]canonicalToolRef] // session-local; atomic swap
	InFlightRequests map[requestIDKey]inflightEntry
	inflightMu       sync.Mutex
	inFlightCount    atomic.Int32
	InitAt           time.Time
	LastUsedAt       time.Time
	deleteStarted    bool
	mu               sync.Mutex // protects LastUsedAt + lifecycle + InstanceID + InitSuccesses + DaemonProtoVer + deleteStarted
	// reinitGroup coalesces concurrent hot-swap (a) self-heal re-initializations
	// of the SAME live daemon binding (keyed Server\x00Daemon\x00Port) into ONE
	// initialize, so a mass daemon restart that fails many in-flight tools/call
	// at once cannot trigger an init-storm. Zero value is ready to use.
	reinitGroup singleflight.Group
	// staleDaemonPorts marks daemon ports whose cached MCP session is stale
	// because the supervisor restarted the daemon (the hot-swap (b) event-driven
	// path: the DaemonRestartWatcher sets these when it observes a per-port
	// current_pid change). The next tools/call to such a port re-initializes
	// BEFORE dispatching, so the client never sees the stale-session failure that
	// (a) would otherwise recover reactively. keys: int port. value:
	// *stalePortState. Growth is bounded to ports this session can actually
	// dispatch to: captured snapshot bindings, initialized daemon sessions, or
	// the current RouteMap. Unrelated live supervisor ports are ignored and rely
	// on the reactive self-heal path if a stale route ever appears later.
	staleDaemonPorts sync.Map
}

type stalePortState struct {
	mu    sync.Mutex
	stale bool
	// generation increments on every restart mark, so a reinit can clear only
	// the stale mark it observed and not a newer mark for the same port.
	generation uint64
}

// markStalePort flags a daemon port's cached session as needing re-init (called
// by HubSessionStore.MarkPortStale from the supervisor-state restart watcher).
// It returns false when the port is outside this session's known participants,
// bounding staleDaemonPorts to ports the session can dispatch to.
func (s *hubSession) markStalePort(port int) bool {
	if !s.tracksDaemonPort(port) {
		return false
	}
	v, _ := s.staleDaemonPorts.LoadOrStore(port, &stalePortState{})
	state := v.(*stalePortState)
	state.mu.Lock()
	state.generation++
	state.stale = true
	state.mu.Unlock()
	return true
}

func (s *hubSession) tracksDaemonPort(port int) bool {
	if port == 0 {
		return false
	}
	if routes := s.RouteMap.Load(); routes != nil {
		for _, ref := range *routes {
			if ref.Port == port {
				return true
			}
		}
	}
	s.mu.Lock()
	for ref := range s.InitSuccesses {
		if ref.Port == port {
			s.mu.Unlock()
			return true
		}
	}
	s.mu.Unlock()
	if snap := s.SnapshotAtInit; snap != nil {
		for _, ref := range snap.Bindings[s.ScopeKey] {
			if ref.Port == port {
				return true
			}
		}
	}
	return false
}

// stalePortStateFor returns the per-port restart state, if the port was ever
// marked stale. The state is not deleted after refresh: callers that already
// resolved the old daemon session before the refresh still need a synchronization
// point where they can wait for the refresh and re-read the current session id.
func (s *hubSession) stalePortStateFor(port int) (*stalePortState, bool) {
	v, ok := s.staleDaemonPorts.Load(port)
	if !ok {
		return nil, false
	}
	return v.(*stalePortState), true
}

// MarkPortStale flags every live session's cached session for the daemon at the
// given port as needing re-init. The hot-swap (b) DaemonRestartWatcher calls
// this when a daemon's reported current_pid changes (a restart); the next
// tools/call to that port re-initializes proactively (no client-visible
// stale-session failure). Returns the number of sessions actually marked.
func (st *HubSessionStore) MarkPortStale(port int) int {
	st.mu.RLock()
	defer st.mu.RUnlock()
	marked := 0
	for _, sess := range st.sessions {
		if sess.markStalePort(port) {
			marked++
		}
	}
	return marked
}

// InFlightCount returns the current in-flight count (atomic load).
//
// Production callers MUST mutate the count only through InsertInFlight
// + RemoveInFlight so the count stays in lockstep with map presence
// under inflightMu. Tests that synthesize an "in-flight" state
// without going through the insert path use incInFlightForTest /
// decInFlightForTest (declared in hub_mcp_session_test.go).
func (s *hubSession) InFlightCount() int32 { return s.inFlightCount.Load() }

// InsertInFlight stores a per-call entry and increments inFlightCount
// under inflightMu so the sweeper's lock-free count check stays
// consistent with map presence. Returns true when a fresh row was
// inserted, false when the entry was overwritten because a row was
// ALREADY present under the same key.
//
// Callers MUST validate key uniqueness upstream: a duplicate insert
// indicates either a hub-side bug (two outstanding calls sharing one
// client_session_id + request id) OR a malicious client replaying
// the request id. Phase 4 enforces uniqueness in the HTTP handler;
// the false return surfaces the duplicate so the handler can refuse
// the call with a -32603 internal error and a partialFailures row
// rather than silently swap the stored entry.
func (s *hubSession) InsertInFlight(key requestIDKey, entry inflightEntry) bool {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if _, existed := s.InFlightRequests[key]; existed {
		// codex bot r2 P1 closure on PR #157: PRESERVE the original
		// entry on duplicate. Earlier code overwrote even though it
		// returned false, clobbering the original DaemonRef +
		// DaemonRequestID. A cancellation arriving while the first
		// call is still running would then be forwarded with the
		// SECOND call's daemon ids — wrong daemon, wrong daemon-
		// req-id — and the first call would become untrackable.
		// The first writer's entry stays canonical; the second
		// caller gets false and must refuse its own call
		// (AggregateToolsCall does so with -32600 "duplicate request
		// id").
		return false
	}
	// codex bot r3 P1 closure on PR #157: increment BEFORE the map
	// write so the sweeper's lock-free `inFlightCount.Load()` check
	// never observes 0 during the window where the entry is being
	// registered. Earlier order (write → increment) had a brief gap
	// where a concurrent sweep could observe count=0 + idle-LastUsedAt
	// and evict the session even though a call had just been
	// registered. inflightMu prevents concurrent inserts; the atomic
	// counter's pre-increment is the cross-mutex visibility hop the
	// sweeper relies on.
	s.inFlightCount.Add(1)
	s.InFlightRequests[key] = entry
	return true
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

func (s *hubSession) markDeleteStarted() {
	s.mu.Lock()
	s.deleteStarted = true
	s.mu.Unlock()
}

func (s *hubSession) markDeleteStartedAndSnapshotDaemonSessions() map[canonicalDaemonRef]daemonInitState {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteStarted = true
	daemonSessions := make(map[canonicalDaemonRef]daemonInitState, len(s.InitSuccesses))
	for ref, dsid := range s.InitSuccesses {
		proto := s.DaemonProtoVer[ref]
		if proto == "" {
			proto = s.ProtocolVersion
		}
		daemonSessions[ref] = daemonInitState{SessionID: dsid, ProtocolVersion: proto}
	}
	return daemonSessions
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
	// sweepDone is closed by sweepLoop on exit. Close() blocks on it
	// so callers can guarantee the goroutine has actually finished
	// before the test binary terminates. Without this, parallel
	// tests on Windows accumulated parked sweepLoop goroutines whose
	// teardown latency tripped the 4-minute go-test timeout (see
	// work-items/bugs/2026-05-12-internal-api-suite-hangs-on-windows.md).
	sweepDone chan struct{}
	closeOnce sync.Once
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
		sweepDone: make(chan struct{}),
	}
	go s.sweepLoop()
	return s
}

// Close cancels the sweep goroutine and waits for it to exit before
// returning. Idempotent via closeOnce — multiple Close calls are
// safe and only the first one blocks. Does NOT drain in-flight
// sessions; the operator is responsible for finishing outstanding
// tools/call work before shutdown.
//
// The wait used to be fire-and-forget, which let parked sweepLoop
// goroutines accumulate across tests on Windows; eventually the
// scheduler couldn't tear them all down within the test binary's
// go-test deadline and the suite hung
// (work-items/bugs/2026-05-12-internal-api-suite-hangs-on-windows.md).
func (s *HubSessionStore) Close() {
	s.closeOnce.Do(func() {
		if s.sweepStop != nil {
			s.sweepStop()
		}
		if s.sweepDone != nil {
			<-s.sweepDone
		}
	})
}

// Create allocates a new hubSession for the given client. Returns
// ErrSessionCapExceeded when the per-client cap is full; at the
// global cap, the LRU session is evicted and Create succeeds.
//
// crypto/rand exhaustion during id generation surfaces as a non-nil
// error from Create — the HTTP handler maps that to 500 Internal
// Server Error. No empty-string session id ever lands in s.sessions.
func (s *HubSessionStore) Create(client, protoVer string, snap *ResolverSnapshot) (*hubSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perClient[client] >= s.opts.MaxPerClient {
		return nil, fmt.Errorf("per-client cap (%d): %w", s.opts.MaxPerClient, ErrSessionCapExceeded)
	}
	id, err := generateSessionID()
	if err != nil {
		return nil, fmt.Errorf("generate session id: %w", err)
	}
	if len(s.sessions) >= s.opts.MaxGlobal {
		// codex bot r3 P1 closure on PR #157: skip in-flight sessions
		// when evicting at global cap. Earlier code always evicted the
		// LRU; under load a long-running tools/call could be evicted
		// mid-flight, making subsequent requests + cancellations on
		// that session fail lookup while daemon work was still running.
		// Walk LRU tail → head, skip any session with inFlightCount > 0.
		// If ALL sessions at global cap are in-flight, refuse the new
		// session with ErrSessionCapExceeded — Phase 4 maps to 429
		// with Retry-After: 30 + a clear "all sessions busy" log entry.
		if !s.evictIdleLRULocked() {
			return nil, fmt.Errorf("global cap (%d) reached and all sessions in-flight: %w", s.opts.MaxGlobal, ErrSessionCapExceeded)
		}
	}
	now := s.now()
	sess := &hubSession{
		ClientSessionID:  id,
		ScopeKey:         client,
		ProtocolVersion:  protoVer,
		SnapshotAtInit:   snap,
		InitSuccesses:    map[canonicalDaemonRef]string{},
		DaemonProtoVer:   map[canonicalDaemonRef]string{},
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

// GetAndTouch is the atomic gate the HTTP handler uses on every
// non-initialize POST: it fetches the session AND updates the
// LastUsedAt timestamp under a single store-lock acquisition.
//
// codex deep-sec phase4 r24 P1 closure on PR #158 (lane #1): the
// pre-r24 handler called Get(sid) then Touch(sid) separately. With
// Get under RLock and Touch under Lock, the sweep goroutine could
// Delete the session between those calls; the handler then
// continued using the stale *hubSession pointer (the Delete only
// removes the map entry, not the underlying struct), causing
// subsequent cancellation / DELETE on the same id to see
// "unknown session" mid-flight. GetAndTouch closes the window —
// either we hold the session AND update its activity atomically,
// or the session was already gone and we report it.
//
// Returns false if the session disappeared (idle-swept, manually
// deleted, or never existed). Callers must treat false as
// "unknown session" exactly the same way they treat Get's false.
func (s *HubSessionStore) GetAndTouch(id string) (*hubSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	sess.mu.Lock()
	sess.LastUsedAt = s.now()
	sess.mu.Unlock()
	if el, ok := s.lruIndex[id]; ok {
		s.lru.MoveToFront(el)
	}
	return sess, true
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
	sess.markDeleteStarted()
	delete(s.sessions, id)
	s.perClient[sess.ScopeKey]--
	if s.perClient[sess.ScopeKey] <= 0 {
		delete(s.perClient, sess.ScopeKey)
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

// evictIdleLRULocked walks the LRU list from tail (oldest) toward head
// (newest) and evicts the first session it finds with inFlightCount
// == 0. Returns true if a session was evicted; false if ALL sessions
// at global cap are in-flight (caller must refuse the new session
// with ErrSessionCapExceeded). Caller MUST hold s.mu.
//
// codex bot r3 P1 closure on PR #157: prevents eviction of an
// active long-running tools/call.
//
// codex bot r4 P1 closure on PR #157: the check + delete must be
// atomic w.r.t. concurrent InsertInFlight. Earlier code loaded the
// atomic counter, then called deleteLocked WITHOUT serializing
// against the per-session inflightMu — a racing InsertInFlight
// (which only takes sess.inflightMu, not s.mu) could bump the count
// between those two operations and have its session evicted out from
// under it. Lock ordering: s.mu (already held by caller) → sess.inflightMu
// is safe because InsertInFlight never reaches for s.mu, so the
// reverse ordering can't form. We hold sess.inflightMu just long enough
// to confirm the count is still 0 at delete time.
func (s *HubSessionStore) evictIdleLRULocked() bool {
	for e := s.lru.Back(); e != nil; e = e.Prev() {
		id, _ := e.Value.(string)
		sess, ok := s.sessions[id]
		if !ok {
			continue
		}
		sess.inflightMu.Lock()
		if sess.inFlightCount.Load() == 0 {
			s.deleteLocked(id)
			sess.inflightMu.Unlock()
			return true
		}
		sess.inflightMu.Unlock()
	}
	return false
}

// sweepLoop runs forever (until sweepCtx is cancelled). Ticks every
// SweepInterval; sweepOnce does the actual scan. Closing sweepDone
// on return is the synchronization point Close() blocks on so
// callers can prove the goroutine has actually finished — without
// this, parked sweepLoop goroutines accumulated across tests and
// the test binary timed out on Windows
// (work-items/bugs/2026-05-12-internal-api-suite-hangs-on-windows.md).
func (s *HubSessionStore) sweepLoop() {
	defer close(s.sweepDone)
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
// initial scan uses a lock-free atomic load (cheap fast-path skip
// for in-flight sessions); the actual delete re-checks under
// sess.inflightMu to prevent a racing InsertInFlight from sliding
// in between the scan and the delete (codex bot r5 P1 closure on
// PR #157 — mirrors the evictIdleLRULocked guard).
//
// Lock ordering: s.mu held throughout. sess.inflightMu acquired
// briefly per-delete. InsertInFlight only takes sess.inflightMu (no
// s.mu reach), so the reverse ordering can't form.
func (s *HubSessionStore) sweepOnce() {
	cutoff := s.now().Add(-s.opts.IdleTimeout)
	s.mu.Lock()
	defer s.mu.Unlock()
	type sweepCandidate struct {
		id   string
		sess *hubSession
	}
	cands := make([]sweepCandidate, 0, len(s.sessions))
	for id, sess := range s.sessions {
		if sess.inFlightCount.Load() != 0 {
			continue // fast path: in-flight work in progress
		}
		sess.mu.Lock()
		lastUsed := sess.LastUsedAt
		sess.mu.Unlock()
		if lastUsed.Before(cutoff) {
			cands = append(cands, sweepCandidate{id: id, sess: sess})
		}
	}
	for _, c := range cands {
		// Re-validate under inflightMu: a racing InsertInFlight may
		// have bumped the count between our initial scan and now.
		c.sess.inflightMu.Lock()
		if c.sess.inFlightCount.Load() == 0 {
			s.deleteLocked(c.id)
		}
		c.sess.inflightMu.Unlock()
	}
}

// generateSessionID returns a 128-bit random hex string. Used as the
// Mcp-Session-Id header value handed back to the client at
// initialize.
//
// crypto/rand exhaustion returns a non-nil error. Callers MUST treat
// that as a 500 Internal Server Error path and refuse to register an
// empty session id; the prior best-effort empty-string return was
// silently aliased under one key in s.sessions, masking the failure.
func generateSessionID() (string, error) {
	var buf [sessionIDByteLength]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}
