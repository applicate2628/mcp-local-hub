package archguard

import (
	"go/build/constraint"
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
)

type localPackageVariant struct {
	Name       string
	Constraint constraint.Expr
}

type goTarget struct {
	GOOS   string
	GOARCH string
}

func buildConstraintForFile(file fileContext) constraint.Expr {
	var explicit constraint.Expr
	if value, ok := leadingBuildConstraint(file.File, file.FSet, file.Source); ok {
		explicit = value
	}
	implicit := filenameBuildConstraint(file.Path)
	switch {
	case explicit == nil:
		return implicit
	case implicit == nil:
		return explicit
	default:
		return &constraint.AndExpr{X: explicit, Y: implicit}
	}
}

func filenameBuildConstraint(filePath string) constraint.Expr {
	base := pathpkg.Base(filePath)
	if !strings.HasSuffix(base, ".go") {
		return nil
	}
	stem := strings.TrimSuffix(base, ".go")
	stem = strings.TrimSuffix(stem, "_test")
	parts := strings.Split(stem, "_")
	if len(parts) < 2 {
		return nil
	}
	last := parts[len(parts)-1]
	if isKnownGOARCH(last) {
		var result constraint.Expr = &constraint.TagExpr{Tag: last}
		if len(parts) >= 3 && isKnownGOOS(parts[len(parts)-2]) {
			result = &constraint.AndExpr{X: &constraint.TagExpr{Tag: parts[len(parts)-2]}, Y: result}
		}
		return result
	}
	if isKnownGOOS(last) {
		return &constraint.TagExpr{Tag: last}
	}
	return nil
}

func packageVariantsConflict(existing []localPackageVariant, candidate localPackageVariant) bool {
	for _, prior := range existing {
		if prior.Name != candidate.Name && buildConstraintsOverlap(prior.Constraint, candidate.Constraint) {
			return true
		}
	}
	return false
}

func compatiblePackageName(variants []localPackageVariant, importer constraint.Expr) (string, bool) {
	names := map[string]struct{}{}
	for _, variant := range variants {
		if buildConstraintsOverlap(importer, variant.Constraint) {
			names[variant.Name] = struct{}{}
		}
	}
	if len(names) != 1 {
		return "", false
	}
	for name := range names {
		return name, true
	}
	return "", false
}

func buildConstraintsOverlap(left, right constraint.Expr) bool {
	expr := andBuildConstraints(left, right)
	if expr == nil {
		return true
	}
	tags := map[string]struct{}{}
	collectConstraintTags(expr, tags)
	releaseSet := make(map[int]struct{})
	featureByTag := make(map[string]architectureFeatureTag)
	hasCompiler := false
	hasCgo := false
	for tag := range tags {
		switch {
		case tag == "gc" || tag == "gccgo":
			hasCompiler = true
		case tag == "cgo":
			hasCgo = true
		default:
			if release, ok := parseGoReleaseTag(tag); ok {
				releaseSet[release] = struct{}{}
				continue
			}
			if feature, ok := parseArchitectureFeatureTag(tag); ok {
				featureByTag[tag] = feature
			}
		}
	}

	features := make([]architectureFeatureTag, 0, len(featureByTag))
	for _, feature := range featureByTag {
		features = append(features, feature)
	}
	sort.Slice(features, func(i, j int) bool { return features[i].Tag < features[j].Tag })

	releaseLevels := []int{0}
	for release := range releaseSet {
		releaseLevels = append(releaseLevels, release)
	}
	sort.Ints(releaseLevels)

	compilers := []string{""}
	if hasCompiler {
		compilers = []string{"gc", "gccgo"}
	}
	for _, target := range knownTargetValues() {
		featureAssignments, exact := architectureFeatureAssignments(target.GOARCH, features)
		if !exact {
			return true
		}
		cgoValues := []bool{false}
		if hasCgo && cgoSupported(target) {
			cgoValues = []bool{false, true}
		}
		for _, featureValues := range featureAssignments {
			for _, compiler := range compilers {
				for _, cgoEnabled := range cgoValues {
					for _, releaseLevel := range releaseLevels {
						fixed := func(tag string) (bool, bool) {
							switch {
							case isKnownGOOS(tag):
								return osTagEnabled(target.GOOS, tag), true
							case tag == "unix":
								return isUnixGOOS(target.GOOS), true
							case isKnownGOARCH(tag):
								return target.GOARCH == tag, true
							case tag == "gc" || tag == "gccgo":
								return compiler == tag, true
							case tag == "cgo":
								return cgoEnabled, true
							}
							if release, ok := parseGoReleaseTag(tag); ok {
								return releaseLevel >= release, true
							}
							if _, ok := featureByTag[tag]; ok {
								return featureValues[tag], true
							}
							return false, false
						}
						if constraintSatisfiable(expr, fixed) {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

func canonicalCustomBuildTag(tag string) string {
	if tag == "boringcrypto" {
		return "goexperiment.boringcrypto"
	}
	return tag
}

func cgoSupported(target goTarget) bool {
	switch target {
	case goTarget{GOOS: "js", GOARCH: "wasm"},
		goTarget{GOOS: "linux", GOARCH: "ppc64"},
		goTarget{GOOS: "openbsd", GOARCH: "ppc64"},
		goTarget{GOOS: "plan9", GOARCH: "386"},
		goTarget{GOOS: "plan9", GOARCH: "amd64"},
		goTarget{GOOS: "plan9", GOARCH: "arm"},
		goTarget{GOOS: "wasip1", GOARCH: "wasm"}:
		return false
	default:
		return true
	}
}

func parseGoReleaseTag(tag string) (int, bool) {
	if !strings.HasPrefix(tag, "go1.") {
		return 0, false
	}
	minorText := strings.TrimPrefix(tag, "go1.")
	if minorText == "" {
		return 0, false
	}
	for _, r := range minorText {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	minor, err := strconv.Atoi(minorText)
	if err != nil || minor <= 0 {
		return 0, false
	}
	return minor, true
}

func osTagEnabled(goos, tag string) bool {
	if goos == tag {
		return true
	}
	switch goos {
	case "android":
		return tag == "linux"
	case "ios":
		return tag == "darwin"
	case "illumos":
		return tag == "solaris"
	default:
		return false
	}
}

func isKnownGOOS(value string) bool {
	switch value {
	case "aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios", "js", "linux", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows":
		return true
	default:
		return false
	}
}

func isUnixGOOS(value string) bool {
	switch value {
	case "aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios", "linux", "netbsd", "openbsd", "solaris":
		return true
	default:
		return false
	}
}

func isKnownGOARCH(value string) bool {
	switch value {
	case "386", "amd64", "arm", "arm64", "loong64", "mips", "mipsle", "mips64", "mips64le", "ppc64", "ppc64le", "riscv64", "s390x", "sparc64", "wasm":
		return true
	default:
		return false
	}
}

func knownTargetValues() []goTarget {
	return []goTarget{
		{GOOS: "aix", GOARCH: "ppc64"},
		{GOOS: "android", GOARCH: "386"}, {GOOS: "android", GOARCH: "amd64"}, {GOOS: "android", GOARCH: "arm"}, {GOOS: "android", GOARCH: "arm64"},
		{GOOS: "darwin", GOARCH: "amd64"}, {GOOS: "darwin", GOARCH: "arm64"},
		{GOOS: "dragonfly", GOARCH: "amd64"},
		{GOOS: "freebsd", GOARCH: "386"}, {GOOS: "freebsd", GOARCH: "amd64"}, {GOOS: "freebsd", GOARCH: "arm"}, {GOOS: "freebsd", GOARCH: "arm64"}, {GOOS: "freebsd", GOARCH: "riscv64"},
		{GOOS: "illumos", GOARCH: "amd64"},
		{GOOS: "ios", GOARCH: "amd64"}, {GOOS: "ios", GOARCH: "arm64"},
		{GOOS: "js", GOARCH: "wasm"},
		{GOOS: "linux", GOARCH: "386"}, {GOOS: "linux", GOARCH: "amd64"}, {GOOS: "linux", GOARCH: "arm"}, {GOOS: "linux", GOARCH: "arm64"}, {GOOS: "linux", GOARCH: "loong64"}, {GOOS: "linux", GOARCH: "mips"}, {GOOS: "linux", GOARCH: "mips64"}, {GOOS: "linux", GOARCH: "mips64le"}, {GOOS: "linux", GOARCH: "mipsle"}, {GOOS: "linux", GOARCH: "ppc64"}, {GOOS: "linux", GOARCH: "ppc64le"}, {GOOS: "linux", GOARCH: "riscv64"}, {GOOS: "linux", GOARCH: "s390x"}, {GOOS: "linux", GOARCH: "sparc64"},
		{GOOS: "netbsd", GOARCH: "386"}, {GOOS: "netbsd", GOARCH: "amd64"}, {GOOS: "netbsd", GOARCH: "arm"}, {GOOS: "netbsd", GOARCH: "arm64"},
		{GOOS: "openbsd", GOARCH: "386"}, {GOOS: "openbsd", GOARCH: "amd64"}, {GOOS: "openbsd", GOARCH: "arm"}, {GOOS: "openbsd", GOARCH: "arm64"}, {GOOS: "openbsd", GOARCH: "mips64"}, {GOOS: "openbsd", GOARCH: "ppc64"}, {GOOS: "openbsd", GOARCH: "riscv64"},
		{GOOS: "plan9", GOARCH: "386"}, {GOOS: "plan9", GOARCH: "amd64"}, {GOOS: "plan9", GOARCH: "arm"},
		{GOOS: "solaris", GOARCH: "amd64"},
		{GOOS: "wasip1", GOARCH: "wasm"},
		{GOOS: "windows", GOARCH: "386"}, {GOOS: "windows", GOARCH: "amd64"}, {GOOS: "windows", GOARCH: "arm64"},
	}
}
