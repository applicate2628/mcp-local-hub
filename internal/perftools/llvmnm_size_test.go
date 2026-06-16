package perftools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestLLVMNMTool_NotInstalled asserts the symbol-table tool returns a
// clean IsError when llvm-objdump is absent, mirroring the disassembly
// handler's installed-gate behavior.
func TestLLVMNMTool_NotInstalled(t *testing.T) {
	tb := &PerfToolbox{tools: &ToolCatalog{
		LLVMObjdump: &ToolInfo{Installed: false, Error: "not on PATH: exec: \"llvm-objdump\""},
	}}
	args, _ := json.Marshal(map[string]any{
		"binary":       "build/app.exe",
		"project_root": ".",
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	res, err := tb.llvmNMTool(t.Context(), req)
	if err != nil {
		t.Fatalf("llvmNMTool returned unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true when llvm-objdump not installed")
	}
	if body := contentText(res); !strings.Contains(body, "llvm-objdump not installed") {
		t.Fatalf("expected not-installed error, got: %s", body)
	}
}

// TestLLVMNMTool_RequiresProjectRoot mirrors
// TestLLVMObjdumpTool_RequiresProjectRoot: the project_root boundary is
// mandatory before the binary path is touched.
func TestLLVMNMTool_RequiresProjectRoot(t *testing.T) {
	tb := &PerfToolbox{tools: &ToolCatalog{
		LLVMObjdump: &ToolInfo{Installed: true, Path: "not-used"},
	}}
	args, _ := json.Marshal(map[string]any{
		"binary": "build/app.exe",
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	res, err := tb.llvmNMTool(t.Context(), req)
	if err != nil {
		t.Fatalf("llvmNMTool returned unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true for missing project_root")
	}
	if body := contentText(res); !strings.Contains(body, "missing required parameter: project_root") {
		t.Fatalf("expected missing project_root error, got: %s", body)
	}
}

// TestLLVMSizeTool_RequiresProjectRoot is the section-header twin of the
// NM project_root gate test.
func TestLLVMSizeTool_RequiresProjectRoot(t *testing.T) {
	tb := &PerfToolbox{tools: &ToolCatalog{
		LLVMObjdump: &ToolInfo{Installed: true, Path: "not-used"},
	}}
	args, _ := json.Marshal(map[string]any{
		"binary": "build/app.exe",
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	res, err := tb.llvmSizeTool(t.Context(), req)
	if err != nil {
		t.Fatalf("llvmSizeTool returned unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true for missing project_root")
	}
	if body := contentText(res); !strings.Contains(body, "missing required parameter: project_root") {
		t.Fatalf("expected missing project_root error, got: %s", body)
	}
}

// TestLLVMNMTool_RejectsExtraArgsBypass asserts the symbol-table handler
// runs the same extra_args guard as llvm_objdump: hostile positional /
// response-file / path-valued entries can't reach runCaptureLimited.
// Mirrors TestLLVMObjdumpTool_RejectsExtraArgsBypass +
// TestLLVMObjdumpTool_RejectsExtraArgsPathValuedFlag.
func TestLLVMNMTool_RejectsExtraArgsBypass(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "build", "app.exe")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatalf("mkdir build dir: %v", err)
	}
	if err := os.WriteFile(bin, []byte("fake"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	tb := &PerfToolbox{tools: &ToolCatalog{
		LLVMObjdump: &ToolInfo{Installed: true, Path: "not-used"},
	}}

	cases := []struct {
		name      string
		extraArgs []string
		wantSub   string
	}{
		{"response-file", []string{"@/etc/passwd"}, "must be a flag"},
		{"positional-absolute", []string{"/etc/passwd"}, "must be a flag"},
		{"positional-relative", []string{"foo.o"}, "must be a flag"},
		{"flag-then-positional", []string{"--demangle", "secret.bin"}, "must be a flag"},
		{"path-valued-flag", []string{"--build-id=/tmp/foo"}, "path-valued"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(map[string]any{
				"binary":       bin,
				"project_root": root,
				"extra_args":   tc.extraArgs,
			})
			req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

			res, err := tb.llvmNMTool(t.Context(), req)
			if err != nil {
				t.Fatalf("llvmNMTool: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected IsError=true for hostile extra_args %v", tc.extraArgs)
			}
			if body := contentText(res); !strings.Contains(body, tc.wantSub) {
				t.Fatalf("expected %q in error, got: %s", tc.wantSub, body)
			}
		})
	}
}

// TestLLVMSizeTool_RejectsExtraArgsBypass is the section-header twin of
// TestLLVMNMTool_RejectsExtraArgsBypass.
func TestLLVMSizeTool_RejectsExtraArgsBypass(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "build", "app.exe")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatalf("mkdir build dir: %v", err)
	}
	if err := os.WriteFile(bin, []byte("fake"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	tb := &PerfToolbox{tools: &ToolCatalog{
		LLVMObjdump: &ToolInfo{Installed: true, Path: "not-used"},
	}}

	cases := []struct {
		name      string
		extraArgs []string
		wantSub   string
	}{
		{"response-file", []string{"@/etc/passwd"}, "must be a flag"},
		{"positional-absolute", []string{"/etc/passwd"}, "must be a flag"},
		{"flag-then-positional", []string{"--wide", "secret.bin"}, "must be a flag"},
		{"path-valued-flag", []string{"--dsym=/tmp/foo"}, "path-valued"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(map[string]any{
				"binary":       bin,
				"project_root": root,
				"extra_args":   tc.extraArgs,
			})
			req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

			res, err := tb.llvmSizeTool(t.Context(), req)
			if err != nil {
				t.Fatalf("llvmSizeTool: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected IsError=true for hostile extra_args %v", tc.extraArgs)
			}
			if body := contentText(res); !strings.Contains(body, tc.wantSub) {
				t.Fatalf("expected %q in error, got: %s", tc.wantSub, body)
			}
		})
	}
}

// TestLLVMNM_DumpsSymbols builds a real Go binary with symbols intact
// and asserts the symbol-table dump mentions the program's main symbol.
// Mirrors TestLLVMObjdump_DisassemblesBinary's synthetic-binary harness
// (build a dedicated exe so the `-s -w`-stripped test binary doesn't
// produce an empty symbol table). Skips when llvm-objdump or go is
// absent — never spawns a system-installed real tool blindly.
func TestLLVMNM_DumpsSymbols(t *testing.T) {
	cat := DetectTools()
	if !cat.LLVMObjdump.Installed {
		t.Skip("llvm-objdump not on PATH; integration test skipped")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; integration test skipped")
	}

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(src, []byte("package main\nfunc main() { println(\"ok\") }\n"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	exe := filepath.Join(tmp, "hello.exe")
	build := exec.Command("go", "build", "-o", exe, src)
	build.Env = append(os.Environ(), "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}

	tb := &PerfToolbox{tools: cat}
	args, _ := json.Marshal(map[string]any{
		"binary":       exe,
		"project_root": tmp,
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	result, err := tb.llvmNMTool(t.Context(), req)
	if err != nil {
		t.Fatalf("llvmNMTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned IsError=true: %s", contentText(result))
	}

	body := contentText(result)
	// The symbol table of a Go binary with symbols intact must list main.main.
	if !strings.Contains(body, "main.main") {
		t.Errorf("expected main.main symbol in --syms output:\n%s", body[:min(len(body), 800)])
	}
}

// TestLLVMSize_DumpsSections builds a real Go binary and asserts the
// section-header dump mentions the .text section. Section-header tables
// are tiny so no symbol-stripping concern applies, but we keep the
// dedicated-binary build for parity with the NM test.
func TestLLVMSize_DumpsSections(t *testing.T) {
	cat := DetectTools()
	if !cat.LLVMObjdump.Installed {
		t.Skip("llvm-objdump not on PATH; integration test skipped")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; integration test skipped")
	}

	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(src, []byte("package main\nfunc main() { println(\"ok\") }\n"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	exe := filepath.Join(tmp, "hello.exe")
	build := exec.Command("go", "build", "-o", exe, src)
	build.Env = append(os.Environ(), "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}

	tb := &PerfToolbox{tools: cat}
	args, _ := json.Marshal(map[string]any{
		"binary":       exe,
		"project_root": tmp,
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	result, err := tb.llvmSizeTool(t.Context(), req)
	if err != nil {
		t.Fatalf("llvmSizeTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned IsError=true: %s", contentText(result))
	}

	body := contentText(result)
	// Every PE/ELF/Mach-O binary carries a .text section in its section headers.
	if !strings.Contains(body, ".text") && !strings.Contains(body, "Sections") {
		t.Errorf("expected .text section / section-header header in output:\n%s", body[:min(len(body), 800)])
	}
}
