package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
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

type manifestGetter interface {
	ManifestGetWithHash(name string) (yaml string, hash string, err error)
}

type manifestEditor interface {
	ManifestEditWithHash(name, yaml, expectedHash string) (newHash string, err error)
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

type manifestGetResponse struct {
	YAML string `json:"yaml"`
	Hash string `json:"hash"`
}

type manifestEditRequest struct {
	Name         string `json:"name"`
	YAML         string `json:"yaml"`
	ExpectedHash string `json:"expected_hash"`
}

type manifestEditResponse struct {
	Hash string `json:"hash"`
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
	s.mux.HandleFunc("/api/manifest/get", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			writeAPIError(w, fmt.Errorf("name must not be empty"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		yaml, hash, err := s.manifestGetter.ManifestGetWithHash(name)
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "MANIFEST_GET_FAILED")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manifestGetResponse{YAML: yaml, Hash: hash})
	}))
	s.mux.HandleFunc("/api/manifest/edit", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req manifestEditRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			writeAPIError(w, fmt.Errorf("name must not be empty"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		newHash, err := s.manifestEditor.ManifestEditWithHash(name, req.YAML, req.ExpectedHash)
		if err != nil {
			code := "MANIFEST_EDIT_FAILED"
			status := http.StatusInternalServerError
			if errors.Is(err, api.ErrManifestHashMismatch) {
				code = "MANIFEST_HASH_MISMATCH"
				status = http.StatusConflict
			}
			writeAPIError(w, err, status, code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manifestEditResponse{Hash: newHash})
	}))
}
