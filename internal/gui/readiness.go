package gui

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"mcp-local-hub/internal/api"
)

// registerReadinessRoutes wires GET /api/server/readiness — the GUI-facing
// surface of the install readiness check (epic install-and-it-works, area 1).
// With ?server=<name> it returns that one server's ReadinessReport; with no
// param it returns every server's, for a fleet-wide "what needs fixing before
// this works" view. Each report carries per-requirement guided fixes so the
// GUI renders an actionable panel instead of letting a missing dependency or
// unset secret surface later as a cryptic HTTP-502 at the client.
func registerReadinessRoutes(s *Server) {
	s.mux.HandleFunc("/api/server/readiness", s.requireSameOrigin(s.readinessHandler))
}

func (s *Server) readinessHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if name := r.URL.Query().Get("server"); name != "" {
		rep, err := api.CheckServerReadinessByName(name)
		if err != nil {
			// Unknown / unparseable server name. The loader error wraps an
			// os.PathError carrying the manifest's absolute disk path — do NOT
			// echo it to the response (Codex #377 r8). Log the full error for
			// diagnosis; return a redacted 404 that names only the operator's
			// own query input.
			log.Printf("readiness: resolve manifest %q: %v", name, err)
			http.Error(w, fmt.Sprintf("server %q not found or its manifest could not be loaded", name), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(rep)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(api.AllServerReadiness())
}
