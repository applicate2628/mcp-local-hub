package archguard

import (
	"sort"
	"strconv"
	"strings"
)

const maxArchitectureFeatureAssignments = 256

type architectureFeatureMode uint8

const (
	architectureFeatureCumulative architectureFeatureMode = iota + 1
	architectureFeatureExclusive
	architectureFeatureIndependent
	architectureFeatureAlways
	architectureFeatureARM64Version
)

type architectureFeatureTag struct {
	Tag     string
	Arch    string
	Family  string
	Mode    architectureFeatureMode
	Rank    int
	Minimum int
	Major   int
	Minor   int
}

func parseArchitectureFeatureTag(tag string) (architectureFeatureTag, bool) {
	dot := strings.IndexByte(tag, '.')
	if dot <= 0 || dot == len(tag)-1 {
		return architectureFeatureTag{}, false
	}
	arch := tag[:dot]
	if !isKnownGOARCH(arch) {
		return architectureFeatureTag{}, false
	}
	suffix := tag[dot+1:]
	feature := architectureFeatureTag{Tag: tag, Arch: arch}

	switch arch {
	case "amd64":
		if rank, ok := parsePrefixedPositiveInt(suffix, "v"); ok && rank >= 1 && rank <= 4 {
			feature.Family = "amd64.level"
			feature.Mode = architectureFeatureCumulative
			feature.Rank = rank
			feature.Minimum = 1
			return feature, true
		}
	case "arm":
		if rank, err := strconv.Atoi(suffix); err == nil && rank >= 5 && rank <= 7 {
			feature.Family = "arm.level"
			feature.Mode = architectureFeatureCumulative
			feature.Rank = rank
			feature.Minimum = 5
			return feature, true
		}
	case "ppc64", "ppc64le":
		if rank, ok := parsePrefixedPositiveInt(suffix, "power"); ok && rank >= 8 && rank <= 10 {
			feature.Family = arch + ".level"
			feature.Mode = architectureFeatureCumulative
			feature.Rank = rank
			feature.Minimum = 8
			return feature, true
		}
	case "riscv64":
		if rank, ok := parseRISCVFeatureRank(suffix); ok && (rank == 20 || rank == 22 || rank == 23) {
			feature.Family = "riscv64.level"
			feature.Mode = architectureFeatureCumulative
			feature.Rank = rank
			feature.Minimum = 20
			return feature, true
		}
	case "386":
		if suffix == "sse2" || suffix == "softfloat" {
			feature.Family = "386.float"
			feature.Mode = architectureFeatureExclusive
			return feature, true
		}
	case "arm64":
		if major, minor, ok := parseARM64Version(suffix); ok {
			feature.Family = "arm64.version"
			feature.Mode = architectureFeatureARM64Version
			feature.Major = major
			feature.Minor = minor
			return feature, true
		}
	case "mips", "mipsle", "mips64", "mips64le":
		if suffix == "hardfloat" || suffix == "softfloat" {
			feature.Family = arch + ".float"
			feature.Mode = architectureFeatureExclusive
			return feature, true
		}
	case "wasm":
		if suffix == "satconv" || suffix == "signext" {
			feature.Family = "wasm.always"
			feature.Mode = architectureFeatureAlways
			return feature, true
		}
	}

	return architectureFeatureTag{}, false
}

func parsePrefixedPositiveInt(value, prefix string) (int, bool) {
	if !strings.HasPrefix(value, prefix) {
		return 0, false
	}
	text := strings.TrimPrefix(value, prefix)
	if text == "" {
		return 0, false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(text)
	return n, err == nil && n > 0
}

func parseRISCVFeatureRank(value string) (int, bool) {
	if !strings.HasPrefix(value, "rva") || !strings.HasSuffix(value, "u64") {
		return 0, false
	}
	text := strings.TrimSuffix(strings.TrimPrefix(value, "rva"), "u64")
	if text == "" {
		return 0, false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(text)
	return n, err == nil && n > 0
}

func parseARM64Version(value string) (major, minor int, ok bool) {
	if !strings.HasPrefix(value, "v") {
		return 0, 0, false
	}
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != 2 {
		return 0, 0, false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	if errMajor != nil || errMinor != nil {
		return 0, 0, false
	}
	switch major {
	case 8:
		return major, minor, minor >= 0 && minor <= 9
	case 9:
		return major, minor, minor >= 0 && minor <= 5
	default:
		return 0, 0, false
	}
}

func architectureFeatureAssignments(goarch string, features []architectureFeatureTag) ([]map[string]bool, bool) {
	base := make(map[string]bool, len(features))
	groups := make(map[string][]architectureFeatureTag)
	for _, feature := range features {
		if feature.Arch != goarch {
			base[feature.Tag] = false
			continue
		}
		groups[feature.Family] = append(groups[feature.Family], feature)
	}

	assignments := []map[string]bool{base}
	families := make([]string, 0, len(groups))
	for family := range groups {
		families = append(families, family)
	}
	sort.Strings(families)
	for _, family := range families {
		options := architectureFeatureGroupAssignments(groups[family])
		if len(options) == 0 || len(assignments)*len(options) > maxArchitectureFeatureAssignments {
			return nil, false
		}
		next := make([]map[string]bool, 0, len(assignments)*len(options))
		for _, assignment := range assignments {
			for _, option := range options {
				combined := cloneBoolMap(assignment)
				for tag, enabled := range option {
					combined[tag] = enabled
				}
				next = append(next, combined)
			}
		}
		assignments = deduplicateFeatureAssignments(next, features)
	}
	return assignments, true
}

func architectureFeatureGroupAssignments(features []architectureFeatureTag) []map[string]bool {
	if len(features) == 0 {
		return []map[string]bool{{}}
	}
	sort.Slice(features, func(i, j int) bool { return features[i].Tag < features[j].Tag })
	switch features[0].Mode {
	case architectureFeatureCumulative:
		levels := map[int]struct{}{features[0].Minimum: {}}
		for _, feature := range features {
			levels[feature.Rank] = struct{}{}
		}
		ordered := make([]int, 0, len(levels))
		for level := range levels {
			ordered = append(ordered, level)
		}
		sort.Ints(ordered)
		out := make([]map[string]bool, 0, len(ordered))
		for _, selected := range ordered {
			assignment := make(map[string]bool, len(features))
			for _, feature := range features {
				assignment[feature.Tag] = selected >= feature.Rank
			}
			out = append(out, assignment)
		}
		return deduplicateFeatureAssignments(out, features)
	case architectureFeatureExclusive:
		values := exclusiveArchitectureFeatureValues(features[0].Family)
		if len(values) == 0 {
			values = make([]string, 0, len(features)+1)
			values = append(values, "")
			for _, feature := range features {
				values = append(values, feature.Tag)
			}
		}
		out := make([]map[string]bool, 0, len(values))
		for _, selected := range values {
			assignment := make(map[string]bool, len(features))
			for _, feature := range features {
				assignment[feature.Tag] = feature.Tag == selected
			}
			out = append(out, assignment)
		}
		return deduplicateFeatureAssignments(out, features)
	case architectureFeatureIndependent:
		if len(features) >= 63 || 1<<len(features) > maxArchitectureFeatureAssignments {
			return nil
		}
		out := make([]map[string]bool, 0, 1<<len(features))
		for mask := 0; mask < 1<<len(features); mask++ {
			assignment := make(map[string]bool, len(features))
			for i, feature := range features {
				assignment[feature.Tag] = mask&(1<<i) != 0
			}
			out = append(out, assignment)
		}
		return out
	case architectureFeatureAlways:
		assignment := make(map[string]bool, len(features))
		for _, feature := range features {
			assignment[feature.Tag] = true
		}
		return []map[string]bool{assignment}
	case architectureFeatureARM64Version:
		return arm64VersionAssignments(features)
	default:
		return nil
	}
}

func arm64VersionAssignments(features []architectureFeatureTag) []map[string]bool {
	type version struct {
		major int
		minor int
	}
	versions := make([]version, 0, 16)
	for minor := 0; minor <= 9; minor++ {
		versions = append(versions, version{major: 8, minor: minor})
	}
	for minor := 0; minor <= 5; minor++ {
		versions = append(versions, version{major: 9, minor: minor})
	}
	out := make([]map[string]bool, 0, len(versions))
	for _, selected := range versions {
		assignment := make(map[string]bool, len(features))
		for _, feature := range features {
			assignment[feature.Tag] = arm64VersionSupports(selected.major, selected.minor, feature.Major, feature.Minor)
		}
		out = append(out, assignment)
	}
	return deduplicateFeatureAssignments(out, features)
}

func arm64VersionSupports(selectedMajor, selectedMinor, requiredMajor, requiredMinor int) bool {
	if selectedMajor == requiredMajor {
		return requiredMinor <= selectedMinor
	}
	if selectedMajor == 9 && requiredMajor == 8 {
		return requiredMinor <= selectedMinor+5 && requiredMinor <= 9
	}
	return false
}

func exclusiveArchitectureFeatureValues(family string) []string {
	switch family {
	case "386.float":
		return []string{"386.sse2", "386.softfloat"}
	case "mips.float":
		return []string{"mips.hardfloat", "mips.softfloat"}
	case "mipsle.float":
		return []string{"mipsle.hardfloat", "mipsle.softfloat"}
	case "mips64.float":
		return []string{"mips64.hardfloat", "mips64.softfloat"}
	case "mips64le.float":
		return []string{"mips64le.hardfloat", "mips64le.softfloat"}
	default:
		return nil
	}
}

func cloneBoolMap(source map[string]bool) map[string]bool {
	clone := make(map[string]bool, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func deduplicateFeatureAssignments(assignments []map[string]bool, features []architectureFeatureTag) []map[string]bool {
	tags := make([]string, 0, len(features))
	seenTags := make(map[string]struct{}, len(features))
	for _, feature := range features {
		if _, exists := seenTags[feature.Tag]; exists {
			continue
		}
		seenTags[feature.Tag] = struct{}{}
		tags = append(tags, feature.Tag)
	}
	sort.Strings(tags)
	seen := make(map[string]struct{}, len(assignments))
	out := make([]map[string]bool, 0, len(assignments))
	for _, assignment := range assignments {
		var key strings.Builder
		for _, tag := range tags {
			if assignment[tag] {
				key.WriteByte('1')
			} else {
				key.WriteByte('0')
			}
		}
		if _, exists := seen[key.String()]; exists {
			continue
		}
		seen[key.String()] = struct{}{}
		out = append(out, assignment)
	}
	return out
}
