// internal/gui/status.go
package gui

import (
	"encoding/json"
	"net/http"
)

func registerStatusRoutes(s *Server) {
	s.mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rows, err := s.status.Status()
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "STATUS_FAILED")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows)
	})
}
