package api

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

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

// SettingsList returns all settings as a key→value map.
func (a *API) SettingsList() (map[string]string, error) {
	return a.SettingsListIn(SettingsPath())
}

// SettingsListIn is the tempdir-capable form of SettingsList.
func (a *API) SettingsListIn(path string) (map[string]string, error) {
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

// SettingsGet returns the value for a key.
func (a *API) SettingsGet(key string) (string, error) {
	return a.SettingsGetIn(SettingsPath(), key)
}

// SettingsGetIn is the tempdir-capable form of SettingsGet.
func (a *API) SettingsGetIn(path, key string) (string, error) {
	all, err := a.SettingsListIn(path)
	if err != nil {
		return "", err
	}
	v, ok := all[key]
	if !ok {
		return "", fmt.Errorf("setting %q not found", key)
	}
	return v, nil
}

// SettingsSet writes a key=value pair, creating the file if needed.
func (a *API) SettingsSet(key, value string) error {
	return a.SettingsSetIn(SettingsPath(), key, value)
}

// SettingsSetIn is the tempdir-capable form of SettingsSet.
func (a *API) SettingsSetIn(path, key, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	all, err := a.SettingsListIn(path)
	if err != nil {
		return err
	}
	all[key] = value
	data, err := yaml.Marshal(all)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
