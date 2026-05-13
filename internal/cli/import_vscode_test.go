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
//
// G6 sub-PR 4 closure: the prior fixture used an http-type entry
// that the projection skipped with a G6-deferral warning. Now that
// http entries project to transport=remote-http instead of skipping,
// the warning surface needs a different deterministic trigger. Use
// an entry with no command and no url — that's still skipped with
// a clear warning regardless of G6 status.
func TestImportVSCodeWorkspaceCmd_RoutesWarningsThroughCobraStderr(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".vscode"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{
  "servers": {
    "broken": {}
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
	if !strings.Contains(stderrBuf.String(), "missing both command and url") {
		t.Errorf("stderr should reference the missing-fields skip warning; got: %q", stderrBuf.String())
	}
	// Stdout must be empty when no entries projected (EmptyResult).
	if strings.TrimSpace(stdoutBuf.String()) != "" {
		t.Errorf("stdout should be empty when all entries skipped; got: %q", stdoutBuf.String())
	}
}
