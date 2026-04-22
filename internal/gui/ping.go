// internal/gui/ping.go
package gui

import (
	"encoding/json"
	"net/http"
)

func registerPingRoutes(s *Server) {
	s.mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"pid":     s.cfg.PID,
			"version": s.cfg.Version,
		})
	})
	s.mux.HandleFunc("/api/activate-window", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s.onActivateWindow != nil {
			s.onActivateWindow()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
