// internal/gui/demigrate.go
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"

	"mcp-local-hub/internal/api"
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
		report, err := s.demigrater.Demigrate(req.Servers, req.Clients)
		if err != nil {
			// Setup-level failure (e.g., manifest load failed for every
			// requested server). No per-row data to surface; 500.
			writeAPIError(w, err, http.StatusInternalServerError, "DEMIGRATE_FAILED")
			return
		}
		// Defensive: a nil report on nil error is treated as an empty
		// success (no rows touched). Encode an empty payload so the
		// frontend always parses the same shape.
		if report == nil {
			report = &api.DemigrateReport{}
		}
		// Bug-bash B1 closure (#7): per-row failures surface via 207
		// Multi-Status with the structured body, NOT as a flattened
		// 500 error blob. The frontend iterates report.Failed[] to
		// render per-cell error rows with individual retry context.
		// Full success → 200 (with structured body too, so the
		// frontend always parses the same shape).
		status := http.StatusOK
		if len(report.Failed) > 0 {
			status = http.StatusMultiStatus
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(report)
	}))
}
