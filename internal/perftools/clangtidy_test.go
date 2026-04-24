package perftools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestClangTidyTool_RejectsOversizedOutput(t *testing.T) {
	// Fake clang-tidy is a bash script; Windows cannot execute a file without
	// a recognized PATHEXT extension through os/exec. The output-limit path
	// is already exercised by internal/perftools TestRunCaptureLimited; this
	// is just a handler-wiring test. Skip on Windows.
	if runtime.GOOS == "windows" {
		t.Skip("bash-script fake clang-tidy not executable on Windows")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "clang-tidy")
	script := "#!/usr/bin/env bash\nhead -c $((9 * 1024 * 1024)) </dev/zero | tr '\\000' 'A'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake clang-tidy: %v", err)
	}

	tb := &PerfToolbox{tools: &ToolCatalog{
		ClangTidy: &ToolInfo{Installed: true, Path: fake},
	}}
	args, _ := json.Marshal(map[string]any{
		"files":        []string{"main.cpp"},
		"project_root": ".",
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	res, err := tb.clangTidyTool(t.Context(), req)
	if err != nil {
		t.Fatalf("clangTidyTool returned unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true for oversized clang-tidy output")
	}
	body := contentText(res)
	if !strings.Contains(body, "output exceeded limits") {
		t.Fatalf("expected output limit message, got: %s", body)
	}
}
