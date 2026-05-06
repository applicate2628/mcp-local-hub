// internal/gui/cleanup.go
package gui

import (
	"encoding/json"
	"net/http"

	"mcp-local-hub/internal/api"
)

// registerCleanupRoutes wires the cleanup endpoints behind the same-origin
// gate. Per design memo Q5=E and the existing secrets D5 escalation
// pattern (`internal/api/secrets.go:500`), the destructive guard is a
// boolean `confirm`-style flag (here `dry_run`) plus the existing CSRF
// + DNS-rebind + loopback-only chain. The frontend Maintenance modal
// is responsible for the typed-confirmation UX before flipping
// `dry_run` to false.
func registerCleanupRoutes(s *Server) {
	s.mux.HandleFunc("/api/cleanup/orphans", s.requireSameOrigin(s.cleanupOrphansHandler))
}

// cleanupRequest is the JSON body for POST /api/cleanup/orphans.
//
//   - DryRun=true  → preview; lists candidates, kills nothing.
//   - DryRun=false → execute; kills the matched processes.
//   - MinAgeSec    → ignore processes younger than this. 0 → backend
//                    default (60s). Anti-foot-gun for legitimate
//                    in-flight installs.
//   - Server       → optional manifest-name filter. Empty → all manifests.
type cleanupRequest struct {
	DryRun    bool   `json:"dry_run"`
	MinAgeSec int64  `json:"min_age_sec"`
	Server    string `json:"server"`
}

// cleanupResponse is the JSON body returned for both dry-run and apply
// modes. Mirrors the CLI shape from `internal/cli/cleanup.go` so a
// future operator-CLI bridge can format either output identically.
type cleanupResponse struct {
	Orphans []api.OrphanProcess `json:"orphans"`
	Killed  int                 `json:"killed"`
	Skipped int                 `json:"skipped"`
}

// cleanupOrphansHandler handles POST /api/cleanup/orphans.
//
// Method-restricted to POST so a CSRF probe via `<img src=...>` cannot
// trigger a kill: the same-origin wrapper handles CSRF on POST, and a
// browser cannot issue a JSON-bodied POST cross-origin without a
// preflight that the wrapper also rejects.
//
// Errors map to status:
//   - 400 BadRequest      — JSON parse failure.
//   - 405 MethodNotAllowed — non-POST.
//   - 500 InternalServerError — backend error.
//   - 200 OK              — both dry-run and apply success paths.
func (s *Server) cleanupOrphansHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req cleanupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Empty ManifestDir → embed-first path (mirrors the convention in
	// internal/gui/extract_manifest.go and migrate.go).
	orphans, err := s.cleanup.CleanupOrphans(api.CleanupOpts{
		ManifestDir: "",
		Server:      req.Server,
		DryRun:      req.DryRun,
		MinAgeSec:   req.MinAgeSec,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
			"code":  "CLEANUP_ORPHANS_FAILED",
		})
		return
	}

	resp := cleanupResponse{Orphans: orphans}
	if !req.DryRun {
		for _, o := range orphans {
			if o.KillErr == "" {
				resp.Killed++
			} else {
				resp.Skipped++
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
