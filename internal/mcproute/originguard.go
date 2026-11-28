// Package mcproute owns the workspace MCP HTTP handlers and port-bound
// listener guards used by the supervised route daemon and the GUI listener.
package mcproute

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// AllowedHost reports whether hostport names the loopback host this server
// is bound to at port. Mirrors browsers' behavior of omitting the port
// number for the scheme's default port (":80" for http).
//
// port <= 0 means "not yet bound / not configured" and always rejects — a
// server with no known port can never be a valid same-origin target.
func AllowedHost(hostport string, port int) bool {
	if port <= 0 {
		return false
	}
	host, p, err := net.SplitHostPort(hostport)
	if err != nil {
		// No explicit port: browsers omit :80 for the default HTTP port.
		// Accept the bare-host form only when bound to port 80.
		if port != 80 {
			return false
		}
		host = hostport
	} else if p != strconv.Itoa(port) {
		return false
	}
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1"
}

// AllowedOrigin reports whether origin is this server's own loopback origin
// at port. It is the Origin-header counterpart to AllowedHost: an Origin
// carrying credentials, a query string, a fragment, a non-http scheme, or a
// non-root path is rejected outright; otherwise the host portion is checked
// against AllowedHost.
func AllowedOrigin(origin string, port int) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Path != "" && u.Path != "/" {
		return false
	}
	return AllowedHost(u.Host, port)
}

// RequireAllowedHost applies the listener's DNS-rebinding guard.
func RequireAllowedHost(port func() int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !AllowedHost(r.Host, port()) {
			WriteAPIError(w, fmt.Errorf("host %q not allowed", r.Host),
				http.StatusForbidden, "HOST_NOT_ALLOWED")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireSameOrigin is a middleware that enforces a same-origin policy
// on mutating routes. It rejects browser-driven cross-origin POSTs
// while still allowing direct curl/script callers (which have no
// Origin/Sec-Fetch-Site headers).
//
// Two checks must agree when both browser headers are present:
//
//  1. `Origin`, when present, must match this local listener origin. Empty Origin
//     (curl, native clients) passes.
//  2. `Sec-Fetch-Site`, when present, must not be cross-site.
//
// If either present check fails, returns 403 with the api-error envelope so
// CSRF attempts surface clearly in logs.
func RequireSameOrigin(port func() int, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && !AllowedOrigin(origin, port()) {
			WriteAPIError(w, fmt.Errorf("origin %q not allowed", origin),
				http.StatusForbidden, "CROSS_ORIGIN")
			return
		}

		sfs := r.Header.Get("Sec-Fetch-Site")
		if sfs != "" && sfs != "same-origin" && sfs != "none" {
			WriteAPIError(w, fmt.Errorf("cross-origin %s request rejected", sfs),
				http.StatusForbidden, "CROSS_ORIGIN")
			return
		}

		next(w, r)
	}
}

// WriteAPIError is the canonical error-envelope shape from spec §4.3.
// Shared by all /api/* handlers added in Tasks 9–15.
func WriteAPIError(w http.ResponseWriter, err error, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": err.Error(),
		"code":  code,
	})
}
