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

	"mcp-local-hub/internal/cbuild/mcp"
)

func TestMain(m *testing.M) {
	if os.Getenv("MCP_CBUILD_PROTOCOL_FAKE_CMAKE") == "1" {
		for _, arg := range os.Args[1:] {
			switch arg {
			case "--list-presets=configure":
				fmt.Println("Available configure presets:\n\n  \"default\"")
			case "--list-presets=build":
				fmt.Println("Available build presets:\n\n  \"default\"")
			case "--list-presets=test":
				fmt.Println("Available test presets:\n\n  \"default\"")
			case "--list-presets=workflow":
				fmt.Println("Available workflow presets:\n\n  \"default\"")
			}
		}
		os.Exit(0)
	}
	if os.Getenv("MCP_CBUILD_FAKE_LIST") == "1" {
		for _, a := range os.Args[1:] {
			switch a {
			case "--list-presets=configure":
				fmt.Println("Available configure presets:\n\n  \"visible\" - Visible Display")
			case "--list-presets=build":
				fmt.Println("Available build presets:\n\n  \"visible-build\"")
			case "--list-presets=test":
				fmt.Println("Available test presets:\n\n  \"visible-test\"")
			case "--list-presets=workflow":
				fmt.Println("Available workflow presets:\n\n  \"visible-workflow\"")
			}
		}
		os.Exit(0)
	}
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

func writeFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestCmakeBuildRejectsFlagTarget(t *testing.T) {
	for _, bad := range []string{"--clean-first", "--", "-j"} {
		_, err := callWithArgs(t, toolNamed(t, "cmake_build"), map[string]any{"preset": "default", "working_dir": t.TempDir(), "targets": []string{bad}})
		var pe *mcp.ParamError
		if err == nil || !errors.As(err, &pe) {
			t.Errorf("target %q error = %v, want *mcp.ParamError", bad, err)
		}
	}
}

func TestVcpkgSearchRejectsFlagQuery(t *testing.T) {
	_, err := callWithArgs(t, toolNamed(t, "vcpkg_search"), map[string]any{"query": "--x-json"})
	var pe *mcp.ParamError
	if err == nil || !errors.As(err, &pe) {
		t.Errorf("error = %v, want *mcp.ParamError", err)
	}
}

func TestPresetFlagEqualsForm(t *testing.T) {
	for _, name := range []string{"dev", "release", "--help", "-x", "with space"} {
		if got, want := presetFlag(name), "--preset="+name; got != want {
			t.Errorf("presetFlag(%q) = %q, want %q", name, got, want)
		}
	}
}

func copyExe(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open self: %v", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("create fake: %v", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatalf("copy fake: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close fake: %v", err)
	}
}

func TestCmakeListPresetsDelegatesToCMake(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate test binary: %v", err)
	}
	fakeCmake := filepath.Join(t.TempDir(), exeName("cmake"))
	copyExe(t, self, fakeCmake)
	t.Setenv("MCP_CBUILD_FAKE_LIST", "1")
	t.Setenv("CMAKE_BIN", fakeCmake)
	result, err := callWithArgs(t, toolNamed(t, "cmake_list_presets"), map[string]any{"working_dir": t.TempDir()})
	if err != nil {
		t.Fatalf("cmake_list_presets: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Configure []presetInfo `json:"configurePresets"`
		Build     []presetInfo `json:"buildPresets"`
		Test      []presetInfo `json:"testPresets"`
		Workflow  []presetInfo `json:"workflowPresets"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Configure) != 1 || got.Configure[0] != (presetInfo{Name: "visible", DisplayName: "Visible Display"}) {
		t.Errorf("configure presets = %+v", got.Configure)
	}
	if len(got.Build) != 1 || got.Build[0].Name != "visible-build" || len(got.Test) != 1 || got.Test[0].Name != "visible-test" || len(got.Workflow) != 1 || got.Workflow[0].Name != "visible-workflow" {
		t.Errorf("preset lists = build:%+v test:%+v workflow:%+v", got.Build, got.Test, got.Workflow)
	}
}

func TestCmakeListPresetsRefusesUnknownOutput(t *testing.T) {
	if _, err := parseCMakePresetListing("configure", "CMake output changed\n"); err == nil {
		t.Fatal("unknown nonempty CMake output was accepted")
	}
}

func TestSettleConfiguredCacheFailureClearsPriorMapping(t *testing.T) {
	source := t.TempDir()
	sourceKey, err := canonicalExistingDir(source)
	if err != nil {
		t.Fatal(err)
	}
	b := &builder{configured: map[configuredCacheKey]string{
		{sourceDir: sourceKey, preset: "dev"}: filepath.Join(source, "old-build"),
	}}
	if _, err := b.settleConfiguredCache(source, "dev", "CMake completed without a locator"); err == nil {
		t.Fatal("settlement accepted output without CMake's completion line")
	}
	b.configuredMu.Lock()
	_, retained := b.configured[configuredCacheKey{sourceDir: sourceKey, preset: "dev"}]
	b.configuredMu.Unlock()
	if retained {
		t.Fatal("failed configure settlement retained a stale cache mapping")
	}
}

func TestPresetArgvWiring(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate test binary: %v", err)
	}
	bindir := t.TempDir()
	fakeCmake := filepath.Join(bindir, exeName("cmake"))
	copyExe(t, self, fakeCmake)
	copyExe(t, self, filepath.Join(bindir, exeName("ctest")))
	t.Setenv("MCP_CBUILD_FAKE_ARGV", "1")
	t.Setenv("CMAKE_BIN", fakeCmake)
	dir := t.TempDir()
	buildDir := filepath.Join(dir, "out", "weird")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, buildDir, "CMakeCache.txt", fmt.Sprintf("CMAKE_CACHEFILE_DIR:INTERNAL=%s\nCMAKE_HOME_DIRECTORY:INTERNAL=%s\n", buildDir, dir))
	rawTailOf := func(t *testing.T, tl mcp.Tool, args map[string]any) string {
		t.Helper()
		res, err := callWithArgs(t, tl, args)
		if err != nil {
			t.Fatalf("%s call: %v", tl.Name(), err)
		}
		encoded, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			RawTail string `json:"raw_tail"`
		}
		if err := json.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
		return result.RawTail
	}
	hasLine := func(tail, want string) bool {
		for _, line := range strings.Split(tail, "\n") {
			if line == want {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{{"cmake_configure", map[string]any{"preset": "-weird", "working_dir": dir}}, {"cmake_build", map[string]any{"preset": "-weird", "working_dir": dir}}, {"cmake_test", map[string]any{"preset": "-weird", "working_dir": dir}}, {"cmake_workflow", map[string]any{"preset": "-weird", "working_dir": dir}}} {
		tail := rawTailOf(t, toolNamed(t, tc.tool), tc.args)
		if !hasLine(tail, "--preset=-weird") || hasLine(tail, "-weird") {
			t.Errorf("%s argv = %q, want exactly --preset=-weird", tc.tool, tail)
		}
	}
	tail := rawTailOf(t, toolNamed(t, "cmake_clean"), map[string]any{"preset": "-weird", "working_dir": dir})
	canonicalBuildDir, err := canonicalExistingDir(buildDir)
	if err != nil {
		t.Fatalf("canonical build directory: %v", err)
	}
	if !hasLine(tail, "--build") || !hasLine(tail, canonicalBuildDir) || !hasLine(tail, "--target") || !hasLine(tail, "clean") || hasLine(tail, "--preset=-weird") {
		t.Errorf("cmake_clean argv = %q, want --build <cache-dir> --target clean", tail)
	}
}

func TestCmakeCleanPurgeRefusesNonDirectoryTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "CMakeLists.txt")
	writeFile(t, dir, "CMakeLists.txt", "project(contents)\n")
	if _, err := purgeBuildDirWithinSource(dir, target); err == nil {
		t.Fatal("purge guard accepted a non-directory target")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "project(contents)\n" {
		t.Errorf("non-directory target changed: data=%q err=%v", data, err)
	}
}
