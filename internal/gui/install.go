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
	Install(name string) error
}

// realInstaller is the production adapter for /api/install. Follows the
// realManifestCreator / realManifestValidator idiom: empty value receiver,
// lazy api.NewAPI() per call so tests can swap the interface without
// needing to stub a constructor. api.API.Install takes InstallOpts; we
// populate only Server because the GUI Save & Install flow is "install
// everything declared in the manifest you just saved."
type realInstaller struct{}

func (realInstaller) Install(name string) error {
	return api.NewAPI().Install(api.InstallOpts{Server: name})
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
	InstallAll(servers []string) []api.InstallResult
}

// realInstallBulkAPI is the production adapter for POST /api/install-all.
// When servers is empty it installs every shipped manifest
// (InstallAllWithOpts consults the embed-first manifest set). When servers
// is non-empty it filters to that subset by installing each name in turn,
// preserving the same []InstallResult row shape so the handler renders one
// path regardless of filter.
type realInstallBulkAPI struct{}

func (realInstallBulkAPI) InstallAll(servers []string) []api.InstallResult {
	a := api.NewAPI()
	if len(servers) == 0 {
		return a.InstallAllWithOpts(api.InstallAllOpts{})
	}
	results := make([]api.InstallResult, 0, len(servers))
	for _, name := range servers {
		err := a.Install(api.InstallOpts{Server: name})
		results = append(results, api.InstallResult{Server: name, Err: err})
	}
	return results
}

type installRequest struct {
	Name string `json:"name"`
}

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
		if err := s.installer.Install(name); err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "INSTALL_FAILED")
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
		results := s.installBulk.InstallAll(servers)
		rows := make([]installResultDTO, 0, len(results))
		anyFailed := false
		for _, res := range results {
			row := installResultDTO{Server: res.Server}
			if res.Err != nil {
				row.Error = res.Err.Error()
				anyFailed = true
			}
			rows = append(rows, row)
		}
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
