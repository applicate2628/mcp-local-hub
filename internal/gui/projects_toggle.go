// internal/gui/projects_toggle.go
//
// Per-project-GUI Phase 3a — the WRITE backend. POST /api/projects/toggle is
// the IMMEDIATE per-row toggle (design decision 8: no matrix/Apply, no
// cross-file transaction) that enables/disables ONE server in ONE project
// substrate. It is the composition root that:
//
//  1. validates the request (same-origin via requireSameOrigin; body shape),
//  2. resolves the SINGLE write owner via clients.ProjectToggleOwner (the GUI
//     never branches on a client name to pick the owner — decision 5),
//  3. dispatches to the resolved owner, each of which REUSES an existing
//     hardened pipeline (no new write mechanism):
//     - A  workspace LSP            → api.Register / api.Unregister
//     - B  cursor/vscode/claude-Project (object member) →
//     clients.ToggleProjectObjectMember → mutateJSONObjectMemberPath →
//     WriteConfigFile → SecureWriteClientConfig
//     - B-claude Local (array move) → clients.ToggleClaudeMcpjsonMembership →
//     hujson + WriteConfigFile → SecureWriteClientConfig (NEVER deletes
//     mcpServers — decision 5)
//     - C  groups                   → api.ReadModifyWriteGroups, then the SAME
//     republishGroupsSnapshot seam /api/groups uses (so a membership change is
//     effective in the live hub immediately, not only after a restart); ENABLE
//     is name-gated against the same routable-server set /api/groups validates
//     against.
//  4. READS BACK the new state from the same readers /api/projects composes, so
//     the response reflects the persisted result, not the requested intent.
//
// SECURITY (threat model T1–T6):
//   - T1 project-path TOCTOU: the project config path is resolved ONLY via
//     clients.ProjectScanConfigPaths (realRoot-contained, fixed RelFile join,
//     symlink-checked — no 4th path-logic copy), and every write goes through
//     SecureWriteClientConfig (handle-relative, O_NOFOLLOW, atomic, DACL-before-
//     bytes).
//   - T2 ~/.claude.json corruption: the Local writer uses the comment+sibling-
//     preserving hujson family; the array move never deletes mcpServers.
//   - T4 CSRF: registered behind requireSameOrigin.
//   - T5 leak: errors go through writeAPIErrorRedacted (stable code, fixed body,
//     reason server-side only).
//   - T6 inertness: disabled = inert in its substrate (the substrate IS the
//     gate; no consumer-side conditional).
//
// The symlink-follow policy for the claude-local read+write is the SINGLE owner
// api.OperatorAllowsClientConfigSymlink, computed in this api/gui layer and
// injected DOWN into the clients-package writer (the R3 inject pattern) — the
// clients package never re-derives it from the environment.
package gui

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// projectToggleRequest is the POST /api/projects/toggle body.
//
//   - Root is the project root (Model A workspace path / Model B project root).
//     Required for scopes A + B; ignored for scope C (groups key off Group).
//   - Client is the client id (cursor / vscode / claude-code) for Model B; it
//     selects the per-client substrate. Ignored for A and C.
//   - Scope is the disambiguator that picks the write owner alongside Client
//     (the same client, claude-code, has TWO project substrates).
//   - Server is the server name to toggle.
//   - Enable: true = approve/add/register; false = disapprove/remove/unregister.
//   - Group is the group name for scope C (group-servers).
//   - Value is the object-member value to SET on a Model-B ENABLE (the config
//     shape for this client; ignored on disable, where the member is removed).
//     P3a does not synthesize the value (that would absorb the migrate machinery
//     — out of scope); the caller supplies it. P3a's read API does NOT provide a
//     value-source field (the r2 `toggle_value` was removed — finding 4); sourcing
//     P3b SHIPPED #434 settled the value-source NAMES-only: object-member
//     re-enable is cold-only (no backend value-source; the GUI Re-add CTA routes
//     to Add/Catalog). The residual D2 gap (the Re-add CTA links to a bare
//     #/add-server, not a pre-filled restore) is tracked OPEN in
//     work-items/backlog/2026-06-25-p3b-reenable-value-source.md. The endpoint
//     still accepts a caller-supplied value (used only on enable).
//   - Languages narrows a Model-A workspace register/unregister to specific LSP
//     languages; empty = all (the api.Register/Unregister default).
type projectToggleRequest struct {
	Root      string                     `json:"root"`
	Client    string                     `json:"client"`
	Scope     clients.ProjectToggleScope `json:"scope"`
	Server    string                     `json:"server"`
	Enable    bool                       `json:"enable"`
	Group     string                     `json:"group,omitempty"`
	Value     map[string]any             `json:"value,omitempty"`
	Languages []string                   `json:"languages,omitempty"`
}

// projectToggleResponse is the read-back result: the substrate that was written
// and the server's NEW persisted enabled state in that substrate.
type projectToggleResponse struct {
	Scope   clients.ProjectToggleScope `json:"scope"`
	Server  string                     `json:"server"`
	Enabled bool                       `json:"enabled"`
	// Warnings carries best-effort backend warnings (e.g. a Model-A register
	// scheduler warning) without failing the toggle.
	Warnings []string `json:"warnings,omitempty"`
}

func registerProjectsToggleRoutes(s *Server) {
	s.mux.HandleFunc("/api/projects/toggle", s.requireSameOrigin(s.projectsToggleHandler))
}

func (s *Server) projectsToggleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req projectToggleRequest
	if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "BAD_REQUEST")
		return
	}
	if strings.TrimSpace(req.Server) == "" {
		writeAPIError(w, errBadToggleField("server is required"), http.StatusBadRequest, "PROJECT_TOGGLE_INVALID")
		return
	}

	// SINGLE-owner ownership resolution. The GUI never branches on the client
	// name to choose a writer — clients.ProjectToggleOwner is the one classifier.
	owner := clients.ProjectToggleOwner(req.Client, req.Scope)

	switch owner {
	case clients.OwnerWorkspaceRegister:
		s.toggleWorkspaceLSP(w, req)
	case clients.OwnerProjectObjectMember:
		s.toggleProjectObjectMember(w, req)
	case clients.OwnerClaudeLocalMembership:
		s.toggleClaudeLocalMembership(w, req)
	case clients.OwnerGroupServers:
		s.toggleGroupServers(r.Context(), w, req)
	default:
		// No write owner for this (client, scope) — refuse (no write) rather
		// than guess. The substrate the toggle named is unsupported.
		writeAPIError(w, errBadToggleField("unsupported (client, scope) for a project toggle"),
			http.StatusBadRequest, "PROJECT_TOGGLE_UNSUPPORTED")
	}
}

// toggleWorkspaceLSP — Model A. enable=register / disable=unregister the
// workspace LSP daemon for the requested languages. api.Register/Unregister own
// the supervisor-intent + workspace_registry + per-client GLOBAL writes; this
// handler only dispatches and reads back the result from the returned report.
func (s *Server) toggleWorkspaceLSP(w http.ResponseWriter, req projectToggleRequest) {
	if strings.TrimSpace(req.Root) == "" {
		writeAPIError(w, errBadToggleField("root is required for a workspace toggle"),
			http.StatusBadRequest, "PROJECT_TOGGLE_INVALID")
		return
	}
	a := api.NewAPI()
	resp := projectToggleResponse{Scope: req.Scope, Server: req.Server}
	if req.Enable {
		rep, err := a.Register(req.Root, req.Languages, api.RegisterOpts{})
		if err != nil {
			writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "PROJECT_TOGGLE_FAILED", "/api/projects/toggle")
			return
		}
		resp.Enabled = rep != nil && len(rep.Entries) > 0
		if rep != nil {
			resp.Warnings = rep.Warnings
		}
	} else {
		rep, err := a.Unregister(req.Root, req.Languages)
		if err != nil {
			writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "PROJECT_TOGGLE_FAILED", "/api/projects/toggle")
			return
		}
		resp.Enabled = false // unregistered → not enabled
		if rep != nil {
			resp.Warnings = rep.Warnings
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// toggleProjectObjectMember — Model B (cursor/vscode/claude-code .mcp.json
// Project scope). The project config path is resolved ONLY via
// clients.ProjectScanConfigPaths (T1 path-safety: realRoot-contained, fixed
// RelFile, symlink-checked); a client absent from that map (its parent/leaf
// escaped the root, or it has no project scope) is refused — never written.
func (s *Server) toggleProjectObjectMember(w http.ResponseWriter, req projectToggleRequest) {
	if strings.TrimSpace(req.Root) == "" {
		writeAPIError(w, errBadToggleField("root is required for a project-config toggle"),
			http.StatusBadRequest, "PROJECT_TOGGLE_INVALID")
		return
	}
	_, configPaths, err := clients.ProjectScanConfigPaths(req.Root)
	if err != nil {
		// Leak-safe: ProjectScanConfigPaths errors are already generic, but
		// redact at the boundary so neither the message nor a wrapped path
		// reaches the wire.
		writeAPIErrorRedacted(w, err, http.StatusBadRequest, "PROJECT_ROOT_INVALID", "/api/projects/toggle")
		return
	}
	configPath, ok := configPaths[req.Client]
	if !ok {
		// The client has no SAFE project config path under this root (dropped by
		// the symlink-containment guard, or no project scope for this client).
		writeAPIError(w, errBadToggleField("client has no project-local config path under this root"),
			http.StatusBadRequest, "PROJECT_TOGGLE_UNSUPPORTED")
		return
	}

	// On enable, a member value is required (P3a does not synthesize it). On
	// disable the value is ignored (pure member remove).
	var value any
	if req.Enable {
		if len(req.Value) == 0 {
			writeAPIError(w, errBadToggleField("value (the config member shape) is required to enable a project-config server"),
				http.StatusBadRequest, "PROJECT_TOGGLE_INVALID")
			return
		}
		value = req.Value
	}

	if err := clients.ToggleProjectObjectMember(req.Client, configPath, req.Server, value, req.Enable); err != nil {
		writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "PROJECT_TOGGLE_FAILED", "/api/projects/toggle")
		return
	}

	// Read back: is the member now present in the project config file?
	enabled, err := clients.ProjectObjectMemberPresent(req.Client, configPath, req.Server)
	if err != nil {
		writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "PROJECT_TOGGLE_FAILED", "/api/projects/toggle")
		return
	}
	writeJSON(w, http.StatusOK, projectToggleResponse{Scope: req.Scope, Server: req.Server, Enabled: enabled})
}

// toggleClaudeLocalMembership — Model B-claude Local. Moves the server name
// between ~/.claude.json projects.<key>.{enabled,disabled}McpjsonServers (NEVER
// deletes mcpServers — decision 5). The symlink policy is the single owner
// api.OperatorAllowsClientConfigSymlink, injected DOWN into the clients writer.
//
// The realRoot the write keys off is the SYMLINK-RESOLVED project root from
// ProjectScanConfigPaths (the SAME real path the scan + the claude-local READER
// use), so a symlinked root matches the projects.<key> Claude wrote at the real
// path. ProjectScanConfigPaths also enforces the T1 root path-safety contract
// even though the ~/.claude.json write itself targets the fixed home file.
func (s *Server) toggleClaudeLocalMembership(w http.ResponseWriter, req projectToggleRequest) {
	if strings.TrimSpace(req.Root) == "" {
		writeAPIError(w, errBadToggleField("root is required for a claude-local toggle"),
			http.StatusBadRequest, "PROJECT_TOGGLE_INVALID")
		return
	}
	realRoot, _, err := clients.ProjectScanConfigPaths(req.Root)
	if err != nil {
		writeAPIErrorRedacted(w, err, http.StatusBadRequest, "PROJECT_ROOT_INVALID", "/api/projects/toggle")
		return
	}

	allow := api.OperatorAllowsClientConfigSymlink()
	if err := clients.ToggleClaudeMcpjsonMembership(realRoot, req.Server, req.Enable, allow); err != nil {
		writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "PROJECT_TOGGLE_FAILED", "/api/projects/toggle")
		return
	}

	// Read back through the single-owner predicate IsMcpjsonServerEnabled.
	local, err := clients.ReadClaudeLocalScope(realRoot, allow)
	if err != nil {
		writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "PROJECT_TOGGLE_FAILED", "/api/projects/toggle")
		return
	}
	writeJSON(w, http.StatusOK, projectToggleResponse{
		Scope:   req.Scope,
		Server:  req.Server,
		Enabled: local.IsMcpjsonServerEnabled(req.Server),
	})
}

// toggleGroupServers — Model C. Adds/removes the server from the group's
// servers list via api.ReadModifyWriteGroups (atomic under hub-mcp.lock,
// lost-update-safe), then re-publishes the live hub snapshot through the SAME
// seam /api/groups uses (bot PR #433 finding 1) so a membership change takes
// effect IMMEDIATELY instead of only after a hub restart.
//
// VALIDATION (bot PR #433 finding 2): on ENABLE the requested server name is
// validated against the SAME routable-server set /api/groups validates against
// (s.groups.AvailableServers() → RoutableServerNames()) BEFORE it is persisted
// — so a same-origin POST can no longer append an arbitrary / non-routable name
// to groups.yaml (the authoring-boundary strictness the snapshot builder
// relaxes). DISABLE is not name-gated: removing a stale member is exactly the
// cleanup an operator should always be allowed to do (mirrors groupsUpsert,
// which validates only the body's server list, never a removal).
//
// REPUBLISH (bot PR #433 finding 1): when the gate-ON hub listener is live, the
// in-memory ResolverSnapshot is rebuilt from the current manifests + the NEW
// groups via republishGroupsSnapshot — the SAME per-Server seam
// writeGroupMutationWithForce calls, so a disabled server is dropped from the
// /g/<group>/mcp surface in real time. The durable groups.yaml write already
// landed, so a republish failure is non-fatal: it is surfaced as a Warning
// (mirroring the groups screen's restart_required banner) rather than failing
// the toggle. A benign request-context cancel (client disconnect / deadline
// before the lock could be acquired) is NOT a republish failure — the next
// mutation/bind republishes — so it is swallowed exactly as the groups tail does.
func (s *Server) toggleGroupServers(ctx context.Context, w http.ResponseWriter, req projectToggleRequest) {
	group := strings.TrimSpace(req.Group)
	if group == "" {
		writeAPIError(w, errBadToggleField("group is required for a group toggle"),
			http.StatusBadRequest, "PROJECT_TOGGLE_INVALID")
		return
	}

	// Finding 2: name gate on ENABLE only, against the SAME routable set
	// /api/groups uses. Reject an unknown/non-routable name (redacted 400)
	// BEFORE persisting it. The disable path skips this so a stale member can
	// always be cleaned up.
	if req.Enable {
		known, err := s.groups.AvailableServers()
		if err != nil {
			writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "PROJECT_TOGGLE_FAILED", "/api/projects/toggle")
			return
		}
		if !groupHasServer(known, req.Server) {
			// Leak-safe: a fixed message + stable code; never echo the known set
			// (server names are not secret, but the redacted boundary keeps the
			// wire shape stable + minimal — the server-side log is unaffected).
			writeAPIError(w, errBadToggleField("server is not a known routable server"),
				http.StatusBadRequest, "PROJECT_TOGGLE_UNKNOWN_SERVER")
			return
		}
	}

	// Finding 1 (bot PR #433 r3): ABORT a missing-group toggle BEFORE any write.
	// The callback returns the errGroupNotFound SENTINEL (the SAME one the
	// /api/groups DELETE callback uses) when the target group is absent —
	// ReadModifyWriteGroups propagates a non-nil callback error WITHOUT writing
	// (writeGroupsLocked never runs), so a toggle of a non-existent group makes
	// NO write to (and never CREATES) groups.yaml. The handler then maps the
	// sentinel to the 404. Returning nil,nil before would normalize/create the
	// file (lost the "no write on not-found" invariant the bot flagged).
	mutErr := api.ReadModifyWriteGroups(func(cfg *api.GroupsConfig) ([]string, error) {
		for i := range cfg.Groups {
			if cfg.Groups[i].Name != group {
				continue
			}
			if req.Enable {
				cfg.Groups[i].Servers = ensureGroupServer(cfg.Groups[i].Servers, req.Server)
			} else {
				cfg.Groups[i].Servers = removeGroupServer(cfg.Groups[i].Servers, req.Server)
				// Finding 5 (bot PR #433 r3): when REMOVING a server from a group's
				// servers list, ALSO drop its tools_hidden[server] entry. The
				// /api/groups authoring boundary REJECTS a tools_hidden key for a
				// non-member (groupsUpsert: "tools_hidden names server which is not a
				// member"), so leaving the filter behind would put groups.yaml in an
				// editor-rejected state AND silently re-apply the stale hidden-tool
				// filter if the server is later re-added. groupsUpsert avoids this by
				// replacing the WHOLE Group row from the request body; this surgical
				// servers-list edit must mirror that invariant explicitly.
				cfg.Groups[i].ToolsHidden = pruneToolsHidden(cfg.Groups[i].ToolsHidden, req.Server)
			}
			return nil, nil
		}
		return nil, errGroupNotFound
	})
	if mutErr != nil {
		if errors.Is(mutErr, errGroupNotFound) {
			writeAPIError(w, errBadToggleField("group not found"), http.StatusNotFound, "PROJECT_TOGGLE_GROUP_NOT_FOUND")
			return
		}
		writeAPIErrorRedacted(w, mutErr, http.StatusInternalServerError, "PROJECT_TOGGLE_FAILED", "/api/projects/toggle")
		return
	}

	// Read back: is the server now a member of the group?
	cfg, err := api.LoadGroups()
	if err != nil {
		writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "PROJECT_TOGGLE_FAILED", "/api/projects/toggle")
		return
	}
	enabled := false
	for _, g := range cfg.Groups {
		if g.Name == group {
			enabled = groupHasServer(g.Servers, req.Server)
			break
		}
	}

	resp := projectToggleResponse{Scope: req.Scope, Server: req.Server, Enabled: enabled}

	// Finding 1: republish the live snapshot through the SAME seam /api/groups
	// uses, so the membership change is effective immediately (a disabled server
	// stops routing in the hub now, not at the next restart). Non-fatal on
	// failure — the durable write already committed.
	if s.HubMcpEndpointActive() {
		if rerr := s.republishGroupsSnapshot(ctx); rerr != nil {
			if !errors.Is(rerr, context.Canceled) && !errors.Is(rerr, context.DeadlineExceeded) {
				_ = api.LogHubMcpEvent("warn", "project-group-toggle-republish-failed", map[string]any{
					"group": group,
					"err":   rerr.Error(),
				})
				resp.Warnings = append(resp.Warnings,
					"group membership saved, but the live hub snapshot could not be refreshed — restart the hub to apply")
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// ensureGroupServer appends server iff absent, preserving order + sorting for a
// deterministic on-disk shape (groups.yaml servers are operator-readable).
func ensureGroupServer(in []string, server string) []string {
	for _, s := range in {
		if s == server {
			return in
		}
	}
	out := append(append([]string{}, in...), server)
	sort.Strings(out)
	return out
}

// removeGroupServer drops every occurrence of server, preserving order.
func removeGroupServer(in []string, server string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != server {
			out = append(out, s)
		}
	}
	return out
}

// pruneToolsHidden deletes the per-server hidden-tool filter for `server` from a
// group's tools_hidden map when that server is removed from the group's servers
// list (finding 5). It mirrors the /api/groups authoring invariant — every
// tools_hidden key MUST be a current member (groupsUpsert rejects a non-member
// key) — so a surgical member removal does not leave an editor-rejected,
// silently-re-applicable stale filter behind. A nil/empty map stays nil; the map
// is returned to nil when its last entry is removed so groups.yaml omits the
// empty `tools_hidden:` key (matching the omitempty YAML tag).
func pruneToolsHidden(in map[string][]string, server string) map[string][]string {
	if _, ok := in[server]; !ok {
		return in
	}
	delete(in, server)
	if len(in) == 0 {
		return nil
	}
	return in
}

func groupHasServer(in []string, server string) bool {
	for _, s := range in {
		if s == server {
			return true
		}
	}
	return false
}

// errBadToggleField is a small typed-message helper so the redacted-error
// boundary still logs a precise reason server-side while the wire gets the
// stable code + a fixed body (writeAPIError on a 4xx uses err.Error(), which is
// our own non-leaking message — no os.PathError, no filesystem layout).
type toggleFieldError string

func (e toggleFieldError) Error() string { return string(e) }

func errBadToggleField(msg string) error { return toggleFieldError(msg) }
