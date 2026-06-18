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
// SAME source the Servers matrix and the snapshot builder derive from:
// api.ManifestList() (embed-first server names).
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
	"encoding/json"
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
}

type realGroupsAPI struct{}

func (realGroupsAPI) LoadGroups() (api.GroupsConfig, error) { return api.LoadGroups() }
func (realGroupsAPI) ReadModifyWriteGroups(mutate func(cfg *api.GroupsConfig) ([]string, error)) error {
	return api.ReadModifyWriteGroups(mutate)
}
func (realGroupsAPI) AvailableServers() ([]string, error) {
	return api.NewAPI().RoutableServerNames()
}

// republishGroupsSnapshot drives the live hub-snapshot re-publish after a
// groups.yaml mutation. The re-publish seam is the per-Server
// s.groupsRepublishFn field (a test fake when set); production leaves it nil
// and calls publishResolverSnapshotForHubBind(s.api) directly — the same
// choke point startHubMcpListener uses at gate-ON bind.
func (s *Server) republishGroupsSnapshot() error {
	if s.groupsRepublishFn != nil {
		return s.groupsRepublishFn(s.api)
	}
	return publishResolverSnapshotForHubBind(s.api)
}

func registerGroupsRoutes(s *Server) {
	s.mux.HandleFunc("/api/groups", s.requireSameOrigin(s.groupsHandler))
}

// groupDTO is one group row in the GET / POST wire shape. Servers is
// always a non-nil array. ToolsHidden is emitted only when non-empty
// (omitempty) so a group with only `servers` has a compact shape.
type groupDTO struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Servers     []string            `json:"servers"`
	ToolsHidden map[string][]string `json:"tools_hidden,omitempty"`
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
	rows := make([]groupDTO, 0, len(cfg.Groups))
	for _, g := range cfg.Groups {
		rows = append(rows, groupToDTO(g))
	}
	if servers == nil {
		servers = []string{}
	}
	writeJSON(w, http.StatusOK, groupsListResponse{Groups: rows, AvailableServers: servers})
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "GROUPS_INVALID_JSON")
		return
	}

	name := strings.TrimSpace(body.Name)
	if err := api.ValidateGroupName(name); err != nil {
		writeAPIError(w, err, http.StatusBadRequest, "GROUPS_INVALID_NAME")
		return
	}

	// Known-server gate (AUTHORING-boundary strictness, decision §POST 2).
	// Source = api.ManifestList(), the same set the snapshot builder reads
	// manifests from. An unknown server is a 400, not a silent skip.
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
	if err := s.groups.ReadModifyWriteGroups(func(cfg *api.GroupsConfig) ([]string, error) {
		replaced := false
		for i := range cfg.Groups {
			if cfg.Groups[i].Name == name {
				cfg.Groups[i] = updated
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

	dto := groupToDTO(updated)
	s.writeGroupMutation(w, &dto)
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
		writeAPIError(w, err, http.StatusInternalServerError, "GROUPS_WRITE_FAILED")
		return
	}

	s.writeGroupMutation(w, nil)
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
func (s *Server) writeGroupMutation(w http.ResponseWriter, group *groupDTO) {
	hubLive := s.HubMcpEndpointActive()
	restartRequired := !hubLive
	if hubLive {
		if err := s.republishGroupsSnapshot(); err != nil {
			// The durable write succeeded; only the in-place live publish
			// failed. Surface as restart_required (not a 500) so the
			// operator restarts the hub to pick up the persisted edit.
			_ = api.LogHubMcpEvent("warn", "groups-republish-failed", map[string]any{
				"err": err.Error(),
			})
			restartRequired = true
		}
	}
	writeJSON(w, http.StatusOK, groupMutationResponse{
		Group:           group,
		RestartRequired: restartRequired,
		HubLive:         hubLive,
	})
}
