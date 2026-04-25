package api

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// UsageRef records one (server, env_var) pair where a secret key is
// referenced. Used in the GET /api/secrets envelope and as the input
// to delete/rotate UI flows that need exact ref locations.
type UsageRef struct {
	Server string `json:"server"`
	EnvVar string `json:"env_var"`
}

// ManifestError surfaces a per-manifest parse failure during the
// secrets scan. Only failures of the {Name, Env} narrow projection
// are recorded — full-schema drift in unrelated fields is not
// reported (per memo §2.5).
type ManifestError struct {
	Name  string `json:"name,omitempty"`
	Path  string `json:"path"`
	Error string `json:"error"`
}

// scanProjection is the narrow YAML shape the scan parses. Anything
// outside Name and Env is ignored. This keeps the scan tolerant: a
// manifest with a malformed `daemons` block still contributes its
// secret refs.
type scanProjection struct {
	Name string            `yaml:"name"`
	Env  map[string]string `yaml:"env"`
}

// ScanManifestEnv walks the embed-first/disk-fallback manifest set and
// returns a map of secret keys to the (server, env_var) refs that use
// them, plus a list of per-manifest parse errors. The function never
// returns a non-nil error on per-manifest parse failures — callers
// must inspect the manifest_errors slice for those.
func ScanManifestEnv() (map[string][]UsageRef, []ManifestError, error) {
	names, err := listManifestNamesEmbedFirst()
	if err != nil {
		return nil, nil, fmt.Errorf("list manifests: %w", err)
	}

	usage := make(map[string][]UsageRef)
	var errs []ManifestError

	for _, name := range names {
		raw, err := loadManifestYAMLEmbedFirst(name)
		if err != nil {
			errs = append(errs, ManifestError{
				Path:  name + "/manifest.yaml",
				Error: err.Error(),
			})
			continue
		}
		var proj scanProjection
		parseErr := yaml.Unmarshal(raw, &proj)
		if parseErr != nil {
			// Try a name-only fallback so the ManifestError can carry a Name when known.
			var nameOnly struct {
				Name string `yaml:"name"`
			}
			if nameErr := yaml.Unmarshal(raw, &nameOnly); nameErr == nil && nameOnly.Name != "" {
				errs = append(errs, ManifestError{Name: nameOnly.Name, Path: name + "/manifest.yaml", Error: parseErr.Error()})
			} else {
				errs = append(errs, ManifestError{Path: name + "/manifest.yaml", Error: parseErr.Error()})
			}
			continue
		}
		if proj.Name == "" {
			errs = append(errs, ManifestError{
				Path:  name + "/manifest.yaml",
				Error: "missing name field",
			})
			continue
		}
		for envKey, envVal := range proj.Env {
			if !strings.HasPrefix(envVal, "secret:") {
				continue
			}
			key := strings.TrimPrefix(envVal, "secret:")
			usage[key] = append(usage[key], UsageRef{
				Server: proj.Name,
				EnvVar: envKey,
			})
		}
	}

	// Sort each usage[] slice by Server name for deterministic output.
	for k := range usage {
		sort.Slice(usage[k], func(i, j int) bool {
			return usage[k][i].Server < usage[k][j].Server
		})
	}

	return usage, errs, nil
}
