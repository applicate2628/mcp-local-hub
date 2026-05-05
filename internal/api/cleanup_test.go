package api

import (
	"strings"
	"testing"
)

// TestIsBroadLauncherToken covers the input forms the normalizer must
// strip before comparing against the launcher denylist. PR #121's
// initial implementation only matched bare lowercase tokens and missed
// .exe-suffixed names, absolute paths, and quoted forms that real
// wmic CommandLine output contains.
func TestIsBroadLauncherToken(t *testing.T) {
	broad := []string{
		"node", "npx", "python", "python3", "py", "uv", "uvx",
		// .exe / .cmd / .bat / .ps1 suffixed (Windows scripts call these)
		"node.exe", "npx.cmd", "uvx.exe", "python.exe", "python3.exe",
		// absolute paths (wmic on Windows often emits these)
		`C:\Program Files\nodejs\node.exe`,
		`C:\Users\dima_\AppData\Roaming\npm\npx.cmd`,
		`/usr/bin/python3`,
		// quoted (PowerShell command-line style)
		`'node'`, `"python"`,
		// upper / mixed case
		"NODE", "Python.EXE",
		// trailing whitespace
		"node ", "  npx  ",
		// empty / whitespace-only — should be filtered too so we don't
		// pollute the pattern list
		"", "   ",
	}
	for _, p := range broad {
		if !isBroadLauncherToken(p) {
			t.Errorf("isBroadLauncherToken(%q) = false, want true", p)
		}
	}
	specific := []string{
		"GDB-MCP", "wolfram", "paper-search-mcp", "node-grpc-server",
		"my-python-tool", "/usr/bin/wolfram",
	}
	for _, p := range specific {
		if isBroadLauncherToken(p) {
			t.Errorf("isBroadLauncherToken(%q) = true, want false (specific identifier)", p)
		}
	}
}

// TestParseOrphanDetectionIgnoresOurDaemons verifies that a wmic line whose
// CommandLine references our own daemon invocation (`mcphub.exe daemon`) is
// NOT counted as an orphan — the child of that daemon is legitimate.
func TestParseOrphanDetectionIgnoresOurDaemons(t *testing.T) {
	wmicCsv := `Node,CommandLine,CreationDate,ParentProcessId,ProcessId,WorkingSetSize
HOST,"uv run --directory .../GDB-MCP python server.py",20260417180000.000000+180,555,1001,40000000
HOST,"D:\dev\mcp-local-hub\mcphub.exe daemon --server gdb --daemon default",20260417180000.000000+180,999,555,15000000
HOST,"uv run --directory .../GDB-MCP python server.py",20260417170000.000000+180,1,2002,42000000
`
	orphans := parseOrphans(strings.NewReader(wmicCsv), []string{"GDB-MCP"})
	// PID 1001 has parent 555 which is mcphub.exe daemon — NOT orphan.
	// PID 2002 has parent 1 — ORPHAN.
	if len(orphans) != 1 {
		t.Fatalf("expected 1 orphan, got %d", len(orphans))
	}
	if orphans[0].PID != 2002 {
		t.Errorf("orphan PID: got %d, want 2002", orphans[0].PID)
	}
}
