//go:build !windows

// internal/gui/state_relax_setting_other.go
//
// Non-Windows stub for the state-read-relax setting toggle. On POSIX
// there is no equivalent of HKCU\Environment that survives reboot
// AND is automatically inherited by login-session children — env
// vars get exported from shell profile files (.bashrc, .zshrc,
// fish.config) which are user-managed. The handler returns 501 Not
// Implemented so the frontend can render a "Set the env var in your
// shell profile" hint instead of pretending to flip a registry
// switch that doesn't exist.

package gui

import (
	"encoding/json"
	"net/http"
)

func registerStateRelaxSettingRoutes(s *Server) {
	s.mux.HandleFunc("/api/settings/state-read-relax", s.requireSameOrigin(s.stateRelaxSettingHandler))
}

func (s *Server) stateRelaxSettingHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":    "not_supported_on_this_os",
		"code":     "STATE_RELAX_TOGGLE_UNSUPPORTED",
		"detail":   "Toggling the state-read relax env var via GUI is Windows-only. On POSIX, export MCPHUB_ALLOW_UNHARDENED_STATE_READF=1 from your shell profile (~/.bashrc / ~/.zshrc / fish.config).",
		"platform": "non-windows",
	})
}
