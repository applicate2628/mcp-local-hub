// internal/gui/csrf.go
package gui

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
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

func (s *Server) allowedHost(hostport string) bool {
	wantPort := s.effectivePort()
	if wantPort <= 0 {
		return false
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		// No explicit port: browsers omit :80 for default HTTP port.
		// Accept the bare-host form only when the GUI is bound to port 80.
		if wantPort != 80 {
			return false
		}
		host = hostport
	} else if port != fmt.Sprintf("%d", wantPort) {
		return false
	}
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1"
}

func (s *Server) allowedOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Path != "" && u.Path != "/" {
		return false
	}
	return s.allowedHost(u.Host)
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
