// internal/gui/groups.go
//
// GUI authoring surface for groups/namespaces (groups Phase 5b-1). A
// "group" is a named set of MCP servers exposed as a scoped tool surface
// served on /g/<group>/mcp (decision
// work-items/decisions/2026-06-18-groups-namespaces-tool-visibility.md).
// Phases 4a-5a built the DATA layer (groups.yaml loader/writer,
// kind-namespaced snapshot merge, per-group token row, per-tool filter)
// and the SERVE path (/g/ route, group-scoped initialize). This file is
// the AUTHORING endpoint the GUI Groups screen will consume to create /
// edit / delete groups:
//
//   - GET    /api/groups            — list groups + the available server
//     names a picker offers.
//   - POST   /api/groups            — create-or-update one group.
//   - DELETE /api/groups?name=<n>   — remove one group (404 if absent).
//
// It is the thin HTTP wrapper over the api data layer
// (api.LoadGroups for the read path / api.ReadModifyWriteGroups for the
// atomic mutation path / api.ValidateGroupName), mirroring
// internal/gui/client_install_prefs.go: every method is wrapped in
// s.requireSameOrigin, inputs are validated server-side, and there is no
// arbitrary path / file target — the only persistence sink is the fixed
// <state-dir>/groups.yaml written atomically by the api layer's hardened
// pipeline.
//
// AUTHORING-boundary strictness vs. runtime tolerance: the snapshot
// builder SKIPS a group member naming an unknown server (decision claim 5
// — a stale config never faults routing). At the AUTHORING boundary,
// though, an unknown server is a 400 — an operator typo should surface
// immediately, not silently route nothing. The known-server set is the
// locally-routable server names from api.NewAPI().RoutableServerNames()
// (the R1 fix — filtered to servers with a local daemon ref, so a group
// can never bind a daemonless / transport=remote-http server it could
// never route to).
//
// Live re-publish (closes the Phase 3b staleness for GUI edits): this
// endpoint runs in the GUI process, which OWNS the running hub listener.
// After a successful write, if the gate-ON hub listener is live, the
// handler re-publishes the ResolverSnapshot from the current manifests +
// the NEW groups via the SAME publishResolverSnapshotForHubBind seam the
// Phase 3 startup choke point calls — so a GUI group edit takes effect
// immediately (and EnsureGroupTokens runs for the new group key, making
// /g/<newgroup>/mcp instantly authable). When the hub is NOT live
// (gate-OFF, or bind failed) the write still persists; the response
// carries restart_required=true so the GUI can show a "restart hub to
// apply" banner.
package gui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

// errGroupNotFound is the sentinel a ReadModifyWriteGroups DELETE callback
// returns when the named group is absent, so the handler maps it to a 404
// GROUPS_NOT_FOUND rather than a generic 500. The error never leaves the
// package (the handler translates it before writing the response).
var errGroupNotFound = errors.New("group not found")

// hubGroupRoutePrefix / hubMcpRouteSuffix mirror hubGroupPrefix / hubPathSuffix
// in internal/api/hub_mcp_handler.go (which are unexported). Used to build the
// /g/<group>/mcp connection URL the Groups GET path surfaces (B4). They are the
// route grammar the api handler's parseHubPathFromURL recognizes, so the URL
// the GUI hands the operator is the exact path the hub serves.
const (
	hubGroupRoutePrefix = "/g/"
	hubMcpRouteSuffix   = "/mcp"
)

// groupsAPI is the narrow seam the groups handler needs over the api data
// layer. Behind an interface so handler tests inject a fake and never
// touch the real groups.yaml / hub snapshot. Production wires
// realGroupsAPI (lazy api.NewAPI() per call, matching the realDemigrater /
// realDismisser idiom in server.go).
type groupsAPI interface {
	// LoadGroups returns the parsed groups.yaml (empty + nil error when
	// the file is absent). Read-only — the GET path uses it.
	LoadGroups() (api.GroupsConfig, error)
	// ReadModifyWriteGroups runs an ATOMIC load→mutate→write under ONE held
	// hub-mcp.lock (so concurrent POST/DELETE can't lost-update), then prunes
	// the token rows of any groups the callback reports deleted. The POST
	// (create-or-update) and DELETE paths both route through it.
	ReadModifyWriteGroups(mutate func(cfg *api.GroupsConfig) ([]string, error)) error
	// AvailableServers returns the known server names a group may bind —
	// filtered to LOCALLY-ROUTABLE servers (those with a local daemon ref;
	// transport=remote-http / daemonless servers are excluded because a group
	// can never route to them). The same locality the snapshot builder honors.
	AvailableServers() ([]string, error)
	// HubInstanceID returns the hub endpoint's long-lived InstanceID (the
	// X-Mcphub-Instance-Id header value a /g/ client must send) and true when
	// the endpoint file is present + readable. Read-only; no lock. (B4
	// connection details.)
	HubInstanceID() (string, bool)
	// GroupToken returns the loopback bearer token for a group's "g:<name>"
	// scope key (the X-Mcphub-Hub-Token header value) from the live token
	// table, and true when a row exists. Read-only snapshot read. (B4.)
	GroupToken(group string) (string, bool)
}

type realGroupsAPI struct{}

func (realGroupsAPI) LoadGroups() (api.GroupsConfig, error) { return api.LoadGroups() }
func (realGroupsAPI) ReadModifyWriteGroups(mutate func(cfg *api.GroupsConfig) ([]string, error)) error {
	return api.ReadModifyWriteGroups(mutate)
}
func (realGroupsAPI) AvailableServers() ([]string, error) {
	return api.NewAPI().RoutableServerNames()
}
func (realGroupsAPI) HubInstanceID() (string, bool) {
	ep, err := api.LoadHubEndpoint()
	if err != nil || ep.InstanceID == "" {
		return "", false
	}
	return ep.InstanceID, true
}
func (realGroupsAPI) GroupToken(group string) (string, bool) {
	tbl := api.CurrentTokenTable()
	tok, ok := tbl.Tokens[api.GroupScopeKey(group)]
	if !ok || tok == "" {
		return "", false
	}
	return tok, true
}

// republishGroupsSnapshot drives the live hub-snapshot re-publish after a
// groups.yaml (or manifest) mutation. The re-publish seam is the per-Server
// s.groupsRepublishFn field (a test fake when set); production leaves it nil
// and calls publishResolverSnapshotForHubBind(ctx, s.api) directly — the same
// choke point startHubMcpListener uses at gate-ON bind.
//
// ctx is the request context (R4-4): it bounds the ctx-aware hub-mcp.lock
// acquisition inside PublishGroupsSnapshotLocked so a stuck sibling holder
// cannot freeze the HTTP handler past the request lifetime.
func (s *Server) republishGroupsSnapshot(ctx context.Context) error {
	if s.groupsRepublishFn != nil {
		return s.groupsRepublishFn(ctx, s.api)
	}
	return publishResolverSnapshotForHubBind(ctx, s.api)
}

func registerGroupsRoutes(s *Server) {
	s.mux.HandleFunc("/api/groups", s.requireSameOrigin(s.groupsHandler))
}

// groupDTO is one group row in the GET / POST wire shape. Servers is
// always a non-nil array. ToolsHidden is emitted only when non-empty
// (omitempty) so a group with only `servers` has a compact shape.
//
// Connection is populated ONLY on the GET list path (groupsList) — the
// mutation responses (POST/DELETE via groupToDTO) leave it nil so they stay
// compact. It carries the operator-facing /g/<group>/mcp connection triple
// (B4). localhost same-origin only — the token is the operator's own loopback
// bearer, intended to be surfaced in the same-origin GUI.
type groupDTO struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Servers     []string            `json:"servers"`
	ToolsHidden map[string][]string `json:"tools_hidden,omitempty"`
	Connection  *groupConnectionDTO `json:"connection,omitempty"`
	// ProjectPath is the group's per-project binding (P3c, design §10.1). It is
	// the canonical project key the group is bound to, or "" when the group is
	// UNBOUND / GLOBAL (visible in every project lens). Emitted with omitempty so
	// a global group keeps the compact pre-P3c wire shape; the per-project-lens UI
	// reads it to render "bound to this project" vs "global (all projects)". The
	// /api/projects backend filter is the SINGLE owner of the binding predicate —
	// this field is informational for the UI label only, never re-derived into a
	// client-side filter.
	ProjectPath string `json:"project_path,omitempty"`
}

// groupConnectionDTO is the copy-pasteable connection info for a group's
// /g/<group>/mcp route (B4 — bot R3). Available is true ONLY when the gate-ON
// hub listener is live AND bound AND a token row + instance id exist; in that
// case URL/InstanceID/Token are the three values a client must use (the URL
// plus the X-Mcphub-Hub-Token and X-Mcphub-Instance-Id headers). When the hub
// is gate-OFF / not bound, Available is false and Hint tells the operator how
// to bring the endpoint up instead of presenting a dead URL with a live token.
type groupConnectionDTO struct {
	Available  bool   `json:"available"`
	URL        string `json:"url,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
	Token      string `json:"token,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

// groupsListResponse is the GET wire shape. Groups is always a non-nil
// array (registry order from groups.yaml). AvailableServers lets the GUI
// offer a server picker without a second round-trip.
type groupsListResponse struct {
	Groups           []groupDTO `json:"groups"`
	AvailableServers []string   `json:"available_servers"`
}

// groupMutationResponse is the POST / DELETE wire shape. Group is the
// updated row (nil on DELETE). RestartRequired is true when the write
// persisted but the live hub could NOT be re-published in-place (gate-OFF
// or hub not live) — the GUI shows a "restart hub to apply" banner.
// HubLive reports whether the gate-ON hub listener was live at mutation
// time (the reason RestartRequired is false vs. true), so the GUI can word
// the banner precisely.
type groupMutationResponse struct {
	Group           *groupDTO `json:"group,omitempty"`
	RestartRequired bool      `json:"restart_required"`
	HubLive         bool      `json:"hub_live"`
}

// groupToDTO maps an api.Group to the wire DTO with a non-nil Servers
// slice. A nil/empty ToolsHidden maps to nil (omitted by omitempty).
func groupToDTO(g api.Group) groupDTO {
	servers := g.Servers
	if servers == nil {
		servers = []string{}
	}
	var hidden map[string][]string
	if len(g.ToolsHidden) > 0 {
		hidden = g.ToolsHidden
	}
	return groupDTO{
		Name:        g.Name,
		Description: g.Description,
		Servers:     servers,
		ToolsHidden: hidden,
		ProjectPath: g.ProjectPath,
	}
}

// groupsHandler dispatches GET / POST / DELETE on /api/groups. Method
// dispatch + JSON helpers mirror clientInstallPrefsHandler.
func (s *Server) groupsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.groupsList(w, r)
	case http.MethodPost:
		s.groupsUpsert(w, r)
	case http.MethodDelete:
		s.groupsDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// groupsList handles GET /api/groups. Returns every group from groups.yaml
// (empty array when the file is absent) plus the available server names a
// picker offers.
func (s *Server) groupsList(w http.ResponseWriter, _ *http.Request) {
	cfg, err := s.groups.LoadGroups()
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "GROUPS_LIST_FAILED")
		return
	}
	servers, err := s.groups.AvailableServers()
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "GROUPS_SERVERS_FAILED")
		return
	}
	// B4: resolve the hub connection prerequisites ONCE for the whole list
	// (port + instance id are group-independent; only the token is per-group).
	port, hubLive := s.HubMcpBoundPort()
	instanceID, hasInstance := s.groups.HubInstanceID()
	rows := make([]groupDTO, 0, len(cfg.Groups))
	for _, g := range cfg.Groups {
		dto := groupToDTO(g)
		dto.Connection = s.groupConnection(g.Name, port, hubLive, instanceID, hasInstance)
		rows = append(rows, dto)
	}
	if servers == nil {
		servers = []string{}
	}
	writeJSON(w, http.StatusOK, groupsListResponse{Groups: rows, AvailableServers: servers})
}

// groupConnection builds the B4 copy-pasteable connection triple for one
// group's /g/<group>/mcp route. It is "available" ONLY when the gate-ON hub
// listener is live + bound (hubLive), the endpoint instance id is present
// (hasInstance), AND a per-group loopback token row exists. Otherwise it
// returns a not-available placeholder with a hint so the GUI never shows a dead
// URL paired with a real token. localhost same-origin only.
func (s *Server) groupConnection(name string, port int, hubLive bool, instanceID string, hasInstance bool) *groupConnectionDTO {
	if !hubLive {
		return &groupConnectionDTO{
			Available: false,
			Hint:      "Start the aggregated hub (Settings → Expose a single aggregated hub URL) to get this group's endpoint.",
		}
	}
	token, hasToken := s.groups.GroupToken(name)
	if !hasInstance || !hasToken {
		// The hub is live but the auth seam isn't fully primed yet (token row
		// not ensured, or endpoint not readable). Tell the operator to restart
		// the hub to re-ensure rather than handing out an unauthable URL.
		return &groupConnectionDTO{
			Available: false,
			Hint:      "The hub is running but this group's auth row is not ready yet — restart the hub (or re-save the group) to ensure it.",
		}
	}
	return &groupConnectionDTO{
		Available:  true,
		URL:        fmt.Sprintf("http://127.0.0.1:%d%s%s%s", port, hubGroupRoutePrefix, name, hubMcpRouteSuffix),
		InstanceID: instanceID,
		Token:      token,
	}
}

// groupsUpsert handles POST /api/groups with body
// {"name","description?","servers":[...],"tools_hidden?":{...}}. It
// create-or-updates ONE group: read the full set, replace (or append) the
// row matching name, write the full set back. Validation (400 on failure):
//   - name via api.ValidateGroupName (non-empty, no ':' kind-prefix forge);
//   - every named server must be a known server (api.ManifestList) AND
//     every tools_hidden key must be one of this group's own servers —
//     the AUTHORING-boundary strictness the snapshot builder relaxes.
//
// On success it re-publishes the live hub snapshot (if the gate-ON
// listener is live) and returns the updated group.
func (s *Server) groupsUpsert(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string              `json:"name"`
		Description string              `json:"description"`
		Servers     []string            `json:"servers"`
		ToolsHidden map[string][]string `json:"tools_hidden"`
	}
	if err := decodeJSONBodyLimited(w, r, &body, maxManifestBodyBytes); err != nil {
		writeDecodeBodyError(w, err, "GROUPS_INVALID_JSON")
		return
	}

	name := strings.TrimSpace(body.Name)
	if err := api.ValidateGroupName(name); err != nil {
		writeAPIError(w, err, http.StatusBadRequest, "GROUPS_INVALID_NAME")
		return
	}

	// Known-server gate (AUTHORING-boundary strictness, decision §POST 2).
	// Source = s.groups.AvailableServers() → RoutableServerNames() (the R1
	// fix): the locally-routable server names, the same locality the snapshot
	// builder honors. An unknown server is a 400, not a silent skip.
	known, err := s.groups.AvailableServers()
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "GROUPS_SERVERS_FAILED")
		return
	}
	knownSet := make(map[string]bool, len(known))
	for _, n := range known {
		knownSet[n] = true
	}
	for _, srv := range body.Servers {
		if !knownSet[srv] {
			writeAPIError(w,
				fmt.Errorf("unknown server %q (known: %s)", srv, strings.Join(known, " | ")),
				http.StatusBadRequest, "GROUPS_UNKNOWN_SERVER")
			return
		}
	}
	// Every tools_hidden key must be one of THIS group's own member servers
	// — a per-tool filter for a server the group does not bind is a no-op
	// the authoring boundary should reject as an operator error.
	memberSet := make(map[string]bool, len(body.Servers))
	for _, srv := range body.Servers {
		memberSet[srv] = true
	}
	for srv := range body.ToolsHidden {
		if !memberSet[srv] {
			writeAPIError(w,
				fmt.Errorf("tools_hidden names server %q which is not a member of group %q", srv, name),
				http.StatusBadRequest, "GROUPS_HIDDEN_NONMEMBER")
			return
		}
	}

	servers := body.Servers
	if servers == nil {
		servers = []string{}
	}
	updated := api.Group{
		Name:        name,
		Description: body.Description,
		Servers:     servers,
		ToolsHidden: body.ToolsHidden,
	}
	// Atomic read-modify-write under ONE held hub-mcp.lock so two concurrent
	// POSTs can't lost-update (each reading the same baseline, the later write
	// clobbering the earlier). The callback edits the loaded set in place;
	// create-or-update deletes nothing (empty deleted-set).
	//
	// Match-to-replace is CASE-INSENSITIVE (C5-case): group-name uniqueness is
	// case-folded in the api layer, so a POST of "frontend" when "Frontend"
	// already exists UPDATES that one group in place (adopting the new casing)
	// rather than appending a second row the write-path uniqueness owner would
	// then reject as a confusing 500. This keeps create-or-update consistent
	// with the case-insensitive uniqueness invariant.
	// finalGroup is the row actually written: on REPLACE it is `updated` with
	// the matched row's ProjectPath copied over (set inside the callback under
	// the held lock); on CREATE it stays `updated` with ProjectPath=="".
	var finalGroup = updated
	if err := s.groups.ReadModifyWriteGroups(func(cfg *api.GroupsConfig) ([]string, error) {
		replaced := false
		for i := range cfg.Groups {
			if strings.EqualFold(cfg.Groups[i].Name, name) {
				// PRESERVE the existing per-project binding (P3c bot R2 P2): the
				// Groups-screen upsert POST body carries NO project_path (binding
				// is changed ONLY via the /api/projects/group-binding handler), so
				// building `updated` from the body alone would SILENTLY CLEAR a
				// bound group's project_path on every description/members/hidden-
				// tools edit. Carry the matched row's ProjectPath forward so a
				// Groups-screen edit never touches the binding. A brand-new group
				// (the append branch below) correctly stays ProjectPath=="" /
				// unbound — a new group isn't bound to any project.
				preserved := updated
				preserved.ProjectPath = cfg.Groups[i].ProjectPath
				cfg.Groups[i] = preserved
				finalGroup = preserved
				replaced = true
				break
			}
		}
		if !replaced {
			cfg.Groups = append(cfg.Groups, updated)
		}
		return nil, nil
	}); err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "GROUPS_WRITE_FAILED")
		return
	}

	dto := groupToDTO(finalGroup)
	s.writeGroupMutation(r.Context(), w, &dto)
}

// groupsDelete handles DELETE /api/groups?name=<name> — remove one group;
// 404 when absent. Mirrors the upsert write + live re-publish tail.
func (s *Server) groupsDelete(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeAPIError(w, fmt.Errorf("query parameter 'name' is required"),
			http.StatusBadRequest, "GROUPS_NAME_REQUIRED")
		return
	}

	// Atomic read-modify-write under ONE held hub-mcp.lock. The callback
	// removes the matching row (signalling 404 via errGroupNotFound when
	// absent) and reports the deleted group name so ReadModifyWriteGroups
	// prunes its "g:<name>" token row under the same held lock — no stale
	// auth row survives a delete.
	err := s.groups.ReadModifyWriteGroups(func(cfg *api.GroupsConfig) ([]string, error) {
		idx := -1
		for i := range cfg.Groups {
			if cfg.Groups[i].Name == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, errGroupNotFound
		}
		cfg.Groups = append(cfg.Groups[:idx], cfg.Groups[idx+1:]...)
		return []string{name}, nil
	})
	if err != nil {
		if errors.Is(err, errGroupNotFound) {
			writeAPIError(w, fmt.Errorf("group %q not found", name),
				http.StatusNotFound, "GROUPS_NOT_FOUND")
			return
		}
		// B3 (bot R3): a token-prune failure is NOT a delete failure — the
		// groups.yaml write committed, so the group is durably gone. 500-ing
		// here would SKIP the republish below and leave the deleted group
		// routable (the snapshot still carries it) until a hub restart. Treat
		// ErrTokenPruneFailed as a degraded SUCCESS: fall through to the shared
		// write+republish tail (which drops the group from the snapshot →
		// isKnownGroup 404) with restart_required forced so the operator
		// restarts the hub to clear the stale token row. Only a genuine
		// write/load failure is a 500.
		if !errors.Is(err, api.ErrTokenPruneFailed) {
			writeAPIError(w, err, http.StatusInternalServerError, "GROUPS_WRITE_FAILED")
			return
		}
		_ = api.LogHubMcpEvent("warn", "groups-delete-token-prune-failed", map[string]any{
			"group": name,
			"err":   err.Error(),
		})
		s.writeGroupMutationForceRestart(r.Context(), w, nil)
		return
	}

	s.writeGroupMutation(r.Context(), w, nil)
}

// writeGroupMutation is the shared write + live-re-publish tail for POST
// and DELETE. After a successful groups.yaml write it re-publishes the hub
// snapshot in-place WHEN the gate-ON hub listener is live; otherwise it
// flags restart_required so the GUI surfaces a "restart hub to apply"
// banner. A re-publish failure is NON-fatal to the mutation: the write
// already landed durably (the authored config is the source of truth), so
// we still return 200 but flag restart_required=true so a wedged in-place
// publish degrades to the same "restart to apply" path as gate-OFF rather
// than reporting a false success.
func (s *Server) writeGroupMutation(ctx context.Context, w http.ResponseWriter, group *groupDTO) {
	s.writeGroupMutationWithForce(ctx, w, group, false)
}

// writeGroupMutationForceRestart is the degraded-success variant used by the
// DELETE path when the groups.yaml write committed but the token-prune step
// failed (api.ErrTokenPruneFailed). The republish still runs (dropping the
// group from the snapshot so its /g/ route 404s), but restart_required is
// forced true so the GUI tells the operator to restart the hub to clear the
// stranded token row even when the in-place republish itself succeeded.
func (s *Server) writeGroupMutationForceRestart(ctx context.Context, w http.ResponseWriter, group *groupDTO) {
	s.writeGroupMutationWithForce(ctx, w, group, true)
}

// writeGroupMutationWithForce is the shared write + live-re-publish tail. When
// forceRestart is true the response always carries restart_required=true
// regardless of whether the in-place republish succeeded (the token-prune
// degraded path).
func (s *Server) writeGroupMutationWithForce(ctx context.Context, w http.ResponseWriter, group *groupDTO, forceRestart bool) {
	hubLive := s.HubMcpEndpointActive()
	restartRequired := !hubLive
	if hubLive {
		if err := s.republishGroupsSnapshot(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// Benign (concurrency-lane F1): the caller's request context
				// ended (client disconnect / deadline) before the in-place
				// republish could acquire hub-mcp.lock, so the publish was
				// never attempted. The durable write already landed and the
				// next mutation / bind republishes — the hub is NOT wedged, so
				// no restart is owed and this is not a failure worth a warn.
				// (forceRestart, set for the token-prune degraded path below,
				// is independent and still honored.)
			} else {
				// The durable write succeeded; only the in-place live publish
				// failed. Surface as restart_required (not a 500) so the
				// operator restarts the hub to pick up the persisted edit.
				_ = api.LogHubMcpEvent("warn", "groups-republish-failed", map[string]any{
					"err": err.Error(),
				})
				restartRequired = true
			}
		}
	}
	if forceRestart {
		restartRequired = true
	}
	writeJSON(w, http.StatusOK, groupMutationResponse{
		Group:           group,
		RestartRequired: restartRequired,
		HubLive:         hubLive,
	})
}
