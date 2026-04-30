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

// Real-process exercise: spawn a tiny child that exits with a known
// code, Wait for it, and confirm formatChildExit captures the exit
// code into the suffix. Uses os.Args[0] re-exec — the standard Go
// pattern for testing process-exit behavior without a platform-
// specific helper script.
func TestFormatChildExit_RealProcessShowsExitCode(t *testing.T) {
	if os.Getenv("CHILD_EXIT_CODE") != "" {
		// We are the child. Exit with the requested code so the parent
		// can read it back via ProcessState.
		code := 0
		fmt.Sscanf(os.Getenv("CHILD_EXIT_CODE"), "%d", &code)
		os.Exit(code)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFormatChildExit_RealProcessShowsExitCode$")
	cmd.Env = append(os.Environ(), "CHILD_EXIT_CODE=42")
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
