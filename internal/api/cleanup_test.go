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
		`C:\Users\alice\AppData\Roaming\npm\npx.cmd`,
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
	wmicCsv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
HOST,"uv run --directory .../GDB-MCP python server.py",20260417180000.000000+180,C:\Users\u\.local\bin\uv.exe,555,1001,40000000
HOST,"D:\dev\mcp-local-hub\mcphub.exe daemon --server gdb --daemon default",20260417180000.000000+180,D:\dev\mcp-local-hub\mcphub.exe,999,555,15000000
HOST,"uv run --directory .../GDB-MCP python server.py",20260417170000.000000+180,C:\Users\u\.local\bin\uv.exe,1,2002,42000000
`
	swapOrphanParentState(t, deadParent) // deterministic: absent parent = dead = real orphan
	orphans, _ := parseOrphans(strings.NewReader(wmicCsv), []string{"GDB-MCP"})
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

func TestParseProcessRowsKeepsCommaBearingExecutablePathAligned(t *testing.T) {
	const created = "20260417180000.123456+180"
	const cmdline = "node server.js --setting-sources=user,project,local"
	const exePath = `C:\Users\Doe, Jane\AppData\Local\Programs\nodejs\node.exe`
	wmicCsv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
HOST,` + cmdline + `,` + created + `,` + exePath + `,555,1001,40000000
`
	rows, byPID, _ := parseProcessRows(strings.NewReader(wmicCsv))
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.cmdline != cmdline {
		t.Errorf("cmdline = %q, want %q", row.cmdline, cmdline)
	}
	if !row.created.Equal(parseWmicDate(created)) {
		t.Errorf("created = %s, want %s", row.created, parseWmicDate(created))
	}
	if row.exePath != exePath {
		t.Errorf("exePath = %q, want %q", row.exePath, exePath)
	}
	if row.ppid != 555 {
		t.Errorf("ppid = %d, want 555", row.ppid)
	}
	if row.pid != 1001 {
		t.Errorf("pid = %d, want 1001", row.pid)
	}
	if row.ram != 40000000 {
		t.Errorf("ram = %d, want 40000000", row.ram)
	}
	if _, ok := byPID[1001]; !ok {
		t.Fatalf("byPID missing parsed PID 1001")
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
			in:   `"C:\Users\alice\AppData\Roaming\npm\node.exe" -y @modelcontextprotocol/server-memory`,
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
		// Codex bot PR #143 round 5 P2: extensionless executable WITH a
		// path separator (so the round 2 fix's "bare basename" guard
		// doesn't apply) plus a later argument containing `.exe`. The
		// extension scan used to anchor on the argument's `helper.exe`
		// because the search ran over the entire cmdline. Post-fix:
		// the character after the first whitespace is `-` (flag marker)
		// → first-whitespace terminates the path → returns `python`.
		{
			name: "extensionless path-with-separator + flag-arg + .exe in later arg (round 5 P2)",
			in:   `C:\tools\python -m server --cache C:\tmp\helper.exe`,
			want: "python",
		},
		{
			name: "extensionless POSIX path + flag-arg",
			in:   `/usr/local/bin/python3 -m server --hook /tmp/post.cmd`,
			want: "python3",
		},
		// Defense: when the character after the first whitespace is NOT
		// flag-like, the extension scan SHOULD still run (preserves the
		// WMIC-stripped quoted Windows path case from round 1).
		{
			name: "WMIC-stripped path with extension still resolves correctly after r5 fix",
			in:   `C:\Program Files\nodejs\node.exe -y server-memory`,
			want: "node.exe",
		},
		// Defense: extensionless path with a PATH-LIKE arg continuation
		// (no extension anywhere). Falls back to first-whitespace.
		{
			name: "extensionless path + non-flag arg without extension",
			in:   `/usr/local/bin/runner config.json`,
			want: "runner",
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

// TestIsOurOwnProcessSharedParser exercises isOurOwnProcess against the
// scenarios that exposed the parser-inconsistency safety bug: codex
// deep-sec PR #143 round 4 finding A1. Pre-fix, isOurOwnProcess used a
// naive first-space split and returned `Program` for a real
// `C:\Program Files\mcphub\mcphub.exe daemon ...` row, so parseOrphans
// would NOT have skipped it — a live hub daemon could be classified as
// orphan and killed. Post-fix, both functions delegate to the shared
// firstTokenBasename helper.
func TestIsOurOwnProcessSharedParser(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// The architecture lane's exact safety scenario: WMIC-stripped
		// quoted Windows path with embedded space lands on isOurOwnProcess
		// AS-IS. Before the parser unification this returned false (split
		// gave `Program`); after, the extension-anchor finds `.exe` and
		// the basename `mcphub.exe` matches the allowlist.
		{
			name: "Windows Program Files mcphub.exe (WMIC-stripped quotes) — would have been killed",
			in:   `C:\Program Files\mcphub\mcphub.exe daemon --server gdb`,
			want: true,
		},
		// POSIX-style Program Files path WITHOUT the .exe extension —
		// extension lookup fails, falls through to first-whitespace split.
		// The first whitespace is between `mcphub` and `daemon`, so the
		// first token is `C:\Program` and the basename is `Program`,
		// which is NOT in the allowlist. Documented to anchor the
		// behavior contract.
		{
			name: "Windows Program Files mcphub no-extension — false (first-space wins)",
			in:   `C:\Program Files\mcphub\mcphub daemon`,
			want: false,
		},
		{
			name: "quoted exe with args",
			in:   `"C:\path\mcp.exe" --foo`,
			want: true,
		},
		{
			name: "tab separator between exe and args",
			in:   "mcphub.exe\tdaemon",
			want: true,
		},
		{
			name: "mixed-case basename matches allowlist",
			in:   `MCPHUB.EXE daemon`,
			want: true,
		},
		// Negative: a real-world unrelated process under Program Files
		// must NOT match. The fix must not over-correct into a false
		// positive that would skip a real orphan.
		{
			name: "unrelated nodejs under Program Files — false",
			in:   `C:\Program Files\nodejs\node.exe -y server`,
			want: false,
		},
		// Negative: bare basename not in the allowlist.
		{
			name: "bare uvx — false",
			in:   `uvx mcp-server-time`,
			want: false,
		},
		// Defense: empty / whitespace-only must NOT panic and must NOT
		// classify as our process.
		{
			name: "empty",
			in:   ``,
			want: false,
		},
		{
			name: "whitespace only",
			in:   `   `,
			want: false,
		},
		// Codex deep-sec finding A3 / Q1: a cmdline that uses `\n`
		// between the executable and its first argument must still
		// parse as our process by basename — if the parser swallowed
		// the newline as part of the first token, isOurOwnProcess
		// would have returned false for legitimate mcphub.exe rows.
		{
			name: "newline separator (LF) between exe and args",
			in:   "mcphub.exe\ndaemon --server gdb",
			want: true,
		},
		{
			name: "Windows-style CRLF separator between exe and args",
			in:   "mcphub.exe\r\ndaemon",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isOurOwnProcess(tc.in)
			if got != tc.want {
				t.Errorf("isOurOwnProcess(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactCmdlineNewlineHandling guards codex deep-sec PR #143 round 4
// finding A3 / Q1: an executable cmdline that uses `\n` (or `\r\n`) as
// the separator between the exe path and its arguments MUST split at the
// newline. Pre-fix, only space/tab were treated as separators, so a
// `node.exe\n--api-key=sk-secret` cmdline merged the entire string into
// the "first token" and filepath.Base returned the WHOLE input — the
// API-key argument leaked into cmdline_display.
func TestRedactCmdlineNewlineHandling(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "newline between basename and arg containing API key",
			in:   "node.exe\n--api-key=sk-secret",
			want: "node.exe",
		},
		{
			name: "CRLF between basename and arg",
			in:   "python.exe\r\n-m server",
			want: "python.exe",
		},
		// Newline ends the first token like a space — the path resolves
		// to the basename of the directory + .exe component.
		{
			name: "Windows path with embedded space then newline-arg",
			in:   "C:\\Program Files\\foo\\bar.exe\n-arg",
			want: "bar.exe",
		},
		// CR alone (rare but legal as a separator) also ends the token.
		{
			name: "CR alone between basename and arg",
			in:   "node.exe\r--key=sk-leak",
			want: "node.exe",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactCmdlineForDisplay(tc.in)
			if got != tc.want {
				t.Errorf("redactCmdlineForDisplay(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Defense in depth: the secret/argument string must NEVER
			// appear in the output. Asserting absence catches future
			// regressions where the separator set silently shrinks.
			if strings.Contains(got, "sk-secret") || strings.Contains(got, "sk-leak") || strings.Contains(got, "--") {
				t.Errorf("redactCmdlineForDisplay(%q) leaked argv: %q", tc.in, got)
			}
		})
	}
}

// TestRedactCmdlineLengthCap verifies codex deep-sec PR #143 round 4
// finding S2: a pathologically long basename input must not bloat the
// JSON wire / orphan-table UI. The 256-byte cap is shared between the
// helper and any consumer; truncation appends "..." so the UI shows the
// "this was clipped" affordance.
func TestRedactCmdlineLengthCap(t *testing.T) {
	// Build a 1 KB basename — the entire string is one token (no
	// whitespace, no path separator), so firstTokenBasename treats it
	// as a single-token cmdline. After capping, length is exactly 256
	// and the suffix is the truncation marker.
	long := strings.Repeat("a", 1024)
	got := redactCmdlineForDisplay(long)
	if len(got) != 256 {
		t.Errorf("redactCmdlineForDisplay(1KB basename) length = %d, want 256", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("redactCmdlineForDisplay(1KB basename) = %q, want ... suffix", got[len(got)-10:])
	}
	// Defense: when the input is comfortably under the cap, output is
	// returned unchanged (no spurious truncation marker).
	short := "node.exe"
	if redactCmdlineForDisplay(short) != "node.exe" {
		t.Errorf("redactCmdlineForDisplay(short) modified an under-cap input")
	}
	// Boundary: a 256-byte basename must NOT be truncated (fits exactly).
	exact := strings.Repeat("b", 256)
	gotExact := redactCmdlineForDisplay(exact)
	if gotExact != exact {
		t.Errorf("redactCmdlineForDisplay(exactly 256) modified a fits-exactly input")
	}
	// Boundary: a 257-byte basename triggers the cap.
	over := strings.Repeat("c", 257)
	gotOver := redactCmdlineForDisplay(over)
	if len(gotOver) != 256 || !strings.HasSuffix(gotOver, "...") {
		t.Errorf("redactCmdlineForDisplay(257) = len=%d suffix=%q, want len=256 ending in ...", len(gotOver), gotOver[len(gotOver)-3:])
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
		Cmdline:        `"C:\Users\alice\private\workspace\node.exe" --api-key=sk-leakable`,
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
