// Tests for the A6 expansion of CleanupOrphans:
//   - Client.AllStdioEntries plumbing (covered in clients pkg already)
//   - patternsFromClientStdio: per-entry pattern emission, dedup,
//     broad-launcher skip, short-arg skip, leading-dash skip
//   - isKnownClientLauncher: basename allowlist, cross-platform
//     separator handling, .exe stripping
//   - parseOrphans honors the client-launcher allowlist when walking
//     the parent chain

package api

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withHermeticHomeForCleanup redirects every adapter constructor
// (Client.AllStdioEntries reads via os.UserHomeDir) to a fresh
// temp dir for the duration of the test. Returns the tmp dir so
// callers can seed per-client config files at known locations.
func withHermeticHomeForCleanup(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmp)
	} else {
		t.Setenv("HOME", tmp)
	}
	return tmp
}

func writeCleanupFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPatternsFromClientStdio_AggregatesAndDedups(t *testing.T) {
	home := withHermeticHomeForCleanup(t)

	// Seed codex with one stdio entry + one HTTP entry.
	// stdio should contribute "mcp-language-server" + "clangd" (≥4 chars,
	// not a flag) but NOT "--lsp" (starts with -). HTTP entry contributes
	// nothing (no command).
	writeCleanupFile(t, filepath.Join(home, ".codex", "config.toml"), `
[mcp_servers.lsp1]
command = "mcp-language-server"
args = ["--lsp", "clangd"]

[mcp_servers.gdb]
url = "http://localhost:9129/mcp"
`)
	// Seed claude with a DUPLICATE mcp-language-server entry (for a
	// different language) to verify dedup. Also adds a node.js entry
	// where "node" must be filtered by isBroadLauncherToken but
	// "server.js" must contribute.
	writeCleanupFile(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {
    "lsp2": {
      "type": "stdio",
      "command": "mcp-language-server",
      "args": ["--lsp", "pylsp"]
    },
    "nodey": {
      "type": "stdio",
      "command": "node",
      "args": ["server.js", "-p", "8080"]
    }
  }
}`)

	got := patternsFromClientStdio()

	// Convert to set for assertion.
	have := map[string]bool{}
	for _, p := range got {
		have[p] = true
	}
	// Args go through the stricter argIsDiscriminatingPattern
	// filter (length ≥ 8, contains `-@/\.`, not numeric, not a
	// flag). Short generic words like "clangd"/"pylsp" no longer
	// reach the pattern set on their own.
	want := []string{"mcp-language-server", "server.js"}
	for _, w := range want {
		if !have[w] {
			t.Errorf("expected pattern %q in %v", w, got)
		}
	}
	// node should be filtered out by isBroadLauncherToken (broad
	// interpreter token; also fails argIsDiscriminatingPattern's
	// length floor).
	if have["node"] {
		t.Errorf("'node' should be filtered as broad launcher token; got %v", got)
	}
	// Short / flag-shaped / numeric / bare-word args must not appear.
	for _, bad := range []string{"--lsp", "-p", "8080", "clangd", "pylsp"} {
		if have[bad] {
			t.Errorf("disallowed pattern %q present in %v", bad, got)
		}
	}
	// Dedup: "mcp-language-server" must appear exactly once even though
	// codex and claude both reference it.
	count := 0
	for _, p := range got {
		if p == "mcp-language-server" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("dedup failed: 'mcp-language-server' appears %d times in %v", count, got)
	}
}

// TestCleanupOrphans_RefusesServerWithScanClients covers codex bot
// r3 P1: --scan-clients + --server is an out-of-scope kill risk
// because client stdio entries have no server-name key. The two
// flags must be mutually exclusive — opting in to both yields an
// error, not a silent expansion of allPatterns.
func TestCleanupOrphans_RefusesServerWithScanClients(t *testing.T) {
	a := NewAPI()
	_, err := a.CleanupOrphans(CleanupOpts{
		Server:            "some-server",
		ScanClientConfigs: true,
		DryRun:            true,
	})
	if err == nil {
		t.Fatal("expected error for --scan-clients + --server combination")
	}
	if !strings.Contains(err.Error(), "incompatible") {
		t.Errorf("error = %v, want phrase 'incompatible'", err)
	}
}

// TestPatternsFromClientStdio_SkipsDisabledEntry covers codex bot
// r3 P2: an entry marked `"disabled": true` (Antigravity / Cursor /
// VS Code / jsonMCPClient adapters all support the flag) is not
// running and must not contribute kill-patterns. Deriving a pattern
// from a disabled entry would risk matching an unrelated process
// that happens to share the same signature.
func TestPatternsFromClientStdio_SkipsDisabledEntry(t *testing.T) {
	home := withHermeticHomeForCleanup(t)
	writeCleanupFile(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {
    "active-mcp": {
      "command": "my-mcp-server",
      "args": ["--config", "production-config.yaml"]
    },
    "disabled-mcp": {
      "command": "should-be-skipped",
      "args": ["--config", "old-config-name.yaml"],
      "disabled": true
    }
  }
}`)
	got := patternsFromClientStdio()
	have := map[string]bool{}
	for _, p := range got {
		have[p] = true
	}
	if !have["my-mcp-server"] {
		t.Errorf("active entry's command 'my-mcp-server' missing; got %v", got)
	}
	if !have["production-config.yaml"] {
		t.Errorf("active entry's discriminating arg 'production-config.yaml' missing; got %v", got)
	}
	if have["should-be-skipped"] {
		t.Errorf("disabled entry's command leaked into pattern set; got %v", got)
	}
	if have["old-config-name.yaml"] {
		t.Errorf("disabled entry's arg leaked into pattern set; got %v", got)
	}
}

// TestPatternsFromClientStdio_SkipsEnabledFalseEntry covers the
// codex-CLI convention: codex uses `enabled: false` (default true)
// instead of the `disabled` flag. Manual smoke on PR #190 found
// that a codex entry like `[mcp_servers.go]` with
// `command='gopls' enabled=false` was contributing the "gopls"
// pattern to the kill set. collectStdioEntries must skip
// enabled=false the same way it skips disabled=true.
func TestPatternsFromClientStdio_SkipsEnabledFalseEntry(t *testing.T) {
	home := withHermeticHomeForCleanup(t)
	writeCleanupFile(t, filepath.Join(home, ".codex", "config.toml"), `
[mcp_servers.go]
command = "gopls"
args = ["mcp"]
enabled = false

[mcp_servers.active]
command = "active-mcp-server"
args = ["--config", "active-config.yaml"]
`)
	got := patternsFromClientStdio()
	have := map[string]bool{}
	for _, p := range got {
		have[p] = true
	}
	if have["gopls"] {
		t.Errorf("enabled=false entry's command 'gopls' leaked into pattern set; got %v", got)
	}
	if !have["active-mcp-server"] {
		t.Errorf("active entry's command missing; got %v", got)
	}
}

func TestArgIsDiscriminatingPattern(t *testing.T) {
	cases := map[string]bool{
		// Pass: length ≥ 8 AND contains `-@/\.`
		"@playwright/mcp@latest":       true,  // @scoped pkg
		"mcp-server-fetch":             true,  // dashed name
		"my-mcp-server":                true,  // dashed name
		"server.js":                    true,  // 9 chars, has .
		"production-config.yaml":       true,  // multi-word with .
		"C:/Users/x/server.py":         true,  // path with / and .
		`C:\Users\x\server.py`:         true,  // path with \ and .
		"start-mcp-server":             true,  // dashed
		// Fail: length < 8
		"":      false,
		"x":     false,
		"clangd": false, // 6 chars
		"pylsp":  false, // 5 chars
		"memory": false, // 6 chars
		"false":  false, // 5 chars
		"3.13":   false, // 4 chars (also numeric+dot but length filter wins)
		// Fail: flag (starts with -)
		"--lsp":                false,
		"--some-very-long-flag": false,
		// Fail: all-digits
		"12345678": false,
		// Fail: ≥ 8 chars but no separator (bare word, generic risk)
		"localhost":   false,
		"production":  false,
		"executing":   false,
		"absolutevalues": false,
	}
	for in, want := range cases {
		if got := argIsDiscriminatingPattern(in); got != want {
			t.Errorf("argIsDiscriminatingPattern(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPatternsFromClientStdio_NoInstalledClients(t *testing.T) {
	withHermeticHomeForCleanup(t)
	// Fresh empty home — no clients installed anywhere.
	got := patternsFromClientStdio()
	if len(got) != 0 {
		t.Errorf("expected nil/empty, got %v", got)
	}
}

// TestPatternsFromClientStdio_WrapperArgInterpretersFiltered covers
// codex bot r1 P1.1 on PR #190: a wrapper stdio entry like
// `{command:"uv", args:["run","python","my-mcp.py"]}` previously
// emitted "python" / "node" / "npx" through the args branch
// because isBroadLauncherToken was applied only to the command
// basename. Without this guard, parseOrphans would flag every
// random python.exe on the workstation as orphan.
func TestPatternsFromClientStdio_WrapperArgInterpretersFiltered(t *testing.T) {
	home := withHermeticHomeForCleanup(t)
	writeCleanupFile(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {
    "wrap1": {
      "command": "uv",
      "args": ["run", "python", "my-mcp.py"]
    },
    "wrap2": {
      "command": "uvx",
      "args": ["--from", "git+...", "node", "server.js"]
    },
    "wrap3": {
      "command": "/usr/local/bin/uvx",
      "args": ["npx", "my-server"]
    }
  }
}`)
	got := patternsFromClientStdio()
	have := map[string]bool{}
	for _, p := range got {
		have[p] = true
	}
	// All bare-interpreter args must be filtered.
	for _, banned := range []string{"python", "node", "npx", "uv", "uvx", "python3"} {
		if have[banned] {
			t.Errorf("broad-launcher token %q must NOT be in pattern set; got %v", banned, got)
		}
	}
	// Discriminating args (the actual server names) survive.
	for _, want := range []string{"my-mcp.py", "server.js", "my-server"} {
		if !have[want] {
			t.Errorf("expected discriminating arg %q in %v", want, got)
		}
	}
}

func TestPatternsFromClientStdio_NumericOnlyArgsDropped(t *testing.T) {
	// "8080" passes the length floor (4 chars) and does not start
	// with '-', but it is purely numeric so isAllDigits drops it.
	// Numeric-only args (ports, PIDs, timeouts) substring-match
	// unrelated processes (e.g. a random web server on :8080) and
	// would produce false-positive orphan flags.
	home := withHermeticHomeForCleanup(t)
	writeCleanupFile(t, filepath.Join(home, ".claude.json"), `{
  "mcpServers": {
    "x": {"type":"stdio","command":"foo","args":["8080","server.js"]}
  }
}`)
	got := patternsFromClientStdio()
	have := map[string]bool{}
	for _, p := range got {
		have[p] = true
	}
	if have["8080"] {
		t.Errorf("'8080' should be dropped (numeric-only); got %v", got)
	}
	if !have["server.js"] {
		t.Errorf("'server.js' should be emitted (mixed alphanumeric); got %v", got)
	}
}

func TestIsAllDigits(t *testing.T) {
	cases := map[string]bool{
		"":       false,
		"0":      true,
		"123":    true,
		"8080":   true,
		"abc":    false,
		"12a3":   false,
		"a123":   false,
		"123.45": false,
		"-123":   false,
	}
	for in, want := range cases {
		if got := isAllDigits(in); got != want {
			t.Errorf("isAllDigits(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIsKnownClientLauncher(t *testing.T) {
	cases := []struct {
		in   string
		want bool
		why  string
	}{
		{"", false, "empty cmdline rejected"},
		{"claude.exe", true, "bare claude"},
		{"claude", true, "POSIX claude"},
		{"Claude.exe", true, "case-insensitive"},
		{`C:\Users\u\AppData\Local\Programs\claude\claude.exe`, true, "Windows absolute path"},
		{`/Applications/Claude.app/Contents/MacOS/claude`, true, "macOS bundle binary"},
		{"codex.exe --some flag", true, "with trailing args"},
		{"gemini-cli.exe", false, "different-named launcher not in allowlist"},
		{"cursor.exe", true, "cursor IDE"},
		{"code.exe", true, "VS Code binary"},
		{"cascade.exe", true, "Antigravity Cascade IDE"},
		{"antigravity.exe", true, "Antigravity ship-name"},
		{"qwen.exe", true, "Qwen CLI"},
		// Codex bot r1 P1.2 on PR #190: wrapper-based installs use
		// .cmd/.bat/.ps1 shims around the real binary. Without
		// suffix normalization beyond .exe, the ancestor guard
		// failed and stdio children of live wrapper-launched
		// clients were eligible for killing.
		{"claude.cmd", true, "Windows .cmd wrapper install"},
		{"codex.bat", true, "Windows .bat wrapper install"},
		{"gemini.ps1", true, "PowerShell .ps1 wrapper install"},
		{"GEMINI.CMD --foo", true, "case-insensitive .cmd wrapper"},
		{"node.exe --inspect server.js", false, "node is not a launcher"},
		{"python.exe -m foo", false, "python is not a launcher"},
		{"explorer.exe", false, "explorer is the re-parenting target, not a launcher"},
		{`"C:\Program Files\Cursor\Cursor.exe" --some flag`, true, "quoted Windows path with embedded space"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := isKnownClientLauncher(tc.in)
			if got != tc.want {
				t.Errorf("isKnownClientLauncher(%q) = %v, want %v (%s)", tc.in, got, tc.want, tc.why)
			}
		})
	}
}

func TestStripExtension(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"mcphub":            "mcphub",
		"mcphub.exe":        "mcphub",
		"MCPHUB.EXE":        "MCPHUB",   // case-insensitive suffix match, preserves case of body
		"server.cmd":        "server",
		"start.bat":         "start",
		"deploy.ps1":        "deploy",
		"foo.bar":           "foo.bar",  // unrecognized extension preserved
		"x.exe.bak":         "x.exe.bak", // .bak is unrecognized; do NOT strip the .exe behind it
	}
	for in, want := range cases {
		if got := stripExtension(in); got != want {
			t.Errorf("stripExtension(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBasenameAcrossSeparators_Cleanup(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"foo":          "foo",
		"foo/bar":      "bar",
		`foo\bar`:      "bar",
		`C:\x\y.exe`:   "y.exe",
		"/usr/bin/foo": "foo",
		"a/b\\c":       "c",
	}
	for in, want := range cases {
		if got := basenameAcrossSeparators(in); got != want {
			t.Errorf("basenameAcrossSeparators(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPatternsFromClientStdio_AntigravityMcphubCommandFiltered
// covers codex bot r5 P1 on PR #190: Antigravity stdio entries are
// written with `command = "...\mcphub.exe"` (a relay invocation,
// not the actual MCP server). The command-basename branch was
// emitting "mcphub" as a pattern, which then substring-matched any
// unrelated process whose cmdline mentioned `mcphub` (operator
// shell history, scripts, status displays). patternIsTooBroad
// must reject the mcphub binary basename in addition to client
// launchers and broad interpreters.
func TestPatternsFromClientStdio_AntigravityMcphubCommandFiltered(t *testing.T) {
	home := withHermeticHomeForCleanup(t)
	writeCleanupFile(t, filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"), `{
  "mcpServers": {
    "memory": {
      "command": "C:\\Users\\u\\AppData\\Roaming\\mcphub\\mcphub.exe",
      "args": ["relay", "--server", "memory", "--daemon", "claude"],
      "disabled": false
    }
  }
}`)
	got := patternsFromClientStdio()
	have := map[string]bool{}
	for _, p := range got {
		have[p] = true
	}
	if have["mcphub"] {
		t.Errorf("'mcphub' must NOT be in pattern set (Antigravity relay command basename); got %v", got)
	}
	if have["mcp"] {
		t.Errorf("'mcp' (legacy binary name) must NOT be in pattern set; got %v", got)
	}
	// Antigravity entries write `command = mcphub.exe` (filtered)
	// and args = ["relay","--server","<s>","--daemon","<client>"].
	// All args are either short (<8: "memory", "claude") or
	// flag-shaped (--server, --daemon) so the new
	// argIsDiscriminatingPattern filter rejects them all. With
	// every arg dropped AND the command filtered, the entry
	// contributes ZERO patterns — which is the safe outcome:
	// Antigravity relay entries have no discriminating signal we
	// can reliably extract without risking false positives.
	if len(got) > 0 {
		t.Errorf("Antigravity-only fixture should contribute zero patterns; got %v", got)
	}
}

// TestPatternsFromClientStdio_AntigravityRelaySecurityFindingPoC
// pins the codex security finding e8745334 regression: when an
// Antigravity relay entry references a long server name (>= 8 chars,
// with a separator — passes argIsDiscriminatingPattern), the args
// would emit those server names as global kill patterns. The new
// per-entry skip (any entry whose command is the mcphub binary)
// stops the leak at the source — no patterns whatsoever from
// any Antigravity relay entry, regardless of arg length.
//
// Original PoC server name "time" was already blocked by the
// 8-char-minimum argIsDiscriminatingPattern, but a longer server
// name like "paper-search-mcp" would have survived. This test uses
// the LONG-name shape to confirm the skip-by-command path.
func TestPatternsFromClientStdio_AntigravityRelaySecurityFindingPoC(t *testing.T) {
	home := withHermeticHomeForCleanup(t)
	writeCleanupFile(t, filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"), `{
  "mcpServers": {
    "paper-search-mcp": {
      "command": "C:\\Users\\u\\AppData\\Roaming\\mcphub\\mcphub.exe",
      "args": ["relay", "--server", "paper-search-mcp", "--daemon", "claude"],
      "disabled": false
    },
    "long-named-server": {
      "command": "C:\\path\\to\\mcphub.exe",
      "args": ["relay", "--server", "long-named-server", "--daemon", "codex"],
      "disabled": false
    }
  }
}`)
	got := patternsFromClientStdio()
	// EVERY Antigravity entry is skipped wholesale because command is
	// the mcphub binary. The fact that "paper-search-mcp" and
	// "long-named-server" would PASS argIsDiscriminatingPattern (≥8
	// chars, has separator) is irrelevant — the per-entry skip
	// happens BEFORE the per-arg filter.
	if len(got) > 0 {
		t.Errorf("Antigravity-only fixture must contribute ZERO patterns regardless of server-name length; got %v", got)
	}
}

func TestPatternIsTooBroad(t *testing.T) {
	cases := map[string]bool{
		"":                    true,
		"mcphub":              true,
		"mcphub.exe":          true,
		"MCPHUB":              true,
		"mcp":                 true,
		"mcp.exe":             true,
		"node":                true,
		"python":              true,
		"npx":                 true,
		"uv":                  true,
		"uvx":                 true,
		"claude":              true,
		"codex":               true,
		"claude.cmd":          true,
		"GEMINI":              true,
		"mcp-language-server": false,
		"memory":              false,
		"server.js":           false,
		"my-mcp-server":       false,
	}
	for in, want := range cases {
		if got := patternIsTooBroad(in); got != want {
			t.Errorf("patternIsTooBroad(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestPatternsFromClientStdio_AntigravityDaemonArgFiltered: the
// codex bot r2 P1 fix (filter known-client basenames out of args)
// is now superseded by the tighter argIsDiscriminatingPattern
// filter. Antigravity --daemon args like "claude" are 6 chars and
// fail the length floor. Belt-and-suspenders: the known-client
// filter in patternIsTooBroad ALSO drops them if a future loosening
// of the length floor would otherwise re-admit them.
func TestPatternsFromClientStdio_AntigravityDaemonArgFiltered(t *testing.T) {
	home := withHermeticHomeForCleanup(t)
	writeCleanupFile(t, filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"), `{
  "mcpServers": {
    "memory": {
      "command": "C:\\Users\\u\\AppData\\Roaming\\mcphub\\mcphub.exe",
      "args": ["relay", "--server", "memory", "--daemon", "claude"],
      "disabled": false
    }
  }
}`)
	got := patternsFromClientStdio()
	have := map[string]bool{}
	for _, p := range got {
		have[p] = true
	}
	for _, banned := range []string{"claude", "codex", "gemini", "cursor", "code", "qwen", "antigravity", "cascade"} {
		if have[banned] {
			t.Errorf("known launcher token %q must NOT be in pattern set (Antigravity relay --daemon arg); got %v", banned, got)
		}
	}
}

// TestParseOrphans_SkipsRowThatIsClientLauncher covers the row-level
// guard added in r2: even when patternsFromClientStdio is somehow
// fooled into emitting a launcher basename (Antigravity --daemon
// arg before the r2 fix, or any future bug introducing one), the
// parseOrphans loop must NOT flag a launcher process for killing.
// The ancestor walk only inspects parents, not the row itself.
func TestParseOrphans_SkipsRowThatIsClientLauncher(t *testing.T) {
	// claude.exe directly — parent is explorer.exe (no client in
	// ancestor chain). Pattern "claude" matches the row's cmdline.
	// Without the row-level guard, claude.exe would be flagged.
	csv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
host,"C:\Windows\explorer.exe",20260515090000.000000+000,C:\Windows\explorer.exe,1000,4000,200000000
host,"C:\Users\u\AppData\Local\Programs\claude\claude.exe --foo",20260515100000.000000+000,C:\Users\u\AppData\Local\Programs\claude\claude.exe,4000,5000,300000000
`
	patterns := []string{"claude"}
	swapOrphanParentState(t, deadParent) // deterministic: absent parent = dead = real orphan
	out, _ := parseOrphans(strings.NewReader(csv), patterns)
	for _, o := range out {
		if o.PID == 5000 {
			t.Errorf("PID 5000 (claude.exe itself) was flagged as orphan despite being a known launcher; row-level guard is failing. cmdline=%q", o.Cmdline)
		}
	}
}

// TestParseOrphans_SkipsClientLauncherDescendant verifies that a
// process whose ancestor chain contains a known client launcher
// (claude.exe in this fixture) is NOT flagged as orphan, even when
// its cmdline matches a pattern. This is the A6 safety net against
// killing live stdio MCP children of currently-running clients.
func TestParseOrphans_SkipsClientLauncherDescendant(t *testing.T) {
	// 3-row CSV: claude.exe (root) → node.exe (child).
	// Header line is required (parseOrphans skips it).
	// Fields: Node, CommandLine, CreationDate, ExecutablePath, ParentProcessId, ProcessId, WorkingSetSize
	csv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
host,"C:\Users\u\AppData\Local\Programs\claude\claude.exe",20260515103000.000000+000,C:\Users\u\AppData\Local\Programs\claude\claude.exe,1000,2000,150000000
host,"node.exe c:\path\to\server.js",20260515102000.000000+000,C:\Program Files\nodejs\node.exe,2000,3000,80000000
`
	patterns := []string{"server.js"}
	swapOrphanParentState(t, deadParent) // deterministic: absent parent = dead = real orphan
	out, _ := parseOrphans(strings.NewReader(csv), patterns)
	for _, o := range out {
		if o.PID == 3000 {
			t.Errorf("PID 3000 (node.exe child of claude.exe) was flagged as orphan; should be skipped via known-client allowlist. cmdline=%q", o.Cmdline)
		}
	}
}

// TestParseOrphans_FlagsClientLessOrphan verifies the dual case:
// when the cmdline matches a pattern but NO client launcher (nor
// mcphub daemon) is in the ancestor chain, the process IS flagged
// as orphan. Re-parented orphans (parent=explorer.exe or svchost
// after the spawning client died) hit this path.
func TestParseOrphans_FlagsClientLessOrphan(t *testing.T) {
	// node.exe (PID 3000) parent=explorer.exe (PID 4000). Neither
	// is mcphub daemon nor a known client → orphan.
	csv := `Node,CommandLine,CreationDate,ExecutablePath,ParentProcessId,ProcessId,WorkingSetSize
host,"C:\Windows\explorer.exe",20260515090000.000000+000,C:\Windows\explorer.exe,1000,4000,200000000
host,"node.exe c:\path\to\server.js",20260515102000.000000+000,C:\Program Files\nodejs\node.exe,4000,3000,80000000
`
	patterns := []string{"server.js"}
	swapOrphanParentState(t, deadParent) // deterministic: absent parent = dead = real orphan
	out, _ := parseOrphans(strings.NewReader(csv), patterns)
	var found bool
	for _, o := range out {
		if o.PID == 3000 {
			found = true
		}
	}
	if !found {
		t.Errorf("PID 3000 (node.exe re-parented to explorer.exe) was NOT flagged as orphan; A6 reverse-lookup is failing.")
	}
}
