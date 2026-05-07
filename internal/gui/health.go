// internal/gui/health.go
package gui

import (
	"encoding/json"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

// registerHealthRoute wires GET /api/health behind the same-origin gate.
// All other /api/* routes go through requireSameOrigin too — even
// read-only ones — so cross-origin pages cannot probe local hub state
// (see csrf.go).
func registerHealthRoute(s *Server) {
	s.mux.HandleFunc("/api/health", s.requireSameOrigin(s.healthHandler))
}

// healthHandler implements GET /api/health[?include=…][&refresh=…].
//
// Query parameters:
//
//   - include=probes          → adds probes section
//   - include=capabilities    → adds BOTH probes AND capabilities
//                                (capability discovery requires probe)
//   - include=probes,capabilities → equivalent to capabilities
//   - refresh=true|1          → busts cache for included sections
//                                (rate-limited at the api package layer
//                                via daemonsRefreshMinMs / probesRefreshMinMs
//                                / capabilitiesRefreshMinMs — excess
//                                refreshes silently downgrade to cached)
//
// Unknown include tokens are silently ignored — forward-compat for
// G4-introduced sections, so a client that sends `include=newkind`
// against an older binary doesn't get a 400.
//
// Errors:
//
//   - non-GET → 405 with `Allow: GET`
//   - backend error → 500 + {error, code:"HEALTH_BACKEND_FAILED"}
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	opts := parseHealthOpts(r.URL.Query())
	snap, err := s.health.HealthSnapshot(opts)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
			"code":  "HEALTH_BACKEND_FAILED",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap)
}

// parseHealthOpts reads the canonical /api/health query string into
// api.HealthOpts:
//
//   - ?include=probes,capabilities  (comma-separated, repeatable key)
//   - ?refresh=true (or "1")
//
// Tokens are case-insensitive and trimmed of whitespace. Unknown tokens
// are silently dropped — forward-compat for G4 sections so an older
// binary doesn't 400 on a future client. Per spec,
// include=capabilities implies IncludeProbes because capability
// discovery walks probe-OK daemons; the api-layer HealthSnapshot already
// enforces that, but we surface the implication here too so the
// captured opts in tests reflect the user-visible contract.
func parseHealthOpts(q map[string][]string) api.HealthOpts {
	var opts api.HealthOpts
	if vals, ok := q["include"]; ok {
		for _, v := range vals {
			for _, tok := range strings.Split(v, ",") {
				switch strings.TrimSpace(strings.ToLower(tok)) {
				case "probes":
					opts.IncludeProbes = true
				case "capabilities":
					opts.IncludeCapabilities = true
					opts.IncludeProbes = true // implied
				}
			}
		}
	}
	if vals, ok := q["refresh"]; ok && len(vals) > 0 {
		opts.Refresh = strings.EqualFold(vals[0], "true") || vals[0] == "1"
	}
	return opts
}
