// internal/gui/csrf.go
package gui

import (
	"fmt"
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHost(r.Host) {
			writeAPIError(w, fmt.Errorf("host %q not allowed", r.Host),
				http.StatusForbidden, "HOST_NOT_ALLOWED")
			return
		}
		next.ServeHTTP(w, r)
	})
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

// requireSameOrigin is a middleware that enforces a same-origin policy
// on mutating routes. It rejects browser-driven cross-origin POSTs
// while still allowing direct curl/script callers (which have no
// Origin/Sec-Fetch-Site headers).
//
// Two checks must agree when both browser headers are present:
//
//  1. `Origin`, when present, must match this local GUI origin. Empty Origin
//     (curl, native clients) passes.
//  2. `Sec-Fetch-Site`, when present, must not be cross-site.
//
// If either present check fails, returns 403 with the api-error envelope so
// CSRF attempts surface clearly in logs.
func (s *Server) requireSameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && !s.allowedOrigin(origin) {
			writeAPIError(w, fmt.Errorf("origin %q not allowed", origin),
				http.StatusForbidden, "CROSS_ORIGIN")
			return
		}

		sfs := r.Header.Get("Sec-Fetch-Site")
		if sfs != "" && sfs != "same-origin" && sfs != "none" {
			writeAPIError(w, fmt.Errorf("cross-origin %s request rejected", sfs),
				http.StatusForbidden, "CROSS_ORIGIN")
			return
		}

		next(w, r)
	}
}
