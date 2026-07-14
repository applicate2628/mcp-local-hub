package cbuild

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/cmd/mcp-cbuild/internal/mcp"
)

// --- tool argument shapes ----------------------------------------------------

type listPresetsArgs struct {
	WorkingDir string `json:"working_dir"`
}

type configureArgs struct {
	Preset     string            `json:"preset"`
	WorkingDir string            `json:"working_dir"`
	Fresh      bool              `json:"fresh"`
	Defines    map[string]string `json:"defines"`
	TimeoutSec int               `json:"timeout_sec"`
}

type buildArgs struct {
	Preset     string   `json:"preset"`
	WorkingDir string   `json:"working_dir"`
	Targets    []string `json:"targets"`
	Jobs       int      `json:"jobs"`
	Verbose    bool     `json:"verbose"`
	Config     string   `json:"config"`
	TimeoutSec int      `json:"timeout_sec"`
}

type testArgs struct {
	Preset      string `json:"preset"`
	WorkingDir  string `json:"working_dir"`
	Regex       string `json:"regex"`
	Jobs        int    `json:"jobs"`
	OutputJUnit string `json:"output_junit"`
	TimeoutSec  int    `json:"timeout_sec"`
}

type workflowArgs struct {
	Preset     string `json:"preset"`
	WorkingDir string `json:"working_dir"`
	TimeoutSec int    `json:"timeout_sec"`
}

type cleanArgs struct {
	WorkingDir    string `json:"working_dir"`
	Preset        string `json:"preset"`
	PurgeBuildDir bool   `json:"purge_build_dir"`
	TimeoutSec    int    `json:"timeout_sec"`
}

type vcpkgInstallArgs struct {
	WorkingDir string `json:"working_dir"`
	CleanAfter bool   `json:"clean_after"`
	Triplet    string `json:"triplet"`
	TimeoutSec int    `json:"timeout_sec"`
}

type vcpkgListArgs struct {
	WorkingDir string `json:"working_dir"`
	TimeoutSec int    `json:"timeout_sec"`
}

type vcpkgManifestArgs struct {
	WorkingDir string `json:"working_dir"`
}

type vcpkgSearchArgs struct {
	Query      string `json:"query"`
	WorkingDir string `json:"working_dir"`
	TimeoutSec int    `json:"timeout_sec"`
}

// --- tool result shapes ------------------------------------------------------

type configureResult struct {
	Success      bool              `json:"success"`
	ExitCode     int               `json:"exit_code"`
	Diagnostics  []Diagnostic      `json:"diagnostics"`
	CacheSummary map[string]string `json:"cache_summary,omitempty"`
	WallMs       int64             `json:"wall_ms"`
	TimedOut     bool              `json:"timed_out,omitempty"`
	RawTail      string            `json:"raw_tail,omitempty"`
}

type buildResult struct {
	Success      bool         `json:"success"`
	ExitCode     int          `json:"exit_code"`
	Diagnostics  []Diagnostic `json:"diagnostics"`
	BuiltTargets []string     `json:"built_targets,omitempty"`
	WallMs       int64        `json:"wall_ms"`
	TimedOut     bool         `json:"timed_out,omitempty"`
	RawTail      string       `json:"raw_tail,omitempty"`
}

// testCase is one ctest test outcome.
type testCase struct {
	Name       string `json:"name"`
	Status     string `json:"status"` // passed | failed | not_run | timeout | skipped | exception | ...
	WallMs     int64  `json:"wall_ms"`
	OutputTail string `json:"output_tail,omitempty"`
}

type testResult struct {
	Success   bool       `json:"success"`
	ExitCode  int        `json:"exit_code"`
	Total     int        `json:"total"`
	Passed    int        `json:"passed"`
	Failed    int        `json:"failed"`
	Tests     []testCase `json:"tests"`
	JUnitPath string     `json:"junit_path,omitempty"`
	WallMs    int64      `json:"wall_ms"`
	TimedOut  bool       `json:"timed_out,omitempty"`
	RawTail   string     `json:"raw_tail,omitempty"`
}

type workflowResult struct {
	Success     bool         `json:"success"`
	ExitCode    int          `json:"exit_code"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	WallMs      int64        `json:"wall_ms"`
	TimedOut    bool         `json:"timed_out,omitempty"`
	RawTail     string       `json:"raw_tail,omitempty"`
}

type cleanResult struct {
	Success     bool         `json:"success"`
	Purged      bool         `json:"purged"`
	RemovedDir  string       `json:"removed_dir,omitempty"`
	ExitCode    int          `json:"exit_code,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	WallMs      int64        `json:"wall_ms,omitempty"`
	TimedOut    bool         `json:"timed_out,omitempty"`
	RawTail     string       `json:"raw_tail,omitempty"`
}

// installedPackage is one vcpkg package spec (name:triplet@version).
type installedPackage struct {
	Name    string `json:"name"`
	Triplet string `json:"triplet,omitempty"`
	Version string `json:"version,omitempty"`
}

type vcpkgInstallResult struct {
	Success     bool               `json:"success"`
	ExitCode    int                `json:"exit_code"`
	Installed   []installedPackage `json:"installed"`
	Diagnostics []Diagnostic       `json:"diagnostics"`
	WallMs      int64              `json:"wall_ms"`
	TimedOut    bool               `json:"timed_out,omitempty"`
	RawTail     string             `json:"raw_tail,omitempty"`
}

type vcpkgListResult struct {
	Success  bool               `json:"success"`
	ExitCode int                `json:"exit_code"`
	Packages []installedPackage `json:"packages"`
	RawTail  string             `json:"raw_tail,omitempty"`
}

// searchPackage is one vcpkg_search catalog row.
type searchPackage struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

type vcpkgSearchResult struct {
	Success  bool            `json:"success"`
	ExitCode int             `json:"exit_code"`
	Query    string          `json:"query"`
	Packages []searchPackage `json:"packages"`
	RawTail  string          `json:"raw_tail,omitempty"`
}

// --- shared handler helpers --------------------------------------------------

// execOK reports whether a command ran to a clean zero exit (no timeout,
// cancellation, or launch failure).
func execOK(r execResult) bool {
	return r.startErr == nil && !r.TimedOut && !r.Canceled && r.ExitCode == 0
}

// runTool runs bin+args in dir with the given timeout. It returns a Go error
// ONLY when the process could not be launched at all (a genuine environment
// failure surfaced to the client as an isError result); a clean non-zero exit,
// a timeout, or a cancellation is returned as a normal execResult so the caller
// can build a structured success:false payload with diagnostics and raw output.
func (b *builder) runTool(ctx context.Context, timeout time.Duration, dir, bin string, args []string) (execResult, error) {
	res := runCommand(ctx, timeout, dir, bin, args)
	if res.startErr != nil && !res.TimedOut && !res.Canceled {
		return res, fmt.Errorf("could not launch %s: %w", filepath.Base(bin), res.startErr)
	}
	return res, nil
}

// sortedKeys returns the keys of m in deterministic (sorted) order so argv is
// reproducible across calls.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateTargets rejects empty and flag-shaped build targets. A value like
// "--clean-first" or "--" must never reach cmake as a positional target, or
// cmake would reinterpret it as a flag (argument injection). No shell is used,
// so this is the only guard against a flag-shaped target.
func validateTargets(targets []string) error {
	for _, t := range targets {
		if strings.TrimSpace(t) == "" {
			return mcp.NewParamError("targets must not contain empty entries")
		}
		if strings.HasPrefix(strings.TrimSpace(t), "-") {
			return mcp.NewParamError("invalid target %q: must not begin with '-' (it would be parsed as a flag, not a target)", t)
		}
	}
	return nil
}

// presetFlag renders the --preset argument in the =<name> equals form. A preset
// name may legitimately begin with '-' (e.g. "--help"; `cmake --list-presets`
// can list such a name), and passing it as two separate argv elements
// ("--preset", "--help") lets cmake/ctest reinterpret the value as an option —
// `cmake --preset --help` prints help and exits 0 instead of configuring. The
// equals form binds the value to the flag regardless of its leading dashes and is
// accepted by cmake configure/build/workflow and ctest alike, so it is the single
// owner of preset-flag construction across every preset-taking command.
func presetFlag(name string) string {
	return "--preset=" + name
}

// validateDefineKey rejects a -D cache key that would corrupt the argv (no
// shell is used, but an '=' or space in the key would still misparse).
func validateDefineKey(k string) error {
	if k == "" {
		return mcp.NewParamError("define key must not be empty")
	}
	if strings.ContainsAny(k, "= ") {
		return mcp.NewParamError("invalid define key %q: must not contain '=' or whitespace", k)
	}
	return nil
}

// --- CMake tools -------------------------------------------------------------

// cmakeListPresets builds the read-only cmake_list_presets tool.
func (b *builder) cmakeListPresets() mcp.Tool {
	schema := objSchema(map[string]any{"working_dir": workingDirProp()})
	return &funcTool{
		name:        "cmake_list_presets",
		title:       "List CMake presets",
		description: "Parse CMakePresets.json / CMakeUserPresets.json (resolving include + inherits) and return the configure/build/test/workflow presets. Each configure preset also carries its resolved generator, toolchain, and C/C++ compiler when derivable. Read-only; runs no build.",
		schema:      schema,
		handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a listPresetsArgs
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			dir, err := b.workingDir(a.WorkingDir)
			if err != nil {
				return nil, err
			}
			p, err := LoadPresets(dir)
			if err != nil {
				return nil, err
			}
			return p.Result(), nil
		},
	}
}

// cmakeConfigure builds the cmake_configure tool.
func (b *builder) cmakeConfigure() mcp.Tool {
	schema := objSchema(map[string]any{
		"preset":      strProp("Configure preset name (from cmake_list_presets)."),
		"working_dir": workingDirProp(),
		"fresh":       boolProp("Pass --fresh to wipe the cache and reconfigure from scratch (CMake >= 3.24)."),
		"defines":     strMapProp("Extra cache entries, each passed as a separate -D key=value argument."),
		"timeout_sec": intProp("Per-call timeout in seconds (default 300; hard-capped at 60 min)."),
	}, "preset")
	return &funcTool{
		name:        "cmake_configure",
		title:       "CMake configure",
		description: "Run `cmake --preset <preset>` to configure the build tree. Returns structured diagnostics parsed from stdout+stderr, a bounded raw_tail, and (on success) a small CMake cache summary.",
		schema:      schema,
		handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a configureArgs
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			if a.Preset == "" {
				return nil, mcp.NewParamError("preset is required")
			}
			dir, err := b.workingDir(a.WorkingDir)
			if err != nil {
				return nil, err
			}
			cmakeBin, err := resolveCMake()
			if err != nil {
				return nil, err
			}
			argv := []string{presetFlag(a.Preset)}
			if a.Fresh {
				argv = append(argv, "--fresh")
			}
			for _, k := range sortedKeys(a.Defines) {
				if err := validateDefineKey(k); err != nil {
					return nil, err
				}
				argv = append(argv, "-D", k+"="+a.Defines[k])
			}
			timeout := timeoutOrDefault(a.TimeoutSec, defaultConfigureTimeout)
			res, err := b.runTool(ctx, timeout, dir, cmakeBin, argv)
			if err != nil {
				return nil, err
			}
			result := configureResult{
				Success:     execOK(res),
				ExitCode:    res.ExitCode,
				Diagnostics: parseDiagnostics(res.Combined),
				WallMs:      res.WallMs,
				TimedOut:    res.TimedOut,
				RawTail:     rawTail(res.Combined),
			}
			if result.Success {
				result.CacheSummary = readCacheSummary(dir, a.Preset)
			}
			return result, nil
		},
	}
}

// cmakeBuild builds the cmake_build tool.
func (b *builder) cmakeBuild() mcp.Tool {
	schema := objSchema(map[string]any{
		"preset":      strProp("Build preset name."),
		"working_dir": workingDirProp(),
		"targets":     strArrayProp("Targets to build; empty builds the preset's default (all) target."),
		"jobs":        intProp("Parallel build jobs (-j N)."),
		"verbose":     boolProp("Emit verbose build commands (--verbose)."),
		"config":      strProp("Config for multi-config generators, e.g. Debug or Release (--config)."),
		"timeout_sec": intProp("Per-call timeout in seconds (default 600; hard-capped at 60 min)."),
	}, "preset")
	return &funcTool{
		name:        "cmake_build",
		title:       "CMake build",
		description: "Run `cmake --build --preset <preset>` (optionally scoped to targets, with -j / --config / --verbose). Returns structured compiler/linker diagnostics, the built targets, wall time, and a bounded raw_tail.",
		schema:      schema,
		handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a buildArgs
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			if a.Preset == "" {
				return nil, mcp.NewParamError("preset is required")
			}
			// Validate targets before any environment resolution so a flag-shaped
			// target is rejected as a param error regardless of host state.
			if err := validateTargets(a.Targets); err != nil {
				return nil, err
			}
			dir, err := b.workingDir(a.WorkingDir)
			if err != nil {
				return nil, err
			}
			cmakeBin, err := resolveCMake()
			if err != nil {
				return nil, err
			}
			argv := []string{"--build", presetFlag(a.Preset)}
			if len(a.Targets) > 0 {
				argv = append(argv, "--target")
				argv = append(argv, a.Targets...)
			}
			if a.Jobs > 0 {
				argv = append(argv, "-j", strconv.Itoa(a.Jobs))
			}
			if a.Config != "" {
				argv = append(argv, "--config", a.Config)
			}
			if a.Verbose {
				argv = append(argv, "--verbose")
			}
			timeout := timeoutOrDefault(a.TimeoutSec, defaultBuildTimeout)
			res, err := b.runTool(ctx, timeout, dir, cmakeBin, argv)
			if err != nil {
				return nil, err
			}
			result := buildResult{
				Success:     execOK(res),
				ExitCode:    res.ExitCode,
				Diagnostics: parseDiagnostics(res.Combined),
				WallMs:      res.WallMs,
				TimedOut:    res.TimedOut,
				RawTail:     rawTail(res.Combined),
			}
			if result.Success && len(a.Targets) > 0 {
				result.BuiltTargets = a.Targets
			}
			return result, nil
		},
	}
}

// cmakeTest builds the cmake_test tool.
func (b *builder) cmakeTest() mcp.Tool {
	schema := objSchema(map[string]any{
		"preset":       strProp("Test preset name."),
		"working_dir":  workingDirProp(),
		"regex":        strProp("Only run tests whose name matches this regex (ctest -R)."),
		"jobs":         intProp("Parallel test jobs (ctest -j N)."),
		"output_junit": strProp("Write a JUnit XML report to this path (relative paths resolve under working_dir)."),
		"timeout_sec":  intProp("Per-call timeout in seconds (default 1800; hard-capped at 60 min)."),
	}, "preset")
	return &funcTool{
		name:        "cmake_test",
		title:       "CMake test (ctest)",
		description: "Run `ctest --preset <preset>` (optionally filtered by regex, parallelized, JUnit-exported). Returns per-test outcomes with pass/fail counts and a bounded raw_tail.",
		schema:      schema,
		handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a testArgs
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			if a.Preset == "" {
				return nil, mcp.NewParamError("preset is required")
			}
			dir, err := b.workingDir(a.WorkingDir)
			if err != nil {
				return nil, err
			}
			ctestBin, err := resolveCTest()
			if err != nil {
				return nil, err
			}
			junitPath := a.OutputJUnit
			if junitPath != "" && !filepath.IsAbs(junitPath) {
				junitPath = filepath.Join(dir, junitPath)
			}
			argv := []string{presetFlag(a.Preset)}
			if a.Regex != "" {
				argv = append(argv, "-R", a.Regex)
			}
			if a.Jobs > 0 {
				argv = append(argv, "-j", strconv.Itoa(a.Jobs))
			}
			if junitPath != "" {
				argv = append(argv, "--output-junit", junitPath)
			}
			timeout := timeoutOrDefault(a.TimeoutSec, defaultTestTimeout)
			res, err := b.runTool(ctx, timeout, dir, ctestBin, argv)
			if err != nil {
				return nil, err
			}
			tests := parseCTest(res.Combined)
			passed, failed := 0, 0
			for _, tc := range tests {
				switch tc.Status {
				case "passed":
					passed++
				case "skipped":
					// counted in total only, neither pass nor fail
				default:
					failed++
				}
			}
			result := testResult{
				Success:  execOK(res),
				ExitCode: res.ExitCode,
				Total:    len(tests),
				Passed:   passed,
				Failed:   failed,
				Tests:    tests,
				WallMs:   res.WallMs,
				TimedOut: res.TimedOut,
				RawTail:  rawTail(res.Combined),
			}
			if a.OutputJUnit != "" {
				result.JUnitPath = junitPath
			}
			return result, nil
		},
	}
}

// cmakeWorkflow builds the cmake_workflow tool.
func (b *builder) cmakeWorkflow() mcp.Tool {
	schema := objSchema(map[string]any{
		"preset":      strProp("Workflow preset name (chains configure+build+test steps)."),
		"working_dir": workingDirProp(),
		"timeout_sec": intProp("Per-call timeout in seconds (default 2400; hard-capped at 60 min)."),
	}, "preset")
	return &funcTool{
		name:        "cmake_workflow",
		title:       "CMake workflow",
		description: "Run `cmake --workflow --preset <preset>`, executing the preset's configure/build/test steps in order. Returns combined structured diagnostics and a bounded raw_tail.",
		schema:      schema,
		handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a workflowArgs
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			if a.Preset == "" {
				return nil, mcp.NewParamError("preset is required")
			}
			dir, err := b.workingDir(a.WorkingDir)
			if err != nil {
				return nil, err
			}
			cmakeBin, err := resolveCMake()
			if err != nil {
				return nil, err
			}
			argv := []string{"--workflow", presetFlag(a.Preset)}
			timeout := timeoutOrDefault(a.TimeoutSec, defaultWorkflowTimeout)
			res, err := b.runTool(ctx, timeout, dir, cmakeBin, argv)
			if err != nil {
				return nil, err
			}
			return workflowResult{
				Success:     execOK(res),
				ExitCode:    res.ExitCode,
				Diagnostics: parseDiagnostics(res.Combined),
				WallMs:      res.WallMs,
				TimedOut:    res.TimedOut,
				RawTail:     rawTail(res.Combined),
			}, nil
		},
	}
}

// cmakeClean builds the cmake_clean tool.
func (b *builder) cmakeClean() mcp.Tool {
	schema := objSchema(map[string]any{
		"working_dir":     workingDirProp(),
		"preset":          strProp("Configure preset whose build tree to clean (required — resolves the build directory)."),
		"purge_build_dir": boolProp("Instead of `--target clean`, delete the entire resolved build directory. Refused unless the directory is strictly inside the project (path-escape guard)."),
		"timeout_sec":     intProp("Per-call timeout in seconds (default 300; hard-capped at 60 min)."),
	}, "preset")
	return &funcTool{
		name:        "cmake_clean",
		title:       "CMake clean",
		description: "Clean a build tree. `preset` is a CONFIGURE preset; it resolves the build directory. By default runs `cmake --build <resolved build dir> --target clean` (so a build preset named differently from the configure preset is never required). With purge_build_dir it removes the whole resolved build directory, refusing any path not strictly inside the project directory (never RemoveAll on an arbitrary path).",
		schema:      schema,
		handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a cleanArgs
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			if a.Preset == "" {
				return nil, mcp.NewParamError("preset is required (it resolves the build directory to clean)")
			}
			dir, err := b.workingDir(a.WorkingDir)
			if err != nil {
				return nil, err
			}

			// Both clean modes key off the CONFIGURE preset, which resolves the
			// build DIRECTORY. `preset` is never a build preset here.
			p, err := LoadPresets(dir)
			if err != nil {
				return nil, err
			}
			binaryDir, err := p.ResolvedBinaryDir(a.Preset)
			if err != nil {
				return nil, err
			}

			if a.PurgeBuildDir {
				// The path-escape guard is the single owner of purge safety: it
				// refuses unresolved/unknown-namespace macros, the source dir
				// itself, and anything outside the source tree before RemoveAll.
				safe, err := resolveBuildDirWithinSource(binaryDir, dir, a.Preset)
				if err != nil {
					return nil, err
				}
				if err := os.RemoveAll(safe); err != nil {
					return nil, fmt.Errorf("purge build dir %q: %w", safe, err)
				}
				return cleanResult{Success: true, Purged: true, RemovedDir: safe}, nil
			}

			// Non-purge: run `cmake --build <build-dir> --target clean`. Building
			// by the resolved build DIRECTORY (not `--build --preset <name>`)
			// avoids the build-vs-configure preset mismatch: a configure preset
			// name sent to `cmake --build --preset` fails "No such build preset"
			// whenever the build preset is named differently (e.g. configure
			// "dev" vs build "dev-build"). This also cleans a legitimate
			// out-of-source build tree, which the configure preset already names.
			buildDir, _, err := expandBinaryDirToAbs(binaryDir, dir, a.Preset)
			if err != nil {
				return nil, err
			}
			cmakeBin, err := resolveCMake()
			if err != nil {
				return nil, err
			}
			argv := []string{"--build", buildDir, "--target", "clean"}
			timeout := timeoutOrDefault(a.TimeoutSec, defaultCleanTimeout)
			res, err := b.runTool(ctx, timeout, dir, cmakeBin, argv)
			if err != nil {
				return nil, err
			}
			return cleanResult{
				Success:     execOK(res),
				Purged:      false,
				ExitCode:    res.ExitCode,
				Diagnostics: parseDiagnostics(res.Combined),
				WallMs:      res.WallMs,
				TimedOut:    res.TimedOut,
				RawTail:     rawTail(res.Combined),
			}, nil
		},
	}
}

// --- vcpkg tools -------------------------------------------------------------

// vcpkgInstall builds the vcpkg_install tool.
func (b *builder) vcpkgInstall() mcp.Tool {
	schema := objSchema(map[string]any{
		"working_dir": workingDirProp(),
		"clean_after": boolProp("Pass --clean-after-build to free per-package build trees after install."),
		"triplet":     strProp("Target triplet, e.g. x64-windows (vcpkg --triplet)."),
		"timeout_sec": intProp("Per-call timeout in seconds (default 1800; hard-capped at 60 min)."),
	})
	return &funcTool{
		name:        "vcpkg_install",
		title:       "vcpkg install (manifest mode)",
		description: "Run manifest-mode `vcpkg install` in the directory holding vcpkg.json to install the declared dependencies. Requires VCPKG_ROOT or a vcpkg on PATH; fails closed with a clear message when absent. Returns the best-effort installed package list plus structured diagnostics.",
		schema:      schema,
		handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a vcpkgInstallArgs
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			dir, err := b.workingDir(a.WorkingDir)
			if err != nil {
				return nil, err
			}
			if !fileExists(filepath.Join(dir, "vcpkg.json")) {
				return nil, fmt.Errorf("no vcpkg.json in %s: vcpkg_install runs in manifest mode and needs a vcpkg.json", dir)
			}
			vcpkgBin, err := resolveVcpkg()
			if err != nil {
				return nil, err
			}
			argv := []string{"install"}
			if a.Triplet != "" {
				argv = append(argv, "--triplet", a.Triplet)
			}
			if a.CleanAfter {
				argv = append(argv, "--clean-after-build")
			}
			timeout := timeoutOrDefault(a.TimeoutSec, defaultVcpkgTimeout)
			res, err := b.runTool(ctx, timeout, dir, vcpkgBin, argv)
			if err != nil {
				return nil, err
			}
			return vcpkgInstallResult{
				Success:     execOK(res),
				ExitCode:    res.ExitCode,
				Installed:   parseVcpkgInstalled(res.Combined),
				Diagnostics: parseDiagnostics(res.Combined),
				WallMs:      res.WallMs,
				TimedOut:    res.TimedOut,
				RawTail:     rawTail(res.Combined),
			}, nil
		},
	}
}

// vcpkgList builds the vcpkg_list tool.
func (b *builder) vcpkgList() mcp.Tool {
	schema := objSchema(map[string]any{
		"working_dir": workingDirProp(),
		"timeout_sec": intProp("Per-call timeout in seconds (default 120; hard-capped at 60 min)."),
	})
	return &funcTool{
		name:        "vcpkg_list",
		title:       "vcpkg list",
		description: "Run `vcpkg list` and return the installed packages as {name, triplet, version}. Requires VCPKG_ROOT or a vcpkg on PATH; fails closed when absent.",
		schema:      schema,
		handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a vcpkgListArgs
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			dir, err := b.workingDir(a.WorkingDir)
			if err != nil {
				return nil, err
			}
			vcpkgBin, err := resolveVcpkg()
			if err != nil {
				return nil, err
			}
			timeout := timeoutOrDefault(a.TimeoutSec, defaultQueryTimeout)
			res, err := b.runTool(ctx, timeout, dir, vcpkgBin, []string{"list"})
			if err != nil {
				return nil, err
			}
			return vcpkgListResult{
				Success:  execOK(res),
				ExitCode: res.ExitCode,
				Packages: parseVcpkgList(res.Combined),
				RawTail:  rawTail(res.Combined),
			}, nil
		},
	}
}

// vcpkgManifest builds the read-only vcpkg_manifest tool.
func (b *builder) vcpkgManifest() mcp.Tool {
	schema := objSchema(map[string]any{"working_dir": workingDirProp()})
	return &funcTool{
		name:        "vcpkg_manifest",
		title:       "vcpkg manifest summary",
		description: "Parse and summarize vcpkg.json (dependencies, features, builtin-baseline, overrides). Read-only; runs no vcpkg command.",
		schema:      schema,
		handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a vcpkgManifestArgs
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			dir, err := b.workingDir(a.WorkingDir)
			if err != nil {
				return nil, err
			}
			m, err := LoadManifest(dir)
			if err != nil {
				return nil, err
			}
			return m, nil
		},
	}
}

// vcpkgSearch builds the vcpkg_search tool.
func (b *builder) vcpkgSearch() mcp.Tool {
	schema := objSchema(map[string]any{
		"query":       strProp("Package name or substring to search for."),
		"working_dir": workingDirProp(),
		"timeout_sec": intProp("Per-call timeout in seconds (default 120; hard-capped at 60 min)."),
	}, "query")
	return &funcTool{
		name:        "vcpkg_search",
		title:       "vcpkg search",
		description: "Run `vcpkg search <query>` and return matching catalog packages as {name, version, description}. Requires VCPKG_ROOT or a vcpkg on PATH; fails closed when absent.",
		schema:      schema,
		handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var a vcpkgSearchArgs
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			if strings.TrimSpace(a.Query) == "" {
				return nil, mcp.NewParamError("query is required")
			}
			// Reject a flag-shaped query so it cannot be reinterpreted by vcpkg as
			// a flag (consistent with the cmake_build target guard).
			if strings.HasPrefix(strings.TrimSpace(a.Query), "-") {
				return nil, mcp.NewParamError("invalid query %q: must not begin with '-'", a.Query)
			}
			dir, err := b.workingDir(a.WorkingDir)
			if err != nil {
				return nil, err
			}
			vcpkgBin, err := resolveVcpkg()
			if err != nil {
				return nil, err
			}
			timeout := timeoutOrDefault(a.TimeoutSec, defaultQueryTimeout)
			res, err := b.runTool(ctx, timeout, dir, vcpkgBin, []string{"search", a.Query})
			if err != nil {
				return nil, err
			}
			return vcpkgSearchResult{
				Success:  execOK(res),
				ExitCode: res.ExitCode,
				Query:    a.Query,
				Packages: parseVcpkgSearch(res.Combined),
				RawTail:  rawTail(res.Combined),
			}, nil
		},
	}
}
