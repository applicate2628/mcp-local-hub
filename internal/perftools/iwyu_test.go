package perftools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestIWYUTool_RejectsOversizedStdout(t *testing.T) {
	res := callIWYUHelper(t, "helper-iwyu-stdout")
	if !res.IsError {
		t.Fatalf("expected IsError=true for oversized IWYU stdout; body=%s", contentText(res))
	}
	if body := contentText(res); !strings.Contains(body, "output exceeded limits") {
		t.Fatalf("expected output limit error, got: %s", body)
	}
}

func TestIWYUTool_RejectsOversizedStderr(t *testing.T) {
	res := callIWYUHelper(t, "helper-iwyu-stderr")
	if !res.IsError {
		t.Fatal("expected IsError=true for oversized IWYU stderr")
	}
	if body := contentText(res); !strings.Contains(body, "output exceeded limits") {
		t.Fatalf("expected output limit error, got: %s", body)
	}
}

// iwyuHelperModeEnv carries the mode from the parent test into the
// re-exec'd test binary that stands in for the iwyu binary. We use an
// env var instead of an extra-args positional because the new strict
// validator (validatePerfToolExtraArgs) rejects positional entries —
// including the `--, mode` flag-stop form the original test used.
const iwyuHelperModeEnv = "MCP_LOCAL_HUB_TEST_IWYU_HELPER_MODE"

func callIWYUHelper(t *testing.T, mode string) *mcp.CallToolResult {
	t.Helper()
	// Path-validation requires file to exist inside project_root, so we
	// stage a real file there and let the tool resolve it. The mode is
	// passed via env var; extra_args carries only flag-shaped entries.
	root := t.TempDir()
	file := filepath.Join(root, "main.cpp")
	if err := os.WriteFile(file, []byte("// empty\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv(iwyuHelperModeEnv, mode)
	tb := &PerfToolbox{tools: &ToolCatalog{
		IWYU: &ToolInfo{Installed: true, Path: os.Args[0]},
	}}
	args, _ := json.Marshal(map[string]any{
		"file":         file,
		"project_root": root,
		"extra_args":   []string{"-test.run=TestIWYUHelperProcess"},
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	res, err := tb.iwyuTool(t.Context(), req)
	if err != nil {
		t.Fatalf("iwyuTool returned unexpected error: %v", err)
	}
	return res
}

func TestIWYUHelperProcess(t *testing.T) {
	mode := os.Getenv(iwyuHelperModeEnv)
	if mode == "" {
		return
	}

	payload := strings.Repeat("A", 3*1024*1024)
	switch mode {
	case "helper-iwyu-stdout":
		fmt.Fprint(os.Stdout, payload)
	case "helper-iwyu-stderr":
		fmt.Fprint(os.Stderr, payload)
	}
	os.Exit(0)
}

func TestIWYUTool_RequiresProjectRoot(t *testing.T) {
	tb := &PerfToolbox{tools: &ToolCatalog{
		IWYU: &ToolInfo{Installed: true, Path: "not-used"},
	}}
	args, _ := json.Marshal(map[string]any{
		"file": "main.cpp",
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	res, err := tb.iwyuTool(t.Context(), req)
	if err != nil {
		t.Fatalf("iwyuTool returned unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true for missing project_root")
	}
	if body := contentText(res); !strings.Contains(body, "missing required parameter: project_root") {
		t.Fatalf("expected missing project_root error, got: %s", body)
	}
}

func TestIWYUTool_RejectsFileOutsideProjectRoot(t *testing.T) {
	projectRoot := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "main.cpp")
	if err := os.WriteFile(outsideFile, []byte("// content\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	tb := &PerfToolbox{tools: &ToolCatalog{
		IWYU: &ToolInfo{Installed: true, Path: "not-used"},
	}}
	args, _ := json.Marshal(map[string]any{
		"file":         outsideFile,
		"project_root": projectRoot,
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	res, err := tb.iwyuTool(t.Context(), req)
	if err != nil {
		t.Fatalf("iwyuTool returned unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true for file outside project_root")
	}
	if body := contentText(res); !strings.Contains(body, "must be inside project_root") {
		t.Fatalf("expected project_root boundary error, got: %s", body)
	}
}

func TestIWYUTool_RejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation requires privileges on many hosts")
	}
	projectRoot := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.cpp")
	if err := os.WriteFile(outsideFile, []byte("// secret\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	link := filepath.Join(projectRoot, "main.cpp")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	_, err := validateIWYUFilePath(projectRoot, link)
	if err == nil {
		t.Fatal("expected symlink escape to fail validation")
	}
	if !strings.Contains(err.Error(), "must be inside project_root") {
		t.Fatalf("expected project_root boundary error, got: %v", err)
	}
}

func TestIWYUTool_RejectsExtraArgsPositional(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "main.cpp")
	if err := os.WriteFile(file, []byte("// empty\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	tb := &PerfToolbox{tools: &ToolCatalog{
		IWYU: &ToolInfo{Installed: true, Path: "not-used"},
	}}
	args, _ := json.Marshal(map[string]any{
		"file":         file,
		"project_root": root,
		"extra_args":   []string{"/etc/passwd"},
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	res, err := tb.iwyuTool(t.Context(), req)
	if err != nil {
		t.Fatalf("iwyuTool returned unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true for positional file in extra_args")
	}
	if body := contentText(res); !strings.Contains(body, "must be a flag") {
		t.Fatalf("expected flag-only error, got: %s", body)
	}
}

func TestIWYUTool_RejectsExtraArgsResponseFile(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "main.cpp")
	if err := os.WriteFile(file, []byte("// empty\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	tb := &PerfToolbox{tools: &ToolCatalog{
		IWYU: &ToolInfo{Installed: true, Path: "not-used"},
	}}
	args, _ := json.Marshal(map[string]any{
		"file":         file,
		"project_root": root,
		"extra_args":   []string{"@evil"},
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	res, err := tb.iwyuTool(t.Context(), req)
	if err != nil {
		t.Fatalf("iwyuTool returned unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true for @-response-file in extra_args")
	}
	if body := contentText(res); !strings.Contains(body, "must be a flag") {
		t.Fatalf("expected flag-only error, got: %s", body)
	}
}

// TestValidateIWYUExtraArgs_AllowsBenignClangFlags asserts plain
// clang flags (`-std=...`, `-I...`, `-D...`, `-W...`) pass through —
// they affect compile semantics, not file access via IWYU's flag set,
// and IWYU consumers routinely supply them in extra_args.
func TestValidateIWYUExtraArgs_AllowsBenignClangFlags(t *testing.T) {
	cases := [][]string{
		{"-std=c++17"},
		{"-Iinclude"},
		{"-DFOO=1"},
		{"-Wall"},
		{"-std=c++20", "-Iinclude", "-DFOO=1", "-Wall"},
	}
	for _, c := range cases {
		got, err := validateIWYUExtraArgs(c)
		if err != nil {
			t.Errorf("validateIWYUExtraArgs(%q): expected ok, got %v", c, err)
			continue
		}
		if len(got) != len(c) {
			t.Errorf("validateIWYUExtraArgs(%q): returned len %d, want %d", c, len(got), len(c))
		}
	}
}

// TestValidateIWYUExtraArgs_RejectsPathValuedIWYUFlags guards the
// path-valued-flag bypass that the original "starts with '-'" check
// missed. IWYU exposes per-tool flags via `-Xiwyu`, several of which
// take a filesystem path (`--mapping_file=<file>`,
// `--export_mappings=<dir>`, `--check_also`, `--keep`). Allowing them
// in extra_args would let a caller read attacker-chosen files even
// when `file` is path-bounded.
func TestValidateIWYUExtraArgs_RejectsPathValuedIWYUFlags(t *testing.T) {
	cases := [][]string{
		{"-Xiwyu"},
		{"--mapping_file=/tmp/foo"},
		{"--mapping_file"},
		{"--export_mappings=/tmp/foo"},
		{"--export_mappings"},
		{"--check_also=/tmp/foo"},
		{"--keep=/tmp/foo"},
		{"-std=c++17", "--mapping_file=/etc/passwd"},
	}
	for _, c := range cases {
		_, err := validateIWYUExtraArgs(c)
		if err == nil {
			t.Errorf("validateIWYUExtraArgs(%q): expected rejection, got nil", c)
			continue
		}
		if !strings.Contains(err.Error(), "path-valued") {
			t.Errorf("validateIWYUExtraArgs(%q): expected path-valued error, got %v", c, err)
		}
	}
}

// TestValidateIWYUExtraArgs_RejectsPositionalAndResponseFile reasserts
// that the basic-check shared with llvm-objdump still applies through
// the IWYU validator (positional input files and `@FILE` directives
// remain rejected).
func TestValidateIWYUExtraArgs_RejectsPositionalAndResponseFile(t *testing.T) {
	cases := [][]string{
		{"foo.cpp"},
		{"/etc/passwd"},
		{"@evil"},
		{"@/etc/passwd"},
		{""},
	}
	for _, c := range cases {
		_, err := validateIWYUExtraArgs(c)
		if err == nil {
			t.Errorf("validateIWYUExtraArgs(%q): expected rejection, got nil", c)
			continue
		}
		if !strings.Contains(err.Error(), "must be a flag") &&
			!strings.Contains(err.Error(), "empty entry") {
			t.Errorf("validateIWYUExtraArgs(%q): expected flag-only or empty error, got %v", c, err)
		}
	}
}

// TestIWYUTool_RejectsExtraArgsPathValuedFlag asserts the tool handler
// surface returns IsError for path-valued IWYU flags, mirroring the
// llvm-objdump bypass test. Belt-and-braces: validator unit tests
// cover the function, this one covers the call site.
func TestIWYUTool_RejectsExtraArgsPathValuedFlag(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "main.cpp")
	if err := os.WriteFile(file, []byte("// empty\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	tb := &PerfToolbox{tools: &ToolCatalog{
		IWYU: &ToolInfo{Installed: true, Path: "not-used"},
	}}
	args, _ := json.Marshal(map[string]any{
		"file":         file,
		"project_root": root,
		"extra_args":   []string{"--mapping_file=/etc/passwd"},
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	res, err := tb.iwyuTool(t.Context(), req)
	if err != nil {
		t.Fatalf("iwyuTool returned unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true for path-valued IWYU flag in extra_args")
	}
	if body := contentText(res); !strings.Contains(body, "path-valued") {
		t.Fatalf("expected path-valued error, got: %s", body)
	}
}

// (Schema-side enforcement is asserted indirectly by
// TestIWYUTool_RequiresProjectRoot above: even when a client crafts a
// raw tools/call that omits project_root, the runtime returns an
// IsError result with the missing-required-parameter message.)
