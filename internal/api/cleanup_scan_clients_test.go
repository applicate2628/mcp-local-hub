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
	// Expected entries:
	want := []string{"mcp-language-server", "clangd", "pylsp", "server.js"}
	for _, w := range want {
		if !have[w] {
			t.Errorf("expected pattern %q in %v", w, got)
		}
	}
	// node should be filtered out by isBroadLauncherToken
	if have["node"] {
		t.Errorf("'node' should be filtered as broad launcher token; got %v", got)
	}
	// short args/flags must not appear
	for _, bad := range []string{"--lsp", "-p", "8080"} {
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

func TestPatternsFromClientStdio_NoInstalledClients(t *testing.T) {
	withHermeticHomeForCleanup(t)
	// Fresh empty home — no clients installed anywhere.
	got := patternsFromClientStdio()
	if len(got) != 0 {
		t.Errorf("expected nil/empty, got %v", got)
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

// TestParseOrphans_SkipsClientLauncherDescendant verifies that a
// process whose ancestor chain contains a known client launcher
// (claude.exe in this fixture) is NOT flagged as orphan, even when
// its cmdline matches a pattern. This is the A6 safety net against
// killing live stdio MCP children of currently-running clients.
func TestParseOrphans_SkipsClientLauncherDescendant(t *testing.T) {
	// 3-row CSV: claude.exe (root) → node.exe (child).
	// Header line is required (parseOrphans skips it).
	// Fields: Node, CommandLine, CreationDate, ParentProcessId, ProcessId, WorkingSetSize
	csv := `Node,CommandLine,CreationDate,ParentProcessId,ProcessId,WorkingSetSize
host,"C:\Users\u\AppData\Local\Programs\claude\claude.exe",20260515103000.000000+000,1000,2000,150000000
host,"node.exe c:\path\to\server.js",20260515102000.000000+000,2000,3000,80000000
`
	patterns := []string{"server.js"}
	out := parseOrphans(strings.NewReader(csv), patterns)
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
	csv := `Node,CommandLine,CreationDate,ParentProcessId,ProcessId,WorkingSetSize
host,"C:\Windows\explorer.exe",20260515090000.000000+000,1000,4000,200000000
host,"node.exe c:\path\to\server.js",20260515102000.000000+000,4000,3000,80000000
`
	patterns := []string{"server.js"}
	out := parseOrphans(strings.NewReader(csv), patterns)
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
