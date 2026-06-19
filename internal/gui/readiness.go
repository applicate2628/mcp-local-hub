package gui

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// registerReadinessRoutes wires /api/server/readiness — the GUI-facing surface
// of the install readiness check (epic install-and-it-works, area 1).
//
//	GET  ?server=<name>  → that saved server's ReadinessReport
//	GET  (no param)      → every server's, for a fleet-wide view
//	POST {yaml}          → readiness of a DRAFT manifest the Add/Edit-server
//	                       screen is composing, BEFORE it exists on disk — so the
//	                       panel shows live readiness (incl. which secrets are
//	                       unset) as the operator fills the form.
//
// Each report carries per-requirement guided fixes so the GUI renders an
// actionable panel instead of letting a missing dependency or unset secret
// surface later as a cryptic HTTP-502 at the client.
func registerReadinessRoutes(s *Server) {
	s.mux.HandleFunc("/api/server/readiness", s.requireSameOrigin(s.readinessHandler))
}

type readinessDraftRequest struct {
	YAML string `json:"yaml"`
}

func (s *Server) readinessHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.readinessByName(w, r)
	case http.MethodPost:
		s.readinessDraft(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) readinessByName(w http.ResponseWriter, r *http.Request) {
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
		writeReadinessJSON(w, rep)
		return
	}
	writeReadinessJSON(w, api.AllServerReadiness())
}

// readinessDraft checks a DRAFT manifest (not yet saved) the Add/Edit-server
// screen is composing — same {yaml} body shape as /api/manifest/validate — so
// the panel can show readiness (and the inline secret-entry prompts) before the
// manifest exists on disk.
func (s *Server) readinessDraft(w http.ResponseWriter, r *http.Request) {
	var req readinessDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	m, err := config.ParseManifest(strings.NewReader(req.YAML))
	if err != nil {
		// A draft mid-edit is frequently not-yet-valid; surface a redacted 400
		// (the parse error can wrap field detail) so the panel shows "fix the
		// manifest first" without leaking host paths.
		http.Error(w, "draft manifest could not be parsed", http.StatusBadRequest)
		return
	}
	rep := api.CheckServerReadiness(m)
	// Mirror the SAME two name gates Save & Install runs in ManifestCreate, so a
	// draft the create path would reject never renders "Ready to install" and then
	// fails before install starts (Codex #378 r4/r6):
	//   1. CheckManifestName — a frontend-regex-valid but backend-reserved name
	//      (Windows `con` / `nul.txt` / `aux`).
	//   2. ManifestValidateMode(Strict) — strict-mode-only rejections the regular
	//      validate accepts, notably a `__` substring in the server name.
	// Both errors are about the manifest/name (validation text, not a host path),
	// so they are safe to echo.
	nameErr := api.CheckManifestName(m.Name)
	if nameErr == nil {
		if _, strictErr := api.NewAPI().ManifestValidateMode(req.YAML, api.ValidateModeStrict); strictErr != nil {
			nameErr = strictErr
		}
	}
	if nameErr != nil {
		rep.Ready = false
		rep.Requirements = append([]api.ReadinessRequirement{{
			Name:   "server name",
			OK:     false,
			Reason: nameErr.Error(),
			Fix:    "choose a name the backend accepts (avoid reserved names like con/nul/aux and a `__` substring)",
		}}, rep.Requirements...)
	}
	writeReadinessJSON(w, rep)
}

func writeReadinessJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
