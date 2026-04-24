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

type installRequest struct {
	Name string `json:"name"`
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
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "BAD_REQUEST")
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
}
