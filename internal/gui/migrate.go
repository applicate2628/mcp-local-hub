// internal/gui/migrate.go
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type migrateRequest struct {
	Servers []string `json:"servers"`
}

func registerMigrateRoutes(s *Server) {
	s.mux.HandleFunc("/api/migrate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req migrateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		if err := s.migrator.Migrate(req.Servers); err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "MIGRATE_FAILED")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
