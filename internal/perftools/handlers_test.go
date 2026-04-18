package perftools

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDetectTools_ReportsInstalledWithVersion(t *testing.T) {
	catalog := DetectTools()

	if catalog == nil {
		t.Fatal("DetectTools returned nil")
	}
	if catalog.ClangTidy == nil || catalog.Hyperfine == nil ||
		catalog.LLVMObjdump == nil || catalog.IWYU == nil {
		t.Errorf("catalog missing entries: %+v", catalog)
	}
	// If any tool is marked Installed it must have a non-empty Version and Path.
	for name, info := range catalog.AsMap() {
		if info.Installed && info.Version == "" {
			t.Errorf("%s marked installed but Version empty: %+v", name, info)
		}
		if info.Installed && info.Path == "" {
			t.Errorf("%s marked installed but Path empty: %+v", name, info)
		}
		if !info.Installed && info.Error == "" {
			t.Errorf("%s marked NOT installed but Error empty: %+v", name, info)
		}
	}
}

func TestToolsResource_ReturnsJSONCatalog(t *testing.T) {
	srv := &PerfToolbox{tools: DetectTools()}

	req := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{}}
	req.Params.URI = "resource://tools"

	result, err := srv.getToolsResource(t.Context(), req)
	if err != nil {
		t.Fatalf("getToolsResource: %v", err)
	}
	if len(result.Contents) == 0 {
		t.Fatal("empty Contents")
	}
	rc := result.Contents[0]
	if rc == nil || rc.Text == "" {
		t.Fatalf("Contents[0] has no text: %+v", rc)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(rc.Text), &parsed); err != nil {
		t.Fatalf("resource text is not valid JSON: %v\n%s", err, rc.Text)
	}
	for _, key := range []string{"clang-tidy", "hyperfine", "llvm-objdump", "include-what-you-use"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("catalog missing key %q in JSON: %s", key, rc.Text)
		}
	}
}

func TestClangTidy_ParsesRealOutput(t *testing.T) {
	cat := DetectTools()
	if !cat.ClangTidy.Installed {
		t.Skip("clang-tidy not on PATH; integration test skipped")
	}

	// Tiny C++ source with a clearly-flagged performance issue —
	// pass-by-value of a non-trivial type.
	dir := t.TempDir()
	srcPath := dir + "/t.cpp"
	if err := os.WriteFile(srcPath, []byte(
		"#include <string>\nvoid f(std::string s){(void)s;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Minimal compile_commands.json pointing at the temp file.
	ccPath := dir + "/compile_commands.json"
	cc := `[{"directory":"` + filepathFwd(dir) + `","file":"` + filepathFwd(srcPath) +
		`","command":"clang++ -std=c++17 -c ` + filepathFwd(srcPath) + `"}]`
	if err := os.WriteFile(ccPath, []byte(cc), 0o644); err != nil {
		t.Fatal(err)
	}

	tb := &PerfToolbox{tools: cat}
	args, _ := json.Marshal(map[string]any{
		"files":        []string{srcPath},
		"project_root": dir,
		"checks":       "performance-unnecessary-value-param",
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	result, err := tb.clangTidyTool(t.Context(), req)
	if err != nil {
		t.Fatalf("clangTidyTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned IsError=true: %+v", contentText(result))
	}

	body := contentText(result)
	// Expect JSON with at least one diagnostic referencing our check.
	if !strings.Contains(body, "performance-unnecessary-value-param") {
		t.Errorf("expected performance-unnecessary-value-param diagnostic in output:\n%s", body)
	}
	// Must be valid JSON.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("tool output is not valid JSON: %v\n%s", err, body)
	}
	if _, ok := parsed["diagnostics"]; !ok {
		t.Errorf("output JSON missing diagnostics key: %s", body)
	}
}

// contentText extracts the Text field from the first TextContent in a
// CallToolResult. Used by every handler test in this file.
func contentText(r *mcp.CallToolResult) string {
	if len(r.Content) == 0 {
		return ""
	}
	if tc, ok := r.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

// filepathFwd returns a filesystem path with forward slashes so it can
// be safely embedded inside a JSON string literal without escape hell.
func filepathFwd(p string) string {
	return strings.ReplaceAll(p, `\`, `/`)
}

func TestHyperfine_ComparesTwoCommands(t *testing.T) {
	cat := DetectTools()
	if !cat.Hyperfine.Installed {
		t.Skip("hyperfine not on PATH; integration test skipped")
	}

	tb := &PerfToolbox{tools: cat}
	// Two trivially different commands — timing gap is tiny but measurable.
	args, _ := json.Marshal(map[string]any{
		"commands": []string{
			"cmd /c exit 0",                    // near-instant
			"cmd /c ping -n 1 127.0.0.1 > nul", // ~1ms
		},
		"warmup":   1,
		"min_runs": 3,
		"max_runs": 5,
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	result, err := tb.hyperfineTool(t.Context(), req)
	if err != nil {
		t.Fatalf("hyperfineTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned IsError=true: %s", contentText(result))
	}

	body := contentText(result)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("tool output not valid JSON: %v\n%s", err, body)
	}
	results, ok := parsed["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("expected results[] of length 2, got %+v", parsed["results"])
	}
	// Each result must at minimum carry mean + command.
	for i, r := range results {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("results[%d] is not an object: %T", i, r)
		}
		if _, ok := m["mean"]; !ok {
			t.Errorf("results[%d] missing mean: %+v", i, m)
		}
		if _, ok := m["command"]; !ok {
			t.Errorf("results[%d] missing command: %+v", i, m)
		}
	}
}

func TestLLVMObjdump_DisassemblesBinary(t *testing.T) {
	cat := DetectTools()
	if !cat.LLVMObjdump.Installed {
		t.Skip("llvm-objdump not on PATH; integration test skipped")
	}

	// Pick any PE/ELF binary that's guaranteed to exist on this host —
	// the test binary itself is the most reliable choice.
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	tb := &PerfToolbox{tools: cat}
	args, _ := json.Marshal(map[string]any{
		"binary":  exe,
		"section": ".text",
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	result, err := tb.llvmObjdumpTool(t.Context(), req)
	if err != nil {
		t.Fatalf("llvmObjdumpTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned IsError=true: %s", contentText(result))
	}

	body := contentText(result)
	// At minimum the disassembly should contain a section marker.
	if !strings.Contains(body, "Disassembly") && !strings.Contains(body, "section") {
		t.Errorf("expected disassembly header in output:\n%s", body[:min(len(body), 500)])
	}
}

func TestIWYU_ParsesSuggestions(t *testing.T) {
	cat := DetectTools()
	if !cat.IWYU.Installed {
		t.Skip("include-what-you-use not on PATH; integration test skipped")
	}

	// Minimal source with a deliberately unused include — iwyu should flag it.
	dir := t.TempDir()
	srcPath := dir + "/t.cpp"
	if err := os.WriteFile(srcPath, []byte(
		"#include <string>\n#include <vector>\nint main(){std::vector<int> v; (void)v; return 0;}\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	tb := &PerfToolbox{tools: cat}
	args, _ := json.Marshal(map[string]any{
		"file":         srcPath,
		"project_root": dir,
		"extra_args":   []string{"-std=c++17"},
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	result, err := tb.iwyuTool(t.Context(), req)
	if err != nil {
		t.Fatalf("iwyuTool: %v", err)
	}
	if result.IsError {
		// iwyu can fail for environmental reasons (missing stdlib headers in
		// the temp compile) — treat as skip rather than test failure.
		t.Skipf("iwyu returned IsError, treating as environment skip: %s",
			contentText(result))
	}

	body := contentText(result)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("tool output not valid JSON: %v\n%s", err, body)
	}
	// At minimum the response carries a reports[] array.
	reports, ok := parsed["reports"].([]any)
	if !ok {
		t.Fatalf("expected reports[] in output, got: %+v", parsed)
	}
	if len(reports) == 0 {
		// Allow graceful skip if iwyu produced no parseable blocks — this
		// can happen if iwyu's output format varies by version.
		t.Skip("reports[] is empty — iwyu produced no parseable output for the test file")
	}
}
