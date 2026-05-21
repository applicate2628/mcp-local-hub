package serena_routing

import (
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

// DefaultSessionTTL is the lazy-expiration window for SessionRouter
// entries. Per Phase C.3, sessions stay bound until 24h of inactivity
// elapse, at which point Cleanup drops them.
const DefaultSessionTTL = 24 * time.Hour

// sessionBinding holds the per-session workspace handle plus the
// timestamp at which the session was last active. BindSession and
// successful LookupSession calls both refresh lastSeen so TTL is
// idle-based.
type sessionBinding struct {
	workspace *api.WorkspaceEntry
	lastSeen  time.Time
}

// SessionRouter implements the sticky-session half of the serena
// routing middleware: it remembers which workspace a given MCP
// session id was last bound to, so subsequent no-path tool calls
// (list_memories, get_current_config, etc.) forward to the same
// daemon as the path-aware call that established the binding.
//
// Lifecycle:
//
//   - BindSession is invoked AFTER a successful WorkspaceResolver
//     resolution and upstream response on a path-aware call.
//   - LookupSession is invoked at the start of a no-path call;
//     successful lookups refresh lastSeen, and callers handle the nil
//     result per their fallback policy
//     (Phase F may default to a single-workspace shortcut or ask
//     the client to retry with a path argument).
//   - UnbindSession is invoked on MCP session close events.
//   - Cleanup is invoked periodically (by the host) to drop bindings
//     whose lastSeen is older than ttl. Because LookupSession refreshes
//     lastSeen, a session making only no-path calls remains bound until
//     it is idle for ttl.
//
// Concurrency: all exported methods are safe for parallel use.
// BindSession, LookupSession, UnbindSession, and Cleanup use Lock when
// they mutate binding state; Len uses RLock for diagnostics.
type SessionRouter struct {
	mu       sync.RWMutex
	sessions map[string]*sessionBinding
	clock    func() time.Time // injectable; defaults to time.Now
}

// NewSessionRouter returns an empty SessionRouter. The router uses
// time.Now() for activity timestamps; tests that need to manipulate
// the clock should use NewSessionRouterWithClock.
func NewSessionRouter() *SessionRouter {
	return NewSessionRouterWithClock(time.Now)
}

// NewSessionRouterWithClock returns an empty SessionRouter that uses
// clock for all activity timestamps. The clock seam exists so tests
// can advance "now" for TTL expiration without sleeping.
func NewSessionRouterWithClock(clock func() time.Time) *SessionRouter {
	if clock == nil {
		clock = time.Now
	}
	return &SessionRouter{
		sessions: make(map[string]*sessionBinding),
		clock:    clock,
	}
}

// BindSession records that sessionID is currently routed to ws and
// stamps the binding with the current clock time.
//
// Idempotent: re-binding the same session id either updates the
// workspace (if a path-aware call resolved to a different workspace
// than the previous binding) or just refreshes lastSeen. The previous
// binding is dropped without ceremony -- session-scoped state on the
// upstream daemon is the upstream daemon job to manage.
//
// A nil ws is silently ignored: callers should not bind unresolved
// workspaces, and we prefer the no-op over installing a nil entry
// that would later look identical to "unbound" in LookupSession.
func (s *SessionRouter) BindSession(sessionID string, ws *api.WorkspaceEntry) {
	if sessionID == "" || ws == nil {
		return
	}
	wsCopy := *ws // defensive copy: caller may mutate the source value
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sessionID] = &sessionBinding{
		workspace: &wsCopy,
		lastSeen:  s.clock(),
	}
}

// LookupSession returns the workspace currently bound to sessionID,
// or nil if no binding exists. The returned pointer is a fresh
// value-copy of the stored entry so concurrent BindSession /
// UnbindSession on the same session id cannot mutate it.
//
// A successful lookup refreshes lastSeen, making the TTL idle-based:
// bindings expire after ttl with no BindSession or LookupSession
// activity.
func (s *SessionRouter) LookupSession(sessionID string) *api.WorkspaceEntry {
	if sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.sessions[sessionID]
	if !ok || b == nil || b.workspace == nil {
		return nil
	}
	b.lastSeen = s.clock()
	wsCopy := *b.workspace
	return &wsCopy
}

// UnbindSession drops the binding for sessionID. No-op if the session
// is not bound. Invoked from MCP session close events.
func (s *SessionRouter) UnbindSession(sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

// Cleanup drops bindings whose lastSeen is older than DefaultSessionTTL
// before now. Returns the number of bindings expired.
//
// The host is expected to invoke Cleanup on a periodic timer (e.g.
// every 1h) so the map does not grow unboundedly. Calling Cleanup at
// arbitrary now values is safe; no global state is touched.
func (s *SessionRouter) Cleanup(now time.Time) int {
	return s.CleanupWithTTL(now, DefaultSessionTTL)
}

// CleanupWithTTL is the explicit-TTL variant of Cleanup, exposed for
// tests that need to assert expiration without waiting 24 simulated
// hours. ttl is the maximum inactivity allowed; bindings whose
// lastSeen + ttl is <= now are dropped.
func (s *SessionRouter) CleanupWithTTL(now time.Time, ttl time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := now.Add(-ttl)
	n := 0
	for id, b := range s.sessions {
		if b == nil || !b.lastSeen.After(cutoff) {
			delete(s.sessions, id)
			n++
		}
	}
	return n
}

// Len returns the number of currently-bound sessions. Useful for
// diagnostics and test assertions.
func (s *SessionRouter) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}
