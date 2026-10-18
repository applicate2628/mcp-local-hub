package cbuild

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/cbuild/mcp"
)

func TestMain(m *testing.M) {
	if os.Getenv("MCP_CBUILD_FAKE_CTEST") != "" {
		fmt.Println("1/2 Test #1: first .................. Passed 0.01 sec")
		if os.Getenv("MCP_CBUILD_FAKE_CTEST") == "large" {
			fmt.Println(strings.Repeat("x", maxCapturedOutput+1))
		}
		fmt.Println("2/2 Test #2: last ................... Passed 0.01 sec")
		fmt.Println("100% tests passed, 0 tests failed out of 2")
		os.Exit(0)
	}
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
	// Clean requires a preset-specific settlement, not discovery of any cache.
	b := &builder{configured: make(map[configuredCacheKey]string)}
	if _, err := b.settleConfiguredCache(dir, "-weird", "-- Build files have been written to: "+buildDir); err != nil {
		t.Fatal(err)
	}
	tail := rawTailOf(t, b.cmakeClean(), map[string]any{"preset": "-weird", "working_dir": dir})
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

func TestExecutionResultsRetainRawTailInMCPChannels(t *testing.T) {
	const tail = "unrecognized output\r\nsecond line\n"
	for _, tc := range []struct {
		name   string
		result any
		want   string
	}{
		{"configure", configureResult{}, `{"success":false,"exit_code":0,"diagnostics":null,"wall_ms":0,"raw_tail":""}`},
		{"build", buildResult{}, `{"success":false,"exit_code":0,"diagnostics":null,"wall_ms":0,"raw_tail":""}`},
		{"test", testResult{}, `{"success":false,"exit_code":0,"total":0,"passed":0,"failed":0,"tests":null,"wall_ms":0,"raw_tail":""}`},
		{"workflow", workflowResult{}, `{"success":false,"exit_code":0,"diagnostics":null,"wall_ms":0,"raw_tail":""}`},
		{"clean", cleanResult{}, `{"success":false,"purged":false,"raw_tail":""}`},
		{"vcpkg_install", vcpkgInstallResult{}, `{"success":false,"exit_code":0,"installed":null,"diagnostics":null,"wall_ms":0,"raw_tail":""}`},
		{"vcpkg_list", vcpkgListResult{}, `{"success":false,"exit_code":0,"packages":null,"raw_tail":""}`},
		{"vcpkg_search", vcpkgSearchResult{}, `{"success":false,"exit_code":0,"query":"","packages":null,"raw_tail":""}`},
		{"nonempty", buildResult{Success: true, BuiltTargets: []string{"example"}, WallMs: 12, RawTail: tail}, `{"success":true,"exit_code":0,"diagnostics":null,"built_targets":["example"],"wall_ms":12,"raw_tail":"unrecognized output\r\nsecond line\n"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			check := func(channel string, encoded []byte) {
				t.Helper()
				var got map[string]any
				if err := json.Unmarshal(encoded, &got); err != nil {
					t.Fatalf("%s decode: %v", channel, err)
				}
				value, present := got["raw_tail"]
				if _, isString := value.(string); !present || !isString {
					t.Fatalf("%s raw_tail = %#v, present = %v; want a present string", channel, value, present)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s fields = %#v, want %#v", channel, got, want)
				}
			}
			domain, err := json.Marshal(tc.result)
			if err != nil {
				t.Fatal(err)
			}
			check("domain", domain)

			tool := &funcTool{name: "result", handler: func(context.Context, json.RawMessage) (any, error) {
				return tc.result, nil
			}}
			server := mcp.NewServer("test", "0", []mcp.Tool{tool})
			request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"result","arguments":{}}}` + "\n"
			var output bytes.Buffer
			if err := server.Serve(context.Background(), strings.NewReader(request), &output); err != nil {
				t.Fatalf("Serve: %v", err)
			}
			var response struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      int             `json:"id"`
				Error   json.RawMessage `json:"error"`
				Result  struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
					IsError           bool            `json:"isError"`
					StructuredContent json.RawMessage `json:"structuredContent"`
				} `json:"result"`
			}
			if err := json.Unmarshal(output.Bytes(), &response); err != nil {
				t.Fatalf("response decode: %v", err)
			}
			if response.JSONRPC != "2.0" || response.ID != 1 || len(response.Error) != 0 || response.Result.IsError {
				t.Fatalf("unexpected response: %s", output.Bytes())
			}
			if len(response.Result.Content) != 1 || response.Result.Content[0].Type != "text" {
				t.Fatalf("unexpected content: %+v", response.Result.Content)
			}
			check("text", []byte(response.Result.Content[0].Text))
			check("structuredContent", response.Result.StructuredContent)
		})
	}
}
