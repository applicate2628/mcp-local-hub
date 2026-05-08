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
		// Codex bot PR #143 round 1 P2: WMIC's splitCSVLine strips quotes
		// from CommandLine fields, so a process launched as
		// `"C:\Program Files\nodejs\node.exe" -y server-memory` arrives
		// at parseOrphans as the unquoted-with-embedded-spaces form below.
		// Pre-fix: first-space split produced "Program" (basename of
		// "C:\Program"). Post-fix: findWindowsExeExtensionEnd locates
		// `.exe ` and the boundary stays inside the path → "node.exe".
		{
			name: "Windows path with embedded space (WMIC-stripped quotes)",
			in:   `C:\Program Files\nodejs\node.exe -y @modelcontextprotocol/server-memory`,
			want: "node.exe",
		},
		{
			name: "Windows path with embedded space + uppercase EXE",
			in:   `C:\Program Files\My Tool\App.EXE --port 8080`,
			want: "App.EXE",
		},
		{
			name: "Windows .cmd with embedded space",
			in:   `C:\Program Files\Common Files\runner.cmd /c task`,
			want: "runner.cmd",
		},
		{
			name: "Windows .bat with embedded space",
			in:   `D:\Program Files (x86)\Tools\start.bat arg`,
			want: "start.bat",
		},
		{
			name: "Windows .ps1 with embedded space",
			in:   `C:\My Scripts\run.ps1 -Mode Verbose`,
			want: "run.ps1",
		},
		{
			name: "Windows .com legacy executable with embedded space",
			in:   `C:\Program Files\Old App\thing.com /flag`,
			want: "thing.com",
		},
		// Defense: argument that happens to mention an extension must NOT
		// take precedence over an earlier executable boundary. Here the
		// real exe is `app.exe` at the front; the trailing `.exe` inside
		// the argument should be ignored because we pick the EARLIEST
		// boundary that's followed by whitespace.
		{
			name: "argument containing .exe must not win over earlier executable boundary",
			in:   `app.exe --target C:\other\thing.exe`,
			want: "app.exe",
		},
		// POSIX-shaped path where a directory contains the literal string
		// ".exe" (rare but legal): no whitespace after the dir's `.exe`,
		// so the helper skips it and falls back to first-space splitting,
		// which here gives the actual first token correctly.
		{
			name: "POSIX path passes through (no .exe boundary)",
			in:   `/opt/my-app/bin/runner --config /etc/runner.conf`,
			want: "runner",
		},
		// Edge case: the cmdline IS just an exe path with embedded spaces
		// and NO arguments. End-of-string takes the place of whitespace
		// after the extension.
		{
			name: "Windows path with embedded space, no args",
			in:   `C:\Program Files\Foo\bar.exe`,
			want: "bar.exe",
		},
		// Codex bot PR #143 round 2 P2: when the actual executable is a
		// bare basename (no extension, no path separator) and an
		// argument later contains a Windows path WITH `.exe`, the
		// extension lookup must NOT pick the argument as the boundary.
		// Pre-fix: `uvx mcp-server-time --cache C:\tmp\helper.exe` →
		// `helper.exe` (wrong). Post-fix: first-token has no separator,
		// extension lookup is skipped, naive first-whitespace split
		// returns `uvx`.
		{
			name: "extensionless first token + argument with .exe path (regression guard)",
			in:   `uvx mcp-server-time --cache C:\tmp\helper.exe`,
			want: "uvx",
		},
		{
			name: "extensionless first token + argument with .cmd path",
			in:   `python3 -m server --hook C:\hooks\post.cmd`,
			want: "python3",
		},
		{
			name: "extensionless first token + argument referencing UNC path",
			in:   `node --inspect \\server\share\runner.exe`,
			want: "node",
		},
		// Defense: bare-basename token like `app.exe` (HAS extension but
		// NO path separator) — first-whitespace IS the boundary. The
		// extension lookup is bypassed to keep the basename path simple
		// and to avoid scanning past arguments that may contain
		// extension-shaped substrings.
		{
			name: "basename with extension but no path separator",
			in:   `app.exe arg1 C:\other\thing.exe`,
			want: "app.exe",
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
