// internal/gui/client_install_prefs.go
//
// GUI management surface for the operator override of the default-install
// client set (Settings → Clients panel; redesign spec §9 multi-agent table
// / line 204). The compile-time default-install set is the fixed
// {claude-code, codex-cli, cursor} trio (clients.DefaultInstallClientNames,
// derived from each clientRegistry descriptor's defaultInstall flag). This
// handler lets the operator override that set from the GUI without editing
// source: the chosen set is persisted to gui-preferences.yaml and becomes
// the effective default for installs that do NOT request an explicit
// --clients / ClientsInclude target.
//
// The persistence + validation + effective-set resolution all live in the
// api layer (internal/api/client_install_prefs.go); this file is the thin
// HTTP wrapper, mirroring how internal/gui/settings.go delegates to
// api.NewAPI().SettingsList()/SettingsSet(). Wiring: the api layer's
// API.DefaultInstallClientNamesOverride feeds installClientPredicate's
// default-set fallback (via BuildPlanOpts.DefaultClientsOverride at the
// Install entry points), so the toggle ACTUALLY changes which clients a
// default install touches — it is not a half-wired UI.
//
// Security posture mirrors lsp_trusted_roots_handler.go:
//   - both methods are wrapped in s.requireSameOrigin (registered below),
//   - inputs are validated server-side: every requested client name must be
//     a clients.SupportedClientNames() member (the api SetDefault... gate
//     rejects unknown names), and the set must be non-empty,
//   - there is NO arbitrary path / file target — the only persistence sink
//     is the fixed gui-preferences.yaml under the per-user data dir, written
//     atomically by the api layer. No path-traversal surface exists.
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

// clientInstallPrefsViewFn / clientInstallPrefsSetFn are package-level
// indirections so a unit test can stub the api round-trip without touching
// the real gui-preferences.yaml. Production leaves them nil and the handler
// calls api.NewAPI() directly (the same direct-api pattern
// lspTrustedRootsHandler uses). A test sets these to a fake before driving
// the handler and restores them after.
var (
	clientInstallPrefsViewFn func() (api.ClientInstallToggleSnapshot, error)
	clientInstallPrefsSetFn  func(names []string) error
)

func clientInstallPrefsView() (api.ClientInstallToggleSnapshot, error) {
	if clientInstallPrefsViewFn != nil {
		return clientInstallPrefsViewFn()
	}
	return api.NewAPI().ClientInstallToggleView()
}

func clientInstallPrefsSetNames(names []string) error {
	if clientInstallPrefsSetFn != nil {
		return clientInstallPrefsSetFn(names)
	}
	return api.NewAPI().SetDefaultInstallClientNames(names)
}

func registerClientInstallPrefsRoutes(s *Server) {
	s.mux.HandleFunc("/api/client-install-prefs", s.requireSameOrigin(s.clientInstallPrefsHandler))
}

// clientInstallPrefsClientDTO is one client row in the GET response: its
// stable id, whether the COMPILE-TIME registry marks it default-install
// (so the UI can label the canonical trio), and whether it is currently in
// the effective default-install set.
type clientInstallPrefsClientDTO struct {
	Name           string `json:"name"`
	CompileDefault bool   `json:"compile_default"`
	Selected       bool   `json:"selected"`
}

// clientInstallPrefsResponse is the wire shape of GET / POST. Clients is
// always a non-nil array (registry order). OverrideActive reports whether
// an explicit operator override is persisted (vs. the compile-time trio
// fallback) so the UI can show a "using defaults" vs. "customized" hint.
type clientInstallPrefsResponse struct {
	Clients        []clientInstallPrefsClientDTO `json:"clients"`
	OverrideActive bool                          `json:"override_active"`
}

// clientInstallPrefsHandler dispatches GET / POST on
// /api/client-install-prefs. Method dispatch + JSON error helpers mirror
// lspTrustedRootsHandler (internal/gui/lsp_trusted_roots_handler.go).
func (s *Server) clientInstallPrefsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.clientInstallPrefsList(w, r)
	case http.MethodPost:
		s.clientInstallPrefsSet(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// clientInstallPrefsList handles GET /api/client-install-prefs. Returns
// every supported client with its compile-default + selected flags plus
// whether an override is active. An absent gui-preferences.yaml yields the
// compile-time trio selected (override_active=false) — the normal
// first-run path.
func (s *Server) clientInstallPrefsList(w http.ResponseWriter, _ *http.Request) {
	snap, err := clientInstallPrefsView()
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "CLIENT_INSTALL_PREFS_LIST_FAILED")
		return
	}
	writeJSON(w, http.StatusOK, snapshotToResponse(snap))
}

// clientInstallPrefsSet handles POST /api/client-install-prefs with body
// {"clients":["claude-code","cursor",...]}. The set must be non-empty and
// every name must be a supported client; the api layer is the authoritative
// validator (unknown name → 400, empty set → 400). On success it returns
// the refreshed snapshot so the UI re-renders without a follow-up GET.
func (s *Server) clientInstallPrefsSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Clients []string `json:"clients"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "CLIENT_INSTALL_PREFS_INVALID_JSON")
		return
	}
	if err := clientInstallPrefsSetNames(body.Clients); err != nil {
		// The api layer returns "unknown client ..." for an unsupported
		// name and "must name at least one ..." for an empty set — both
		// are operator-input errors → 400 so the UI surfaces a precise
		// message instead of a generic 500.
		if strings.Contains(err.Error(), "unknown client") || strings.Contains(err.Error(), "at least one") {
			writeAPIError(w, err, http.StatusBadRequest, "CLIENT_INSTALL_PREFS_INVALID")
			return
		}
		writeAPIError(w, err, http.StatusInternalServerError, "CLIENT_INSTALL_PREFS_SET_FAILED")
		return
	}
	snap, err := clientInstallPrefsView()
	if err != nil {
		// The write landed but the post-write read failed — surface as 500
		// so the UI refreshes manually (mirrors respondWithTrustedRoots).
		writeAPIError(w, err, http.StatusInternalServerError, "CLIENT_INSTALL_PREFS_LIST_FAILED")
		return
	}
	writeJSON(w, http.StatusOK, snapshotToResponse(snap))
}

// snapshotToResponse maps the api snapshot to the snake_case wire DTO with a
// non-nil Clients slice.
func snapshotToResponse(snap api.ClientInstallToggleSnapshot) clientInstallPrefsResponse {
	rows := make([]clientInstallPrefsClientDTO, 0, len(snap.Rows))
	for _, row := range snap.Rows {
		rows = append(rows, clientInstallPrefsClientDTO{
			Name:           row.Name,
			CompileDefault: row.CompileDefault,
			Selected:       row.Selected,
		})
	}
	return clientInstallPrefsResponse{Clients: rows, OverrideActive: snap.OverrideActive}
}
