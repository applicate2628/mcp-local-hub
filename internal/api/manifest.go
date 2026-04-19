package api

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mcp-local-hub/internal/config"
)

// ManifestList returns the sorted list of server names that have a
// manifest, unioning the embed FS (shipped with the binary, source of
// truth in production) with the on-disk defaultManifestDir (used by
// dev flows where a new manifest hasn't been compiled in yet).
//
// Before this changed, ManifestList ONLY looked at disk — so a canonical
// ~/.local/bin/mcphub.exe invoked from %TEMP% reported 0 servers even
// though 10 were baked into the binary. That was split-brain with the
// daemon (which always reads from embed).
func (a *API) ManifestList() ([]string, error) {
	return listManifestNamesEmbedFirst()
}

// ManifestListIn is the tempdir-capable form of ManifestList.
func (a *API) ManifestListIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "manifest.yaml")); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// ManifestGet returns the raw YAML of the named server's manifest,
// reading from the embed FS first (production) with disk fallback for
// dev flow. See listManifestNamesEmbedFirst for the rationale.
func (a *API) ManifestGet(name string) (string, error) {
	data, err := loadManifestYAMLEmbedFirst(name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ManifestGetIn is the tempdir-capable form of ManifestGet.
func (a *API) ManifestGetIn(dir, name string) (string, error) {
	path := filepath.Join(dir, name, "manifest.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ManifestCreate writes a new manifest under the default servers dir.
// Rejects if the server name already has a manifest — use ManifestEdit
// to change existing ones.
func (a *API) ManifestCreate(name, yaml string) error {
	return a.ManifestCreateIn(defaultManifestDir(), name, yaml)
}

// ManifestCreateIn is the tempdir-capable form of ManifestCreate.
func (a *API) ManifestCreateIn(dir, name, yaml string) error {
	target := filepath.Join(dir, name, "manifest.yaml")
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("manifest %q already exists at %s; use edit instead", name, target)
	}
	if warnings := a.ManifestValidate(yaml); len(warnings) > 0 {
		return fmt.Errorf("manifest has validation errors: %s", strings.Join(warnings, "; "))
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	return os.WriteFile(target, []byte(yaml), 0644)
}

// ManifestEdit replaces an existing manifest after validation. Fails if
// the manifest doesn't exist; use ManifestCreate for new entries.
func (a *API) ManifestEdit(name, yaml string) error {
	return a.ManifestEditIn(defaultManifestDir(), name, yaml)
}

// ManifestEditIn is the tempdir-capable form of ManifestEdit.
func (a *API) ManifestEditIn(dir, name, yaml string) error {
	target := filepath.Join(dir, name, "manifest.yaml")
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("manifest %q does not exist; use create instead", name)
	}
	if warnings := a.ManifestValidate(yaml); len(warnings) > 0 {
		return fmt.Errorf("manifest has validation errors: %s", strings.Join(warnings, "; "))
	}
	return os.WriteFile(target, []byte(yaml), 0644)
}

// ManifestValidate parses a manifest YAML and returns any structural
// issues (missing required fields, unknown kind/transport values). Empty
// slice means the manifest passes basic validation. Does NOT check that
// referenced binaries, ports, or secrets actually exist — that's caller
// responsibility at install time.
func (a *API) ManifestValidate(yaml string) []string {
	var warnings []string
	reader := strings.NewReader(yaml)
	m, err := config.ParseManifest(reader)
	if err != nil {
		return []string{err.Error()}
	}
	// ParseManifest calls m.Validate internally, so if we reach here the
	// structural validation passed. Add secondary soft checks:
	if len(m.Daemons) == 0 {
		warnings = append(warnings, "no daemons declared")
	}
	for _, d := range m.Daemons {
		if d.Port == 0 {
			warnings = append(warnings, fmt.Sprintf("daemon %q has port=0", d.Name))
		}
	}
	return warnings
}

// ManifestDelete removes the named server's manifest directory. Does NOT
// uninstall the server — caller should run Uninstall first for a clean
// teardown.
func (a *API) ManifestDelete(name string) error {
	return a.ManifestDeleteIn(defaultManifestDir(), name)
}

// ManifestDeleteIn is the tempdir-capable form of ManifestDelete.
func (a *API) ManifestDeleteIn(dir, name string) error {
	target := filepath.Join(dir, name)
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("manifest %q does not exist", name)
	}
	return os.RemoveAll(target)
}
