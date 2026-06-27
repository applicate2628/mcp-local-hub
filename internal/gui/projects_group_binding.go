// internal/gui/projects_group_binding.go
//
// Per-project-GUI Phase 3c — the group↔project BINDING write (design decision
// §10.1). POST /api/projects/group-binding {group, project_path} binds ONE
// group to ONE project (empty project_path = UNBIND → global). It is the
// composition root that:
//
//  1. validates the request (same-origin via requireSameOrigin; body shape);
//  2. validates project_path WITHOUT requiring it to exist — a group can be
//     authored before the project is registered (§10.1). The path is clean+abs
//     validated (relative / traversal rejected) then normalized via the SINGLE
//     join-key owner clients.CanonicalProjectKey (so a stored value is always a
//     canonical key, never a raw operator path);
//  3. mutates groups.yaml through the EXISTING atomic, lost-update-safe owner
//     api.ReadModifyWriteGroups (the SAME path /api/groups + /api/projects/toggle
//     use) — it edits Group.ProjectPath in place under one held hub-mcp.lock;
//  4. returns the group's new binding so the GUI reflects the persisted result.
//
// NO snapshot republish (§5/T3): project_path is DATA-ONLY — it is neither the
// "g:<name>" scope key nor a /g/<name>/mcp route segment, and
// BuildResolverSnapshotFromManifestsAndGroups never reads it. A bind/unbind
// changes neither routing nor membership nor the per-tool filter, so the live
// hub snapshot is unaffected; only the /api/projects project-lens READ filter
// changes. (Contrast /api/projects/toggle's group-servers path, which DOES
// republish because it mutates membership.)
//
// SECURITY (threat model, §10.1 T3/T4/T5):
//   - T3 groups.yaml: the only persistence sink is the fixed
//     <state-dir>/groups.yaml via ReadModifyWriteGroups (atomic temp+rename,
//     flock, DACL gate); project_path is data-only and cannot feed the route.
//   - T4 CSRF: registered behind requireSameOrigin.
//   - T5 leak: errors go through writeAPIErrorRedacted (stable code, fixed body,
//     reason server-side only).
package gui

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// projectGroupBindingRequest is the POST /api/projects/group-binding body.
//   - Group is the group name to (re)bind. Required.
//   - ProjectPath is the project root to bind the group to. EMPTY = unbind
//     (make the group global / visible in every project lens). A non-empty
//     value is validated (clean+abs, no traversal) and normalized to the
//     canonical project key before persisting; it is NOT required to exist.
type projectGroupBindingRequest struct {
	Group       string `json:"group"`
	ProjectPath string `json:"project_path"`
}

// projectGroupBindingResponse echoes the group's NEW persisted binding so the
// GUI reflects the result, not the request intent. ProjectPath is "" when the
// group is global (unbound).
type projectGroupBindingResponse struct {
	Group       string `json:"group"`
	ProjectPath string `json:"project_path"`
}

func registerProjectsGroupBindingRoutes(s *Server) {
	s.mux.HandleFunc("/api/projects/group-binding", s.requireSameOrigin(s.projectsGroupBindingHandler))
}

func (s *Server) projectsGroupBindingHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req projectGroupBindingRequest
	if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "BAD_REQUEST")
		return
	}

	group := strings.TrimSpace(req.Group)
	if group == "" {
		writeAPIError(w, fmt.Errorf("group is required"),
			http.StatusBadRequest, "PROJECT_GROUP_BINDING_INVALID")
		return
	}

	// Validate + normalize the project_path. Empty = unbind (global). A non-empty
	// value must be clean+abs (no relative / no traversal) but is NOT required to
	// exist (§10.1 — a group can be authored before the project is registered).
	// After validation it is reduced to the SINGLE canonical join key via
	// clients.CanonicalProjectKey so a stored value matches what the /api/projects
	// aggregate keys each project on.
	canonical, err := normalizeBindingProjectPath(req.ProjectPath)
	if err != nil {
		// err is our own non-leaking message (no os.PathError, no filesystem
		// layout); a plain 400 with the stable code.
		writeAPIError(w, err, http.StatusBadRequest, "PROJECT_GROUP_BINDING_INVALID")
		return
	}

	// Atomic read-modify-write under ONE held hub-mcp.lock. The callback sets
	// ProjectPath in place on the matching group; a missing group returns the
	// errGroupNotFound SENTINEL so ReadModifyWriteGroups propagates a non-nil
	// callback error WITHOUT writing (no write on not-found, mirroring the
	// /api/projects/toggle group path). Binding deletes nothing → empty
	// deleted-set → no token-row prune.
	mutErr := api.ReadModifyWriteGroups(func(cfg *api.GroupsConfig) ([]string, error) {
		for i := range cfg.Groups {
			if cfg.Groups[i].Name != group {
				continue
			}
			cfg.Groups[i].ProjectPath = canonical
			return nil, nil
		}
		return nil, errGroupNotFound
	})
	if mutErr != nil {
		if errors.Is(mutErr, errGroupNotFound) {
			writeAPIError(w, fmt.Errorf("group %q not found", group),
				http.StatusNotFound, "PROJECT_GROUP_BINDING_NOT_FOUND")
			return
		}
		writeAPIErrorRedacted(w, mutErr, http.StatusInternalServerError,
			"PROJECT_GROUP_BINDING_FAILED", "/api/projects/group-binding")
		return
	}

	writeJSON(w, http.StatusOK, projectGroupBindingResponse{
		Group:       group,
		ProjectPath: canonical,
	})
}

// normalizeBindingProjectPath validates a binding project_path and reduces it to
// the canonical project key. An EMPTY input is the UNBIND form → returns ("",
// nil) (global). A non-empty input is validated to be a CLEAN ABSOLUTE path with
// no traversal segments (mirroring the clients.ProjectScanConfigPaths IsAbs +
// Clean round-trip guard) but is deliberately NOT stat'd — §10.1 allows binding
// to a not-yet-registered project. On success it returns
// clients.CanonicalProjectKey(path), the SAME join key the aggregate composes on
// (so a stored binding always matches a project's aggregate key). The error
// messages never echo the raw path so they are safe to surface on a 400.
func normalizeBindingProjectPath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", nil // unbind → global
	}
	if pathHasControlChars(p) {
		return "", fmt.Errorf("project_path contains control characters")
	}
	// Separator normalization BEFORE the IsAbs + clean round-trip guard, mirroring
	// ProjectScanConfigPaths: the frontend canonicalizes roots with FORWARD slashes
	// (`C:/dev/proj`), but filepath.Clean produces native separators, so a
	// forward-slash Windows root would fail the round-trip guard otherwise.
	// FromSlash swaps `/`→`\` on Windows and is a no-op on POSIX, and does NOT
	// collapse `..` segments — so `C:/dev/../etc` still fails the round-trip guard
	// below and is rejected.
	native := filepath.FromSlash(p)
	if !filepath.IsAbs(native) {
		return "", fmt.Errorf("project_path must be an absolute path")
	}
	// Clean + round-trip equality rejects any path not already in canonical clean
	// form (embeds `..`, `.`, or redundant separators) — denying traversal
	// smuggled through a not-yet-collapsed path. CanonicalProjectKey itself would
	// silently COLLAPSE a `..`, so this guard runs FIRST to reject it loudly.
	if filepath.Clean(native) != native {
		return "", fmt.Errorf("project_path must be a clean absolute path (no '.', '..', or redundant separators)")
	}
	// Reduce to the single canonical join key. CanonicalProjectKey is a pure,
	// CWD-free normalization (ToSlash + case-fold on Windows) — it does NOT stat
	// or resolve symlinks, so a not-yet-existing project binds cleanly.
	key := clients.CanonicalProjectKey(native)
	if key == "" {
		// Defensive: a cleaned absolute path always normalizes to a non-empty key;
		// a "" here would be an unaddressable binding, so refuse rather than store it.
		return "", fmt.Errorf("project_path could not be normalized to a project key")
	}
	return key, nil
}
