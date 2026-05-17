// internal/gui/status.go
package gui

import (
	"encoding/json"
	"net/http"
)

// registerStatusRoutes wires GET /api/status. Phase 6 of G2 re-sourced
// this handler from s.health (shared with /api/health) so both
// endpoints serve from one TTL+singleflight cache: a single underlying
// StatusWithOpts call inside the daemons-section TTL window now feeds
// /api/status, /api/health, /api/health?include=probes, and
// /api/health?include=capabilities (when the latter two need the
// daemons backbone). Zero drift between surfaces.
//
// Wire shape is preserved byte-for-byte: DaemonStatusSnapshot returns
// the canonical []api.DaemonStatus (TaskName, NextRun, Health, and
// workspace-scoped fields all intact). The thinner DaemonRow form lives
// only inside HealthSnapshot.Daemons.Items.
func registerStatusRoutes(s *Server) {
	s.mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rows, err := s.health.DaemonStatusSnapshot(r.Context())
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "STATUS_FAILED")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows)
	})
}
