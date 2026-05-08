package api

import (
	"encoding/json"
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
	// Cleanup-6: the parsed orphan must carry the unredacted Cmdline
	// (server-side use: manifest pattern matching, CLI display, NeverKill
	// enforcement) AND a redacted CmdlineDisplay populated by
	// redactCmdlineForDisplay (the wire-format field). For an unquoted
	// `uv run --directory .../GDB-MCP python server.py` the basename is
	// `uv` — args and paths are stripped.
	if orphans[0].Cmdline == "" {
		t.Error("orphan.Cmdline empty; want raw command line preserved server-side")
	}
	if orphans[0].CmdlineDisplay != "uv" {
		t.Errorf("orphan.CmdlineDisplay = %q, want %q (basename only)", orphans[0].CmdlineDisplay, "uv")
	}
}

// TestRedactCmdlineForDisplay covers Cleanup-6 wire-format redaction:
// the GUI orphan table renders OrphanProcess.cmdline_display; that field
// must hold only the executable basename (no path, no arguments) so
// operator screenshots and browser dev-tools don't leak workspace paths,
// usernames, or possible API-keys-in-args. See cleanup.go comments for
// the full design rationale.
func TestRedactCmdlineForDisplay(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "quoted Windows path with args",
			in:   `"C:\Users\dima\AppData\Roaming\npm\node.exe" -y @modelcontextprotocol/server-memory`,
			want: "node.exe",
		},
		{
			name: "unquoted Windows exe with args",
			in:   `python.exe -m mcp_server`,
			want: "python.exe",
		},
		{
			name: "POSIX absolute path with args",
			in:   `/usr/local/bin/uvx mcp-server-time`,
			want: "uvx",
		},
		{
			name: "empty",
			in:   ``,
			want: "<unknown>",
		},
		{
			name: "whitespace only",
			in:   `   `,
			want: "<unknown>",
		},
		{
			name: "bare basename no args",
			in:   `node`,
			want: "node",
		},
		{
			name: "quoted with spaces in path",
			in:   `"C:\Program Files\nodejs\node.exe" server.js`,
			want: "node.exe",
		},
		{
			name: "unterminated quote (malformed) does not panic",
			in:   `"C:\path\foo.exe`,
			want: "foo.exe",
		},
		{
			name: "POSIX with shell args containing flags",
			in:   `/usr/bin/python3 -m server --api-key=sk-secret`,
			want: "python3",
		},
		{
			name: "tab separator between exe and args",
			in:   "node.exe\t-y\t@scope/pkg",
			want: "node.exe",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactCmdlineForDisplay(tc.in)
			if got != tc.want {
				t.Errorf("redactCmdlineForDisplay(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestOrphanProcessJSONOmitsRawCmdline guards the wire-format invariant:
// the raw `Cmdline` field MUST NOT appear in JSON output (it carries
// workspace paths and possible secrets-in-args). Only `cmdline_display`
// — the basename — is exposed to the GUI.
func TestOrphanProcessJSONOmitsRawCmdline(t *testing.T) {
	op := OrphanProcess{
		PID:            1234,
		ParentID:       1,
		Server:         "memory",
		RAMBytes:       100 * 1024 * 1024,
		Cmdline:        `"C:\Users\dima\private\workspace\node.exe" --api-key=sk-leakable`,
		CmdlineDisplay: "node.exe",
		AgeSec:         60,
	}
	b, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	if strings.Contains(js, "sk-leakable") {
		t.Errorf("JSON contains the raw cmdline argv (leak): %s", js)
	}
	if strings.Contains(js, "private\\\\workspace") || strings.Contains(js, "private/workspace") {
		t.Errorf("JSON contains the raw cmdline path (leak): %s", js)
	}
	if !strings.Contains(js, `"cmdline_display":"node.exe"`) {
		t.Errorf("JSON missing cmdline_display field: %s", js)
	}
	// Sanity: the legacy `"cmdline":...` key must NOT appear (only
	// `cmdline_display` does — substring `"cmdline":` would only match
	// the legacy key, not `"cmdline_display"`).
	if strings.Contains(js, `"cmdline":`) {
		t.Errorf("JSON still contains legacy `cmdline` key: %s", js)
	}
}
