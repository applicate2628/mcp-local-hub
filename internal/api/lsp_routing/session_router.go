package lsp_routing

import (
	"sort"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

// DefaultSessionTTL is the idle-expiration window for LSP router session
// disambiguation state.
const DefaultSessionTTL = 24 * time.Hour

type sessionBinding struct {
	workspaces map[string]api.WorkspaceEntry
	lastSeen   time.Time
}

// SessionRouter tracks which workspaces a client-side LSP router session has
// touched. Path-bearing tools/call requests add a workspace; pathless calls can
// route only when exactly one workspace has been touched.
type SessionRouter struct {
	mu       sync.Mutex
	sessions map[string]*sessionBinding
	clock    func() time.Time
}

func NewSessionRouter() *SessionRouter {
	return NewSessionRouterWithClock(time.Now)
}

func NewSessionRouterWithClock(clock func() time.Time) *SessionRouter {
	if clock == nil {
		clock = time.Now
	}
	return &SessionRouter{
		sessions: make(map[string]*sessionBinding),
		clock:    clock,
	}
}

// TouchWorkspace records that sessionID successfully routed to ws. A nil
// workspace or empty session id is ignored.
func (s *SessionRouter) TouchWorkspace(sessionID string, ws *api.WorkspaceEntry) {
	if s == nil || sessionID == "" || ws == nil || ws.WorkspaceKey == "" {
		return
	}
	wsCopy := *ws
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.sessions[sessionID]
	if b == nil {
		b = &sessionBinding{workspaces: map[string]api.WorkspaceEntry{}}
		s.sessions[sessionID] = b
	}
	b.workspaces[wsCopy.WorkspaceKey] = wsCopy
	b.lastSeen = s.clock()
}

// Candidates returns the workspaces touched by sessionID, sorted by workspace
// path then key for stable ambiguous-error rendering. Successful lookup
// refreshes the idle timestamp even when no workspace has been touched yet.
func (s *SessionRouter) Candidates(sessionID string) []api.WorkspaceEntry {
	if s == nil || sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.sessions[sessionID]
	if b == nil {
		return nil
	}
	b.lastSeen = s.clock()
	out := make([]api.WorkspaceEntry, 0, len(b.workspaces))
	for _, ws := range b.workspaces {
		out = append(out, ws)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WorkspacePath != out[j].WorkspacePath {
			return out[i].WorkspacePath < out[j].WorkspacePath
		}
		return out[i].WorkspaceKey < out[j].WorkspaceKey
	})
	return out
}

// EnsureSession records an initialized session before it touches any
// workspace. This lets empty sessions expire even if a client never makes a
// file-scoped call.
func (s *SessionRouter) EnsureSession(sessionID string) {
	if s == nil || sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[sessionID] == nil {
		s.sessions[sessionID] = &sessionBinding{workspaces: map[string]api.WorkspaceEntry{}}
	}
	s.sessions[sessionID].lastSeen = s.clock()
}

func (s *SessionRouter) UnbindSession(sessionID string) {
	if s == nil || sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

func (s *SessionRouter) Cleanup(now time.Time) int {
	return s.CleanupWithTTL(now, DefaultSessionTTL)
}

func (s *SessionRouter) CleanupWithTTL(now time.Time, ttl time.Duration) int {
	if s == nil {
		return 0
	}
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

func (s *SessionRouter) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}
