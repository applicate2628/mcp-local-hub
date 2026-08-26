package reversedepgraph

import (
	"encoding/json"
	"sort"
	"strings"

	"mcp-local-hub/internal/vcpkgmcp/portname"
)

func ScanDeclaredSuperset(body []byte) ([]string, bool) {
	var manifest struct {
		Name         string                     `json:"name"`
		Dependencies json.RawMessage            `json:"dependencies"`
		Features     map[string]json.RawMessage `json:"features"`
	}
	if len(body) == 0 || json.Unmarshal(body, &manifest) != nil {
		return nil, false
	}
	if strings.TrimSpace(manifest.Name) != "" {
		if _, err := portname.Parse(strings.TrimSpace(manifest.Name)); err != nil {
			return nil, false
		}
	}
	set := map[string]struct{}{}
	if len(manifest.Dependencies) != 0 {
		if !scanDependencyArray(manifest.Dependencies, set) {
			return nil, false
		}
	}
	for _, rawFeature := range manifest.Features {
		var feature struct {
			Dependencies json.RawMessage `json:"dependencies"`
		}
		if json.Unmarshal(rawFeature, &feature) != nil {
			return nil, false
		}
		if len(feature.Dependencies) != 0 && !scanDependencyArray(feature.Dependencies, set) {
			return nil, false
		}
	}
	dependencies := make([]string, 0, len(set))
	for dependency := range set {
		dependencies = append(dependencies, dependency)
	}
	sort.Strings(dependencies)
	return dependencies, true
}

func scanDependencyArray(raw json.RawMessage, set map[string]struct{}) bool {
	var dependencies []json.RawMessage
	if json.Unmarshal(raw, &dependencies) != nil {
		return false
	}
	for _, rawDependency := range dependencies {
		var name string
		if json.Unmarshal(rawDependency, &name) != nil {
			var object struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(rawDependency, &object) != nil || object.Name == "" {
				return false
			}
			name = object.Name
		}
		name = strings.TrimSpace(name)
		if _, err := portname.Parse(name); err != nil {
			return false
		}
		set[name] = struct{}{}
	}
	return true
}

func PotentialCandidates(candidates []Candidate, selected string) []Candidate {
	byName := map[string]Candidate{}
	for _, candidate := range candidates {
		byName[candidate.Name] = candidate
	}
	type reach uint8
	const (
		reachUnknown reach = iota
		reachNo
		reachYes
		reachUncertain
	)
	memo := map[string]reach{}
	visiting := map[string]bool{}
	var resolve func(string) reach
	resolve = func(name string) reach {
		if name == selected {
			return reachYes
		}
		if value := memo[name]; value != reachUnknown {
			return value
		}
		candidate, exists := byName[name]
		if !exists || !candidate.Inspectable {
			return reachUncertain
		}
		if visiting[name] {
			return reachNo
		}
		visiting[name] = true
		result := reachNo
		for _, dependency := range candidate.DeclaredDependencies {
			next := resolve(dependency)
			if next == reachYes {
				result = reachYes
				break
			}
			if next == reachUncertain {
				result = reachUncertain
			}
		}
		visiting[name] = false
		memo[name] = result
		return result
	}
	potential := []Candidate{}
	for _, candidate := range candidates {
		if result := resolve(candidate.Name); result == reachYes || result == reachUncertain {
			potential = append(potential, candidate)
		}
	}
	sort.Slice(potential, func(i, j int) bool { return potential[i].Name < potential[j].Name })
	return potential
}
