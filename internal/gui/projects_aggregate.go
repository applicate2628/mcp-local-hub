// internal/gui/projects_aggregate.go
//
// Per-project-GUI Phase 3a — GET /api/projects, the A+B+C aggregate
// (design decision 6). It composes ONE DTO per canonical project, joining the
// three per-project mechanisms by the SINGLE join key clients.CanonicalProjectKey:
//
//   - A (workspaces): the workspace registry's per-(key, language) LSP rows for
//     this project, grouped by canonical project key.
//   - B (project scan): the SAME read path /api/projects/scan uses — a SEPARATE
//     ScanFrom with the project-scoped ConfigPaths resolver
//     (clients.ProjectScanConfigPaths, NEVER DefaultScanConfigPaths, so the
//     global Servers-matrix scan stays byte-identical) + the P2b claude-LOCAL
//     enrichment (both claude scopes: .mcp.json Project + ~/.claude.json Local).
//   - C (groups): the groups from groups.yaml. P3c FILTERS each project's groups
//     by the §10.1 binding: a project's Groups list carries every group bound to
//     THAT project (Group.ProjectPath == the project's canonical key) plus every
//     unbound/global group (ProjectPath == ""). groupVisibleInProject is the
//     SINGLE backend-side owner of that predicate; the frontend reads the
//     filtered per-project list and never re-derives it. The top-level Groups
//     stays as the project-unfiltered set (each row carries its project_path so
//     the UI can label bound-vs-global).
//
// READ-ONLY: it only reads the registry, the project config files, ~/.claude.json
// (once per project), and groups.yaml. It writes NOTHING. It uses the PROJECT
// resolver throughout, so a /api/projects call can never perturb the global scan.
package gui

import (
	"net/http"
	"sort"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// projectAggregateDTO is one project's composed lens.
type projectAggregateDTO struct {
	// Key is the canonical project key (clients.CanonicalProjectKey of the
	// workspace path) — the stable join id the frontend addresses.
	Key string `json:"key"`
	// WorkspacePath is the human-readable project root (the registry's
	// workspace_path; the first one seen for this canonical key).
	WorkspacePath string `json:"workspace_path"`
	// Entries are the Model-A workspace LSP rows (per language) for this project.
	Entries []workspaceEntryDTO `json:"entries"`
	// Scan is the Model-B project-scoped scan result (per-client substrate
	// labels via ClientPresence + per-server enabled/disabled via the P2b
	// ProjectEnabled/ProjectScope fields). nil when the project root could not
	// be resolved/scanned (e.g. the workspace directory was deleted); ScanError
	// then carries a stable code so the row still renders.
	Scan      *api.ScanResult `json:"scan,omitempty"`
	ScanError string          `json:"scan_error,omitempty"`
	// Groups are the Model-C groups VISIBLE in THIS project's lens (P3c, design
	// §10.1): every group bound to this project (ProjectPath == this Key) PLUS
	// every unbound/global group (ProjectPath == ""). This is the
	// SINGLE-BACKEND-OWNED binding-filter output (groupVisibleInProject is the one
	// predicate); the frontend reads this list directly and NEVER re-derives the
	// filter. Always a non-nil array (empty when no group is visible). Each
	// groupDTO carries its project_path so the UI can label bound-vs-global.
	Groups []groupDTO `json:"groups"`
}

// projectsAggregateResponse is the GET /api/projects body. The top-level Groups
// is the project-UNFILTERED set (each row carries its project_path so the UI can
// label bound-vs-global); each project's OWN binding-filtered subset lives in
// projectAggregateDTO.Groups (P3c, the §10.1 binding filter).
type projectsAggregateResponse struct {
	Projects []projectAggregateDTO `json:"projects"`
	Groups   []groupDTO            `json:"groups"`
	// GroupsError is a stable code when groups.yaml failed to load, so the
	// frontend distinguishes "no groups" from "load failed" (mirrors the P1
	// Projects screen's "? groups (load failed)" affordance).
	GroupsError string `json:"groups_error,omitempty"`
}

func registerProjectsAggregateRoutes(s *Server) {
	s.mux.HandleFunc("/api/projects", s.requireSameOrigin(s.projectsAggregateHandler))
}

func (s *Server) projectsAggregateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// A — the workspace registry is the set of known projects.
	reg, err := loadWorkspaceRegistryProd()
	if err != nil {
		writeAPIErrorRedacted(w, err, http.StatusInternalServerError, "PROJECTS_REGISTRY_FAILED", "/api/projects")
		return
	}

	// Group the registry rows by CANONICAL project key (the single join owner),
	// preserving the first-seen human-readable path per key. Sorted iteration so
	// the DTO order is deterministic across runs.
	type projAcc struct {
		path    string
		entries []workspaceEntryDTO
	}
	byKey := map[string]*projAcc{}
	keyOrder := []string{}
	for _, ws := range reg.Workspaces {
		key := clients.CanonicalProjectKey(ws.WorkspacePath)
		if key == "" {
			continue // a malformed/empty path cannot be a project key
		}
		acc, ok := byKey[key]
		if !ok {
			acc = &projAcc{path: ws.WorkspacePath}
			byKey[key] = acc
			keyOrder = append(keyOrder, key)
		}
		acc.entries = append(acc.entries, workspaceEntryDTOFromAPI(ws))
	}
	sort.Strings(keyOrder)

	// C — groups, loaded ONCE up front so the per-project binding filter
	// (groupVisibleInProject) can be applied as each project DTO is built. A load
	// failure is non-fatal: the frontend renders "groups load failed" while the
	// projects still show. allGroupRows is the project-unfiltered set surfaced at
	// the top of the response (each row carries its project_path so the UI can
	// label bound-vs-global); per-project filtering reuses cfg.Groups directly.
	var cfg api.GroupsConfig
	var groupsErr string
	allGroupRows := []groupDTO{}
	cfg, gerr := api.LoadGroups()
	if gerr != nil {
		groupsErr = "GROUPS_LIST_FAILED"
	} else {
		port, hubLive := s.HubMcpBoundPort()
		instanceID, hasInstance := s.groups.HubInstanceID()
		allGroupRows = make([]groupDTO, 0, len(cfg.Groups))
		for _, g := range cfg.Groups {
			d := groupToDTO(g)
			d.Connection = s.groupConnection(g.Name, port, hubLive, instanceID, hasInstance)
			allGroupRows = append(allGroupRows, d)
		}
	}

	guiPort := s.Port()
	projects := make([]projectAggregateDTO, 0, len(keyOrder))
	for _, key := range keyOrder {
		acc := byKey[key]
		dto := projectAggregateDTO{
			Key:           key,
			WorkspacePath: acc.path,
			Entries:       acc.entries,
			// C (P3c) — the per-project binding filter. groupVisibleInProject is
			// the SINGLE owner of the `bound-to-this OR global-unbound` predicate;
			// the frontend never re-derives it. Empty (non-nil) when groups failed
			// to load or none are visible.
			Groups: filterGroupsForProject(cfg.Groups, key),
		}
		sort.Slice(dto.Entries, func(i, j int) bool { return dto.Entries[i].Language < dto.Entries[j].Language })

		// B — the project-scoped scan (+ claude-local enrichment). A
		// resolve/scan failure for ONE project does not fail the whole aggregate:
		// it carries a stable per-project ScanError and the A/C data still
		// renders. The reason is logged server-side (the realRoot/path may be in
		// the error) but never put on the wire (T5).
		scan, scanErr := s.scanOneProject(acc.path, guiPort)
		if scanErr != "" {
			dto.ScanError = scanErr
		} else {
			dto.Scan = scan
		}
		projects = append(projects, dto)
	}

	resp := projectsAggregateResponse{Projects: projects, Groups: allGroupRows, GroupsError: groupsErr}
	writeJSON(w, http.StatusOK, resp)
}

// groupVisibleInProject is the SINGLE OWNER of the per-project binding predicate
// (design §10.1): a group is visible in a project's lens when it is bound to THAT
// project (its canonical ProjectPath equals the project's canonical key) OR it is
// UNBOUND / GLOBAL (ProjectPath == "" — every pre-P3c group, plus any group the
// operator explicitly made global). Both sides are already canonical keys: the
// stored ProjectPath was normalized via clients.CanonicalProjectKey by the
// binding write owner, and projectKey is the aggregate's CanonicalProjectKey, so
// this is a direct string compare with no re-normalization here. The frontend
// reads the filtered per-project Groups list this predicate produces and never
// re-implements it.
func groupVisibleInProject(g api.Group, projectKey string) bool {
	return g.ProjectPath == "" || g.ProjectPath == projectKey
}

// filterGroupsForProject returns the project-lens-visible subset of groups
// (groupVisibleInProject), mapped to the wire DTO. The result is a non-nil array
// (empty when nothing is visible). The Connection triple is left nil here: the
// project lens lists groups for binding/membership, not for the B4 copy-paste
// connection details (which the top-level Groups + the dedicated Groups screen
// own), so a per-project group row stays compact.
func filterGroupsForProject(groups []api.Group, projectKey string) []groupDTO {
	out := make([]groupDTO, 0, len(groups))
	for _, g := range groups {
		if groupVisibleInProject(g, projectKey) {
			out = append(out, groupToDTO(g))
		}
	}
	return out
}

// scanOneProject runs the Model-B project scan for a single project root via the
// SAME chain /api/projects/scan uses: ProjectScanConfigPaths (path-safety +
// realRoot) → ScanFrom(project ConfigPaths) → EnrichProjectClaudeLocalScope
// (both claude scopes) → sanitizeScanResult (strip Raw blobs). On any failure it
// returns ("", "<stable code>") and logs the reason server-side; on success it
// returns the sanitized result + "".
func (s *Server) scanOneProject(root string, guiPort int) (*api.ScanResult, string) {
	realRoot, configPaths, err := clients.ProjectScanConfigPaths(root)
	if err != nil {
		// The registered workspace dir may have been deleted/moved — a normal,
		// recoverable per-project state, not an aggregate failure.
		return nil, "PROJECT_ROOT_INVALID"
	}
	result, err := api.NewAPI().ScanFrom(api.ScanOpts{ConfigPaths: configPaths, GUIPort: guiPort})
	if err != nil {
		return nil, "PROJECT_SCAN_FAILED"
	}
	if err := api.EnrichProjectClaudeLocalScope(result, realRoot); err != nil {
		return nil, "PROJECT_SCAN_FAILED"
	}
	// P3a stays NAMES-only (bot PR #433 r3 finding 4, lead adjudication): the r2
	// `toggle_value` Raw-copy provision is REMOVED — it re-exposed secret-bearing
	// config sanitizeScanResult strips on purpose. P3b SHIPPED #434 settled this
	// NAMES-only (object-member re-enable is cold-only via Add/Catalog, no backend
	// value-source); the residual D2 cold-re-enable UX gap is tracked OPEN in
	// work-items/backlog/2026-06-25-p3b-reenable-value-source.md.
	return sanitizeScanResult(result), ""
}
