package api

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// settingsMu serializes read-validate-write over gui-preferences.yaml.
// Mirrors vaultMutex in secrets.go. Memo §6.2 (Codex r1 P1.5): without
// this, concurrent PUT /api/settings/<a> and PUT /api/settings/<b> can
// each read {x:1, y:2}, modify their own key, and the slower writer
// silently drops the faster writer's change.
var settingsMu sync.Mutex

// SettingsPath returns the canonical preferences file location (in the
// per-user data dir — same as secrets).
func SettingsPath() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "gui-preferences.yaml")
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "mcp-local-hub", "gui-preferences.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "gui-preferences.yaml"
	}
	return filepath.Join(home, ".local", "share", "mcp-local-hub", "gui-preferences.yaml")
}

// SettingsList returns the schema-resolved snapshot: every registry key
// (settable + action) mapped to either the persisted value or its
// registry default. Unknown keys in the YAML file are NOT included in
// the returned map (they are preserved on disk via SettingsSet, but not
// exposed through the schema-resolved API).
func (a *API) SettingsList() (map[string]string, error) {
	return a.SettingsListIn(SettingsPath())
}

// SettingsListIn is the tempdir-capable form.
func (a *API) SettingsListIn(path string) (map[string]string, error) {
	raw, err := readRawSettingsMap(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, def := range SettingsRegistry {
		if def.Type == TypeAction {
			continue
		}
		if v, ok := raw[def.Key]; ok {
			out[def.Key] = v
			continue
		}
		out[def.Key] = def.Default
	}
	return out, nil
}

// SettingsGet returns the value for a key (registry default if not
// persisted). Returns an error if the key is unknown or is an action.
func (a *API) SettingsGet(key string) (string, error) {
	return a.SettingsGetIn(SettingsPath(), key)
}

// SettingsGetIn is the tempdir-capable form.
func (a *API) SettingsGetIn(path, key string) (string, error) {
	def := findDef(key)
	if def == nil {
		return "", fmt.Errorf("unknown setting %q", key)
	}
	if def.Type == TypeAction {
		return "", fmt.Errorf("%q is an action; use 'mcp settings invoke' (coming in A4-b)", key)
	}
	all, err := a.SettingsListIn(path)
	if err != nil {
		return "", err
	}
	return all[key], nil
}

// SettingsSet writes a key=value pair, creating the file if needed.
// Validates against the registry, preserves unknown keys on the way
// through (memo §2.2 Codex r1 P2.1), and serializes the read-modify-write
// via settingsMu (memo §6.2 Codex r1 P1.5).
func (a *API) SettingsSet(key, value string) error {
	return a.SettingsSetIn(SettingsPath(), key, value)
}

// SettingsSetIn is the tempdir-capable form.
func (a *API) SettingsSetIn(path, key, value string) error {
	def := findDef(key)
	if def == nil {
		return fmt.Errorf("unknown setting %q", key)
	}
	if err := validate(def, value); err != nil {
		return fmt.Errorf("invalid value for %s: %v", key, err)
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	raw, err := readRawSettingsMap(path)
	if err != nil {
		return err
	}
	raw[key] = value
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// readRawSettingsMap reads the file as a flat map[string]string. Unknown
// keys (e.g., a typo or a future-deferred entry written by CLI ahead of
// A4-b's GUI editor) are preserved verbatim. Returns an empty map if
// the file does not exist.
func readRawSettingsMap(path string) (map[string]string, error) {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}
