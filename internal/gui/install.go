package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

// installer is the narrow interface the /api/install handler needs.
// The handler targets "install one server by name" — the GUI always
// knows the manifest name (it just wrote it), so DaemonFilter/DryRun/
// Writer are not exposed here. realInstaller below wires sensible
// defaults: no daemon filter (install every daemon in the manifest),
// DryRun=false, Writer=nil (falls back to os.Stderr inside api.Install).
type installer interface {
	Install(name string, guiPort int) error
}

// realInstaller is the production adapter for /api/install. Follows the
// realManifestCreator / realManifestValidator idiom: empty value receiver,
// lazy api.NewAPI() per call so tests can swap the interface without
// needing to stub a constructor. api.API.Install takes InstallOpts; we
// populate the server and the bound GUI listener port because the API routing
// owner needs both facts to select the stable client target.
type realInstaller struct{}

func (realInstaller) Install(name string, guiPort int) error {
	return realInstallerInstall(api.InstallOpts{Server: name, GUIPort: guiPort})
}

// Package-local seams keep the production adapters testable without letting
// route tests touch the live API state. Production still creates a fresh API
// per adapter call, matching the established GUI adapter pattern.
var realInstallerInstall = func(opts api.InstallOpts) error {
	return api.NewAPI().Install(opts)
}

// uninstaller is the narrow interface the DELETE /api/install/:server
// handler needs. It returns the structured *api.UninstallReport so the
// handler can surface per-client / per-task warnings as a 207 instead of
// collapsing them into a single error string. realUninstaller below wires
// the lazy api.NewAPI() per call, matching the realInstaller idiom — this
// is the seam that keeps the DESTRUCTIVE api.Uninstall out of the test
// path (manifest_test / install_test inject a fake that records the
// server arg and never touches the live fleet).
type uninstaller interface {
	Uninstall(server string) (*api.UninstallReport, error)
}

// realUninstaller is the production adapter for DELETE /api/install/:server.
type realUninstaller struct{}

func (realUninstaller) Uninstall(server string) (*api.UninstallReport, error) {
	return api.NewAPI().Uninstall(server)
}

// installBulkAPI is the narrow interface the POST /api/install-all handler
// needs. api.InstallAllWithOpts returns []InstallResult and never an error
// at the call level (per-server errors ride inside each row's Err), so the
// interface mirrors that exactly. realInstallBulkAPI wires the lazy
// api.NewAPI(); the optional clients filter is left at install-all defaults
// (all clients) — the GUI bulk-install affordance installs everything.
type installBulkAPI interface {
	InstallAll(servers []string, guiPort int) []api.InstallResult
}

// realInstallBulkAPI is the production adapter for POST /api/install-all.
// When servers is empty it installs every shipped manifest
// (InstallAllWithOpts consults the embed-first manifest set). When servers
// is non-empty it filters to that subset by installing each name in turn,
// preserving the same []InstallResult row shape so the handler renders one
// path regardless of filter.
type realInstallBulkAPI struct{}

func (realInstallBulkAPI) InstallAll(servers []string, guiPort int) []api.InstallResult {
	if len(servers) == 0 {
		return realInstallBulkAll(api.InstallAllOpts{GUIPort: guiPort})
	}
	results := make([]api.InstallResult, 0, len(servers))
	for _, name := range servers {
		err := realInstallBulkOne(api.InstallOpts{Server: name, GUIPort: guiPort})
		results = append(results, api.InstallResult{Server: name, Err: err})
	}
	return results
}

var realInstallBulkAll = func(opts api.InstallAllOpts) []api.InstallResult {
	return api.NewAPI().InstallAllWithOpts(opts)
}

var realInstallBulkOne = func(opts api.InstallOpts) error {
	return api.NewAPI().Install(opts)
}

type installRequest struct {
	Name string `json:"name"`
}

// installShadowWarnFn resolves the pre-existing embed-vs-disk collision warning
// for a just-installed server (empty when none). Package-var seam mirroring the
// installer/uninstaller injection idiom so the route tests can drive both the
// no-warning (204) and warning (200-body) shapes deterministically without
// depending on the api package's exe-derived defaultManifestDir. Production
// binds the api single owner.
var installShadowWarnFn = api.EmbeddedDiskShadowWarning

// uninstallResultDTO is the snake_case wire shape for one api.UninstallReport.
// api.UninstallReport carries no JSON tags (Go-default PascalCase), so we map
// it explicitly to keep the /api/* surface snake_case-consistent. Slices are
// normalized to non-nil so the frontend never special-cases null.
type uninstallResultDTO struct {
	Server          string   `json:"server"`
	TasksDeleted    []string `json:"tasks_deleted"`
	TaskDeleteWarns []string `json:"task_delete_warns"`
	ClientsUpdated  []string `json:"clients_updated"`
	ClientWarns     []string `json:"client_warns"`
}

func newUninstallResultDTO(rep *api.UninstallReport) uninstallResultDTO {
	dto := uninstallResultDTO{
		Server:          rep.Server,
		TasksDeleted:    rep.TasksDeleted,
		TaskDeleteWarns: rep.TaskDeleteWarns,
		ClientsUpdated:  rep.ClientsUpdated,
		ClientWarns:     rep.ClientWarns,
	}
	if dto.TasksDeleted == nil {
		dto.TasksDeleted = []string{}
	}
	if dto.TaskDeleteWarns == nil {
		dto.TaskDeleteWarns = []string{}
	}
	if dto.ClientsUpdated == nil {
		dto.ClientsUpdated = []string{}
	}
	if dto.ClientWarns == nil {
		dto.ClientWarns = []string{}
	}
	return dto
}

// installResultDTO is one row of the POST /api/install-all response. Mirrors
// the servers.go bulk-action convention: an empty error string means the
// row succeeded; a non-empty error promotes the overall response to 207.
type installResultDTO struct {
	Server string `json:"server"`
	Error  string `json:"error"`
	// Warning carries the pre-existing embed-vs-disk collision notice (a disk
	// manifest shadowing a shipped name that the embed-first install ignores)
	// so the operator sees it in the response, not only on process stderr
	// (bot PR #494 P2). Empty for the common case; omitted from the wire.
	Warning string `json:"warning,omitempty"`
}

// registerInstallRoutes wires POST /api/install onto the server's mux.
// The handler accepts `name` via either the query string (`?name=...`)
// or a JSON body ({"name":"..."}); the query path is the one the frontend
// uses so install triggers are shell-greppable in server logs. 204 on
// success, writeAPIError envelope on failure.
func registerInstallRoutes(s *Server) {
	s.mux.HandleFunc("/api/install", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			var req installRequest
			if err := decodeJSONBodyLimited(w, r, &req, maxControlBodyBytes); err != nil {
				writeDecodeBodyError(w, err, "BAD_REQUEST")
				return
			}
			name = strings.TrimSpace(req.Name)
		}
		if name == "" {
			writeAPIError(w, fmt.Errorf("name required"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		if err := s.installer.Install(name, s.Port()); err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "INSTALL_FAILED")
			return
		}
		// gui-events.log audit row (deep-review P3 finding): emit only
		// after Install committed successfully. Identifier is the server
		// name only — no secret material is ever in scope for this route.
		s.events.PublishOperatorAction("install", api.CurrentOSUser(), map[string]any{
			"server": name,
		})
		// Surface a pre-existing embed-vs-disk collision to the operator (bot PR
		// #494 P2): the shipped manifest installed, but a shadowing disk copy was
		// ignored. Only then does the route return a body — the common no-collision
		// case keeps the 204 contract the frontend already handles.
		if warn := installShadowWarnFn(name); warn != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"warning": warn})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	// DELETE /api/install/:server — uninstall one server (delete its
	// scheduler tasks / supervisor-intent rows, remove its hub-managed
	// client entries). Registered as the /api/install/ subtree; the exact
	// "/api/install" path above is more specific and takes precedence in
	// net/http.ServeMux, so a POST to /api/install never reaches here.
	//
	// Returns 200 with {"uninstall_results": {...}} on a clean teardown, or
	// 207 Multi-Status when the report carries any per-task or per-client
	// warning — mirroring the demigrate/migrate partial-failure convention
	// (the uninstall still succeeded structurally; the warnings are
	// per-target diagnostics the frontend surfaces).
	s.mux.HandleFunc("/api/install/", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.Header().Set("Allow", "DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		server := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/install/"))
		if server == "" || strings.Contains(server, "/") {
			writeAPIError(w, fmt.Errorf("server name required"), http.StatusBadRequest, "BAD_REQUEST")
			return
		}
		report, err := s.uninstaller.Uninstall(server)
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "UNINSTALL_FAILED")
			return
		}
		// gui-events.log audit row: uninstall commits real fleet mutations
		// (scheduler/supervisor-intent teardown + hub-managed client-config
		// removal), the same audit class as the single-install row above.
		// Emitted after Uninstall returned nil (structural success) so it
		// covers both the clean 200 and the partial-warning 207 below; the
		// identifier is the server name only (no secret material in scope).
		s.events.PublishOperatorAction("uninstall", api.CurrentOSUser(), map[string]any{
			"server": server,
		})
		dto := newUninstallResultDTO(report)
		status := http.StatusOK
		if len(dto.TaskDeleteWarns) > 0 || len(dto.ClientWarns) > 0 {
			status = http.StatusMultiStatus // 207 — partial teardown
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"uninstall_results": dto})
	}))

	// POST /api/install-all — install every shipped manifest, or the
	// ?servers=comma,list subset when present. Returns 200 with
	// {"install_results": [{server, error}, ...]}; 207 Multi-Status when any
	// row carries a non-empty error — same bulk-action convention as
	// /api/restart-all in servers.go.
	s.mux.HandleFunc("/api/install-all", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		servers := parseServersFilter(r.URL.Query().Get("servers"))
		results := s.installBulk.InstallAll(servers, s.Port())
		rows := make([]installResultDTO, 0, len(results))
		anyFailed := false
		installed := 0
		failed := 0
		for _, res := range results {
			row := installResultDTO{Server: res.Server}
			if res.Err != nil {
				row.Error = res.Err.Error()
				anyFailed = true
				failed++
			} else {
				installed++
				// Surface a pre-existing embed-vs-disk collision to the operator in
				// the response row (bot PR #494 P2) — the shipped manifest installed,
				// but a shadowing disk copy was ignored.
				row.Warning = installShadowWarnFn(res.Server)
			}
			rows = append(rows, row)
		}
		// gui-events.log audit row: one summary row for the bulk install
		// (each row is a real fleet mutation, same audit class as the single
		// /api/install row). `requested` is the explicit ?servers subset or
		// "all"; installed/failed are the committed counts — no secret
		// material is in scope for install identifiers.
		requested := any("all")
		if len(servers) > 0 {
			requested = servers
		}
		s.events.PublishOperatorAction("install-all", api.CurrentOSUser(), map[string]any{
			"requested": requested,
			"installed": installed,
			"failed":    failed,
		})
		status := http.StatusOK
		if anyFailed {
			status = http.StatusMultiStatus // 207
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"install_results": rows})
	}))
}

// parseServersFilter splits the ?servers=a,b,c query value into a trimmed,
// empties-dropped slice. Empty / absent → nil, which the install-all adapter
// treats as "install everything".
func parseServersFilter(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if name := strings.TrimSpace(part); name != "" {
			out = append(out, name)
		}
	}
	return out
}
