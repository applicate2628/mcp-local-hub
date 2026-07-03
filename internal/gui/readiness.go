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
	if err := decodeJSONBodyLimited(w, r, &req, maxManifestBodyBytes); err != nil {
		http.Error(w, decodeBodyErrorText(err), decodeBodyStatusCode(err))
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
	// before install starts (Codex #378 r4/r6/r11). This is a tracked temporary
	// re-derivation of the write gate, guarded by TestReadinessHandler_DraftPOST_
	// MirrorsManifestWriteGate; the end-state is one shared write-admission owner
	// (work-items/decisions/2026-06-20-draft-readiness-mirrors-write-gate-follow-up.md).
	//   1. CheckManifestName — a frontend-regex-valid but backend-reserved name
	//      (Windows `con` / `nul.txt` / `aux`).
	//   2. create-mode existence — ManifestCreateIn rejects an already-saved disk
	//      manifest, while edit mode must allow the edit target to exist.
	//   2b. edit-mode existence — ManifestEditInWithHash rejects a missing edit
	//      target, so a deleted-after-load manifest must block draft readiness too.
	//   3. ManifestValidateMode(Strict) warnings/errors — the storage path treats
	//      manifestBlockingWarnings as hard errors even when strictErr is nil.
	nameErr := api.CheckManifestName(m.Name)
	mode := strings.TrimSpace(req.Mode)
	editName := strings.TrimSpace(req.EditName)
	var blockers []api.ReadinessRequirement
	if nameErr != nil {
		blockers = append(blockers, api.ReadinessRequirement{
			Name:   "server name",
			OK:     false,
			Reason: nameErr.Error(),
			Fix:    "choose a name the backend accepts (avoid reserved names like con/nul/aux and a `__` substring)",
		})
	} else {
		if mode == "create" && m.Name != editName {
			// Mirror the ManifestCreateIn embed-collision refusal (CALL the gate
			// predicate api.IsEmbeddedManifestName — do NOT re-derive it, per
			// feedback_readiness_mirror_gate_via_dryrun_not_reimpl): a shipped
			// (built-in) server name cannot be saved as a disk manifest because
			// install reads embed-first, so Save & Install would refuse it. Surfacing
			// it here keeps the panel from showing "Ready to install" then failing the
			// create. Independent of manifestPresence (a pure membership predicate).
			if api.IsEmbeddedManifestName(m.Name) {
				blockers = append(blockers, api.ReadinessRequirement{
					Name:   "shipped server name",
					OK:     false,
					Reason: fmt.Sprintf("%q is a built-in shipped server; a disk manifest under this name is ignored at install (the shipped manifest installs)", m.Name),
					Fix:    fmt.Sprintf("rename the server (e.g. %q) to save a customized copy", m.Name+"-custom"),
				})
			}
			if s.manifestPresence != nil {
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
		} else if mode == "edit" && editName != "" && s.manifestPresence != nil {
			exists, err := s.manifestPresence.ManifestExists(editName)
			if err != nil {
				log.Printf("readiness: check edit manifest existence %q: %v", editName, err)
			} else if !exists {
				blockers = append(blockers, api.ReadinessRequirement{
					Name:   "manifest missing",
					OK:     false,
					Reason: fmt.Sprintf("manifest %q no longer exists — it was deleted after this edit screen loaded; reload or recreate", editName),
					Fix:    "reload the edit screen or recreate the server manifest before saving",
				})
			}
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
