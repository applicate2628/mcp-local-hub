// internal/gui/demigrate.go
package gui

import (
	"encoding/json"
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
		if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
			writeDecodeBodyError(w, err, "BAD_REQUEST")
			return
		}
		report, err := s.demigrater.Demigrate(req.Servers, req.Clients)
		if err != nil {
			// Setup-level failure (e.g., manifest load failed for every
			// requested server). No per-row data to surface; 500. The
			// setup error can wrap an *os.PathError embedding the operator's
			// absolute home path, so log server-side + return a stable
			// opaque message (G16 P2).
			writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "DEMIGRATE_FAILED", "/api/demigrate")
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
		// gui-events.log audit row (deep-review P3 finding): emit one row
		// per server/client pair that actually rolled back. A fully-failed
		// request (Restored empty) leaves no row. Identifiers only
		// (server/client names); no secret material is ever in scope here.
		if len(report.Restored) > 0 {
			restored := make([]map[string]any, 0, len(report.Restored))
			for _, r := range report.Restored {
				restored = append(restored, map[string]any{"server": r.Server, "client": r.Client})
			}
			s.events.PublishOperatorAction("demigrate", api.CurrentOSUser(), map[string]any{
				"restored":     restored,
				"failed_count": len(report.Failed),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(report)
	}))
}
