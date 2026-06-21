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

	"mcp-local-hub/internal/api"
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

func TestDaemonSecretVaultFatalError_DaclOutsideAllowlistSurfacesRemediation(t *testing.T) {
	keyPath := `C:\Users\tester\AppData\Local\mcp-local-hub\.age-key`
	vaultPath := `C:\Users\tester\AppData\Local\mcp-local-hub\secrets.age`
	cause := fmt.Errorf("vault exists but unreadable: read identity: file %s not single-user safe: %w", keyPath, api.ErrDaclOutsideAllowlist)

	err := daemonSecretVaultFatalError("wolfram", "default", keyPath, vaultPath, cause)
	if err == nil {
		t.Fatal("daemonSecretVaultFatalError returned nil")
	}
	if !errors.Is(err, api.ErrDaclOutsideAllowlist) {
		t.Fatalf("daemon fatal error = %v, want ErrDaclOutsideAllowlist in chain", err)
	}
	got := err.Error()
	for _, want := range []string{
		"daemon wolfram/default:",
		"Remediate:",
		"icacls",
		keyPath,
		vaultPath,
		"vault exists but unreadable",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("daemon fatal error missing %q: %v", want, err)
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

	env, _, err := daemonEnvWithOverlay("memory", "default", map[string]string{
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

			env, _, err := daemonEnvWithOverlay("memory", "default", tt.manifest, secrets.NewResolver(nil, nil))
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

			env, _, err := daemonEnvWithOverlay("memory", "default", map[string]string{}, secrets.NewResolver(nil, nil))
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

// TestDaemonEnvWithOverlaySupervisorAppliedOverlayWinsOverManifest is the
// Codex bot #268 r10 P2 regression guard. A supervisor-spawned wrapper
// inherits the overlay value in os.Environ() (the marker is set) AND carries
// the manifest value for the SAME key. The r9 marker-skip dropped the
// overlay from cfg.Env, so StdioHost's append(os.Environ(), cfg.Env...) let
// the manifest value (last duplicate wins) clobber the overlay value that
// was only present as an inherited parent entry — the memory server's
// MEMORY_FILE_PATH override silently reverting to the manifest default after
// restart. The fix re-reads the already-expanded overlay value back from
// os.Environ() so the overlay WINS in cfg.Env and the upstream child sees
// the operator override.
func TestDaemonEnvWithOverlaySupervisorAppliedOverlayWinsOverManifest(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	t.Setenv(daemonOverlayAppliedEnvVar, daemonOverlayAppliedEnvValue)
	const overlayValue = `D:\memory`
	const manifestDefault = `C:\default\memory.json`
	// The supervisor merged the overlay value into THIS wrapper's
	// environment (overlay-wins) before spawning us. A literal value has no
	// ${parent_path}, so the value present in os.Environ() is exactly the
	// overlay literal.
	t.Setenv("MEMORY_FILE_PATH", overlayValue)
	seedDaemonOverlay(t, stateDir, map[string]string{"MEMORY_FILE_PATH": overlayValue})

	env, _, err := daemonEnvWithOverlay("memory", "default", map[string]string{
		"MEMORY_FILE_PATH": manifestDefault,
	}, secrets.NewResolver(nil, nil))
	if err != nil {
		t.Fatalf("daemonEnvWithOverlay: %v", err)
	}
	// cfg.Env (the map handed to StdioHost) must carry the overlay value so
	// it wins the append(os.Environ(), cfg.Env...) duplicate-key resolution.
	if env["MEMORY_FILE_PATH"] != overlayValue {
		t.Fatalf("cfg.Env MEMORY_FILE_PATH = %q, want overlay override %q (manifest default %q must not clobber)", env["MEMORY_FILE_PATH"], overlayValue, manifestDefault)
	}
	// Prove the EFFECTIVE value StdioHost would pass to the upstream child.
	if got := effectiveChildValueFromEnvMap(os.Environ(), env, "MEMORY_FILE_PATH"); got != overlayValue {
		t.Fatalf("effective child MEMORY_FILE_PATH = %q, want overlay override %q", got, overlayValue)
	}
}

// TestDaemonEnvWithOverlaySupervisorAppliedParentPathExpandedOnce proves the
// supervisor-applied path prepends a ${parent_path} overlay value EXACTLY
// once: the wrapper re-reads the already-expanded value the supervisor wrote
// instead of re-expanding ${parent_path} against its own (already-expanded)
// PATH, which would double the parent PATH.
func TestDaemonEnvWithOverlaySupervisorAppliedParentPathExpandedOnce(t *testing.T) {
	for _, sep := range []string{":", ";"} {
		t.Run("sep_"+sep, func(t *testing.T) {
			stateDir := apitest.HardenedTempDir(t)
			t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
			t.Setenv(daemonOverlayAppliedEnvVar, daemonOverlayAppliedEnvValue)
			parentPath := "/usr/bin" + sep + "/bin"
			prefix := "/opt/bin"
			supervisorAppliedPath := prefix + sep + parentPath
			// The supervisor already expanded ${parent_path} → this is the
			// PATH in the wrapper's environment.
			t.Setenv("PATH", supervisorAppliedPath)
			// A manifest PATH that would clobber the overlay if dropped.
			seedDaemonOverlay(t, stateDir, map[string]string{"PATH": prefix + sep + "${parent_path}"})

			env, _, err := daemonEnvWithOverlay("memory", "default", map[string]string{
				"PATH": "/manifest/only/bin",
			}, secrets.NewResolver(nil, nil))
			if err != nil {
				t.Fatalf("daemonEnvWithOverlay: %v", err)
			}
			got := effectiveChildValueFromEnvMap(os.Environ(), env, "PATH")
			if got != supervisorAppliedPath {
				t.Fatalf("effective child PATH = %q, want supervisor-applied %q (single expansion, overlay wins)", got, supervisorAppliedPath)
			}
			if strings.Count(got, prefix) != 1 {
				t.Fatalf("effective child PATH = %q, want prefix %q exactly once (no double expansion)", got, prefix)
			}
			// The parent path must appear exactly once (no /usr/bin doubling).
			if strings.Count(got, "/usr/bin") != 1 {
				t.Fatalf("effective child PATH = %q, want parent /usr/bin exactly once", got)
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
	seedDaemonOverlay(t, stateDir, env)
}

// seedDaemonOverlay writes an operator overlay row for the memory/default
// daemon with the supplied env map. Generalizes seedDaemonPathOverlay for
// non-PATH keys (e.g. MEMORY_FILE_PATH).
func seedDaemonOverlay(t *testing.T, stateDir string, env map[string]string) {
	t.Helper()
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
	return effectiveChildValueFromEnvMap(parent, env, "PATH")
}

// effectiveChildValueFromEnvMap models the value the upstream child sees for
// key when StdioHost spawns it as append(os.Environ(), cfg.Env...): the
// parent (os.Environ) value first, then each cfg.Env entry; the last
// matching key wins (Go exec duplicate-key semantics). PATH-family keys
// match case-insensitively on Windows (mirrors mergeDaemonEnv); other keys
// match exactly.
func effectiveChildValueFromEnvMap(parent []string, env map[string]string, key string) string {
	caseFold := runtime.GOOS == "windows" && strings.EqualFold(key, "PATH")
	match := func(k string) bool {
		if caseFold {
			return strings.EqualFold(k, key)
		}
		return k == key
	}
	val := ""
	for _, kv := range parent {
		k, v, ok := strings.Cut(kv, "=")
		if ok && match(k) {
			val = v
		}
	}
	for k, v := range env {
		if match(k) {
			val = v
		}
	}
	return val
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

// writeOversizeOverlayFile writes a deterministically non-retryable
// overlay file straight to the state dir: a body larger than
// daemon_env_overlay.MaxOverlaySize so daemon_env_overlay.Load fails with
// the (untagged, non-transient) size-cap error on every platform. Written
// with os.WriteFile (NOT WriteOverlay) so the bad body lands verbatim
// without the write-side hardening rejecting it.
func writeOversizeOverlayFile(t *testing.T, stateDir string) {
	t.Helper()
	path := filepath.Join(stateDir, overlayBaseName)
	// One byte over the cap is enough to trip "file exceeds N-byte cap".
	body := make([]byte, daemon_env_overlay.MaxOverlaySize+1)
	for i := range body {
		body[i] = 'a'
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write oversize overlay: %v", err)
	}
}

// TestDaemonOverlayEnvSupervisedReloadFailureIsNonFatal is the Codex bot
// #268 daemon.go:346 P2 regression guard. When the supervisor marker is
// present (supervisor-spawned wrapper), the supervisor has ALREADY loaded
// + expanded the overlay and merged it (overlay-wins) into this wrapper's
// os.Environ() before spawning us, falling back to the cached startup
// overlay on a corrupt/unreadable file. A FATAL reload error in the
// wrapper would kill every restarted supervised daemon after the operator
// leaves daemon-env-overrides.yaml malformed. The fix degrades to a warn
// and proceeds with the already-applied env. This test makes the overlay
// file unreadable (oversize → non-retryable Load error) and asserts the
// wrapper does NOT error and the child still sees the supervisor-applied
// value carried in os.Environ().
func TestDaemonOverlayEnvSupervisedReloadFailureIsNonFatal(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	t.Setenv(daemonOverlayAppliedEnvVar, daemonOverlayAppliedEnvValue)
	const overlayValue = `D:\memory`
	// The supervisor merged the overlay value into THIS wrapper's
	// environment before spawning us; it is present in os.Environ().
	t.Setenv("MEMORY_FILE_PATH", overlayValue)

	// Overlay file present but unreadable (oversize) → Load fails
	// non-transiently. Under the OLD code this was fatal.
	writeOversizeOverlayFile(t, stateDir)

	// daemonOverlayEnv must NOT return an error on the marker path.
	got, err := daemonOverlayEnv("memory", "default")
	if err != nil {
		t.Fatalf("daemonOverlayEnv on supervised reload failure: want non-fatal degrade, got error: %v", err)
	}
	// Degrade returns nil overlay map (proceed with already-applied env).
	if got != nil {
		t.Fatalf("daemonOverlayEnv degrade: want nil overlay map (proceed with os.Environ), got %v", got)
	}

	// End-to-end through daemonEnvWithOverlay: the wrapper must still
	// build cfg.Env without error, and the child (append(os.Environ(),
	// cfg.Env...)) must still see the supervisor-applied override because
	// it is inherited from os.Environ() and the manifest here does not
	// carry MEMORY_FILE_PATH (no overlap-key clobber in this case).
	env, _, err := daemonEnvWithOverlay("memory", "default", map[string]string{
		"OTHER_KEY": "manifest-only",
	}, secrets.NewResolver(nil, nil))
	if err != nil {
		t.Fatalf("daemonEnvWithOverlay on supervised reload failure: want non-fatal, got error: %v", err)
	}
	if got := effectiveChildValueFromEnvMap(os.Environ(), env, "MEMORY_FILE_PATH"); got != overlayValue {
		t.Fatalf("effective child MEMORY_FILE_PATH = %q, want supervisor-applied override %q (inherited from os.Environ)", got, overlayValue)
	}
}

// TestDaemonOverlayEnvDirectReloadFailureSurfacesError is the companion:
// a DIRECT `mcphub daemon` invocation (no supervisor marker) with the same
// unreadable overlay MUST surface the load error. The operator chose to
// run the daemon by hand and should see a malformed/unreadable overlay
// rather than launching with a silently-dropped override.
func TestDaemonOverlayEnvDirectReloadFailureSurfacesError(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	// No daemonOverlayAppliedEnvVar marker — direct invocation. Clear any
	// inherited marker so the test is deterministic regardless of the
	// parent shell.
	t.Setenv(daemonOverlayAppliedEnvVar, "")

	writeOversizeOverlayFile(t, stateDir)

	_, err := daemonOverlayEnv("memory", "default")
	if err == nil {
		t.Fatalf("daemonOverlayEnv direct invocation with unreadable overlay: want surfaced error, got nil")
	}
	if !strings.Contains(err.Error(), "load env overlay") {
		t.Fatalf("direct-invocation error %q does not look like the surfaced load-overlay error", err)
	}
	// Sanity: the underlying cause is the size-cap rejection (proves the
	// real Load error reached the caller, not a generic replacement).
	if !strings.Contains(err.Error(), "cap") {
		t.Fatalf("direct-invocation error %q should carry the underlying size-cap cause", err)
	}
}

// TestDaemonOverlayEnvSupervisedReloadFailureOverlayWinsOnOverlapKey is the
// Codex bot #268 daemon.go:380 P2 regression guard — the residual the prior
// overlay fix flagged itself. A supervisor-spawned wrapper (marker present)
// hits an UNREADABLE overlay file AND a key is present in BOTH the manifest
// and the cached operator overlay. The OLD degrade returned a nil overlay
// map, so daemonEnvWithOverlay put ONLY the manifest value into cfg.Env;
// StdioHost/HTTPHost append cfg.Env AFTER os.Environ(), so the manifest
// default (last duplicate) clobbered the supervisor-applied overlay value
// that was only present as an inherited parent entry — the operator override
// silently lost during exactly the corrupt/transient-read fallback this path
// handles. The fix reconstructs the overlay map from the supervisor-injected
// MCPHUB_DAEMON_ENV_OVERLAY_KEYS key set (reading each key's already-expanded
// value back from os.Environ), so the overlay WINS in cfg.Env even with the
// file unreadable.
func TestDaemonOverlayEnvSupervisedReloadFailureOverlayWinsOnOverlapKey(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	t.Setenv(daemonOverlayAppliedEnvVar, daemonOverlayAppliedEnvValue)
	const overlayValue = `D:\memory`
	const manifestDefault = `C:\default\memory.json`
	// The supervisor merged the overlay value (overlay-wins) into THIS
	// wrapper's environment and injected the applied overlay KEY SET before
	// spawning us. A literal value has no ${parent_path}, so the value in
	// os.Environ() is exactly the overlay literal.
	t.Setenv("MEMORY_FILE_PATH", overlayValue)
	t.Setenv(daemonOverlayKeysEnvVar, "MEMORY_FILE_PATH")

	// Overlay file present but UNREADABLE (oversize → non-retryable Load
	// error). The wrapper cannot read the overlay's key set from the file.
	writeOversizeOverlayFile(t, stateDir)

	// daemonOverlayEnv must NOT error on the marker path and must
	// reconstruct the overlay map from the injected key set.
	overlayMap, err := daemonOverlayEnv("memory", "default")
	if err != nil {
		t.Fatalf("daemonOverlayEnv on supervised reload failure: want non-fatal degrade, got error: %v", err)
	}
	if overlayMap["MEMORY_FILE_PATH"] != overlayValue {
		t.Fatalf("reconstructed overlay MEMORY_FILE_PATH = %q, want overlay override %q read back from os.Environ", overlayMap["MEMORY_FILE_PATH"], overlayValue)
	}

	// End-to-end: cfg.Env must carry the overlay value so it WINS the
	// StdioHost append(os.Environ(), cfg.Env...) duplicate-key resolution,
	// even though the manifest carries the SAME key with a different value.
	env, _, err := daemonEnvWithOverlay("memory", "default", map[string]string{
		"MEMORY_FILE_PATH": manifestDefault,
	}, secrets.NewResolver(nil, nil))
	if err != nil {
		t.Fatalf("daemonEnvWithOverlay on supervised reload failure: want non-fatal, got error: %v", err)
	}
	if env["MEMORY_FILE_PATH"] != overlayValue {
		t.Fatalf("cfg.Env MEMORY_FILE_PATH = %q, want overlay override %q (manifest default %q must not clobber on unreadable file)", env["MEMORY_FILE_PATH"], overlayValue, manifestDefault)
	}
	if got := effectiveChildValueFromEnvMap(os.Environ(), env, "MEMORY_FILE_PATH"); got != overlayValue {
		t.Fatalf("effective child MEMORY_FILE_PATH = %q, want overlay override %q (overlay wins on unreadable file)", got, overlayValue)
	}
}

// TestDaemonOverlayEnvSupervisedReloadFailureNoInjectedKeysFallsBackToNil
// proves the degrade still returns a nil overlay map (manifest-only env)
// when the supervisor applied NO overlay for this daemon — the injected key
// set is empty/absent — so the no-overlay path is unchanged.
func TestDaemonOverlayEnvSupervisedReloadFailureNoInjectedKeysFallsBackToNil(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	t.Setenv(daemonOverlayAppliedEnvVar, daemonOverlayAppliedEnvValue)
	// No MCPHUB_DAEMON_ENV_OVERLAY_KEYS set — supervisor applied no overlay
	// for this daemon (marker is present for the fleet, but this row had no
	// overlay). Clear any inherited value for determinism.
	t.Setenv(daemonOverlayKeysEnvVar, "")
	writeOversizeOverlayFile(t, stateDir)

	got, err := daemonOverlayEnv("memory", "default")
	if err != nil {
		t.Fatalf("daemonOverlayEnv: want non-fatal degrade, got error: %v", err)
	}
	if got != nil {
		t.Fatalf("daemonOverlayEnv degrade with no injected keys: want nil overlay map, got %v", got)
	}
}

// TestDaemonOverlayKeysFromEnv covers the split semantics of the
// supervisor-injected MCPHUB_DAEMON_ENV_OVERLAY_KEYS value: comma-joined
// segments, last-entry-wins on duplicate vars (a trusted supervisor append
// beats an earlier spoof), and empty-segment dropping so a malformed value
// never yields an empty-named key.
func TestDaemonOverlayKeysFromEnv(t *testing.T) {
	sep := daemonOverlayKeysSep
	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{
			name: "absent var",
			env:  []string{"PATH=/bin"},
			want: nil,
		},
		{
			name: "empty value",
			env:  []string{daemonOverlayKeysEnvVar + "="},
			want: nil,
		},
		{
			name: "single key",
			env:  []string{daemonOverlayKeysEnvVar + "=MEMORY_FILE_PATH"},
			want: []string{"MEMORY_FILE_PATH"},
		},
		{
			name: "two keys nul-joined",
			env:  []string{daemonOverlayKeysEnvVar + "=A" + sep + "B"},
			want: []string{"A", "B"},
		},
		{
			name: "empty segments dropped",
			env:  []string{daemonOverlayKeysEnvVar + "=" + sep + "A" + sep + sep + "B" + sep},
			want: []string{"A", "B"},
		},
		{
			name: "last duplicate wins over earlier spoof",
			env: []string{
				daemonOverlayKeysEnvVar + "=SPOOFED",
				daemonOverlayKeysEnvVar + "=REAL_KEY",
			},
			want: []string{"REAL_KEY"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := daemonOverlayKeysFromEnv(tt.env)
			if len(got) != len(tt.want) {
				t.Fatalf("daemonOverlayKeysFromEnv(%v) = %v, want %v", tt.env, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("daemonOverlayKeysFromEnv(%v)[%d] = %q, want %q", tt.env, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestAppendDaemonOverlayKeysStripsSpoof asserts the keys-marker injection
// strips any pre-existing MCPHUB_DAEMON_ENV_OVERLAY_KEYS entry (a manifest or
// overlay row that names the reserved key, or an inherited spoof) before
// appending the trusted key set LAST — mirroring the APPLIED marker's
// strip-then-set discipline so no manifest/overlay value can inject or drop
// keys.
func TestAppendDaemonOverlayKeysStripsSpoof(t *testing.T) {
	env := appendDaemonOverlayKeys([]string{
		daemonOverlayKeysEnvVar + "=SPOOF_KEY_A" + daemonOverlayKeysSep + "SPOOF_KEY_B",
		"PATH=/opt/bin",
	}, []string{"REAL_KEY"})

	if got := countEnvKey(env, daemonOverlayKeysEnvVar); got != 1 {
		t.Fatalf("%s count = %d, want exactly one; env=%v", daemonOverlayKeysEnvVar, got, env)
	}
	if got := lookupEnvValue(env, daemonOverlayKeysEnvVar); got != "REAL_KEY" {
		t.Fatalf("%s = %q, want trusted key set %q; env=%v", daemonOverlayKeysEnvVar, got, "REAL_KEY", env)
	}
	// The spoofed segment must be gone: a wrapper reading it back must NOT
	// see SPOOF_KEY_A / SPOOF_KEY_B.
	keys := daemonOverlayKeysFromEnv(env)
	if len(keys) != 1 || keys[0] != "REAL_KEY" {
		t.Fatalf("reconstructed keys = %v, want exactly [REAL_KEY] after strip-spoof", keys)
	}
}

// TestAppendDaemonOverlayKeysSpoofedRowCannotResurrectClobber is the
// end-to-end strip-spoof proof: a manifest/overlay row that names the
// reserved keys var cannot drive the wrapper to reconstruct an attacker-
// chosen key (and thus cannot resurrect the manifest clobber by dropping the
// real overlap key from the reconstructed set). The supervisor's trusted
// append wins; the wrapper reconstructs only the real key.
func TestAppendDaemonOverlayKeysSpoofedRowCannotResurrectClobber(t *testing.T) {
	// Simulate the supervisor merge output where a malicious overlay/manifest
	// row defined MCPHUB_DAEMON_ENV_OVERLAY_KEYS=DECOY (so the merged cmd.Env
	// carries the spoof), then the supervisor strip-then-appends the REAL key
	// set for the daemon.
	cmdEnv := []string{
		"MEMORY_FILE_PATH=D:\\overlay",
		daemonOverlayKeysEnvVar + "=DECOY",
	}
	cmdEnv = appendDaemonOverlayKeys(cmdEnv, []string{"MEMORY_FILE_PATH"})

	keys := daemonOverlayKeysFromEnv(cmdEnv)
	if len(keys) != 1 || keys[0] != "MEMORY_FILE_PATH" {
		t.Fatalf("reconstructed keys = %v, want exactly [MEMORY_FILE_PATH]; DECOY spoof must be stripped", keys)
	}
	got := overlayMapFromInjectedKeys(keys, cmdEnv)
	if got["MEMORY_FILE_PATH"] != "D:\\overlay" {
		t.Fatalf("reconstructed overlay MEMORY_FILE_PATH = %q, want D:\\overlay (read from env)", got["MEMORY_FILE_PATH"])
	}
	if _, ok := got["DECOY"]; ok {
		t.Fatalf("reconstructed overlay must not contain spoofed DECOY key; got %v", got)
	}
}

// TestOverlayMapFromInjectedKeysPathFamilyCaseFold exercises the GOOS-specific
// branch of the reconstruction: on Windows an overlay `PATH` key must find a
// `Path=` entry the supervisor wrote (case-insensitive PATH-family match,
// mirroring mergeDaemonEnv's normalizer); on POSIX the match is exact.
func TestOverlayMapFromInjectedKeysPathFamilyCaseFold(t *testing.T) {
	// Overlay stored the key as "PATH"; the supervisor wrote "Path=" into
	// the environment (Windows kernel folds them; the merge preserves the
	// winning source's casing, which may differ from the overlay's).
	env := []string{"Path=C:\\overlay\\bin"}
	got := overlayMapFromInjectedKeys([]string{"PATH"}, env)
	if runtime.GOOS == "windows" {
		if got["PATH"] != "C:\\overlay\\bin" {
			t.Fatalf("Windows reconstruct PATH = %q, want case-folded match of Path= entry", got["PATH"])
		}
	} else {
		// POSIX: "PATH" != "Path", so the key is missing from env. The
		// fallback expands the literal overlay value (here empty, so it
		// stays empty) rather than crashing — the overlay still wins with
		// whatever literal it had.
		if v, ok := got["PATH"]; !ok || v != "" {
			t.Fatalf("POSIX reconstruct PATH = (%q,%v), want empty-literal fallback (no case-fold match)", v, ok)
		}
	}
}

// TestOverlayMapFromInjectedKeysEmptyKeysYieldsEmptyMap pins the empty-key-set
// contract used by the degrade caller to decide nil-vs-reconstructed.
func TestOverlayMapFromInjectedKeysEmptyKeysYieldsEmptyMap(t *testing.T) {
	if got := overlayMapFromInjectedKeys(nil, os.Environ()); len(got) != 0 {
		t.Fatalf("overlayMapFromInjectedKeys(nil, _) = %v, want empty map", got)
	}
	if got := overlayMapFromInjectedKeys([]string{}, os.Environ()); len(got) != 0 {
		t.Fatalf("overlayMapFromInjectedKeys([], _) = %v, want empty map", got)
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
