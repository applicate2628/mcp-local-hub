package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestImportVSCodeWorkspaceCmd_RoutesWarningsThroughCobraStderr guards
// Codex P2 on PR #151 line 68. Warnings must go to cmd.ErrOrStderr()
// so tests can swap the stream via cmd.SetErr; piping `mcphub import
// vscode-workspace ... 2>warn.log` must capture them.
func TestImportVSCodeWorkspaceCmd_RoutesWarningsThroughCobraStderr(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".vscode"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Use an http-type server so the projection skips it with a
	// warning that references G6 — deterministic + non-empty.
	body := `{
  "servers": {
    "remote": {"type": "http", "url": "https://example.com/mcp"}
  }
}`
	if err := os.WriteFile(filepath.Join(ws, ".vscode", "mcp.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write mcp.json: %v", err)
	}

	root := NewRootCmd()
	var stdoutBuf, stderrBuf bytes.Buffer
	root.SetOut(&stdoutBuf)
	root.SetErr(&stderrBuf)
	root.SetArgs([]string{"import", "vscode-workspace", ws})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if !strings.Contains(stderrBuf.String(), "warn:") {
		t.Errorf("stderr should contain 'warn:' prefix; got: %q", stderrBuf.String())
	}
	if !strings.Contains(stderrBuf.String(), "G6") {
		t.Errorf("stderr should reference G6 deferral; got: %q", stderrBuf.String())
	}
	// Stdout must be empty for an all-http-skip case (EmptyResult path).
	if strings.TrimSpace(stdoutBuf.String()) != "" {
		t.Errorf("stdout should be empty when all entries skipped; got: %q", stdoutBuf.String())
	}
}
