// internal/gui/csrf.go
package gui

import (
	"fmt"
	"net/http"
	"strings"
)

// requireSameOrigin is a middleware that enforces a same-origin policy
// on mutating routes. It rejects browser-driven cross-origin POSTs
// while still allowing direct curl/script callers (which have no
// Origin/Sec-Fetch-Site headers).
//
// Two checks, EITHER passing means "trusted source":
//
//  1. `Sec-Fetch-Site` header is "same-origin" or "none". Modern browsers
//     (Chrome 76+, Firefox 90+, Safari 16+) populate this; "none"
//     specifically means user-initiated navigation or extension/script,
//     never cross-origin fetch.
//  2. `Origin` header matches "http://127.0.0.1:<port>" or
//     "http://localhost:<port>" — the only origins the local GUI itself
//     can serve from. Empty Origin (curl, native clients) passes.
//
// If neither check passes, returns 403 with the api-error envelope so
// CSRF attempts surface clearly in logs.
func (s *Server) requireSameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Sec-Fetch-Site (preferred when present).
		sfs := r.Header.Get("Sec-Fetch-Site")
		if sfs != "" {
			if sfs == "same-origin" || sfs == "none" {
				next(w, r)
				return
			}
			writeAPIError(w, fmt.Errorf("cross-origin %s request rejected", sfs),
				http.StatusForbidden, "CROSS_ORIGIN")
			return
		}
		// Fallback: Origin allowlist. Empty Origin = non-browser caller.
		origin := r.Header.Get("Origin")
		if origin == "" {
			next(w, r)
			return
		}
		// Build the expected origins from this server's actual port.
		port := s.Port()
		want := []string{
			fmt.Sprintf("http://127.0.0.1:%d", port),
			fmt.Sprintf("http://localhost:%d", port),
		}
		for _, w2 := range want {
			if strings.EqualFold(origin, w2) {
				next(w, r)
				return
			}
		}
		writeAPIError(w, fmt.Errorf("origin %q not allowed", origin),
			http.StatusForbidden, "CROSS_ORIGIN")
	}
}
