package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/daemonrecovery"
)

// withTempHome redirects SettingsPath to a tempdir for the test duration.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("XDG_DATA_HOME", dir)
	return filepath.Join(dir, "mcp-local-hub", "gui-preferences.yaml")
}

func runCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newSettingsCmdReal()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errb.String(), err
}

func TestCLI_List_GroupedBySection(t *testing.T) {
	withTempHome(t)
	out, _, err := runCLI(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"appearance:", "gui_server:", "daemons:", "backups:", "advanced:"} {
		if !strings.Contains(out, section) {
			t.Errorf("expected section %q in list output:\n%s", section, out)
		}
	}
}

func TestCLI_List_AnnotatesDeferred(t *testing.T) {
	withTempHome(t)
	out, _, err := runCLI(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[deferred") {
		t.Errorf("expected at least one [deferred] annotation:\n%s", out)
	}
	if !strings.Contains(out, "[restart required]") {
		t.Errorf("expected gui_server.port [restart required] annotation:\n%s", out)
	}
}

func TestCLI_List_PrintsCanonicalKeys_NotStripped(t *testing.T) {
	// Codex PR #20 r5 P2: list output must print canonical keys
	// (appearance.theme), not section-stripped (theme), so users can
	// copy directly into `mcp settings get/set`.
	withTempHome(t)
	out, _, err := runCLI(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "appearance.theme = ") {
		t.Errorf("expected canonical 'appearance.theme = ' in list output, got:\n%s", out)
	}
	if !strings.Contains(out, "gui_server.port = ") {
		t.Errorf("expected canonical 'gui_server.port = ' in list output, got:\n%s", out)
	}
	// The OLD (stripped) form must be absent. We use a tighter pattern
	// to avoid matching the section header line "appearance:" which
	// contains "appearance".
	// Old format had a 2-space indent + bare local name + " = "; e.g. "  theme = ".
	// Canonical now: "  appearance.theme = ". So a line starting with
	// "  theme = " would indicate regression.
	for _, badPrefix := range []string{"  theme =", "  density =", "  shell =", "  port =", "  keep_n ="} {
		if strings.Contains(out, badPrefix) {
			t.Errorf("unexpected stripped form %q in list output (Codex r5 P2 regression)", badPrefix)
		}
	}
}

func TestCLI_Get_UnknownKey_Exit1(t *testing.T) {
	withTempHome(t)
	_, _, err := runCLI(t, "get", "no.such.key")
	if err == nil || !strings.Contains(err.Error(), "unknown setting") {
		t.Fatalf("expected unknown-setting error, got %v", err)
	}
}

func TestCLI_Get_ActionKey_Exit1(t *testing.T) {
	withTempHome(t)
	_, _, err := runCLI(t, "get", "advanced.open_app_data_folder")
	if err == nil || !strings.Contains(err.Error(), "is an action") {
		t.Fatalf("expected is-action error, got %v", err)
	}
}

func TestCLI_Get_Deferred_PrintsValueAndStderrWarning(t *testing.T) {
	// gui_server.tray is the canonical Deferred:true TypeBool key that
	// Task 1 did NOT flip (PR #2 will flip it). daemons.weekly_schedule was
	// flipped to Deferred:false by Task 1 so it no longer emits the warning.
	withTempHome(t)
	stdout, stderr, err := runCLI(t, "get", "gui_server.tray")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Error("expected a value on stdout")
	}
	if !strings.Contains(stderr, "[deferred") {
		t.Errorf("expected stderr deferred warning, got %q", stderr)
	}
}

func TestCLI_Set_UnknownKey_Exit1(t *testing.T) {
	withTempHome(t)
	_, _, err := runCLI(t, "set", "no.such.key", "x")
	if err == nil || !strings.Contains(err.Error(), "unknown setting") {
		t.Fatalf("expected unknown-setting error, got %v", err)
	}
}

func TestCLI_Set_ActionKey_Exit1(t *testing.T) {
	withTempHome(t)
	_, _, err := runCLI(t, "set", "advanced.open_app_data_folder", "x")
	if err == nil || !strings.Contains(err.Error(), "cannot set action key") {
		t.Fatalf("expected cannot-set-action error, got %v", err)
	}
}

func TestCLI_Set_Validation_RejectsBadValue(t *testing.T) {
	withTempHome(t)
	_, _, err := runCLI(t, "set", "appearance.theme", "puce")
	if err == nil || !strings.Contains(err.Error(), "invalid value") {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestCLI_Set_DeferredNonAction_SucceedsWithStderrWarning(t *testing.T) {
	// gui_server.tray is the canonical Deferred:true TypeBool key that
	// Task 1 did NOT flip (PR #2 will flip it). daemons.retry_policy was
	// flipped to Deferred:false by Task 1 so it no longer emits the warning.
	withTempHome(t)
	_, stderr, err := runCLI(t, "set", "gui_server.tray", "false")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "deferred to A4-b") {
		t.Errorf("expected stderr deferred warning, got %q", stderr)
	}
	// Confirm value persisted.
	a := api.NewAPI()
	v, err := a.SettingsGet("gui_server.tray")
	if err != nil || v != "false" {
		t.Errorf("expected false persisted, got %q err=%v", v, err)
	}
}

// TestCLI_Get_LegacyKeyAlias verifies that pre-A4 unqualified names accepted
// by `mcp settings get theme` resolve to the canonical appearance.theme value.
// Codex PR #20 r13 P2: disk-side legacyKeyMap migrates YAML; this tests the
// mirror at the CLI lookup layer so existing scripts need no update.
func TestCLI_Get_LegacyKeyAlias(t *testing.T) {
	withTempHome(t)
	// Write via the canonical key so the value is definitely on disk.
	if _, _, err := runCLI(t, "set", "appearance.theme", "dark"); err != nil {
		t.Fatal(err)
	}
	// Read via the legacy alias — must succeed and return the written value.
	out, _, err := runCLI(t, "get", "theme")
	if err != nil {
		t.Fatalf("legacy alias 'theme' must resolve: %v", err)
	}
	if !strings.Contains(out, "dark") {
		t.Errorf("expected 'dark' from legacy 'theme' alias, got: %q", out)
	}
}

// TestCLI_Set_LegacyKeyAlias verifies that writing via a legacy alias lands
// at the canonical key, so a follow-up `mcp settings get appearance.shell`
// returns the value that was written as `mcp settings set shell bash`.
func TestCLI_Set_LegacyKeyAlias(t *testing.T) {
	withTempHome(t)
	if _, _, err := runCLI(t, "set", "shell", "bash"); err != nil {
		t.Fatalf("legacy alias 'shell' must resolve on set: %v", err)
	}
	// Confirm the write landed at the canonical key.
	out, _, err := runCLI(t, "get", "appearance.shell")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bash") {
		t.Errorf("expected canonical write to carry 'bash', got: %q", out)
	}
}

// TestCLI_LegacyAlias_DoesNotShadowNonLegacyKey guards the pass-through
// path: a canonical key that is NOT a legacy alias must still round-trip
// normally through lookupRegistry (regression guard for ResolveLegacyKey).
func TestCLI_LegacyAlias_DoesNotShadowNonLegacyKey(t *testing.T) {
	withTempHome(t)
	if _, _, err := runCLI(t, "set", "gui_server.port", "9300"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, "get", "gui_server.port")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "9300") {
		t.Errorf("non-legacy canonical key must round-trip unchanged, got: %q", out)
	}
}

// TestMain ensures any leftover env state from prior tests doesn't leak.
// The withTempHome helper sets envs per-test via t.Setenv, which is
// auto-restored. No extra setup needed.
//
// It also installs the supervisor IPC test-pipe discriminator so the many
// in-process supervisors a single `go test ./internal/cli/` run spins up under
// one user SID each bind a unique Windows pipe (derived from the per-test
// MCPHUB_STATE_DIR_OVERRIDE) instead of contending on the shared per-SID pipe
// (bug 2026-05-29-cli-supervise-ipc-tests-flaky-in-full-suite.md). This is a
// runtime hook, not a build tag, so it takes effect in the DEFAULT untagged
// `go test ./...` build that CI runs — and is absent from release binaries,
// which never call it (codex bot PR #264 P2). POSIX is a no-op.
//
// FLEET-SAFETY (2026-06-13 incident): a `go test ./internal/cli/` run executed
// a real `mcphub stop --all` against the LIVE fleet — a Migration-path test
// reached the real api.(*API).StopAll() with no per-test state-dir isolation,
// so it resolved the developer's real %LOCALAPPDATA%\mcp-local-hub and stopped
// all 22 daemons. The pre-incident TestMain isolated only the IPC pipe, NOT the
// daemon state dir, so any test that reached real state code (forgot its own
// SetDaemonStateRootForTest, was newly added, or fell through a preflight guard
// that doesn't abort on the host OS) hit the live state directory.
//
// To make it STRUCTURALLY IMPOSSIBLE for any internal/cli test to touch the
// real state dir, install a PROCESS-GLOBAL daemonStateRootOverride pointing at a
// throwaway temp dir for the whole `go test ./internal/cli/` process. This is a
// default safety net, not a replacement for per-test isolation: per-test
// SetDaemonStateRootForTest(t.TempDir()) calls still compose correctly because
// SetDaemonStateRootForTest captures the LIVE override value on entry and
// restores to it on exit — under this TestMain that captured value is the global
// temp dir, so a per-test override + restore can never expose the real dir.
//
// os.Exit-safety: defers do NOT run after os.Exit, so the cleanup is run
// explicitly after capturing m.Run()'s exit code.
//
// SUBPROCESS GAP + BELT-AND-SUSPENDERS (PR #300 r1 P1). The
// SetDaemonStateRootForTest override above is an IN-MEMORY package variable:
// it redirects state-dir resolution only inside THIS test process. A cli test
// that spawns a fresh `mcphub` binary as an OS subprocess (gui_integration_test
// `go run ./cmd/mcphub gui`, daemon_reliability_test built daemon) does NOT
// inherit that variable — the child reaches api.DaemonStateDir() and would
// resolve the REAL per-user %LOCALAPPDATA%\mcp-local-hub state + IPC pipe.
//
// As a default safety net for ANY such subprocess, also export the
// state-relevant env vars pointing at the SAME global temp dir. A child that
// inherits os.Environ() AND is built with the test_state_path_env tag (which
// the subprocess tests now do — see subprocess_state_isolation_test.go) then
// defaults to the temp dir even if a future test forgets to set them per-spawn.
//
// SURGICAL SCOPE: we set ONLY the mcphub-state-relevant vars (LOCALAPPDATA +
// XDG_DATA_HOME + XDG_STATE_HOME). We deliberately do NOT clobber HOME /
// USERPROFILE globally — those have broad side effects on other tests that read
// them. These three are safe to default globally because every in-process cli
// test that reads them (logBaseDir-touching daemon tests, withTempHome) sets
// its OWN value via t.Setenv, which overrides this global default and is
// auto-restored; tests that do NOT set them never read them in-process (the
// production in-process daemonStateDir ignores LOCALAPPDATA / XDG_DATA_HOME and
// uses the in-memory override). The only behavioral effect of the global
// default is to route a forgotten subprocess (or a forgotten in-process
// logBaseDir read) to the throwaway temp dir instead of the real fleet —
// strictly safer.
//
// Restore is handled by t.Setenv-style manual save/restore here since TestMain
// has no *testing.T: we snapshot the prior values and reinstate them before
// os.Exit so a parent harness env is left untouched.
func TestMain(m *testing.M) {
	dispatch := classifyCLITestHelperDispatch(os.Args, os.Getenv)
	if dispatch.invalid {
		_, _ = os.Stderr.WriteString("internal/cli: invalid test helper dispatch\n")
		os.Exit(3)
	}
	// Same-test-binary endpoint for the production hidden confirmation-marker
	// command. The parent still uses the real current executable and strict
	// process containment; this branch only replaces Cobra's ordinary main,
	// which a generated Go test binary does not contain.
	if len(os.Args) == 2 && os.Args[1] == "gui-owner-unknown-confirmation-worker" {
		if err := runGUIOwnerUnknownConfirmationMarkerWorker(os.Stdin, os.Stdout); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	if len(os.Args) == 2 && os.Args[1] == daemonrecovery.CommittedAuditHandoffWorkerCommand {
		if err := daemonrecovery.RunCommittedAuditHandoffWorker(os.Stdin, os.Stdout); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}

	// Env-dump helper fast-path. When the production spawn closure launches THIS
	// test binary as a child (Command=os.Args[0]) to capture the composed
	// cmd.Env, the child is gated ONLY by the sentinel env var — its argv may be
	// ANY shape the descriptor under test carries (e.g. a serena-proxy-shaped
	// `daemon serena-proxy …` argv, where a `-test.run` flag could not lead the
	// argv without breaking serena detection). Short-circuiting here, BEFORE
	// m.Run(), makes the child a pure env-dumper regardless of argv — it writes
	// its own os.Environ() (the closure's composed cmd.Env) to the dump path and
	// exits, never running the suite (so no recursion / full-suite re-run in the
	// child). The sentinel is unset under a normal `go test` run, so this is a
	// no-op there. Constants live in supervise_overlay_marker_spawn_test.go.
	if dispatch.overlayDump {
		dumpPath := os.Getenv(overlayMarkerHelperDumpPathEnv)
		if dumpPath == "" {
			// No dump path means the closure dropped the inherited parent env
			// entirely (the regression the nil-seed guards). Exit non-zero so the
			// parent's spawn-side wait observes a failure rather than an empty dump.
			os.Exit(3)
		}
		_ = os.WriteFile(dumpPath, []byte(strings.Join(os.Environ(), "\n")), 0o600)
		os.Exit(0)
	}

	// Stderr-sink crash-child fast-path. Same sentinel-gated shape as the
	// env-dump helper above: TestSupervisorStderrSink_CapturesRuntimePanic
	// re-execs THIS binary to prove that a real Go runtime panic lands in the
	// sink file. Proving that requires an actual process death, which cannot
	// be staged in-process. Short-circuits before m.Run() so the child never
	// runs the suite. Body lives in supervisor_death_forensics_test.go.
	if stateDir := dispatch.forensicsStateDir; stateDir != "" {
		runForensicsSinkCrashChild(stateDir)
	}

	// Built-in route-daemon spawn fast-path (Increment 1b,
	// work-items/decisions/2026-07-25-supervisor-builtin-singleton-daemon.md).
	// DEFENSE-IN-DEPTH, not the primary guard (reviewer O2): the
	// reconcileSpawnFn default installed below already makes the recursive-exec
	// path this guards against UNREACHABLE via the normal newSuperviseCmd/
	// runSupervise path, because no test in this binary ever reaches the real
	// production spawn closure (makeProductionSpawnFnWithStatePath) anymore
	// unless it calls that constructor directly (as TestProductionSpawnFn_*
	// already does, deliberately, with non-route argv). This fast-path exists
	// as a second, independent layer in case some FUTURE test constructs the
	// production spawn closure directly with a literal route-shaped
	// descriptor (Command=canonicalMcphubPath(), Args=["route","--port",...])
	// and forgets to point Command somewhere other than the test binary: in
	// this test binary, canonicalMcphubPath() resolves to THIS binary, so
	// `exec.Command(cmdPath, "route", "--port", <port>)` would launch THIS
	// test binary with argv[1]=="route" — a bare non-flag positional arg the
	// `flag` package stops parsing at, so `go test`'s own generated main()
	// would fall back to its DEFAULT flags (no `-test.run` filter) and re-run
	// the ENTIRE package's test suite recursively inside what is supposed to
	// be a lightweight spawned child. Unlike the two sentinel-gated helpers
	// above (deliberate, explicitly-invoked test doubles), this path is gated
	// on the descriptor's own fixed, deterministic argv shape rather than an
	// opt-in env sentinel. Mirrors the same "exit immediately, never call
	// m.Run()" shape as the two fast-paths above.
	if dispatch.productionArgv {
		os.Stderr.WriteString("internal/cli test binary invoked with production child argv[1]==\"" + os.Args[1] + "\" " +
			"— exiting immediately instead of " +
			"recursively running the test suite\n")
		os.Exit(0)
	}

	// Re-exec helper children deliberately terminate from inside their selected
	// test (or are terminated by the parent). Run those selected tests before
	// creating the package-owned state root: their process lifetime cannot reach
	// TestMain's normal post-m.Run cleanup, and they do not need package state.
	if dispatch.runSelectedTest {
		os.Exit(m.Run())
	}

	// Package-wide safe default for reconcileSpawnFn (Increment 1b). Before
	// ensureBuiltinRouteDaemonAtStartup existed, supervisor-intent.json was
	// ALWAYS empty in every test's fresh state dir, so runSupervise's initial
	// reconcile pass never had anything to spawn and reconcileSpawnFn==nil
	// (→ falls back to the REAL production spawn closure, supervise.go's
	// `if spawnFn == nil { spawnFn = makeProductionSpawnFnWithStatePath(...) }`)
	// was dead code for every test that doesn't explicitly stub it. Now the
	// built-in route daemon row is ALWAYS seeded, so any test that exercises
	// the real command (newSuperviseCmd / runSupervise) without calling
	// setReconcileSpawnFnForTest reaches a REAL exec.Command(canonicalMcphubPath(),
	// "route", "--port", ...) — i.e. a real child process, real Job-Object
	// lifecycle, and a real (fixed, shared) port bind, none of which any such
	// test was written to expect or tear down. The argv fast-path above closes
	// the worst case (the child recursively re-running this whole suite); this
	// default closes the rest of the class (child-process lifecycle/handle-
	// release races against the test's own tempdir cleanup, and a fixed-port
	// bind that a genuinely concurrent test run could collide on) by making
	// the SAME default runSupervise already falls back to for a nil
	// reconcileSpawnFn a harmless no-op instead of the real production spawn,
	// UNLESS a test explicitly opts in via setReconcileSpawnFnForTest (which
	// every existing spawn-fan-out test in supervise_reconcile_wiring_test.go
	// already does, overriding this default with its own fake and restoring
	// to it afterward — this default is invisible to those tests). No
	// existing test relied on reconcileSpawnFn==nil reaching production
	// (verified: none did, because until now nothing was ever in the intent
	// to spawn), so this changes no test's observed behavior except
	// neutralizing the newly-introduced real-spawn hazard.
	reconcileSpawnFn = func(api.SupervisorDaemon) error { return nil }

	api.EnableSupervisorIPCTestPipeIsolation()

	// stateDirFunc ships env-free in production (productionStateDir →
	// api.DaemonStateDir, NOT reading MCPHUB_STATE_DIR_OVERRIDE — bug
	// 2026-06-03-cli-supervise-statedir-override-ungated). Restore the env read
	// for the whole test package so per-test MCPHUB_STATE_DIR_OVERRIDE redirects
	// still reach supervisor state. The env-read exists ONLY here (a _test.go),
	// so it is absent from the production binary (mirrors
	// EnableSupervisorIPCTestPipeIsolation).
	stateDirFunc = func() (string, error) {
		if override := os.Getenv("MCPHUB_STATE_DIR_OVERRIDE"); override != "" {
			return override, nil
		}
		return api.DaemonStateDir()
	}

	tmp, err := os.MkdirTemp("", "mcphub-cli-test-state-*")
	if err != nil {
		panic("internal/cli TestMain: create global test-state temp dir: " + err.Error())
	}
	restore := api.SetDaemonStateRootForTest(tmp)
	// Redirect every client-adapter path input before installing the audit. The
	// descriptor is shared with API and GUI package test setup.
	restoreClientEnv := clients.ApplyClientConfigSandboxEnvironment(tmp)

	// Default subprocess state-env safety net (see doc comment above).
	// MCPHUB_STATE_DIR_OVERRIDE is the authoritative redirect: it routes the
	// daemon state dir (env-fallback build, BEFORE the resolver) AND feeds the
	// supervisor IPC test-pipe discriminator (EnableSupervisorIPCTestPipeIsolation
	// derives the test pipe name from it — PR #300 r2 P2). Without it, a Windows
	// in-process supervisor test that forgets its own MCPHUB_STATE_DIR_OVERRIDE
	// would bind the PRODUCTION SID pipe \\.\pipe\mcphub-supervisor-<SID> and
	// could collide with the live fleet's supervisor IPC even though its files
	// were redirected. LOCALAPPDATA/XDG redirect the GUI pidport + log base dir.
	restoreEnv := setEnvWithRestore(map[string]string{
		"MCPHUB_STATE_DIR_OVERRIDE": tmp,
		// Global browser kill-switch for the whole cli test binary AND any real
		// `mcphub gui` child a test spawns (inherited env) — no test flashes a
		// browser window even when it spawns a GUI without an explicit
		// --no-browser. Matches gui.SuppressBrowserLaunchEnv (browser.go).
		"MCPHUB_SUPPRESS_BROWSER_LAUNCH": "1",
	})

	// Client-config sandbox audit. Fails any test in this package whose admitted
	// adapters resolve to a config path outside the test sandbox — including
	// adapters constructed by the PRODUCTION CODE under test, which is the shape
	// that let `withHermeticHome` reach the operator's real configs after
	// `mcpFrontPR588Env` had supposedly closed the class. Contract, rationale and
	// the report-mode knob: internal/clients/config_path_sandbox_audit.go.
	auditRestore := clients.EnforceSandboxedConfigPaths(tmp)

	code := m.Run()

	if escapes := auditRestore(); escapes > 0 && code == 0 {
		code = 1
	}
	restoreEnv()
	restoreClientEnv()
	restore()
	if cleanupErr := apitest.RemoveTestMainRoot(tmp); cleanupErr != nil {
		if code == 0 {
			code = 1
		}
		_, _ = os.Stderr.WriteString("internal/cli TestMain cleanup: " + cleanupErr.Error() + "\n")
	}
	os.Exit(code)
}

const cliProductionArgvHelperEnv = "MCPHUB_TEST_PRODUCTION_ARGV_HELPER"

type cliTestHelperDispatch struct {
	invalid           bool
	overlayDump       bool
	forensicsStateDir string
	productionArgv    bool
	runSelectedTest   bool
}

func classifyCLITestHelperDispatch(args []string, getenv func(string) string) cliTestHelperDispatch {
	selector := ""
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "-test.run=") {
			if selector != "" {
				return cliTestHelperDispatch{invalid: true}
			}
			selector = strings.TrimPrefix(arg, "-test.run=")
		}
	}
	type candidate struct{ active, valid bool }
	exitCodeRaw := getenv("MCPHUB_TEST_CHILD_EXIT_CODE")
	_, exitCodeErr := strconv.Atoi(exitCodeRaw)
	productionArgvMarker := getenv(cliProductionArgvHelperEnv)
	overlayActive := getenv(overlayMarkerHelperSentinelEnv) != ""
	candidates := []candidate{
		{exitCodeRaw != "", exitCodeErr == nil && (selector == "^TestFormatChildExit_LargeExitCodeShowsHex$" || selector == "^TestFormatChildExit_RealProcessShowsExitCode$")},
		{getenv(staleExitHelperSentinelEnv) != "", getenv(staleExitHelperSentinelEnv) == "1" && filepath.IsAbs(getenv(staleExitHelperReleaseEnv)) && selector == "^TestStaleExitReleaseHelper$"},
		{getenv("MCPHUB_PRODUCTION_TERMINATE_HELPER") != "", getenv("MCPHUB_PRODUCTION_TERMINATE_HELPER") == "1" && (selector == "TestProductionTerminateFn_HelperSleep" || selector == "^TestProductionTerminateFn_HelperSleep$")},
		{getenv("MCPHUB_PID_MATCH_HELPER") != "", getenv("MCPHUB_PID_MATCH_HELPER") == "1" && (selector == "TestPidMatchesMcphub_HelperSleep" || selector == "^TestPidMatchesMcphub_HelperSleep$")},
		{overlayActive, getenv(overlayMarkerHelperSentinelEnv) == "1" && filepath.IsAbs(getenv(overlayMarkerHelperDumpPathEnv)) && (selector == "^TestSpawnEnvDumpHelper$" || productionArgvMarker == "1" && validCLIOverlayProductionArgv(args))},
		{getenv(forensicsSinkChildEnv) != "", filepath.IsAbs(getenv(forensicsSinkChildEnv)) && ((selector == "TestSupervisorStderrSink_CapturesRuntimePanic" && getenv(forensicsSinkChildModeEnv) == "") || (selector == "TestSupervisorStderrSink_CapturesMainGoroutinePanic" && getenv(forensicsSinkChildModeEnv) == "main"))},
		{productionArgvMarker != "" && !overlayActive, productionArgvMarker == "1" && len(args) == 2 && args[1] == "supervise"},
	}
	active := 0
	selected := -1
	for i, candidate := range candidates {
		if candidate.active {
			active++
			selected = i
			if !candidate.valid {
				return cliTestHelperDispatch{invalid: true}
			}
		}
	}
	if active > 1 {
		return cliTestHelperDispatch{invalid: true}
	}
	if active == 0 {
		if selector == "^TestProductionTerminateFn_HelperSleep$" || selector == "TestProductionTerminateFn_HelperSleep" || selector == "TestPidMatchesMcphub_HelperSleep" || selector == "^TestStaleExitReleaseHelper$" || selector == "^TestSpawnEnvDumpHelper$" || len(args) > 1 && (args[1] == "route" || args[1] == "supervise") {
			return cliTestHelperDispatch{invalid: true}
		}
		return cliTestHelperDispatch{}
	}
	switch selected {
	case 4:
		return cliTestHelperDispatch{overlayDump: true}
	case 5:
		return cliTestHelperDispatch{forensicsStateDir: getenv(forensicsSinkChildEnv)}
	case 6:
		return cliTestHelperDispatch{productionArgv: true}
	default:
		return cliTestHelperDispatch{runSelectedTest: true}
	}
}

func validCLIOverlayProductionArgv(args []string) bool {
	return len(args) == 11 && args[1] == "daemon" && args[2] == "serena-proxy" &&
		args[3] == "--server" && args[4] == "serena" && args[5] == "--workspace" && args[6] != "" &&
		args[7] == "--port" && args[8] == "9121" && args[9] == "--task-name" && args[10] != ""
}

// setEnvWithRestore sets each key=value in the process environment and returns
// a function that reinstates the prior values (unsetting keys that were not
// previously present). Used by TestMain where no *testing.T is available for
// t.Setenv. Restore is best-effort: os.Setenv/Unsetenv errors are ignored
// because TestMain runs before any test and a failure here cannot meaningfully
// be surfaced past os.Exit.
func setEnvWithRestore(kv map[string]string) (restore func()) {
	type prior struct {
		val string
		set bool
	}
	saved := make(map[string]prior, len(kv))
	for k, v := range kv {
		old, ok := os.LookupEnv(k)
		saved[k] = prior{val: old, set: ok}
		_ = os.Setenv(k, v)
	}
	return func() {
		for k, p := range saved {
			if p.set {
				_ = os.Setenv(k, p.val)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	}
}
