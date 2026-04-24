package api

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"mcp-local-hub/internal/config"
)

// validManifestName bounds acceptable server names to lower-case
// letters, digits, dot, underscore, hyphen. Any other character would
// either (a) change how defaultManifestDir's filepath.Join resolves —
// ".." escapes the parent, absolute paths ignore the root, leading
// slashes change the meaning — or (b) collide with OS-specific path
// semantics (colon-drive on Windows, control chars). Restricting the
// charset means we never need to revalidate the joined path, which
// eliminates the class of bugs where name parsing and path resolution
// disagree.
var validManifestName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// checkManifestName rejects names that could escape the manifest
// directory via path traversal, contain absolute-path semantics, or
// land on reserved Windows filenames. Returns a descriptive error so
// the CLI/API surface the reason rather than a generic "bad name".
func checkManifestName(name string) error {
	if name == "" {
		return fmt.Errorf("manifest name: must be non-empty")
	}
	if !validManifestName.MatchString(name) {
		return fmt.Errorf("manifest name %q: must match [a-z0-9][a-z0-9._-]* (lowercase ASCII, digits, '.', '_', '-', and must not start with '.' or '-')", name)
	}
	// Reject any name that resolves to '.' or '..' after clean. The
	// regex already blocks '..' literally, but Clean catches chained
	// forms like '.../..' that a future looser regex might miss.
	if clean := filepath.Clean(name); clean != name || clean == "." || clean == ".." {
		return fmt.Errorf("manifest name %q: resolves to non-canonical path %q", name, clean)
	}
	return nil
}

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
	if err := checkManifestName(name); err != nil {
		return "", err
	}
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

// ManifestGetInWithHash reads the manifest YAML and returns both the
// text and its SHA-256 content hash. Used by the GUI edit flow so
// ManifestEdit can detect external writes that occurred between Load
// and Save (A2b D3 stale-file detection).
func (a *API) ManifestGetInWithHash(dir, name string) (string, string, error) {
	path := filepath.Join(dir, name, "manifest.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	return string(data), ManifestHashContent(data), nil
}

// ManifestGetWithHash is the default-dir convenience wrapper, used by
// GUI handlers which always read from defaultManifestDir().
func (a *API) ManifestGetWithHash(name string) (string, string, error) {
	if err := checkManifestName(name); err != nil {
		return "", "", err
	}
	// Read from disk (not embed) because edit flow only makes sense
	// for user-created / on-disk manifests — you cannot edit embedded
	// shipped manifests in-place.
	return a.ManifestGetInWithHash(defaultManifestDir(), name)
}

// ManifestCreate writes a new manifest under the default servers dir.
// Rejects if the server name already has a manifest — use ManifestEdit
// to change existing ones.
func (a *API) ManifestCreate(name, yaml string) error {
	return a.ManifestCreateIn(defaultManifestDir(), name, yaml)
}

// ManifestCreateIn is the tempdir-capable form of ManifestCreate.
func (a *API) ManifestCreateIn(dir, name, yaml string) error {
	if err := checkManifestName(name); err != nil {
		return err
	}
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
	if err := checkManifestName(name); err != nil {
		return err
	}
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
//
// The regex-guarded name lets us use filepath.Join safely: validManifestName
// cannot contain separators, so the resulting target is always a direct
// child of dir. We still compare Dir(target) to Clean(dir) as a defense in
// depth in case some future edit relaxes the guard or introduces a new
// separator quirk.
func (a *API) ManifestDeleteIn(dir, name string) error {
	if err := checkManifestName(name); err != nil {
		return err
	}
	target := filepath.Join(dir, name)
	cleanDir := filepath.Clean(dir)
	if parent := filepath.Dir(target); parent != cleanDir {
		return fmt.Errorf("manifest delete: resolved path %q escapes manifest dir %q", target, cleanDir)
	}
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("manifest %q does not exist", name)
	}
	return os.RemoveAll(target)
}
