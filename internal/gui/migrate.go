// internal/gui/migrate.go
package gui

import (
	"encoding/json"
	"net/http"

	"mcp-local-hub/internal/api"
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
	s.mux.HandleFunc("/api/migrate", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req migrateRequest
		if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
			writeDecodeBodyError(w, err, "BAD_REQUEST")
			return
		}
		report, err := s.migrator.Migrate(req.Servers, req.Clients)
		if err != nil {
			// Setup-level failure (e.g., manifest load failed for every
			// requested server). No per-row data to surface; 500. The
			// setup error can wrap an *os.PathError embedding the operator's
			// absolute home path, so log server-side + return a stable
			// opaque message (G16 P2).
			writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "MIGRATE_FAILED", "/api/migrate")
			return
		}
		// Defensive: a nil report on nil error is treated as an empty
		// success (no rows touched). Encode an empty payload so the
		// frontend always parses the same shape.
		if report == nil {
			report = &api.MigrateReport{}
		}
		// Bug-bash B1 closure (#7) symmetry with /api/demigrate: per-
		// row failures surface via 207 Multi-Status with the structured
		// body, NOT a flattened 500 error blob. Full success → 200.
		status := http.StatusOK
		if len(report.Failed) > 0 {
			status = http.StatusMultiStatus
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(report)
	}))
}
