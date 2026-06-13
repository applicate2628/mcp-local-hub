package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
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
	api.EnableSupervisorIPCTestPipeIsolation()

	tmp, err := os.MkdirTemp("", "mcphub-cli-test-state-*")
	if err != nil {
		panic("internal/cli TestMain: create global test-state temp dir: " + err.Error())
	}
	restore := api.SetDaemonStateRootForTest(tmp)

	// Default subprocess state-env safety net (see doc comment above).
	restoreEnv := setEnvWithRestore(map[string]string{
		"LOCALAPPDATA":   tmp,
		"XDG_DATA_HOME":  tmp,
		"XDG_STATE_HOME": tmp,
	})

	code := m.Run()

	restoreEnv()
	restore()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
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
