// internal/gui/demigrate.go
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// demigrateRequest is the /api/demigrate POST body.
//
// Servers lists server names whose migrated entries should be rolled back to
// their pre-migrate stdio shape. Clients is optional: when non-empty it
// narrows the rollback to the listed client adapters (matches
// api.DemigrateOpts.ClientsInclude semantics). Empty Clients rolls back
// every (server, client) binding the manifest lists.
type demigrateRequest struct {
	Servers []string `json:"servers"`
	Clients []string `json:"clients,omitempty"`
}

func registerDemigrateRoutes(s *Server) {
	s.mux.HandleFunc("/api/demigrate", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req demigrateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		if err := s.demigrater.Demigrate(req.Servers, req.Clients); err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "DEMIGRATE_FAILED")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
