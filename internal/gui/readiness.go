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
	YAML     string `json:"yaml"`
	Mode     string `json:"mode"`
	EditName string `json:"edit_name"`
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
	// Mirror the SAME gates Save & Install runs in ManifestCreate, so a draft the
	// create path would reject never renders "Ready to install" and then fails
	// before install starts (Codex #378 r4/r6/r11):
	//   1. CheckManifestName — a frontend-regex-valid but backend-reserved name
	//      (Windows `con` / `nul.txt` / `aux`).
	//   2. create-mode existence — ManifestCreateIn rejects an already-saved disk
	//      manifest, while edit mode must allow the edit target to exist.
	//   3. ManifestValidateMode(Strict) warnings/errors — the storage path treats
	//      manifestBlockingWarnings as hard errors even when strictErr is nil.
	nameErr := api.CheckManifestName(m.Name)
	var blockers []api.ReadinessRequirement
	if nameErr != nil {
		blockers = append(blockers, api.ReadinessRequirement{
			Name:   "server name",
			OK:     false,
			Reason: nameErr.Error(),
			Fix:    "choose a name the backend accepts (avoid reserved names like con/nul/aux and a `__` substring)",
		})
	} else if strings.TrimSpace(req.Mode) == "create" && m.Name != strings.TrimSpace(req.EditName) && s.manifestPresence != nil {
		exists, err := s.manifestPresence.ManifestExists(m.Name)
		if err != nil {
			log.Printf("readiness: check manifest existence %q: %v", m.Name, err)
		} else if exists {
			blockers = append(blockers, api.ReadinessRequirement{
				Name:   "manifest exists",
				OK:     false,
				Reason: fmt.Sprintf("manifest %q already exists; use edit instead", m.Name),
				Fix:    "choose a new server name, or open the saved manifest in edit mode",
			})
		}
	}
	warnings, strictErr := api.NewAPI().ManifestValidateMode(req.YAML, api.ValidateModeStrict)
	for _, warning := range warnings {
		blockers = append(blockers, api.ReadinessRequirement{
			Name:   "manifest validation",
			OK:     false,
			Reason: warning,
			Fix:    "fix the manifest validation warning before saving",
		})
	}
	if strictErr != nil {
		blockers = append(blockers, api.ReadinessRequirement{
			Name:   "server name",
			OK:     false,
			Reason: strictErr.Error(),
			Fix:    "choose a name the backend accepts (avoid reserved names like con/nul/aux and a `__` substring)",
		})
	}
	if len(blockers) > 0 {
		rep.Ready = false
		rep.Requirements = append(blockers, rep.Requirements...)
	}
	writeReadinessJSON(w, rep)
}

func writeReadinessJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
