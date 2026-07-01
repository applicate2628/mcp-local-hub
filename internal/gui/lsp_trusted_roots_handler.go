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
	canonical, changed, err := api.BlessDefaultTrustedRootDetailed(root)
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "LSP_TRUSTED_ROOTS_ADD_FAILED")
		return
	}
	// Only audit when the store actually changed — an already-trusted retry is
	// a no-op and must not log a spurious authorization-boundary change (bot r2
	// P3). Log the canonical path the mutation applied (not a recompute) so the
	// row names the path the boundary trusts even for symlinked/non-clean input.
	if changed {
		s.publishTrustedRootAudit("lsp-trusted-root-add", root, canonical)
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
	canonical, changed, err := api.RemoveDefaultTrustedRootDetailed(root)
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "LSP_TRUSTED_ROOTS_REMOVE_FAILED")
		return
	}
	// Only audit an actual de-trust — removing an absent root is a no-op and a
	// stale-UI delete must not log a boundary change (bot r2 P3). Log the
	// canonical path the mutation resolved, not a recompute.
	if changed {
		s.publishTrustedRootAudit("lsp-trusted-root-remove", root, canonical)
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
	if err := decodeJSONBodyLimited(w, r, &body, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "LSP_TRUSTED_ROOTS_INVALID_JSON")
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

// publishTrustedRootAudit emits a gui-events.log "operator-action" row
// (deep-review round-2 P3 finding: this handler mutates the LSP
// trusted-roots authorization boundary — the store the GUI LSP router
// and the serena router's WorkspaceRootTrusted consult before
// first-touch auto-register — but previously emitted no audit trail at
// all, unlike every sibling mutation handler such as secrets.go and
// backups_actions.go). Called ONLY after the mutation actually CHANGED the
// store (the *Detailed variants report `changed`), so an idempotent
// already-trusted add / absent-root remove never logs a spurious
// authorization-boundary change (bot r2 P3). detail carries the raw requested
// root path (not secret material, so logging it verbatim is fine) and the
// CANONICAL root the mutation ACTUALLY APPLIED (canonical_root — passed in from
// Bless/RemoveDefaultTrustedRootDetailed, computed ONCE inside the store's held
// flock; a second out-of-band canonicalize could resolve a symlink differently
// and name a path the store never touched — bot r2 P3 recompute-race), plus the
// resulting trusted-root count; a best-effort re-read failure to compute the
// count is tolerated (count omitted) rather than blocking the audit row or the
// response.
func (s *Server) publishTrustedRootAudit(action, root, canonicalRoot string) {
	detail := map[string]any{"root": root, "canonical_root": canonicalRoot}
	if resp, err := loadTrustedRootsResponse(); err == nil {
		detail["count"] = len(resp.Roots)
	}
	s.events.PublishOperatorAction(action, api.CurrentOSUser(), detail)
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
