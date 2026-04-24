package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// manifestCreator / manifestValidator are the pin-point subsets of api.API
// that the GUI layer calls for manifest writes. Keeping them as Server-local
// interfaces lets us substitute fakes in manifest_test.go without pulling
// the whole API surface.
type manifestCreator interface {
	ManifestCreate(name, yaml string) error
}

type manifestValidator interface {
	ManifestValidate(yaml string) []string
}

type manifestCreateRequest struct {
	Name string `json:"name"`
	YAML string `json:"yaml"`
}

type manifestValidateRequest struct {
	YAML string `json:"yaml"`
}

type manifestValidateResponse struct {
	Warnings []string `json:"warnings"`
}

// registerManifestRoutes wires POST /api/manifest/create and
// POST /api/manifest/validate onto the server's mux.
//
// Both handlers use the requireSameOrigin guard (Sec-Fetch-Site header).
// Validate is POST-only even though it reads nothing — the YAML payload
// goes in the request body and some YAMLs will be large, exceeding safe
// URL length.
func registerManifestRoutes(s *Server) {
	s.mux.HandleFunc("/api/manifest/create", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req manifestCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			writeAPIError(w, fmt.Errorf("name must not be empty"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		if err := s.manifestCreator.ManifestCreate(name, req.YAML); err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "MANIFEST_CREATE_FAILED")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	s.mux.HandleFunc("/api/manifest/validate", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req manifestValidateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		warnings := s.manifestValidator.ManifestValidate(req.YAML)
		if warnings == nil {
			warnings = []string{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manifestValidateResponse{Warnings: warnings})
	}))
}
