package config

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Live smoke: spawn the gdb manifest's command/args end-to-end and verify
// the LLDB warning is suppressed but JSON-RPC initialize still answers.
// Skipped if GDB-MCP isn't cloned at the expected location.
func TestGdbManifestSpawnSuppressesLldbWarning(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only smoke (uv + msys2 path layout)")
	}
	home, _ := os.UserHomeDir()
	gdbMcpDir := filepath.Join(home, ".local", "mcp-servers", "GDB-MCP")
	if _, err := os.Stat(filepath.Join(gdbMcpDir, "server.py")); err != nil {
		t.Skipf("GDB-MCP not cloned at %s (skip); err=%v", gdbMcpDir, err)
	}

	b, err := os.ReadFile("../../servers/gdb/manifest.yaml")
	if err != nil { t.Fatalf("read manifest: %v", err) }
	m, err := ParseManifest(strings.NewReader(string(b)))
	if err != nil { t.Fatalf("parse: %v", err) }

	// ParseManifest already expanded ${HOME} in BaseArgs above.
	cmd := exec.Command(m.Command, m.BaseArgs...)
	cmd.Dir = "" // let uv --directory handle it
	cmd.Stdin = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"smoke","version":"1"}}}` + "\n")
	var stderr, stdout bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	_ = cmd.Run() // server.py keeps stdin open after initialize — Run may return non-nil; not fatal

	stderrStr := stderr.String()
	stdoutStr := stdout.String()
	if strings.Contains(stderrStr, "LLDB Python module not available") {
		t.Errorf("Filter regression — LLDB warning leaked to stderr:\n%s", stderrStr)
	}
	if !strings.Contains(stdoutStr, `"protocolVersion"`) {
		t.Errorf("initialize response missing from stdout:\nSTDOUT=%q\nSTDERR=%q", stdoutStr, stderrStr)
	}
}
