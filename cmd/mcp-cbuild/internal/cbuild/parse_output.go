package cbuild

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Output parsers for the run-based tools. parseDiagnostics (the compiler/linker/
// CMake parser) lives in diagnostics.go; this file owns the ctest and vcpkg
// text-output parsers plus the post-configure CMake cache summary. Each is
// best-effort and order-preserving: unrecognized lines are dropped, but every
// tool also returns a bounded raw_tail so nothing is silently lost.

var (
	// ctest per-test line, e.g.
	//   1/2 Test #1: hello_pass ..............   Passed    0.01 sec
	//   2/2 Test #2: hello_fail ..............***Failed    0.01 sec
	// Group 1 = name, group 2 = status blob (may carry a ``***`` prefix),
	// group 3 = seconds.
	ctestLineRe = regexp.MustCompile(`Test\s+#\d+:\s+(.+?)\s+\.{2,}\s*(.+?)\s+([0-9]+(?:\.[0-9]+)?)\s+sec`)

	// vcpkg list row: name:triplet   version   {description}
	vcpkgListRe = regexp.MustCompile(`^([A-Za-z0-9][\w.+-]*):([A-Za-z0-9_-]+)\s+(\S+)`)

	// vcpkg install-plan row: [ * ] name[features]:triplet -> version
	vcpkgPlanRe = regexp.MustCompile(`^\s*[*+]?\s*([A-Za-z0-9][\w.+-]*)(?:\[[^\]]*\])?:([A-Za-z0-9_-]+)\s*->\s*(\S+)`)

	// vcpkg search row: name  [version]  description (columns are space-padded).
	vcpkgSearchRe = regexp.MustCompile(`^(\S+)(?:\s+(\S+))?\s{2,}(.*)$`)

	// vcpkgPortNameRe validates that the first column of a search row is a real
	// port name (optionally with a [feature] suffix). It rejects status/error
	// lines like "error:  failed to fetch" whose first token ("error:") would
	// otherwise be fabricated into a package result.
	vcpkgPortNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*(?:\[[^\]]*\])?$`)
)

// parseCTest extracts per-test outcomes from ctest console output. The returned
// slice is always non-nil (empty when nothing matched) so it serializes to a
// JSON array.
func parseCTest(raw string) []testCase {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	tests := []testCase{}
	for _, line := range strings.Split(raw, "\n") {
		m := ctestLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		tests = append(tests, testCase{
			Name:   strings.TrimSpace(m[1]),
			Status: normalizeCTestStatus(m[2]),
			WallMs: secToMillis(m[3]),
		})
	}
	return tests
}

// normalizeCTestStatus maps a ctest status blob (“   Passed“, “***Failed“,
// “***Not Run“, “***Timeout“, “***Skipped“, “***Exception: ...“) to a
// stable lower_snake token.
func normalizeCTestStatus(s string) string {
	s = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "*"))
	l := strings.ToLower(s)
	switch {
	case strings.HasPrefix(l, "passed"):
		return "passed"
	case strings.HasPrefix(l, "not run"):
		return "not_run"
	case strings.HasPrefix(l, "timeout"):
		return "timeout"
	case strings.HasPrefix(l, "skipped"):
		return "skipped"
	case strings.HasPrefix(l, "exception"):
		return "exception"
	case strings.HasPrefix(l, "failed"):
		return "failed"
	default:
		// Anything else (e.g. "Child aborted") is a failure the caller counts.
		return strings.ReplaceAll(l, " ", "_")
	}
}

func secToMillis(s string) int64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f * 1000)
}

// parseVcpkgList parses `vcpkg list` output into installed package specs. Always
// non-nil.
func parseVcpkgList(raw string) []installedPackage {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	pkgs := []installedPackage{}
	for _, line := range strings.Split(raw, "\n") {
		m := vcpkgListRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pkgs = append(pkgs, installedPackage{Name: m[1], Triplet: m[2], Version: m[3]})
	}
	return pkgs
}

// parseVcpkgInstalled best-effort extracts the installed package specs from
// `vcpkg install` output by scanning its "name[features]:triplet -> version"
// plan lines. Duplicates (a spec that appears in more than one section) are
// collapsed. Always non-nil.
func parseVcpkgInstalled(raw string) []installedPackage {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	pkgs := []installedPackage{}
	seen := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		m := vcpkgPlanRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := m[1] + ":" + m[2]
		if seen[key] {
			continue
		}
		seen[key] = true
		pkgs = append(pkgs, installedPackage{Name: m[1], Triplet: m[2], Version: m[3]})
	}
	return pkgs
}

// parseVcpkgSearch parses `vcpkg search <query>` output into catalog rows.
// Always non-nil.
func parseVcpkgSearch(raw string) []searchPackage {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	out := []searchPackage{}
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		m := vcpkgSearchRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// Skip rows whose first column is not a real port name (e.g. an
		// "error: ..." status line) so garbage output never fabricates a package.
		if !vcpkgPortNameRe.MatchString(m[1]) {
			continue
		}
		out = append(out, searchPackage{
			Name:        m[1],
			Version:     m[2],
			Description: strings.TrimSpace(m[3]),
		})
	}
	return out
}

// cacheSummaryKeys is the curated set of CMakeCache.txt entries surfaced after a
// successful configure.
var cacheSummaryKeys = map[string]bool{
	"CMAKE_GENERATOR":      true,
	"CMAKE_BUILD_TYPE":     true,
	"CMAKE_CXX_COMPILER":   true,
	"CMAKE_C_COMPILER":     true,
	"CMAKE_CXX_STANDARD":   true,
	"CMAKE_TOOLCHAIN_FILE": true,
	"CMAKE_PROJECT_NAME":   true,
}

// readCacheSummary reads the CMakeCache.txt in the preset's resolved binaryDir
// and returns a small map of interesting entries. It is best-effort: any
// resolution or read failure yields nil, never an error, so it can never break
// a successful configure result.
func readCacheSummary(sourceDir, preset string) map[string]string {
	p, err := LoadPresets(sourceDir)
	if err != nil {
		return nil
	}
	binaryDir, err := p.ResolvedBinaryDir(preset)
	if err != nil {
		return nil
	}
	expanded, err := expandPresetMacros(binaryDir, sourceDir, preset)
	if err != nil {
		return nil
	}
	if strings.Contains(expanded, "${") || strings.Contains(expanded, "$env{") || strings.Contains(expanded, "$penv{") {
		return nil
	}
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(sourceDir, expanded)
	}
	data, err := os.ReadFile(filepath.Join(expanded, "CMakeCache.txt"))
	if err != nil {
		return nil
	}
	summary := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		left, val := line[:eq], line[eq+1:]
		colon := strings.IndexByte(left, ':')
		if colon < 0 {
			continue
		}
		if key := left[:colon]; cacheSummaryKeys[key] {
			summary[key] = val
		}
	}
	if len(summary) == 0 {
		return nil
	}
	return summary
}
