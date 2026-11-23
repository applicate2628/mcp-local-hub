// internal/gui/csrf.go
package gui

import (
	"net/http"

	"mcp-local-hub/internal/mcproute"
)

// httpHandler is the production entrypoint for all GUI HTTP traffic.
// Route-specific middleware still owns method/body validation; this wrapper
// rejects browser DNS-rebinding traffic before it can read or mutate local
// GUI state.
func (s *Server) httpHandler() http.Handler {
	return s.requireAllowedHost(s.mux)
}

// ServeHTTP routes requests through the same host and origin checks used by
// Start. Besides satisfying http.Handler for embedding, this permits hermetic
// in-process integration tests without binding a listener.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.httpHandler().ServeHTTP(w, r)
}

func (s *Server) requireAllowedHost(next http.Handler) http.Handler {
	return mcproute.RequireAllowedHost(s.effectivePort, next)
}

// allowedHost and allowedOrigin delegate to mcproute's port-bound guard
// (internal/mcproute/originguard.go). This Server-side wrapper is the ONLY
// GUI-process coupling the guard has (s.effectivePort()) — see the
// Increment-1 decision record
// (work-items/decisions/2026-07-25-mcp-data-plane-off-gui-onto-supervised-
// front-daemon.md). Behavior is byte-identical to the pre-extraction
// implementation; only the port-independent predicate logic moved.
func (s *Server) allowedHost(hostport string) bool {
	return mcproute.AllowedHost(hostport, s.effectivePort())
}

func (s *Server) allowedOrigin(origin string) bool {
	return mcproute.AllowedOrigin(origin, s.effectivePort())
}

func (s *Server) effectivePort() int {
	if port := s.Port(); port != 0 {
		return port
	}
	return s.cfg.Port
}

func (s *Server) requireSameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return mcproute.RequireSameOrigin(s.effectivePort, next)
}
