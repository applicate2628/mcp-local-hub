// internal/gui/servers.go
package gui

import (
	"encoding/json"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
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
			results, err := s.restart.Restart(name)
			if results == nil {
				results = []api.RestartResult{}
			}
			body := map[string]any{"restart_results": results}
			if err != nil {
				body["error"] = err.Error()
				body["code"] = "RESTART_FAILED"
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(body)
				return
			}
			anyFailed := false
			for _, r := range results {
				if r.Err != "" {
					anyFailed = true
					break
				}
			}
			status := http.StatusOK
			if anyFailed {
				status = http.StatusMultiStatus // 207
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		default:
			http.NotFound(w, r)
		}
	}))
}
