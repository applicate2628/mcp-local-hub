package cbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/cmd/mcp-cbuild/internal/mcp"
)

// TestMain doubles as a fake cmake/ctest for TestPresetArgvWiring: when the
// MCP_CBUILD_FAKE_ARGV marker is set (only by that test, then inherited by the
// child it execs), the re-invoked test binary echoes its argv and exits 0 BEFORE
// any test flag parsing, so the argv the handlers built is captured verbatim.
// This lets the wiring test run cross-platform (Windows included), where Go's
// exec cannot run a shell-script fake directly.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_CBUILD_FAKE_ARGV") == "1" {
		for _, a := range os.Args[1:] {
			fmt.Println(a)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func toolNamed(t *testing.T, name string) mcp.Tool {
	t.Helper()
	for _, tl := range Tools("") {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("tool %q not registered", name)
	return nil
}

func callWithArgs(t *testing.T, tl mcp.Tool, args map[string]any) (any, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tl.Call(context.Background(), b)
}

// TestCmakeBuildRejectsFlagTarget proves a flag-shaped build target is rejected
// as a param error BEFORE any cmake resolution (so it holds on cmake-less
// hosts), preventing argument injection into the cmake command line.
func TestCmakeBuildRejectsFlagTarget(t *testing.T) {
	bld := toolNamed(t, "cmake_build")
	dir := t.TempDir()
	for _, bad := range []string{"--clean-first", "--", "-j"} {
		_, err := callWithArgs(t, bld, map[string]any{
			"preset":      "default",
			"working_dir": dir,
			"targets":     []string{bad},
		})
		if err == nil {
			t.Errorf("target %q: expected a param error, got nil", bad)
			continue
		}
		var pe *mcp.ParamError
		if !errors.As(err, &pe) {
			t.Errorf("target %q: error = %v (%T), want *mcp.ParamError", bad, err, err)
		}
	}

	// A normal target passes validation (it fails later only if cmake is absent,
	// which is a different, non-param error — not asserted here).
	_, err := callWithArgs(t, bld, map[string]any{
		"preset":      "default",
		"working_dir": dir,
		"targets":     []string{"mylib"},
	})
	var pe *mcp.ParamError
	if errors.As(err, &pe) {
		t.Errorf("normal target wrongly rejected as a param error: %v", err)
	}
}

// TestVcpkgSearchRejectsFlagQuery proves a flag-shaped search query is rejected
// as a param error before any vcpkg resolution.
func TestVcpkgSearchRejectsFlagQuery(t *testing.T) {
	search := toolNamed(t, "vcpkg_search")
	_, err := callWithArgs(t, search, map[string]any{"query": "--x-json"})
	if err == nil {
		t.Fatal("expected a param error for a flag-shaped query")
	}
	var pe *mcp.ParamError
	if !errors.As(err, &pe) {
		t.Errorf("error = %v (%T), want *mcp.ParamError", err, err)
	}
}

// TestPresetFlagEqualsForm pins the equals-form fix: a preset name is always
// rendered as a SINGLE "--preset=<name>" argv token, so a flag-shaped preset
// name (e.g. "--help") can never be reinterpreted by cmake/ctest as an option.
func TestPresetFlagEqualsForm(t *testing.T) {
	for _, name := range []string{"dev", "release", "--help", "-x", "with space"} {
		got := presetFlag(name)
		want := "--preset=" + name
		if got != want {
			t.Errorf("presetFlag(%q) = %q, want %q", name, got, want)
		}
		if !strings.HasPrefix(got, "--preset=") {
			t.Errorf("presetFlag(%q) = %q lacks the --preset= equals form", name, got)
		}
	}
}

// copyExe copies the test binary to dst (executable), so the handlers exec a
// fake cmake/ctest that echoes its argv via TestMain.
func copyExe(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open self %q: %v", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("create fake %q: %v", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatalf("copy fake: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close fake: %v", err)
	}
}

// TestPresetArgvWiring runs the actual tools against a fake cmake/ctest (a copy of
// the test binary, echoing argv via TestMain) to prove the handlers pass presets
// as a single "--preset=<name>" token (configure/build/test/workflow) and that
// cmake_clean's non-purge path builds by the resolved build DIRECTORY, never
// "--build --preset". Cross-platform (Windows included).
func TestPresetArgvWiring(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate test binary (%v); presetFlag unit test covers the contract", err)
	}
	bindir := t.TempDir()
	fakeCmake := filepath.Join(bindir, exeName("cmake"))
	copyExe(t, self, fakeCmake)
	// resolveCTest looks for ctest beside CMAKE_BIN; put a fake there so cmake_test
	// does not fall back to a real ctest (which would not echo argv).
	copyExe(t, self, filepath.Join(bindir, exeName("ctest")))
	t.Setenv("MCP_CBUILD_FAKE_ARGV", "1")
	t.Setenv("CMAKE_BIN", fakeCmake)

	dir := t.TempDir()
	writeFile(t, dir, "CMakePresets.json", `{
      "version": 3,
      "configurePresets": [
        { "name": "-weird", "generator": "Ninja", "binaryDir": "${sourceDir}/out/weird" }
      ],
      "buildPresets": [ { "name": "weird-build", "configurePreset": "-weird" } ],
      "testPresets": [ { "name": "weird-test", "configurePreset": "-weird" } ],
      "workflowPresets": [ { "name": "-weird", "steps": [] } ]
    }`)

	rawTailOf := func(t *testing.T, tl mcp.Tool, args map[string]any) string {
		t.Helper()
		res, err := callWithArgs(t, tl, args)
		if err != nil {
			t.Fatalf("%s call: %v", tl.Name(), err)
		}
		b, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("marshal result: %v", err)
		}
		var r struct {
			RawTail string `json:"raw_tail"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		return r.RawTail
	}

	hasLine := func(tail, want string) bool {
		for _, ln := range strings.Split(tail, "\n") {
			if ln == want {
				return true
			}
		}
		return false
	}

	// configure / build / test / workflow: the preset must arrive as one
	// "--preset=-weird" token, never as a separate "-weird" argument.
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"cmake_configure", map[string]any{"preset": "-weird", "working_dir": dir}},
		{"cmake_build", map[string]any{"preset": "-weird", "working_dir": dir}},
		{"cmake_test", map[string]any{"preset": "-weird", "working_dir": dir}},
		{"cmake_workflow", map[string]any{"preset": "-weird", "working_dir": dir}},
	} {
		tail := rawTailOf(t, toolNamed(t, tc.tool), tc.args)
		if !hasLine(tail, "--preset=-weird") {
			t.Errorf("%s: argv missing single token --preset=-weird; raw_tail:\n%s", tc.tool, tail)
		}
		if hasLine(tail, "-weird") {
			t.Errorf("%s: preset leaked as a SEPARATE argv token -weird (flag-injection risk); raw_tail:\n%s", tc.tool, tail)
		}
	}

	// cmake_clean non-purge: builds by the resolved binaryDir, not --build --preset.
	tail := rawTailOf(t, toolNamed(t, "cmake_clean"), map[string]any{"preset": "-weird", "working_dir": dir})
	wantDir := filepath.Clean(filepath.Join(dir, "out", "weird"))
	if !hasLine(tail, "--build") || !hasLine(tail, wantDir) || !hasLine(tail, "--target") || !hasLine(tail, "clean") {
		t.Errorf("cmake_clean argv not `--build %s --target clean`; raw_tail:\n%s", wantDir, tail)
	}
	if hasLine(tail, "--preset=-weird") || hasLine(tail, "--preset") {
		t.Errorf("cmake_clean non-purge wrongly used a --preset flag; raw_tail:\n%s", tail)
	}
}

// TestGeneratorBinaryDirResolvesForCleanAndCacheSummary proves consumers that
// resolve binaryDir locally use the configure preset's merged generator macro,
// matching the directory CMake configured.
func TestGeneratorBinaryDirResolvesForCleanAndCacheSummary(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "CMakePresets.json", `{
      "version": 6,
      "configurePresets": [
        { "name": "dev", "generator": "Ninja",
          "binaryDir": "${sourceDir}/build/${generator}" }
      ]
    }`)
	buildDir := filepath.Join(dir, "build", "Ninja")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatalf("mkdir build dir: %v", err)
	}
	writeFile(t, buildDir, "CMakeCache.txt", "CMAKE_GENERATOR:INTERNAL=Ninja\n")

	t.Run("cache summary", func(t *testing.T) {
		summary := readCacheSummary(dir, "dev")
		if got := summary["CMAKE_GENERATOR"]; got != "Ninja" {
			t.Errorf("CMAKE_GENERATOR = %q, want Ninja; summary = %v", got, summary)
		}
	})

	t.Run("clean purge", func(t *testing.T) {
		result, err := callWithArgs(t, toolNamed(t, "cmake_clean"), map[string]any{
			"preset":          "dev",
			"working_dir":     dir,
			"purge_build_dir": true,
		})
		if err != nil {
			t.Fatalf("cmake_clean: %v", err)
		}
		cleaned, ok := result.(cleanResult)
		if !ok || !cleaned.Success || !cleaned.Purged {
			t.Fatalf("cmake_clean result = %#v, want successful purge", result)
		}
		if _, err := os.Stat(buildDir); !os.IsNotExist(err) {
			t.Errorf("build directory still exists after purge: stat error = %v", err)
		}
	})
}
