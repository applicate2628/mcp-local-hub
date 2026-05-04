package api

import (
	"strings"
	"testing"
)

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

func TestParseOrphansIgnoresBroadLauncherTokens(t *testing.T) {
	wmicCsv := `Node,CommandLine,CreationDate,ParentProcessId,ProcessId,WorkingSetSize
HOST,"node /home/alice/work/unrelated-vite-dev-server.js",20260417170000.000000+180,1,4242,20000000
HOST,"npx create-vite unrelated-project",20260417170000.000000+180,1,4243,21000000
HOST,"python /tmp/server.py --serves-other-app",20260417170000.000000+180,1,4244,22000000
`
	for _, tok := range []string{"node", "npx", "python"} {
		if !isBroadLauncherToken(tok) {
			t.Fatalf("expected %q to be broad", tok)
		}
	}

	// After broad-token filtering in CleanupOrphans, parseOrphans sees no
	// matching patterns and should not classify any unrelated process.
	orphans := parseOrphans(strings.NewReader(wmicCsv), nil)
	if len(orphans) != 0 {
		t.Fatalf("expected 0 orphans, got %d", len(orphans))
	}
}
