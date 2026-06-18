package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
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

// catalogLister is the pin-point subset of api.API backing GET
// /api/catalog (§10 v2a — Catalog descriptions). Same Server-local
// interface idiom as the other manifest subsets so manifest_test.go can
// swap a fake without the whole API surface. It returns the enriched
// {name, description, kind} projection rather than names-only, so the
// Catalog ("store") screen can render a one-line summary per server
// WITHOUT mutating the names-only GET /api/manifests contract (which
// keeps its existing consumers — daemon_env, cleanup — unchanged).
type catalogLister interface {
	CatalogList() ([]config.CatalogFields, error)
}

type manifestListResponse struct {
	// Manifests is always a JSON array — never null — so the frontend
	// can map over it without a null guard. An empty set is 200 [].
	Manifests []string `json:"manifests"`
}

// catalogEntry mirrors config.CatalogFields on the wire ({name,
// description, kind}). Defined here (rather than serializing
// config.CatalogFields directly) so the GUI HTTP contract owns its own
// JSON shape and stays stable if the config struct grows internal
// fields.
type catalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
}

type catalogListResponse struct {
	// Catalog is always a JSON array — never null — so the frontend can
	// map over it without a null guard. An empty set is 200 {"catalog":[]}.
	Catalog []catalogEntry `json:"catalog"`
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
	// RestartRequired / HubLive mirror groupMutationResponse (R4-2 — bot R4).
	// RestartRequired is true when the durable manifest write committed but the
	// in-place live-hub republish failed on a gate-ON hub, so /clients + /g are
	// serving a STALE snapshot until the operator restarts. HubLive reports
	// whether the gate-ON hub listener was live at mutation time so the GUI can
	// word the restart banner precisely. Both false on a gate-OFF host.
	RestartRequired bool `json:"restart_required"`
	HubLive         bool `json:"hub_live"`
}

// manifestMutationResponse is the create / delete wire shape (R4-2 — bot R4).
// Those handlers used to return 204 No Content; they now return 200 with this
// body so the same restart_required signal the edit + group paths carry reaches
// the operator when a gate-ON in-place republish fails. The fields mirror
// groupMutationResponse exactly.
type manifestMutationResponse struct {
	RestartRequired bool `json:"restart_required"`
	HubLive         bool `json:"hub_live"`
}

// republishOnManifestMutation re-publishes the live hub ResolverSnapshot after
// a successful manifest create / edit / delete, but ONLY when the gate-ON hub
// listener is live (HubMcpEndpointActive). B1 (bot R3): a manifest mutation
// changes the set of client_bindings the snapshot is built from, so without a
// republish a running gate-ON hub keeps serving the STALE snapshot for BOTH
// /clients/ AND /g/ routes until a restart or an unrelated group edit happens
// to trigger one. It reuses the SAME publishResolverSnapshotForHubBind seam the
// group-mutation tail (writeGroupMutation) uses — which rebuilds from the
// CURRENT manifests (now including this mutation) plus groups.
//
// R4-2 (bot R4): it returns (restartRequired, hubLive) so the handler can
// surface the gate-ON republish-failure to the OPERATOR rather than swallowing
// it. Before this, a republish failure on a live gate-ON hub was logged only —
// the handler still returned plain success, so the operator had NO signal that
// /clients + /g were serving a STALE snapshot (the group path already surfaced
// restart_required; the manifest path did not). restartRequired is true when
// the hub is live but the in-place republish failed (the durable write landed,
// only the live refresh wedged → "restart hub to apply"). It is false on a
// gate-OFF / hub-not-live host: there is no live snapshot to refresh and the
// next bind re-reads manifests fresh, so no restart is owed for the manifest
// op itself. hubLive mirrors HubMcpEndpointActive at mutation time so the GUI
// can word the banner precisely (matches groupMutationResponse semantics).
//
// A republish failure is NON-fatal: the manifest write already committed
// durably (it is the source of truth), so the handler must NOT 500 the
// already-successful manifest op. The failure is logged so a wedged in-place
// publish is observable; the operator can restart the hub to pick up the
// persisted edit (the same degrade the group path takes).
//
// ctx is the request context (R4-4): it bounds the ctx-aware hub-mcp.lock
// acquisition inside the publish seam so a stuck sibling holder cannot freeze
// the manifest handler past the request lifetime.
func (s *Server) republishOnManifestMutation(ctx context.Context, op string) (restartRequired, hubLive bool) {
	if !s.HubMcpEndpointActive() {
		return false, false
	}
	if err := s.republishGroupsSnapshot(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Benign (concurrency-lane F1): the request context ended (client
			// disconnect / deadline) before the in-place republish could
			// acquire hub-mcp.lock — the publish was never attempted. The
			// durable manifest write already landed and the next mutation /
			// bind republishes, so the hub is NOT wedged: no restart is owed
			// and this is not a failure worth a warn or a restart banner.
			// (F2, accepted: a client that disconnects mid-mutation forfeits
			// the in-place refresh; routing self-heals on the next republish.)
			return false, true
		}
		_ = api.LogHubMcpEvent("warn", "manifest-republish-failed", map[string]any{
			"op":  op,
			"err": err.Error(),
		})
		return true, true
	}
	return false, true
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
		// B1: refresh the live hub snapshot so a gate-ON hub picks up the new
		// server's bindings without a restart (non-fatal, gate-ON guarded).
		// R4-2: surface restart_required when the gate-ON in-place republish
		// failed so the operator knows /clients + /g are serving stale routing.
		restartRequired, hubLive := s.republishOnManifestMutation(r.Context(), "create")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(manifestMutationResponse{
			RestartRequired: restartRequired,
			HubLive:         hubLive,
		})
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
		// B1: refresh the live hub snapshot so a gate-ON hub picks up the
		// edited bindings without a restart (non-fatal, gate-ON guarded).
		// R4-2: surface restart_required when the gate-ON in-place republish
		// failed so the operator knows /clients + /g are serving stale routing.
		restartRequired, hubLive := s.republishOnManifestMutation(r.Context(), "edit")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manifestEditResponse{
			Hash:            newHash,
			RestartRequired: restartRequired,
			HubLive:         hubLive,
		})
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
	// GET /api/catalog — enriched {name, description, kind} projection of
	// every available server (§10 v2a — Catalog descriptions). This is a
	// NEW route, intentionally separate from the names-only GET
	// /api/manifests so existing consumers of that route are untouched;
	// the Catalog ("store") screen consumes this one to render a one-line
	// summary per server. Empty set is 200 {"catalog":[]} (no 404) for the
	// same "no servers yet is normal" reason the manifests route gives.
	s.mux.HandleFunc("/api/catalog", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fields, err := s.catalogLister.CatalogList()
		if err != nil {
			// CatalogList can wrap an *os.PathError from the disk-union
			// read; sanitize like the other manifest handlers.
			log.Printf("/api/catalog: %v", err)
			writeAPIError(w, errors.New("internal error listing catalog"), http.StatusInternalServerError, "CATALOG_LIST_FAILED")
			return
		}
		entries := make([]catalogEntry, 0, len(fields))
		for _, f := range fields {
			entries = append(entries, catalogEntry{Name: f.Name, Description: f.Description, Kind: f.Kind})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(catalogListResponse{Catalog: entries})
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
		// B1: refresh the live hub snapshot so a gate-ON hub drops the deleted
		// server's bindings without a restart (non-fatal, gate-ON guarded).
		// R4-2: surface restart_required when the gate-ON in-place republish
		// failed so the operator knows /clients + /g are serving stale routing.
		restartRequired, hubLive := s.republishOnManifestMutation(r.Context(), "delete")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(manifestMutationResponse{
			RestartRequired: restartRequired,
			HubLive:         hubLive,
		})
	}))
}
