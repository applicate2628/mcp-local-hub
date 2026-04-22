// internal/gui/migrate.go
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// migrateRequest is the /api/migrate POST body.
//
// Servers is the list of server names to migrate. Clients is optional: when
// non-empty it narrows the rewrite to the listed client adapters, matching
// api.MigrateOpts.ClientsInclude semantics. An empty/omitted Clients preserves
// the original "rewrite every client binding configured for these servers"
// behavior — useful for CLI-style "migrate whole server" workflows.
//
// The GUI sends both fields so flipping one (server, client) checkbox does
// not silently rewrite the other client rows on the same server.
type migrateRequest struct {
	Servers []string `json:"servers"`
	Clients []string `json:"clients,omitempty"`
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
		if err := s.migrator.Migrate(req.Servers, req.Clients); err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "MIGRATE_FAILED")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
