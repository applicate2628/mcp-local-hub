package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

// manifestLister / manifestDeleter are the pin-point subsets of api.API
// backing GET /api/manifests and DELETE /api/manifest/:name. Same
// Server-local-interface idiom as the create/validate/get/edit subsets
// above so manifest_test.go can swap fakes without the whole API surface.
type manifestLister interface {
	ManifestList() ([]string, error)
}

type manifestDeleter interface {
	ManifestDelete(name string) error
}

type manifestListResponse struct {
	// Manifests is always a JSON array — never null — so the frontend
	// can map over it without a null guard. An empty set is 200 [].
	Manifests []string `json:"manifests"`
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
			// Codex R1 (#16 P2): err may be an *os.PathError that includes an
			// absolute filesystem path. Log the real error server-side; send
			// a stable sanitized message to the client.
			log.Printf("/api/manifest/create name=%q: %v", name, err)
			writeAPIError(w, errors.New("internal error creating manifest"), http.StatusInternalServerError, "MANIFEST_CREATE_FAILED")
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
			// Codex R1 (#16 P2): err is often *os.PathError on missing or
			// unreadable manifests and leaks the absolute manifest dir.
			log.Printf("/api/manifest/get name=%q: %v", name, err)
			writeAPIError(w, errors.New("internal error reading manifest"), http.StatusInternalServerError, "MANIFEST_GET_FAILED")
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
			// Hash-mismatch is a legitimate client-facing signal with a
			// generic message; pass it through. Other errors can wrap
			// *os.PathError (atomic-rename failure, stat failure during
			// hash-gate read) and must be sanitized before responding.
			if errors.Is(err, api.ErrManifestHashMismatch) {
				writeAPIError(w, err, http.StatusConflict, "MANIFEST_HASH_MISMATCH")
				return
			}
			log.Printf("/api/manifest/edit name=%q: %v", name, err)
			writeAPIError(w, errors.New("internal error writing manifest"), http.StatusInternalServerError, "MANIFEST_EDIT_FAILED")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manifestEditResponse{Hash: newHash})
	}))
	// GET /api/manifests — sorted list of available server names. Empty
	// set is 200 [] (no 404): "no manifests yet" is a normal state the
	// frontend renders as an empty grid, not an error.
	s.mux.HandleFunc("/api/manifests", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		names, err := s.manifestLister.ManifestList()
		if err != nil {
			// ManifestList can wrap an *os.PathError from the disk-union
			// read; sanitize like the other manifest handlers.
			log.Printf("/api/manifests: %v", err)
			writeAPIError(w, errors.New("internal error listing manifests"), http.StatusInternalServerError, "MANIFEST_LIST_FAILED")
			return
		}
		if names == nil {
			names = []string{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manifestListResponse{Manifests: names})
	}))
	// DELETE /api/manifest/:name — remove the named manifest directory.
	// Registered as the /api/manifest/ subtree; the exact-path handlers
	// (/create, /validate, /get, /edit) are more specific and take
	// precedence in net/http.ServeMux, so this only receives :name
	// suffixes that are not one of those reserved words.
	//
	// This does NOT uninstall the server (api.ManifestDelete is
	// teardown-of-the-manifest-only — DELETE /api/install/:server is the
	// uninstall path); the frontend orchestrates uninstall-then-delete.
	s.mux.HandleFunc("/api/manifest/", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.Header().Set("Allow", "DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/manifest/"))
		if name == "" || strings.Contains(name, "/") {
			// Empty (DELETE /api/manifest/) or a nested path — neither is a
			// valid bare :name. api.ManifestDelete's own checkManifestName
			// would also reject these, but a 400 here is the clearer signal.
			writeAPIError(w, fmt.Errorf("manifest name required"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		if err := s.manifestDeleter.ManifestDelete(name); err != nil {
			// api.ManifestDelete returns either a validation error (bad
			// name — client's fault) or a does-not-exist / *os.PathError
			// (leaks the manifest dir). Map the not-exist case to 404 with
			// a stable message and sanitize everything else to a 500.
			log.Printf("/api/manifest/ delete name=%q: %v", name, err)
			if strings.Contains(err.Error(), "does not exist") {
				writeAPIError(w, fmt.Errorf("manifest %q not found", name), http.StatusNotFound, "MANIFEST_NOT_FOUND")
				return
			}
			writeAPIError(w, errors.New("internal error deleting manifest"), http.StatusInternalServerError, "MANIFEST_DELETE_FAILED")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
