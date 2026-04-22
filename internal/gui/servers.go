// internal/gui/servers.go
package gui

import (
	"net/http"
	"strings"
)

// registerServerRoutes wires POST /api/servers/:name/restart onto the mux.
// URL pattern: /api/servers/<name>/<action>. Today the only supported action
// is "restart"; other actions 404 so future additions stay additive.
func registerServerRoutes(s *Server) {
	s.mux.HandleFunc("/api/servers/", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/servers/")
		parts := strings.Split(rest, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.NotFound(w, r)
			return
		}
		name, action := parts[0], parts[1]
		switch action {
		case "restart":
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if err := s.restart.Restart(name); err != nil {
				writeAPIError(w, err, http.StatusInternalServerError, "RESTART_FAILED")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
}
