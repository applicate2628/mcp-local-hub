package cbuild

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// DependencyInfo is one summarized vcpkg.json dependency.
type DependencyInfo struct {
	Name              string   `json:"name"`
	Features          []string `json:"features,omitempty"`
	DefaultFeatures   *bool    `json:"defaultFeatures,omitempty"`
	Platform          string   `json:"platform,omitempty"`
	VersionConstraint string   `json:"versionConstraint,omitempty"` // from "version>="
}

// OverrideInfo is one vcpkg.json version override.
type OverrideInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ManifestResult is the vcpkg_manifest tool payload.
type ManifestResult struct {
	Path            string              `json:"path"`
	Name            string              `json:"name,omitempty"`
	Version         string              `json:"version,omitempty"`
	BuiltinBaseline string              `json:"builtinBaseline,omitempty"`
	Dependencies    []DependencyInfo    `json:"dependencies"`
	Features        map[string][]string `json:"features,omitempty"`
	Overrides       []OverrideInfo      `json:"overrides,omitempty"`
}

type rawManifest struct {
	Name            string            `json:"name"`
	Version         string            `json:"version"`
	VersionString   string            `json:"version-string"`
	VersionSemver   string            `json:"version-semver"`
	VersionDate     string            `json:"version-date"`
	BuiltinBaseline string            `json:"builtin-baseline"`
	Dependencies    []json.RawMessage `json:"dependencies"`
	Features        map[string]struct {
		Description  string            `json:"description"`
		Dependencies []json.RawMessage `json:"dependencies"`
	} `json:"features"`
	Overrides []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"overrides"`
}

// LoadManifest reads and summarizes the vcpkg.json in dir. Read-only.
func LoadManifest(dir string) (*ManifestResult, error) {
	path := filepath.Join(dir, "vcpkg.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var m rawManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	res := &ManifestResult{
		Path:            path,
		Name:            m.Name,
		Version:         firstNonEmpty(m.Version, m.VersionString, m.VersionSemver, m.VersionDate),
		BuiltinBaseline: m.BuiltinBaseline,
		Dependencies:    []DependencyInfo{},
	}
	for _, raw := range m.Dependencies {
		if d, ok := parseDependency(raw); ok {
			res.Dependencies = append(res.Dependencies, d)
		}
	}
	if len(m.Features) > 0 {
		res.Features = map[string][]string{}
		for name, feat := range m.Features {
			deps := []string{}
			for _, raw := range feat.Dependencies {
				if d, ok := parseDependency(raw); ok {
					deps = append(deps, d.Name)
				}
			}
			res.Features[name] = deps
		}
	}
	for _, o := range m.Overrides {
		res.Overrides = append(res.Overrides, OverrideInfo{Name: o.Name, Version: o.Version})
	}
	return res, nil
}

// parseDependency handles a dependency entry that is either a bare string name
// or an object with name/features/platform/version>=/default-features.
func parseDependency(raw json.RawMessage) (DependencyInfo, bool) {
	raw = trimSpaceBytes(raw)
	if len(raw) == 0 {
		return DependencyInfo{}, false
	}
	if raw[0] == '"' {
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			return DependencyInfo{}, false
		}
		return DependencyInfo{Name: name}, true
	}
	var obj struct {
		Name            string   `json:"name"`
		Features        []string `json:"features"`
		DefaultFeatures *bool    `json:"default-features"`
		Platform        string   `json:"platform"`
		VersionGE       string   `json:"version>="`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Name == "" {
		return DependencyInfo{}, false
	}
	return DependencyInfo{
		Name:              obj.Name,
		Features:          obj.Features,
		DefaultFeatures:   obj.DefaultFeatures,
		Platform:          obj.Platform,
		VersionConstraint: obj.VersionGE,
	}, true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
