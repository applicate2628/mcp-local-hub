// internal/gui/settings.go
package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/api"
)

// settingsAPI is the narrow surface used by /api/settings handlers.
// realSettingsAPI delegates to *api.API; tests inject their own.
type settingsAPI interface {
	List() (map[string]string, error)
	Set(key, value string) error
	OpenPath(path string) error
	SettingsPath() string
}

type realSettingsAPI struct{}

func (realSettingsAPI) List() (map[string]string, error)   { return api.NewAPI().SettingsList() }
func (realSettingsAPI) Set(key, value string) error        { return api.NewAPI().SettingsSet(key, value) }
func (realSettingsAPI) SettingsPath() string               { return api.SettingsPath() }
func (realSettingsAPI) OpenPath(path string) error         { return OpenPath(path) }

// configSettingDTO is the JSON shape for non-action settings entries.
// `default` and `value` are ALWAYS emitted (no omitempty) so legitimate
// empty values — most importantly `appearance.default_home` whose
// default is "" with Optional:true — round-trip correctly. Memo §6.1.
//
// Codex r6 P2: rev-1 used a single settingDTO with `omitempty` on
// Default/Value, which dropped those keys for any non-action setting
// whose value happened to be empty. Splitting into two DTO types is
// the only way to guarantee the wire contract: actions omit, configs
// always include.
type configSettingDTO struct {
	Key      string   `json:"key"`
	Section  string   `json:"section"`
	Type     string   `json:"type"`
	Default  string   `json:"default"`        // ALWAYS emitted — no omitempty
	Value    string   `json:"value"`          // ALWAYS emitted — no omitempty
	Enum     []string `json:"enum,omitempty"`
	Min      *int     `json:"min,omitempty"`
	Max      *int     `json:"max,omitempty"`
	Pattern  string   `json:"pattern,omitempty"`
	Optional bool     `json:"optional,omitempty"`
	Deferred bool     `json:"deferred"`
	Help     string   `json:"help"`
}

// actionSettingDTO is the JSON shape for action settings entries.
// `default` and `value` are deliberately absent — actions have no
// stored value. Codex r1 P2.2 + r2 P1.2.
type actionSettingDTO struct {
	Key      string `json:"key"`
	Section  string `json:"section"`
	Type     string `json:"type"` // always "action"
	Deferred bool   `json:"deferred"`
	Help     string `json:"help"`
}

func registerSettingsRoutes(s *Server) {
	s.mux.HandleFunc("/api/settings", s.requireSameOrigin(s.settingsListHandler))
	s.mux.HandleFunc("/api/settings/", s.requireSameOrigin(s.settingsByKeyHandler))
}

// settingsListHandler handles GET /api/settings. Returns a snapshot of
// every registry entry (action + non-action) plus the live actual_port
// at the top level. Memo §6.1.
func (s *Server) settingsListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	values, err := s.settings.List()
	if err != nil {
		writeAPIError(w, err, http.StatusInternalServerError, "SETTINGS_LIST_FAILED")
		return
	}
	// Heterogeneous slice: configSettingDTO or actionSettingDTO per entry.
	settings := make([]any, 0, len(api.SettingsRegistry))
	for _, def := range api.SettingsRegistry {
		if def.Type == api.TypeAction {
			settings = append(settings, actionSettingDTO{
				Key:      def.Key,
				Section:  def.Section,
				Type:     string(def.Type),
				Deferred: def.Deferred,
				Help:     def.Help,
			})
			continue
		}
		v, has := values[def.Key]
		if !has {
			v = def.Default
		}
		settings = append(settings, configSettingDTO{
			Key:      def.Key,
			Section:  def.Section,
			Type:     string(def.Type),
			Default:  def.Default,
			Value:    v,
			Enum:     def.Enum,
			Min:      def.Min,
			Max:      def.Max,
			Pattern:  def.Pattern,
			Optional: def.Optional,
			Deferred: def.Deferred,
			Help:     def.Help,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":    settings,
		"actual_port": s.Port(),
	})
}

// settingsByKeyHandler handles PUT /api/settings/<key> and POST
// /api/settings/<action>. Memo §6.2 + §6.3.
func (s *Server) settingsByKeyHandler(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/api/settings/")
	if key == "" {
		http.NotFound(w, r)
		return
	}
	def := findRegistryDef(key)
	if def == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "unknown_setting",
			"key":   key,
		})
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.settingsPut(w, r, def)
	case http.MethodPost:
		s.settingsPost(w, r, def)
	default:
		if def.Type == api.TypeAction {
			w.Header().Set("Allow", "POST")
		} else {
			w.Header().Set("Allow", "PUT")
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) settingsPut(w http.ResponseWriter, r *http.Request, def *api.SettingDef) {
	if def.Type == api.TypeAction {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": "is_action",
			"key":   def.Key,
		})
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest, "SETTINGS_INVALID_JSON")
		return
	}
	if err := s.settings.Set(def.Key, body.Value); err != nil {
		// Validation failures bubble up from api.SettingsSet with prefix
		// "invalid value for ...:". Map them to 400. Other errors are 500.
		if strings.HasPrefix(err.Error(), "invalid value") {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":  "validation_failed",
				"key":    def.Key,
				"reason": err.Error(),
			})
			return
		}
		writeAPIError(w, err, http.StatusInternalServerError, "SETTINGS_SET_FAILED")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"saved": true,
		"key":   def.Key,
		"value": body.Value,
	})
}

func (s *Server) settingsPost(w http.ResponseWriter, r *http.Request, def *api.SettingDef) {
	if def.Type != api.TypeAction {
		w.Header().Set("Allow", "PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": "not_action",
			"key":   def.Key,
		})
		return
	}
	if def.Deferred {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "deferred_action_not_implemented",
			"key":   def.Key,
		})
		return
	}
	switch def.Key {
	case "advanced.open_app_data_folder":
		dir := filepath.Dir(s.settings.SettingsPath())
		if err := s.settings.OpenPath(dir); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":  "spawn_failed",
				"reason": err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"opened": dir})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "unknown_action",
			"key":   def.Key,
		})
	}
}

// findRegistryDef returns a pointer into api.SettingsRegistry for key, or nil.
// Local helper so this file does not depend on an unexported api function.
func findRegistryDef(key string) *api.SettingDef {
	for i := range api.SettingsRegistry {
		if api.SettingsRegistry[i].Key == key {
			return &api.SettingsRegistry[i]
		}
	}
	return nil
}
