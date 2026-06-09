// internal/gui/lsp_trusted_roots_handler.go
//
// GUI management surface for the LSP trusted-roots store
// (<state-dir>/lsp-trusted-roots.json). The store is the authorization
// boundary the GUI LSP router consults before first-touch auto-register
// (see internal/api/lsp_trusted_roots.go). Before this handler the
// operator had to hand-edit the JSON file; this exposes view / add /
// remove through the GUI Settings "Trusted Roots" panel.
//
// This is PURELY the management UI on top of the #272 gate — it does NOT
// change the containment / bless-on-explicit-register / refusal logic.
// Add (POST) delegates to api.BlessDefaultTrustedRoot (the same hardened
// idempotent append the explicit-register path uses); Remove (DELETE)
// delegates to api.RemoveDefaultTrustedRoot (the inverse — it only ever
// shrinks trust, so it cannot re-open the vulnerability). All three
// methods return the fresh on-disk list so the UI can re-render without a
// follow-up GET.
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/api"
)

func registerLSPTrustedRootsRoutes(s *Server) {
	s.mux.HandleFunc("/api/lsp/trusted-roots", s.requireSameOrigin(s.lspTrustedRootsHandler))
}

// lspTrustedRootsResponse is the wire shape returned by every method.
// Roots is always a non-nil JSON array (empty when the store is absent)
// so the UI never has to special-case null. Path is the absolute store
// path so the operator can still hand-inspect / back up the file.
type lspTrustedRootsResponse struct {
	Roots []string `json:"roots"`
	Path  string   `json:"path"`
}

// lspTrustedRootsHandler dispatches GET / POST / DELETE on
// /api/lsp/trusted-roots. Method dispatch + JSON error helpers mirror
// secretsListOrAddHandler (internal/gui/secrets.go:80).
func (s *Server) lspTrustedRootsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.lspTrustedRootsList(w, r)
	case http.MethodPost:
		s.lspTrustedRootsAdd(w, r)
	case http.MethodDelete:
		s.lspTrustedRootsRemove(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// lspTrustedRootsList handles GET /api/lsp/trusted-roots. An absent store
// is tolerated and returns an empty list (LoadDefaultLSPTrustedRoots
// already maps os.ErrNotExist → empty file).
func (s *Server) lspTrustedRootsList(w http.ResponseWriter, _ *http.Request) {
	resp, err := loadTrustedRootsResponse()
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "LSP_TRUSTED_ROOTS_LIST_FAILED")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// lspTrustedRootsAdd handles POST /api/lsp/trusted-roots with body
// {"root":"<abs path>"}. The root must be non-empty and absolute;
// relative or empty paths are rejected with 400 before any write so a
// fat-fingered relative path never lands in the store (where it could
// not match a canonical workspace root anyway). On success it returns
// the refreshed list.
func (s *Server) lspTrustedRootsAdd(w http.ResponseWriter, r *http.Request) {
	root, ok := decodeTrustedRootBody(w, r)
	if !ok {
		return
	}
	if root == "" {
		writeAPIError(w, fmt.Errorf("root is required"), http.StatusBadRequest, "LSP_TRUSTED_ROOTS_INVALID")
		return
	}
	if !filepath.IsAbs(root) {
		writeAPIError(w, fmt.Errorf("root %q must be an absolute path", root), http.StatusBadRequest, "LSP_TRUSTED_ROOTS_NOT_ABSOLUTE")
		return
	}
	if err := api.BlessDefaultTrustedRoot(root); err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "LSP_TRUSTED_ROOTS_ADD_FAILED")
		return
	}
	respondWithTrustedRoots(w)
}

// lspTrustedRootsRemove handles DELETE /api/lsp/trusted-roots with body
// {"root":"<path>"}. Removing an absent root is an idempotent no-op
// success (api.RemoveDefaultTrustedRoot). The body must still name a
// non-empty root so an empty DELETE cannot accidentally rewrite the
// store. The path is NOT required to be absolute here because the stored
// canonical form is what matters and the remove canonicalizes the input
// the same way the store does; a relative path simply resolves against
// the process cwd and almost certainly matches nothing (no-op).
func (s *Server) lspTrustedRootsRemove(w http.ResponseWriter, r *http.Request) {
	root, ok := decodeTrustedRootBody(w, r)
	if !ok {
		return
	}
	if root == "" {
		writeAPIError(w, fmt.Errorf("root is required"), http.StatusBadRequest, "LSP_TRUSTED_ROOTS_INVALID")
		return
	}
	if err := api.RemoveDefaultTrustedRoot(root); err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "LSP_TRUSTED_ROOTS_REMOVE_FAILED")
		return
	}
	respondWithTrustedRoots(w)
}

// decodeTrustedRootBody decodes the {"root":"..."} body shared by POST
// and DELETE, trims whitespace, and writes a 400 envelope + returns
// ok=false on malformed JSON.
func decodeTrustedRootBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		Root string `json:"root"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "LSP_TRUSTED_ROOTS_INVALID_JSON")
		return "", false
	}
	return strings.TrimSpace(body.Root), true
}

// loadTrustedRootsResponse reads the default store and builds the wire
// response with a non-nil Roots slice and the absolute store path.
func loadTrustedRootsResponse() (lspTrustedRootsResponse, error) {
	path, err := api.DefaultLSPTrustedRootsPath()
	if err != nil {
		return lspTrustedRootsResponse{}, err
	}
	f, err := api.LoadLSPTrustedRoots(path)
	if err != nil {
		return lspTrustedRootsResponse{}, err
	}
	roots := f.Roots
	if roots == nil {
		roots = []string{}
	}
	return lspTrustedRootsResponse{Roots: roots, Path: path}, nil
}

// respondWithTrustedRoots reloads the store after a successful mutation
// and writes the fresh list. A reload failure after a successful write
// is surfaced as 500 — the mutation landed but the post-mutation read
// (e.g. an insecure-parent gate flip mid-request) failed, which the UI
// should treat as "refresh manually".
func respondWithTrustedRoots(w http.ResponseWriter) {
	resp, err := loadTrustedRootsResponse()
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "LSP_TRUSTED_ROOTS_LIST_FAILED")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
