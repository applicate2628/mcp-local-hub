package cbuild

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

func trimSpaceBytes(b []byte) []byte {
	return bytes.TrimSpace(b)
}

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

type presetInfo struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
}

type presetsResult struct {
	ConfigurePresets []presetInfo `json:"configurePresets"`
	BuildPresets     []presetInfo `json:"buildPresets"`
	TestPresets      []presetInfo `json:"testPresets"`
	WorkflowPresets  []presetInfo `json:"workflowPresets"`
}

var cmakePresetLineRe = regexp.MustCompile(`^"([^"]+)"(?:\s+-\s+(.+))?$`)

// parseCMakePresetListing accepts only the names and optional display labels that
// CMake printed. CMake remains the owner of every preset-file semantic.
func parseCMakePresetListing(kind, raw string) ([]presetInfo, error) {
	presets := []presetInfo{}
	seenOutput := false
	seenHeader := false
	header := "Available " + kind + " presets:"
	for _, rawLine := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		seenOutput = true
		if !seenHeader {
			if line != header {
				return nil, fmt.Errorf("unrecognized CMake output for %s presets: %q", kind, line)
			}
			seenHeader = true
			continue
		}
		match := cmakePresetLineRe.FindStringSubmatch(line)
		if match == nil {
			return nil, fmt.Errorf("unrecognized CMake output for %s presets: %q", kind, line)
		}
		preset := presetInfo{Name: match[1]}
		if len(match) > 2 {
			preset.DisplayName = match[2]
		}
		presets = append(presets, preset)
	}
	if seenOutput && !seenHeader {
		return nil, fmt.Errorf("unrecognized CMake output for %s presets", kind)
	}
	return presets, nil
}

func parseCMakeCache(data []byte) map[string]string {
	cache := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		colon := strings.IndexByte(line, ':')
		if eq < 0 || colon < 1 || colon > eq {
			continue
		}
		cache[line[:colon]] = line[eq+1:]
	}
	return cache
}

func cacheSummary(cache map[string]string) map[string]string {
	summary := map[string]string{}
	for key, value := range cache {
		if cacheSummaryKeys[key] {
			summary[key] = value
		}
	}
	if len(summary) == 0 {
		return nil
	}
	return summary
}

func parseConfiguredBuildDir(raw string) (string, error) {
	const prefix = "Build files have been written to:"
	var found string
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "-- ")
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		dir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if dir == "" {
			return "", fmt.Errorf("CMake completion line has no build directory")
		}
		if found != "" && found != dir {
			return "", fmt.Errorf("multiple CMake completion directories")
		}
		found = dir
	}
	if found == "" {
		return "", fmt.Errorf("CMake completion line not found")
	}
	return found, nil
}

func canonicalExistingDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %q", path)
	}
	return filepath.Clean(real), nil
}

func sameCanonicalPath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func validateConfiguredCache(sourceDir, candidate string) (string, map[string]string, error) {
	cacheDir, err := canonicalExistingDir(candidate)
	if err != nil {
		return "", nil, fmt.Errorf("CMake cache directory: %w", err)
	}
	cachePath := filepath.Join(cacheDir, "CMakeCache.txt")
	info, err := os.Lstat(cachePath)
	if err != nil {
		return "", nil, fmt.Errorf("CMake cache: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("CMake cache is not an ordinary file")
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return "", nil, fmt.Errorf("read CMake cache: %w", err)
	}
	cache := parseCMakeCache(data)
	recordedDir, ok := cache["CMAKE_CACHEFILE_DIR"]
	if !ok || recordedDir == "" {
		return "", nil, fmt.Errorf("CMake cache identity mismatch: CMAKE_CACHEFILE_DIR missing")
	}
	recordedDir, err = canonicalExistingDir(recordedDir)
	if err != nil || !sameCanonicalPath(recordedDir, cacheDir) {
		return "", nil, fmt.Errorf("CMake cache identity mismatch: CMAKE_CACHEFILE_DIR")
	}
	source, err := canonicalExistingDir(sourceDir)
	if err != nil {
		return "", nil, fmt.Errorf("CMake source directory: %w", err)
	}
	recordedSource, ok := cache["CMAKE_HOME_DIRECTORY"]
	if !ok || recordedSource == "" {
		return "", nil, fmt.Errorf("CMake cache identity mismatch: CMAKE_HOME_DIRECTORY missing")
	}
	recordedSource, err = canonicalExistingDir(recordedSource)
	if err != nil || !sameCanonicalPath(recordedSource, source) {
		return "", nil, fmt.Errorf("CMake cache identity mismatch: CMAKE_HOME_DIRECTORY")
	}
	return cacheDir, cache, nil
}

func (b *builder) settleConfiguredCache(sourceDir, preset, output string) (map[string]string, error) {
	source, err := canonicalExistingDir(sourceDir)
	if err != nil {
		return nil, err
	}
	key := configuredCacheKey{sourceDir: source, preset: preset}
	b.configuredMu.Lock()
	delete(b.configured, key)
	b.configuredMu.Unlock()
	buildDir, err := parseConfiguredBuildDir(output)
	if err != nil {
		return nil, err
	}
	cacheDir, cache, err := validateConfiguredCache(sourceDir, buildDir)
	if err != nil {
		return nil, err
	}
	b.configuredMu.Lock()
	b.configured[key] = cacheDir
	b.configuredMu.Unlock()
	return cacheSummary(cache), nil
}

func findConfiguredCache(ctx context.Context, sourceDir string) (string, error) {
	var candidates []string
	err := filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "CMakeCache.txt" {
			return nil
		}
		cacheDir, _, err := validateConfiguredCache(sourceDir, filepath.Dir(path))
		if err == nil {
			candidates = append(candidates, cacheDir)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no CMake-configured build tree for source/preset")
	}
	if len(candidates) > 1 {
		return "", fmt.Errorf("multiple CMake-configured build trees; configure the requested preset first")
	}
	return candidates[0], nil
}

func (b *builder) configuredCacheDir(ctx context.Context, sourceDir, preset string) (string, error) {
	source, err := canonicalExistingDir(sourceDir)
	if err != nil {
		return "", err
	}
	key := configuredCacheKey{sourceDir: source, preset: preset}
	b.configuredMu.Lock()
	cacheDir := b.configured[key]
	b.configuredMu.Unlock()
	if cacheDir == "" {
		cacheDir, err = findConfiguredCache(ctx, source)
		if err != nil {
			return "", err
		}
		b.configuredMu.Lock()
		b.configured[key] = cacheDir
		b.configuredMu.Unlock()
	}
	validated, _, err := validateConfiguredCache(source, cacheDir)
	if err != nil {
		return "", err
	}
	return validated, nil
}
