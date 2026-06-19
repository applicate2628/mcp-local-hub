package api

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

func TestLauncherGuidance_KnownLaunchersAreActionable(t *testing.T) {
	for _, cmd := range []string{"uvx", "npx", "node", "python", "go", "mcp-language-server", "mcphub"} {
		disp, fix := LauncherGuidance(cmd)
		if disp == "" || fix == "" {
			t.Errorf("LauncherGuidance(%q) = (%q,%q); both must be non-empty", cmd, disp, fix)
		}
		if !strings.Contains(fix, "`") {
			t.Errorf("LauncherGuidance(%q) fix=%q names no concrete command (no backtick) — not actionable", cmd, fix)
		}
	}
}

func TestLauncherGuidance_UnknownLauncherStillActionable(t *testing.T) {
	disp, fix := LauncherGuidance("totally-made-up-launcher-xyz")
	if disp == "" || !strings.Contains(fix, "PATH") {
		t.Errorf("unknown launcher fallback not actionable: disp=%q fix=%q", disp, fix)
	}
}

func TestCheckServerReadiness_MissingLauncherReportedWithFix(t *testing.T) {
	m := &config.ServerManifest{Name: "demo", Command: "definitely-not-on-path-zzz"}
	rep := CheckServerReadiness(m)
	if rep.Ready {
		t.Fatalf("report Ready=true for a missing launcher; want false")
	}
	var found bool
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "launcher:") {
			found = true
			if r.OK {
				t.Errorf("launcher requirement OK=true for missing launcher")
			}
			if r.Fix == "" {
				t.Errorf("launcher requirement has empty Fix (not actionable)")
			}
		}
	}
	if !found {
		t.Fatalf("no launcher requirement in report: %+v", rep.Requirements)
	}
}

func TestCheckServerReadiness_PresentLauncherReady(t *testing.T) {
	// `go` is on PATH in every Go test environment. Assert the LAUNCHER
	// requirement is OK; overall Ready also depends on the canonical mcphub
	// binary + free ports, which are env-dependent in a test sandbox.
	m := &config.ServerManifest{Name: "demo", Command: "go"}
	rep := CheckServerReadiness(m)
	var launcherOK, found bool
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "launcher:") {
			found, launcherOK = true, r.OK
		}
	}
	if !found || !launcherOK {
		t.Fatalf("launcher requirement not OK for present `go`: %+v", rep.Requirements)
	}
}

func TestCheckServerReadinessByName_EmbeddedServer(t *testing.T) {
	// "memory" is an embedded manifest (command: npx). Resolves without
	// state/network. Its readiness must include a launcher requirement.
	rep, err := CheckServerReadinessByName("memory")
	if err != nil {
		t.Fatalf("CheckServerReadinessByName(memory): %v", err)
	}
	if rep.Server != "memory" {
		t.Errorf("report Server=%q, want memory", rep.Server)
	}
	var hasLauncher bool
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "launcher:") {
			hasLauncher = true
		}
	}
	if !hasLauncher {
		t.Errorf("memory readiness has no launcher requirement: %+v", rep.Requirements)
	}
}

func TestCheckServerReadinessByName_UnknownServerErrors(t *testing.T) {
	if _, err := CheckServerReadinessByName("no-such-server-zzz"); err == nil {
		t.Fatal("CheckServerReadinessByName(unknown) returned nil error; want resolve error")
	}
}

func TestAllServerReadiness_CoversEmbeddedServers(t *testing.T) {
	reports := AllServerReadiness()
	if len(reports) < 5 {
		t.Fatalf("AllServerReadiness returned %d reports; want >= 5 embedded servers", len(reports))
	}
	for _, rep := range reports {
		if rep.Server == "" {
			t.Errorf("report with empty Server name: %+v", rep)
		}
	}
}

func TestCheckServerReadiness_UnsetSecretIsOptionalNotBlocking(t *testing.T) {
	m := &config.ServerManifest{
		Name:    "demo",
		Command: "go", // on PATH in every Go test env
		Env:     map[string]string{"DEMO_KEY": "secret:demo_unset_key_zzz"},
	}
	rep := CheckServerReadiness(m)
	// The unset secret must be ADVISORY (Optional) so it does not block
	// readiness. (Overall Ready also depends on env-dependent mcphub/ports, so
	// assert the secret requirement's Optional flag — the real claim — not the
	// aggregate.)
	var secretReq *ReadinessRequirement
	for i := range rep.Requirements {
		if strings.HasPrefix(rep.Requirements[i].Name, "secret:") {
			secretReq = &rep.Requirements[i]
		}
	}
	if secretReq == nil {
		t.Fatalf("no per-key secret requirement in report: %+v", rep.Requirements)
	}
	if !secretReq.Optional {
		t.Errorf("secret requirement Optional=false; want true (advisory, not a blocker)")
	}
	if secretReq.OK {
		t.Errorf("unset secret OK=true; want false so the GUI prompts to fill it")
	}
	if secretReq.Fix == "" {
		t.Errorf("unset secret has no Fix guidance for the inline prompt")
	}
}

func TestCheckServerReadiness_RequiredBinariesSurfaced(t *testing.T) {
	m := &config.ServerManifest{
		Name:             "demo",
		Command:          "mcphub",
		RequiredBinaries: []string{"definitely-absent-binary-zzz"},
	}
	rep := CheckServerReadiness(m)
	var found bool
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "binary:") {
			found = true
			if r.OK {
				t.Errorf("absent required binary reported OK=true")
			}
			if r.Fix == "" {
				t.Errorf("absent binary has no Fix guidance")
			}
		}
	}
	if !found {
		t.Fatalf("no binary requirement for declared required_binaries: %+v", rep.Requirements)
	}
}

func TestCheckServerReadiness_RemoteHTTPSecretIsBlocking(t *testing.T) {
	rm := &config.ServerManifest{
		Name:      "remote-demo",
		Transport: config.TransportRemoteHTTP,
		URL:       "https://example.com/mcp",
		Headers:   map[string]string{"Authorization": "Bearer ${secret:demo_remote_token_zzz}"},
	}
	rep := CheckServerReadiness(rm)
	var found bool
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "secret (remote):") {
			found = true
			if r.Optional {
				t.Errorf("remote-http secret marked Optional; remote secrets are install-blocking")
			}
			if r.OK {
				t.Errorf("unset remote-http secret reported OK=true")
			}
		}
	}
	if !found {
		t.Fatalf("no remote secret requirement for ${secret:} in headers: %+v", rep.Requirements)
	}
}

func TestCheckServerReadiness_MalformedRemotePlaceholderBlocks(t *testing.T) {
	// A malformed ${secret:...} (space in key) is not matched by the
	// well-formed-key scan, but ExpandSecrets (which buildRemoteHTTPPlan runs)
	// rejects it — readiness must surface it as a blocking "remote config" req.
	m := &config.ServerManifest{
		Name:      "remote-bad",
		Transport: config.TransportRemoteHTTP,
		URL:       "https://example.com/mcp",
		Headers:   map[string]string{"Authorization": "Bearer ${secret:BAD KEY}"},
	}
	rep := CheckServerReadiness(m)
	// The BuildPlan dry-run ("install plan") is the single-owner gate that
	// runs the planner's ExpandSecrets over url/headers, catching the
	// malformed placeholder as a blocking requirement.
	var found bool
	for _, r := range rep.Requirements {
		if r.Name == "install plan" {
			found = true
			if r.OK {
				t.Errorf("malformed remote placeholder reported OK=true")
			}
			if !strings.Contains(r.Reason, "malformed") {
				t.Errorf("install-plan reason does not name the malformed placeholder: %q", r.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("malformed ${secret:} placeholder not surfaced via install-plan: %+v", rep.Requirements)
	}
}

func TestCheckServerReadiness_RelativeScriptCheckedPerDaemonCwd(t *testing.T) {
	// A node manifest with a RELATIVE base_args[0] and TWO daemons whose cwd
	// differs: the script exists under daemon A's cwd but NOT under daemon B's.
	// CheckServerReadiness must emit a per-daemon "script:" requirement for
	// EACH daemon cwd (Codex #377 r10) — A's OK, B's not-OK — not a single
	// check against m.Daemons[0] only. Without per-daemon resolution this test
	// would see only one script requirement.
	dirA := t.TempDir()
	dirB := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dirA, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "build", "index.js"), []byte("//"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &config.ServerManifest{
		Name:     "demo",
		Command:  "node",
		BaseArgs: []string{"build/index.js"},
		Daemons: []config.DaemonSpec{
			{Name: "a", Cwd: dirA},
			{Name: "b", Cwd: dirB},
		},
	}
	rep := CheckServerReadiness(m)
	var scriptCount int
	var aOK, bSeen, bOK bool
	for _, r := range rep.Requirements {
		if !strings.HasPrefix(r.Name, "script:") {
			continue
		}
		scriptCount++
		if strings.Contains(r.Name, "(a)") {
			aOK = r.OK
		}
		if strings.Contains(r.Name, "(b)") {
			bSeen, bOK = true, r.OK
		}
	}
	if scriptCount != 2 {
		t.Fatalf("want 2 per-daemon script requirements (one per daemon cwd); got %d: %+v", scriptCount, rep.Requirements)
	}
	if !aOK {
		t.Errorf("daemon a's script (present under its cwd) must be OK")
	}
	if !bSeen || bOK {
		t.Errorf("daemon b's script (absent under its cwd) must be present-and-not-OK; seen=%v ok=%v", bSeen, bOK)
	}
}

func TestNormalizeLauncher(t *testing.T) {
	cases := map[string]string{
		"uvx":              "uvx",
		"uvx.exe":          "uvx",
		`C:\tools\uvx.exe`: "uvx",
		"npm.cmd":          "npm",
		"python3":          "python3",
		"definitely-bare":  "definitely-bare",
	}
	for in, want := range cases {
		if got := normalizeLauncher(in); got != want {
			t.Errorf("normalizeLauncher(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRuntimeBehindLauncher_NpmAndNpxNeedNode(t *testing.T) {
	// npm shims are #!/usr/bin/env node scripts just like npx, and the command
	// is normalized first so npm.cmd / an absolute npx path still match (Codex
	// #377 r14).
	for _, c := range []string{"npx", "npm", "npm.cmd", `C:\nodejs\npx.exe`} {
		if rt := runtimeBehindLauncher(c); rt != "node" {
			t.Errorf("runtimeBehindLauncher(%q) = %q, want node", c, rt)
		}
	}
	if rt := runtimeBehindLauncher("go"); rt != "" {
		t.Errorf("go is self-contained; runtimeBehindLauncher = %q, want empty", rt)
	}
}

func TestManifestNeedsGit_NormalizesLauncher(t *testing.T) {
	m := &config.ServerManifest{Command: "uvx.exe", BaseArgs: []string{"--from", "git+https://x@abc", "pkg"}}
	if !manifestNeedsGit(m) {
		t.Error("uvx.exe with a git+ source must require git (normalized launcher)")
	}
	if manifestNeedsGit(&config.ServerManifest{Command: "uvx", BaseArgs: []string{"some-pkg"}}) {
		t.Error("uvx without a git+ source must not require git")
	}
}

func TestEntryScriptCheckTargets_EmptyCwdUsesProcessCwd(t *testing.T) {
	// A relative entry script with an EMPTY daemon cwd must still produce a
	// target (resolved against the process cwd, the inherited-launch-cwd proxy)
	// rather than being silently skipped (Codex #377 r14).
	m := &config.ServerManifest{
		Command:  "node",
		BaseArgs: []string{"build/index.js"},
		Daemons:  []config.DaemonSpec{{Name: "default", Cwd: ""}},
	}
	targets := entryScriptCheckTargets(m)
	if len(targets) != 1 {
		t.Fatalf("empty-cwd relative script must yield 1 target; got %d", len(targets))
	}
	if !filepath.IsAbs(targets[0].path) {
		t.Errorf("target path must be absolute (process-cwd-resolved); got %q", targets[0].path)
	}
}

func TestManifestNeedsGit_ScansDaemonTemplateExtraArgs(t *testing.T) {
	// A dynamic-pool manifest carries the git+ source in the daemon template's
	// extra_args_template (appended into the child argv), so it must be scanned
	// (Codex #377 r16).
	m := &config.ServerManifest{
		Command: "uvx",
		DaemonTemplate: &config.DaemonTemplate{
			ExtraArgsTemplate: []string{"--from", "git+https://x@abc", "pkg"},
		},
	}
	if !manifestNeedsGit(m) {
		t.Error("uvx manifest with git+ in daemon_template.extra_args_template must need git")
	}
}

func TestEntryScriptStatus_RejectsDirectory(t *testing.T) {
	// base_args[0] pointing at a directory (e.g. build/ instead of build/index.js)
	// cannot be run as an entry script — reject it like a missing file (Codex
	// #377 r16).
	dir := t.TempDir()
	if ok, reason := entryScriptStatus(dir); ok || !strings.Contains(reason, "directory") {
		t.Errorf("directory must be rejected as an entry script; ok=%v reason=%q", ok, reason)
	}
	f := filepath.Join(dir, "index.js")
	if err := os.WriteFile(f, []byte("//"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := entryScriptStatus(f); !ok {
		t.Error("an existing regular file must be accepted as an entry script")
	}
}

func TestCheckServerReadiness_NativeHTTPInternalPortRange(t *testing.T) {
	// External 60000 is valid, but internal = 60000 + NativeHTTPInternalPortOffset
	// overflows 65535 — the internal upstream port must be reported not-OK
	// (Codex #377 r16).
	origAvail := portAvailable
	portAvailable = func(int) bool { return true } // external bind probe deterministic
	t.Cleanup(func() { portAvailable = origAvail })
	m := &config.ServerManifest{
		Name:      "demo",
		Command:   "go",
		Transport: config.TransportNativeHTTP,
		Daemons:   []config.DaemonSpec{{Name: "default", Port: 60000}},
	}
	rep := CheckServerReadiness(m)
	var found, ok bool
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "internal port") {
			found, ok = true, r.OK
		}
	}
	if !found {
		t.Fatalf("no internal port requirement: %+v", rep.Requirements)
	}
	if ok {
		t.Error("internal upstream port (60000 + offset > 65535) must be not-OK")
	}
}

func TestEntryScriptCheckTargets_SetsPerDaemonName(t *testing.T) {
	// Each per-daemon (relative) target must carry its daemon name so Preflight
	// can filter to the installed daemon and not block on a sibling's missing
	// script (Codex #377 r15).
	dirA := t.TempDir()
	dirB := t.TempDir()
	m := &config.ServerManifest{
		Command:  "node",
		BaseArgs: []string{"build/index.js"},
		Daemons:  []config.DaemonSpec{{Name: "a", Cwd: dirA}, {Name: "b", Cwd: dirB}},
	}
	got := map[string]bool{}
	for _, c := range entryScriptCheckTargets(m) {
		got[c.daemon] = true
	}
	if !got["a"] || !got["b"] {
		t.Errorf("targets must carry per-daemon names a + b for the Preflight filter; got %v", got)
	}
}

func TestCheckServerReadiness_LldbBridgeInvalidAddressNotReady(t *testing.T) {
	// base_args[1] without a port — the lldb-bridge subcommand (lldb.ParseHostPort)
	// rejects it, so readiness must NOT mark it ready even if lldb is installed
	// (Codex #377 r15).
	m := &config.ServerManifest{
		Name:      "lldb",
		Transport: config.TransportStdioBridge,
		Command:   "mcphub",
		BaseArgs:  []string{"lldb-bridge", "localhost"}, // missing :port
	}
	rep := CheckServerReadiness(m)
	var found, ok bool
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "debugger:") {
			found, ok = true, r.OK
		}
	}
	if !found {
		t.Fatalf("no debugger requirement in report: %+v", rep.Requirements)
	}
	if ok {
		t.Error("debugger requirement OK=true for a malformed lldb-bridge address")
	}
}

func TestCheckServerReadiness_FixedPortOutOfRangeNotReady(t *testing.T) {
	// A fixed daemon port outside 1..65535 must be rejected — a failed TCP dial
	// on it would otherwise read as a free port (Codex #377 r15).
	m := &config.ServerManifest{
		Name:    "demo",
		Command: "go",
		Daemons: []config.DaemonSpec{{Name: "default", Port: 70000}},
	}
	rep := CheckServerReadiness(m)
	var found, ok bool
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "port 70000") {
			found, ok = true, r.OK
		}
	}
	if !found {
		t.Fatalf("no port requirement for the fixed daemon: %+v", rep.Requirements)
	}
	if ok {
		t.Error("port requirement OK=true for out-of-range port 70000")
	}
}

func TestLauncherGuidance_UnknownAbsoluteCommandBasenamed(t *testing.T) {
	// An unknown command that is an absolute host path must not leak its
	// directory through the display name OR the fix string — LauncherGuidance
	// feeds both the GUI-rendered Name and Fix (Codex #377 r12).
	abs := filepath.Join(t.TempDir(), "my-secret-tool")
	disp, fix := LauncherGuidance(abs)
	if strings.ContainsRune(disp, filepath.Separator) || strings.Contains(disp, "/") {
		t.Errorf("display leaks a path separator: %q", disp)
	}
	if strings.Contains(fix, filepath.Dir(abs)) {
		t.Errorf("fix leaks the directory %q: %q", filepath.Dir(abs), fix)
	}
}

func TestCheckServerReadiness_LldbBridgeListenerSatisfiesReadiness(t *testing.T) {
	// A live listener on the lldb-bridge's dial address satisfies readiness
	// with NO local lldb binary — the bridge reuses the listener. Asserts the
	// conditional debugger requirement's listener branch (Codex #377 r12).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	m := &config.ServerManifest{
		Name:      "lldb",
		Transport: config.TransportStdioBridge,
		Command:   "mcphub",
		BaseArgs:  []string{"lldb-bridge", ln.Addr().String()},
	}
	rep := CheckServerReadiness(m)
	var found, ok bool
	for _, r := range rep.Requirements {
		if strings.HasPrefix(r.Name, "debugger:") {
			found, ok = true, r.OK
		}
	}
	if !found {
		t.Fatalf("no debugger requirement in report: %+v", rep.Requirements)
	}
	if !ok {
		t.Errorf("debugger requirement OK=false despite a live listener on %s", ln.Addr().String())
	}
}

func TestCheckServerReadiness_ReasonsDoNotLeakAbsolutePaths(t *testing.T) {
	// A manifest whose launcher is an ABSOLUTE host path that does not exist:
	// the "not found on PATH" Reason must name only the basename, never the
	// directory — the GUI renders Reason verbatim, so an absolute host path in
	// it is an info leak (Codex #377 merge-gate P3). If the basename redaction
	// were reverted to a raw %q of m.Command, the Reason would contain the temp
	// dir and this test would fail.
	dir := t.TempDir() // a concrete absolute host path
	absCmd := filepath.Join(dir, "totally-absent-launcher-zzz")
	m := &config.ServerManifest{Name: "demo", Command: absCmd}
	rep := CheckServerReadiness(m)
	for _, r := range rep.Requirements {
		if r.OK {
			continue
		}
		if strings.Contains(r.Reason, dir) {
			t.Errorf("requirement %q Reason leaks the absolute host dir %q: %q", r.Name, dir, r.Reason)
		}
	}
}
