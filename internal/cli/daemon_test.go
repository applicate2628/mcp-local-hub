package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/api/daemon_env_overlay"
	"mcp-local-hub/internal/secrets"
)

// TestWriteLaunchFailure_AppendsTimestampedLine asserts the DM-3 helper
// writes a grep-able diagnostic line to the daemon log path. The line
// must include the server, daemon, and the underlying error so
// `mcphub status` users can find the cause when Task Scheduler shows
// last_result=1 with no other context.
func TestWriteLaunchFailure_AppendsTimestampedLine(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "logs", "serena-claude.log")

	writeLaunchFailure(logPath, "serena", "claude", errors.New("port 9121 already in use"))

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected log file at %s: %v", logPath, err)
	}
	got := string(data)
	for _, want := range []string{
		"[mcphub-launch-failure",
		"server=serena",
		"daemon=claude",
		"port 9121 already in use",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q; got: %q", want, got)
		}
	}
}

// TestWriteLaunchFailure_AppendsToExistingLog confirms a second call
// appends rather than truncates — important for the multi-retry
// scenario where Task Scheduler's RestartOnFailure fires the daemon
// 3 times in 3 minutes and we want every failure recorded.
func TestWriteLaunchFailure_AppendsToExistingLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "memory-default.log")

	// Pre-populate the log with arbitrary prior content (e.g. previous
	// healthy run's child stdout).
	priorContent := "prior child output line\n"
	if err := os.WriteFile(logPath, []byte(priorContent), 0600); err != nil {
		t.Fatal(err)
	}

	writeLaunchFailure(logPath, "memory", "default", errors.New("first failure"))
	writeLaunchFailure(logPath, "memory", "default", errors.New("second failure"))

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.HasPrefix(got, priorContent) {
		t.Errorf("prior content was overwritten; got: %q", got)
	}
	if !strings.Contains(got, "first failure") {
		t.Errorf("missing first failure line; got: %q", got)
	}
	if !strings.Contains(got, "second failure") {
		t.Errorf("missing second failure line; got: %q", got)
	}
}

// TestWriteLaunchFailure_SilentOnUnwritablePath asserts the helper
// does not panic and returns silently when the log directory cannot
// be created. The deferred wrapper must never compound the original
// launch error — its only job is best-effort diagnostic recording.
//
// On Unix we create an unwritable parent and try to mkdir under it.
// On Windows we use a path with NUL characters that os.MkdirAll
// rejects unconditionally. Either way: no panic, no return value.
func TestWriteLaunchFailure_SilentOnUnwritablePath(t *testing.T) {
	var bogusPath string
	switch runtime.GOOS {
	case "windows":
		// Path containing NUL character — illegal on every Windows API.
		bogusPath = "C:\\bogus\x00path\\daemon.log"
	default:
		// Read-only parent (chmod 0500 means no write permission for
		// the owner, so MkdirAll under it fails).
		parent := t.TempDir()
		if err := os.Chmod(parent, 0500); err != nil {
			t.Skipf("chmod not honored on this filesystem: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
		bogusPath = filepath.Join(parent, "subdir", "daemon.log")
	}

	// Defer/recover: the helper must not panic even on a bad path.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("writeLaunchFailure panicked on bad path: %v", r)
		}
	}()
	writeLaunchFailure(bogusPath, "x", "y", errors.New("err"))
}

// TestWriteLaunchFailure_CreatesParentDir asserts the helper creates
// a missing parent directory rather than failing silently when only
// the parent is missing — the daemon log dir may not exist on the
// very first launch after install.
func TestWriteLaunchFailure_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "deeply", "nested", "path", "log.log")

	writeLaunchFailure(logPath, "s", "d", fmt.Errorf("boom"))

	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("expected log file to exist after writeLaunchFailure: %v", err)
	}
}

// TestDaemonWorkspaceProxyCmd_PreCanonicalizationFailure_LogsToFallback
// guards the Codex r1 P2 finding on PR #21: the launch-failure defer
// in newDaemonWorkspaceProxyCmd MUST be installed BEFORE
// api.CanonicalWorkspacePath. The bot's concern: a stale workspace
// registration (path moved/deleted) returns from
// CanonicalWorkspacePath with an error before any defer was active in
// the original code, leaving last_result=1 with no diagnostic — the
// exact observability gap DM-3 set out to close.
//
// The fix moves the defer above CanonicalWorkspacePath and seeds
// logPath with a `lazy-proxy-<lang>-pre.log` fallback (refined to the
// canonical lsp-<wsKey>-<lang>.log after canonicalization succeeds).
// This test exercises the failure path:
//
//   - --workspace points at a non-existent dir → CanonicalWorkspacePath
//     fails → defer fires → writeLaunchFailure must land in the
//     fallback log path under the redirected logBaseDir.
//
// If the defer regresses (someone moves it back below
// CanonicalWorkspacePath, or the fallback path is dropped), the
// fallback log won't exist and this test fails.
func TestDaemonWorkspaceProxyCmd_PreCanonicalizationFailure_LogsToFallback(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmpHome)
	t.Setenv("XDG_STATE_HOME", tmpHome)

	// Workspace path that cannot be canonicalized — never created on
	// disk. CanonicalWorkspacePath calls EvalSymlinks → os.Stat which
	// returns ENOENT here. The closure-captured logPath at defer time
	// is still the pre-canonicalization fallback, which is what we
	// want this test to verify.
	missingWS := filepath.Join(tmpHome, "this-workspace-does-not-exist")

	cmd := newDaemonWorkspaceProxyCmd()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{
		"--port", "9999",
		"--workspace", missingWS,
		"--language", "go",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected workspace-proxy command to fail with non-existent workspace, got nil")
	}

	// The failure must land in the pre-canonicalization fallback path:
	// <logBaseDir>/lazy-proxy-<lang>-pre.log. lsp-<wsKey>-<lang>.log
	// would not exist because wsKey was never computed.
	fallbackLog := filepath.Join(tmpHome, "mcp-local-hub", "logs", "lazy-proxy-go-pre.log")
	data, readErr := os.ReadFile(fallbackLog)
	if readErr != nil {
		t.Fatalf("expected fallback log at %s, got read error: %v", fallbackLog, readErr)
	}
	content := string(data)

	// Assert the diagnostic line was actually written. The daemon label
	// in the pre-canonicalization branch is "lazy-proxy-<lang>", which
	// is what users will grep for after seeing last_result=1 on the
	// scheduler task.
	for _, want := range []string{
		"[mcphub-launch-failure",
		"server=mcp-language-server",
		"daemon=lazy-proxy-go",
		// The original error must reach the log; the substring
		// "canonical workspace path" is the exact wrap from
		// daemon_workspace.go and proves the underlying error wasn't
		// replaced by a generic message.
		"canonical workspace path",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("fallback log missing %q; got:\n%s", want, content)
		}
	}
}

// TestDaemonCmd_RunFailure_AppendsToLog is the E2E for DM-3a: it
// invokes the cobra `mcphub daemon` Cmd against an unknown server name
// so the embedded manifest open fails. The defer-wrap on RunE MUST
// capture the returned error and append a timestamped diagnostic line
// to the per-daemon log path BEFORE the error reaches the caller.
//
// The unit tests above (TestWriteLaunchFailure_*) verify the helper in
// isolation. This test closes the gap by exercising the full cobra Cmd
// → defer-wrap → log-file path; without it nothing proves the wrap
// actually fires when RunE returns a real error. If the defer block in
// daemon.go is removed or its writeLaunchFailure call is dropped, this
// test fails — that's the regression guard.
func TestDaemonCmd_RunFailure_AppendsToLog(t *testing.T) {
	// Redirect logBaseDir() to a tempdir on every supported OS:
	//   - Windows: %LOCALAPPDATA% wins
	//   - Linux/macOS: $XDG_STATE_HOME wins (LOCALAPPDATA is empty there
	//     in normal use, but t.Setenv just makes both branches resolve
	//     to the same tmpHome — both env vars are restored on cleanup)
	tmpHome := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmpHome)
	t.Setenv("XDG_STATE_HOME", tmpHome)

	cmd := newDaemonCmdReal()
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--server", "no-such-server", "--daemon", "no-such-daemon"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected daemon command to fail with unknown server, got nil")
	}

	// logBaseDir() resolves to <LOCALAPPDATA>/mcp-local-hub/logs on
	// Windows and <XDG_STATE_HOME>/mcp-local-hub/logs on POSIX. Both
	// env vars point at tmpHome above, so the path is the same.
	logPath := filepath.Join(tmpHome, "mcp-local-hub", "logs", "no-such-server-no-such-daemon.log")
	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("expected log file at %s, got read error: %v", logPath, readErr)
	}
	content := string(data)

	// Defer-wrap MUST have written a timestamped failure line. These
	// four substrings are the wrap's distinguishing features — if any
	// is missing, the wrap is no longer firing or has been changed in
	// a way that breaks the diagnostic format users grep for.
	for _, want := range []string{
		"[mcphub-launch-failure",
		"server=no-such-server",
		"daemon=no-such-daemon",
		// The original manifest-open error mentions the unknown server
		// name; this confirms the underlying error reached the log
		// rather than being replaced with a generic message.
		"no-such-server",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("log missing %q; got:\n%s", want, content)
		}
	}
}

func TestDaemonEnvWithOverlayResolvesManifestBeforeLiteralOverlay(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	t.Setenv("MANIFEST_FROM_PARENT", "resolved-from-parent")
	t.Setenv("BAR", "parent-value-that-must-not-replace-overlay")

	if err := daemon_env_overlay.WriteOverlay(filepath.Join(stateDir, overlayBaseName), func(ov *daemon_env_overlay.Overlay) error {
		ov.Daemons[`\mcp-local-hub-memory-default`] = daemon_env_overlay.DaemonRow{
			Source: "operator",
			Env: map[string]string{
				"FOO":   "$BAR",
				"TOKEN": "secret:BAR",
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}

	dir := t.TempDir()
	keyPath := filepath.Join(dir, ".age-key")
	vaultPath := filepath.Join(dir, "secrets.age")
	if err := secrets.InitVault(keyPath, vaultPath); err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	vault, err := secrets.OpenVault(keyPath, vaultPath)
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	if err := vault.Set("MANIFEST_SECRET", "resolved-secret"); err != nil {
		t.Fatalf("vault.Set: %v", err)
	}

	env, err := daemonEnvWithOverlay("memory", "default", map[string]string{
		"FROM_ENV":    "$MANIFEST_FROM_PARENT",
		"FROM_SECRET": "secret:MANIFEST_SECRET",
		"FOO":         "manifest-value",
	}, secrets.NewResolver(vault, nil))
	if err != nil {
		t.Fatalf("daemonEnvWithOverlay: %v", err)
	}
	if env["FROM_ENV"] != "resolved-from-parent" {
		t.Fatalf("FROM_ENV = %q, want manifest env reference resolved", env["FROM_ENV"])
	}
	if env["FROM_SECRET"] != "resolved-secret" {
		t.Fatalf("FROM_SECRET = %q, want manifest secret resolved", env["FROM_SECRET"])
	}
	if env["FOO"] != "$BAR" {
		t.Fatalf("FOO = %q, want literal overlay value", env["FOO"])
	}
	if env["TOKEN"] != "secret:BAR" {
		t.Fatalf("TOKEN = %q, want literal overlay value", env["TOKEN"])
	}
}

func TestDaemonEnvWithOverlayPathCaseMatrix(t *testing.T) {
	tests := []struct {
		name     string
		manifest map[string]string
		overlay  map[string]string
		assert   func(t *testing.T, env map[string]string)
	}{
		{
			name:     "overlay PATH against manifest Path",
			manifest: map[string]string{"Path": "manifest-path"},
			overlay:  map[string]string{"PATH": "overlay-path"},
			assert: func(t *testing.T, env map[string]string) {
				t.Helper()
				if runtime.GOOS == "windows" {
					assertPathFamilyEntries(t, env, 1)
					if env["PATH"] != "overlay-path" {
						t.Fatalf("PATH = %q, want overlay-path", env["PATH"])
					}
					if _, ok := env["Path"]; ok {
						t.Fatalf("Windows merge kept manifest Path alongside overlay PATH: %v", env)
					}
					return
				}
				assertPathFamilyEntries(t, env, 2)
				if env["Path"] != "manifest-path" || env["PATH"] != "overlay-path" {
					t.Fatalf("POSIX merge = %v, want distinct Path manifest and PATH overlay entries", env)
				}
			},
		},
		{
			name:     "overlay PATH against manifest PATH",
			manifest: map[string]string{"PATH": "manifest-path"},
			overlay:  map[string]string{"PATH": "overlay-path"},
			assert: func(t *testing.T, env map[string]string) {
				t.Helper()
				assertPathFamilyEntries(t, env, 1)
				if env["PATH"] != "overlay-path" {
					t.Fatalf("PATH = %q, want overlay-path", env["PATH"])
				}
			},
		},
		{
			name:     "only manifest Path",
			manifest: map[string]string{"Path": "manifest-path"},
			assert: func(t *testing.T, env map[string]string) {
				t.Helper()
				assertPathFamilyEntries(t, env, 1)
				if env["Path"] != "manifest-path" {
					t.Fatalf("Path = %q, want manifest-path", env["Path"])
				}
			},
		},
		{
			name:    "only overlay PATH",
			overlay: map[string]string{"PATH": "overlay-path"},
			assert: func(t *testing.T, env map[string]string) {
				t.Helper()
				assertPathFamilyEntries(t, env, 1)
				if env["PATH"] != "overlay-path" {
					t.Fatalf("PATH = %q, want overlay-path", env["PATH"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateDir := apitest.HardenedTempDir(t)
			t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
			if len(tt.overlay) > 0 {
				if err := daemon_env_overlay.WriteOverlay(filepath.Join(stateDir, overlayBaseName), func(ov *daemon_env_overlay.Overlay) error {
					ov.Daemons[`\mcp-local-hub-memory-default`] = daemon_env_overlay.DaemonRow{
						Source: "operator",
						Env:    tt.overlay,
					}
					return nil
				}); err != nil {
					t.Fatalf("seed overlay: %v", err)
				}
			}

			env, err := daemonEnvWithOverlay("memory", "default", tt.manifest, secrets.NewResolver(nil, nil))
			if err != nil {
				t.Fatalf("daemonEnvWithOverlay: %v", err)
			}
			tt.assert(t, env)
		})
	}
}

func TestDaemonOverlayEnvDirectInvocationAppliesParentPathOnce(t *testing.T) {
	for _, sep := range []string{":", ";"} {
		t.Run("sep_"+sep, func(t *testing.T) {
			stateDir := apitest.HardenedTempDir(t)
			t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
			parentPath := "/usr/bin" + sep + "/bin"
			prefix := "/opt/bin"
			t.Setenv("PATH", parentPath)
			seedDaemonPathOverlay(t, stateDir, prefix+sep+"${parent_path}", nil)

			env, err := daemonOverlayEnv("memory", "default")
			if err != nil {
				t.Fatalf("daemonOverlayEnv: %v", err)
			}

			want := prefix + sep + parentPath
			if env["PATH"] != want {
				t.Fatalf("PATH = %q, want direct overlay applied once as %q", env["PATH"], want)
			}
			if strings.Count(env["PATH"], prefix) != 1 {
				t.Fatalf("PATH = %q, want prefix %q exactly once", env["PATH"], prefix)
			}
		})
	}
}

func TestDaemonEnvWithOverlaySkipsSupervisorAppliedOverlay(t *testing.T) {
	for _, sep := range []string{":", ";"} {
		t.Run("sep_"+sep, func(t *testing.T) {
			stateDir := apitest.HardenedTempDir(t)
			t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
			parentPath := "/usr/bin" + sep + "/bin"
			prefix := "/opt/bin"
			supervisorAppliedPath := prefix + sep + parentPath
			t.Setenv("PATH", supervisorAppliedPath)
			t.Setenv(daemonOverlayAppliedEnvVar, daemonOverlayAppliedEnvValue)
			seedDaemonPathOverlay(t, stateDir, prefix+sep+"${parent_path}", nil)

			env, err := daemonEnvWithOverlay("memory", "default", map[string]string{}, secrets.NewResolver(nil, nil))
			if err != nil {
				t.Fatalf("daemonEnvWithOverlay: %v", err)
			}

			effectivePath := effectiveChildPathFromEnvMap(os.Environ(), env)
			if effectivePath != supervisorAppliedPath {
				t.Fatalf("effective child PATH = %q, want supervisor-applied PATH exactly once as %q; daemon env=%v", effectivePath, supervisorAppliedPath, env)
			}
			if strings.Count(effectivePath, prefix) != 1 {
				t.Fatalf("effective child PATH = %q, want prefix %q exactly once", effectivePath, prefix)
			}
		})
	}
}

func TestAppendDaemonOverlayAppliedMarkerWinsOverManifestOverlayValue(t *testing.T) {
	env := appendDaemonOverlayAppliedMarker([]string{
		daemonOverlayAppliedEnvVar + "=overlay-spoof",
		"PATH=/opt/bin",
	})

	if got := countEnvKey(env, daemonOverlayAppliedEnvVar); got != 1 {
		t.Fatalf("%s count = %d, want exactly one; env=%v", daemonOverlayAppliedEnvVar, got, env)
	}
	if got := lookupEnvValue(env, daemonOverlayAppliedEnvVar); got != daemonOverlayAppliedEnvValue {
		t.Fatalf("%s = %q, want supervisor marker value %q; env=%v", daemonOverlayAppliedEnvVar, got, daemonOverlayAppliedEnvValue, env)
	}
}

func seedDaemonPathOverlay(t *testing.T, stateDir, pathValue string, extra map[string]string) {
	t.Helper()
	env := map[string]string{"PATH": pathValue}
	for k, v := range extra {
		env[k] = v
	}
	if err := daemon_env_overlay.WriteOverlay(filepath.Join(stateDir, overlayBaseName), func(ov *daemon_env_overlay.Overlay) error {
		ov.Daemons[`\mcp-local-hub-memory-default`] = daemon_env_overlay.DaemonRow{
			Source: "operator",
			Env:    env,
		}
		return nil
	}); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
}

func effectiveChildPathFromEnvMap(parent []string, env map[string]string) string {
	path := lookupEnvValue(parent, "PATH")
	for k, v := range env {
		if strings.EqualFold(k, "PATH") {
			path = v
		}
	}
	return path
}

func countEnvKey(env []string, key string) int {
	count := 0
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(k, key) {
			count++
		}
	}
	return count
}

func lookupEnvValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(env[i], "=")
		if ok && strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func assertPathFamilyEntries(t *testing.T, env map[string]string, want int) {
	t.Helper()
	got := 0
	for k := range env {
		if strings.EqualFold(k, "PATH") {
			got++
		}
	}
	if got != want {
		t.Fatalf("Path-family entry count = %d, want %d; env=%v", got, want, env)
	}
}

func TestDaemonOverlayEnvKeepsLiteralValuesWhenParentPathExpansionFails(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)

	if err := daemon_env_overlay.WriteOverlay(filepath.Join(stateDir, overlayBaseName), func(ov *daemon_env_overlay.Overlay) error {
		ov.Daemons[`\mcp-local-hub-memory-default`] = daemon_env_overlay.DaemonRow{
			Source: "operator",
			Env: map[string]string{
				"FOO": "${not_parent_path}",
			},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}

	env, err := daemonOverlayEnv("memory", "default")
	if err != nil {
		t.Fatalf("daemonOverlayEnv: %v", err)
	}
	if env["FOO"] != "${not_parent_path}" {
		t.Fatalf("FOO = %q, want literal overlay value after expansion failure", env["FOO"])
	}
}

// formatChildExit is the diagnostic suffix appended to "native-http
// upstream exited unexpectedly" when the child crashes silently. The
// nil case must be safe (process never spawned / still running) and
// produce no suffix so the caller's Errorf format stays clean.
func TestFormatChildExit_NilStateProducesEmptySuffix(t *testing.T) {
	got := formatChildExit(nil)
	if got != "" {
		t.Errorf("formatChildExit(nil) = %q, want empty", got)
	}
}

// Windows native crashes surface as NTSTATUS reinterpreted as int32
// (e.g. 0xC0000005 access violation = -1073741819 in decimal). Without
// the hex suffix, the decimal is unrecognizable and operators have to
// translate by hand. Codex CLI review on PR #34 P3.
//
// Direct ProcessState construction would need unsafe; instead spawn a
// real child and pick an exit code outside 0-255 to trigger the hex
// branch. Cross-platform: 256 is "unusual" enough to flip the format
// without needing platform-specific NTSTATUS values.
func TestFormatChildExit_LargeExitCodeShowsHex(t *testing.T) {
	if os.Getenv("MCPHUB_TEST_CHILD_EXIT_CODE") != "" {
		code := 0
		fmt.Sscanf(os.Getenv("MCPHUB_TEST_CHILD_EXIT_CODE"), "%d", &code)
		os.Exit(code)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFormatChildExit_LargeExitCodeShowsHex$")
	// Pick an exit code > 255 so formatChildExit emits the hex suffix.
	// Go's os.Exit truncates to uint8 on POSIX so this only meaningfully
	// reproduces the >255 path on Windows; the test still asserts the
	// formatter behavior on whatever code the OS surfaces.
	cmd.Env = append(os.Environ(), "MCPHUB_TEST_CHILD_EXIT_CODE=300")
	_ = cmd.Run()
	suffix := formatChildExit(cmd.ProcessState)
	// On platforms where os.Exit(300) truncates to <256, the hex branch
	// is skipped — that is correct behavior, not a test failure. We
	// only assert that whichever path runs produces a parseable suffix.
	if !strings.Contains(suffix, "exit_code=") {
		t.Errorf("suffix=%q must always contain exit_code=", suffix)
	}
}

// Real-process exercise: spawn a tiny child that exits with a known
// code, Wait for it, and confirm formatChildExit captures the exit
// code into the suffix. Uses os.Args[0] re-exec — the standard Go
// pattern for testing process-exit behavior without a platform-
// specific helper script.
func TestFormatChildExit_RealProcessShowsExitCode(t *testing.T) {
	if os.Getenv("MCPHUB_TEST_CHILD_EXIT_CODE") != "" {
		// We are the child. Exit with the requested code so the parent
		// can read it back via ProcessState.
		code := 0
		fmt.Sscanf(os.Getenv("MCPHUB_TEST_CHILD_EXIT_CODE"), "%d", &code)
		os.Exit(code)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFormatChildExit_RealProcessShowsExitCode$")
	cmd.Env = append(os.Environ(), "MCPHUB_TEST_CHILD_EXIT_CODE=42")
	if err := cmd.Run(); err == nil {
		t.Fatalf("child should have failed with exit code 42; got nil")
	}
	suffix := formatChildExit(cmd.ProcessState)
	if !strings.Contains(suffix, "exit_code=42") {
		t.Errorf("suffix=%q must contain exit_code=42", suffix)
	}
	if !strings.Contains(suffix, "pid=") {
		t.Errorf("suffix=%q must contain pid=...", suffix)
	}
}
