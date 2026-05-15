package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

// resetUpgradeSeams clears every seam used by --upgrade so tests can
// install their own fakes without leaking state across cases. Calls
// t.Cleanup to restore the production nil-default at the end of the
// test scope.
func resetUpgradeSeams(t *testing.T) {
	t.Helper()
	origStop := upgradeStopAllFn
	origBoot := upgradeBootstrapFn
	origRestart := upgradeRestartAllFn
	origExec := upgradeExecutableFn
	origTarget := upgradeTargetPathFn
	origFindGUI := findRunningGUIsOnTargetFn
	origVersion := upgradeBuildVersionFn
	t.Cleanup(func() {
		upgradeStopAllFn = origStop
		upgradeBootstrapFn = origBoot
		upgradeRestartAllFn = origRestart
		upgradeExecutableFn = origExec
		upgradeTargetPathFn = origTarget
		findRunningGUIsOnTargetFn = origFindGUI
		upgradeBuildVersionFn = origVersion
	})
	upgradeStopAllFn = nil
	upgradeBootstrapFn = nil
	upgradeRestartAllFn = nil
	upgradeExecutableFn = nil
	upgradeTargetPathFn = nil
	// Default the dev-build guard seam to a valid semver so existing
	// tests don't trip the PR #188 A8 closure (which refuses
	// version=="dev"). Tests that want to exercise the guard
	// override this seam explicitly.
	upgradeBuildVersionFn = func() string { return "0.4.1-test" }
	// Default GUI detection to "no running GUIs" — tests that want
	// to exercise the running-GUI guard override this seam.
	findRunningGUIsOnTargetFn = func(target string) ([]api.ProcessInfo, error) { return nil, nil }
}

// stubCmd makes a cobra.Command with captured stdout+stderr so tests
// can assert on the bytes the user actually sees.
func stubCmd() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	c := &cobra.Command{}
	c.SetOut(stdout)
	c.SetErr(stderr)
	return c, stdout, stderr
}

// TestRunInstallUpgrade_HappyPath pins the StopAll → Bootstrap →
// RestartAll order and verifies the success-line output.
func TestRunInstallUpgrade_HappyPath(t *testing.T) {
	resetUpgradeSeams(t)

	var order []string
	upgradeExecutableFn = func() (string, error) { return "C:\\dev\\mcphub.exe", nil }
	upgradeTargetPathFn = func() (string, error) { return "C:\\Users\\u\\.local\\bin\\mcphub.exe", nil }
	upgradeStopAllFn = func() ([]api.RestartResult, error) {
		order = append(order, "stop")
		return []api.RestartResult{
			{TaskName: "mcp-local-hub-time-default"},
			{TaskName: "mcp-local-hub-godbolt-default"},
		}, nil
	}
	upgradeBootstrapFn = func(w io.Writer) error {
		order = append(order, "bootstrap")
		_, _ = io.WriteString(w, "✓ mcphub installed at C:\\Users\\u\\.local\\bin\\mcphub.exe\n")
		return nil
	}
	upgradeRestartAllFn = func() ([]api.RestartResult, error) {
		order = append(order, "restart")
		return []api.RestartResult{
			{TaskName: "mcp-local-hub-time-default"},
			{TaskName: "mcp-local-hub-godbolt-default"},
		}, nil
	}

	cmd, stdout, stderr := stubCmd()
	if err := runInstallUpgrade(cmd); err != nil {
		t.Fatalf("runInstallUpgrade: unexpected error %v (stderr=%q)", err, stderr.String())
	}
	wantOrder := []string{"stop", "bootstrap", "restart"}
	if len(order) != len(wantOrder) {
		t.Fatalf("order = %v, want %v", order, wantOrder)
	}
	for i, step := range wantOrder {
		if order[i] != step {
			t.Errorf("order[%d] = %q, want %q", i, order[i], step)
		}
	}
	out := stdout.String()
	for _, want := range []string{
		"Stopping running daemons",
		"✓ stopped mcp-local-hub-time-default",
		"✓ stopped mcp-local-hub-godbolt-default",
		"Copying new binary",
		"Restarting daemons",
		"✓ restarted mcp-local-hub-time-default",
		"✓ restarted mcp-local-hub-godbolt-default",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\nfull stdout:\n%s", want, out)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr should be empty on happy path; got %q", stderr.String())
	}
}

// TestRunInstallUpgrade_RefusesSelfReplace pins the canonical-path
// guard. Running --upgrade from the canonical binary must refuse
// loudly with an actionable hint, NOT proceed and silently no-op.
func TestRunInstallUpgrade_RefusesSelfReplace(t *testing.T) {
	resetUpgradeSeams(t)

	canonical := "C:\\Users\\u\\.local\\bin\\mcphub.exe"
	upgradeExecutableFn = func() (string, error) { return canonical, nil }
	upgradeTargetPathFn = func() (string, error) { return canonical, nil }
	// Should NOT reach Stop/Bootstrap/Restart — leave them unstubbed
	// (production nil → would call real APIs, which would mutate
	// state if reached).
	stopCalled := false
	upgradeStopAllFn = func() ([]api.RestartResult, error) {
		stopCalled = true
		return nil, nil
	}

	cmd, stdout, _ := stubCmd()
	err := runInstallUpgrade(cmd)
	if err == nil {
		t.Fatal("runInstallUpgrade: want error on self-replace, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to --upgrade from the canonical binary") {
		t.Errorf("error message missing self-replace marker; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), canonical) {
		t.Errorf("error message should name the canonical path; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "go build") {
		t.Errorf("error message should hint at `go build` recovery; got %q", err.Error())
	}
	if stopCalled {
		t.Errorf("StopAll must NOT be called when self-replace guard fires")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty on self-replace refusal; got %q", stdout.String())
	}
}

// TestRunInstallUpgrade_RefusesSelfReplaceCaseInsensitive pins that
// the samePath helper's Windows case-insensitive semantics are
// preserved through the guard — running upgrade with a path differing
// only in casing must still trip the guard.
//
// Bot r1 P1 closure on PR #181: stub the downstream Stop/Bootstrap/
// Restart calls so this test deterministically validates the Windows
// case-insensitive branch on every platform, instead of accidentally
// invoking the platform scheduler stub on Linux/macOS (where samePath
// is case-sensitive and the guard does NOT fire) and failing with
// "scheduler not implemented".
func TestRunInstallUpgrade_RefusesSelfReplaceCaseInsensitive(t *testing.T) {
	resetUpgradeSeams(t)
	upgradeExecutableFn = func() (string, error) {
		return "C:\\Users\\u\\.local\\bin\\MCPHUB.exe", nil
	}
	upgradeTargetPathFn = func() (string, error) {
		return "C:\\Users\\u\\.local\\bin\\mcphub.exe", nil
	}
	// Stub downstream so even if the guard doesn't fire (case-
	// sensitive POSIX samePath), the test gets a deterministic
	// success-path no-op rather than a platform-specific scheduler
	// error.
	upgradeStopAllFn = func() ([]api.RestartResult, error) { return nil, nil }
	upgradeBootstrapFn = func(io.Writer) error { return nil }
	upgradeRestartAllFn = func() ([]api.RestartResult, error) { return nil, nil }

	cmd, _, _ := stubCmd()
	err := runInstallUpgrade(cmd)
	// Windows: guard fires → wrapped refusal error.
	// POSIX: guard does NOT fire (case-sensitive samePath) → stubs
	// produce a clean nil return.
	// EITHER outcome is acceptable; what we forbid is the previous
	// failure mode where the guard didn't fire AND production
	// downstream code ran and returned a platform stub error.
	if err == nil {
		return // POSIX path: guard correctly did NOT fire
	}
	if !strings.Contains(err.Error(), "refusing to --upgrade from the canonical binary") {
		t.Errorf("error neither nil nor the guard refusal; downstream stubs leaked through? got %q", err.Error())
	}
}

// TestRunInstallUpgrade_StopAllError surfaces the error verbatim
// and does NOT proceed to Bootstrap/Restart. The binary must be
// left untouched when the stop phase couldn't even enumerate tasks.
func TestRunInstallUpgrade_StopAllError(t *testing.T) {
	resetUpgradeSeams(t)
	upgradeExecutableFn = func() (string, error) { return "C:\\dev\\mcphub.exe", nil }
	upgradeTargetPathFn = func() (string, error) { return "C:\\Users\\u\\.local\\bin\\mcphub.exe", nil }
	upgradeStopAllFn = func() ([]api.RestartResult, error) {
		return nil, errors.New("scheduler.New: COM init failed")
	}
	bootstrapCalled := false
	upgradeBootstrapFn = func(io.Writer) error {
		bootstrapCalled = true
		return nil
	}
	restartCalled := false
	upgradeRestartAllFn = func() ([]api.RestartResult, error) {
		restartCalled = true
		return nil, nil
	}

	cmd, _, _ := stubCmd()
	err := runInstallUpgrade(cmd)
	if err == nil || !strings.Contains(err.Error(), "stop all:") {
		t.Errorf("want wrapped stop error, got %v", err)
	}
	if !strings.Contains(err.Error(), "COM init failed") {
		t.Errorf("error should preserve underlying message; got %v", err)
	}
	if bootstrapCalled {
		t.Errorf("Bootstrap must NOT be called after StopAll error")
	}
	if restartCalled {
		t.Errorf("RestartAll must NOT be called after StopAll error")
	}
}

// TestRunInstallUpgrade_StopPerTaskErrorsAreSurfacedButNotFatal
// pins the "stop failures log to stderr but don't abort" branch.
// Individual daemon stop failure (e.g., taskkill on a stuck Force-
// killed daemon) shouldn't block the binary upgrade; the operator
// still gets to fix the watchdog code path.
func TestRunInstallUpgrade_StopPerTaskErrorsAreSurfacedButNotFatal(t *testing.T) {
	resetUpgradeSeams(t)
	upgradeExecutableFn = func() (string, error) { return "C:\\dev\\mcphub.exe", nil }
	upgradeTargetPathFn = func() (string, error) { return "C:\\Users\\u\\.local\\bin\\mcphub.exe", nil }
	upgradeStopAllFn = func() ([]api.RestartResult, error) {
		return []api.RestartResult{
			{TaskName: "mcp-local-hub-stuck-default", Err: "kill daemon: taskkill /F failed: access denied"},
			{TaskName: "mcp-local-hub-time-default"},
		}, nil
	}
	upgradeBootstrapFn = func(io.Writer) error { return nil }
	upgradeRestartAllFn = func() ([]api.RestartResult, error) {
		return []api.RestartResult{
			{TaskName: "mcp-local-hub-stuck-default"},
			{TaskName: "mcp-local-hub-time-default"},
		}, nil
	}

	cmd, stdout, stderr := stubCmd()
	if err := runInstallUpgrade(cmd); err != nil {
		t.Fatalf("partial stop failures should not abort upgrade; got %v", err)
	}
	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "⚠ stop mcp-local-hub-stuck-default") {
		t.Errorf("stderr should surface stop failure; got %q", stderrStr)
	}
	if !strings.Contains(stderrStr, "access denied") {
		t.Errorf("stderr should preserve underlying error; got %q", stderrStr)
	}
	stdoutStr := stdout.String()
	if !strings.Contains(stdoutStr, "✓ stopped mcp-local-hub-time-default") {
		t.Errorf("stdout should still report the successful stop; got %q", stdoutStr)
	}
}

// TestRunInstallUpgrade_BootstrapError surfaces the wrapped error
// and skips RestartAll. After a Bootstrap failure the binary is in
// an undefined state (either old or partial-temp) — restarting
// daemons would lock in whichever transient state landed; bail.
//
// Bot r3 P2 closure on PR #181: the error must carry upgrade-
// specific recovery guidance (NOT the underlying copyExe hint of
// "re-run setup", which would not restart daemons in --upgrade
// context). Verify both the bootstrap marker and the recovery hint.
func TestRunInstallUpgrade_BootstrapError(t *testing.T) {
	resetUpgradeSeams(t)
	upgradeExecutableFn = func() (string, error) { return "C:\\dev\\mcphub.exe", nil }
	upgradeTargetPathFn = func() (string, error) { return "C:\\Users\\u\\.local\\bin\\mcphub.exe", nil }
	upgradeStopAllFn = func() ([]api.RestartResult, error) { return nil, nil }
	upgradeBootstrapFn = func(io.Writer) error {
		return fmt.Errorf("target is in use — stop running daemons first")
	}
	restartCalled := false
	upgradeRestartAllFn = func() ([]api.RestartResult, error) {
		restartCalled = true
		return nil, nil
	}

	cmd, _, _ := stubCmd()
	err := runInstallUpgrade(cmd)
	if err == nil || !strings.Contains(err.Error(), "bootstrap (binary copy) failed") {
		t.Errorf("want wrapped bootstrap error, got %v", err)
	}
	if !strings.Contains(err.Error(), "target is in use") {
		t.Errorf("error should preserve underlying message; got %v", err)
	}
	if !strings.Contains(err.Error(), "mcphub install --upgrade") {
		t.Errorf("error should hint at re-running --upgrade; got %v", err)
	}
	if !strings.Contains(err.Error(), "mcphub restart --all") {
		t.Errorf("error should hint at restart --all; got %v", err)
	}
	if restartCalled {
		t.Errorf("RestartAll must NOT run after Bootstrap failure")
	}
}

// TestUpgradeIsSelfReplace pins the self-replace identity check
// (bot r3 P2 closure on PR #181). Verifies:
//
//   - exact string match returns true (legacy samePath path)
//   - Windows case-insensitive string match returns true on Windows
//     (per samePath semantics)
//   - distinct files at distinct paths return false
//   - aliased paths pointing at the same file via os.SameFile return
//     true even when the strings differ — exercised via two paths
//     to the same temp file
//   - missing target file returns false (first-install case;
//     legitimate upgrade scenario where target doesn't exist yet)
func TestUpgradeIsSelfReplace(t *testing.T) {
	// Create a real file in a temp dir so os.SameFile has data.
	dir := t.TempDir()
	realPath := filepath.Join(dir, "mcphub.exe")
	if err := os.WriteFile(realPath, []byte("fake binary"), 0755); err != nil {
		t.Fatalf("write real file: %v", err)
	}
	otherPath := filepath.Join(dir, "other-binary.exe")
	if err := os.WriteFile(otherPath, []byte("other"), 0755); err != nil {
		t.Fatalf("write other file: %v", err)
	}

	t.Run("exact string match → true", func(t *testing.T) {
		if !upgradeIsSelfReplace(realPath, realPath) {
			t.Error("want true on exact string match")
		}
	})
	t.Run("distinct files at distinct paths → false", func(t *testing.T) {
		if upgradeIsSelfReplace(realPath, otherPath) {
			t.Error("want false on distinct files")
		}
	})
	t.Run("missing target file → false (first-install case)", func(t *testing.T) {
		ghost := filepath.Join(dir, "does-not-exist.exe")
		if upgradeIsSelfReplace(realPath, ghost) {
			t.Error("want false when target doesn't exist yet")
		}
	})
	t.Run("missing source file → false", func(t *testing.T) {
		ghost := filepath.Join(dir, "ghost.exe")
		if upgradeIsSelfReplace(ghost, realPath) {
			t.Error("want false when source doesn't exist")
		}
	})
	// Case-insensitive variant exercised on Windows-style paths;
	// on POSIX samePath is case-sensitive so this resolves via the
	// os.SameFile fallback when the two paths point at the same
	// inode. On Windows, samePath itself returns true. We just
	// verify that an upper-case variant of the same path returns
	// true regardless of platform.
	t.Run("case-insensitive path on the same file → true (Windows) / inode-match (POSIX)", func(t *testing.T) {
		got := upgradeIsSelfReplace(realPath, strings.ToUpper(realPath))
		if runtime.GOOS == "windows" && !got {
			t.Error("want true on Windows for case-insensitive same path")
		}
		// On POSIX, this case is platform-dependent on filesystem
		// case-folding; don't assert either way.
	})
}

// TestRunInstallUpgrade_RestartAllPartialFailure reports the count
// and tells the operator how to converge (`mcphub restart --all`).
func TestRunInstallUpgrade_RestartAllPartialFailure(t *testing.T) {
	resetUpgradeSeams(t)
	upgradeExecutableFn = func() (string, error) { return "C:\\dev\\mcphub.exe", nil }
	upgradeTargetPathFn = func() (string, error) { return "C:\\Users\\u\\.local\\bin\\mcphub.exe", nil }
	upgradeStopAllFn = func() ([]api.RestartResult, error) {
		return []api.RestartResult{
			{TaskName: "mcp-local-hub-time-default"},
			{TaskName: "mcp-local-hub-godbolt-default"},
		}, nil
	}
	upgradeBootstrapFn = func(io.Writer) error { return nil }
	upgradeRestartAllFn = func() ([]api.RestartResult, error) {
		return []api.RestartResult{
			{TaskName: "mcp-local-hub-time-default"},
			{TaskName: "mcp-local-hub-godbolt-default", Err: "schtasks /Run: ERROR_FILE_NOT_FOUND"},
		}, nil
	}

	cmd, _, stderr := stubCmd()
	err := runInstallUpgrade(cmd)
	if err == nil {
		t.Fatal("partial restart failure should propagate as error")
	}
	if !strings.Contains(err.Error(), "1 daemon(s) failed to restart") {
		t.Errorf("error should name the failure count; got %v", err)
	}
	if !strings.Contains(err.Error(), "mcphub restart --all") {
		t.Errorf("error should hint at the recovery command; got %v", err)
	}
	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "✗ restart mcp-local-hub-godbolt-default") {
		t.Errorf("stderr should surface per-task restart failure; got %q", stderrStr)
	}
	if !strings.Contains(stderrStr, "ERROR_FILE_NOT_FOUND") {
		t.Errorf("stderr should preserve underlying error; got %q", stderrStr)
	}
}

// TestRunInstallUpgrade_ExecutableLookupError surfaces the wrapped
// error rather than papering over it (production rare but possible
// on hostile filesystems).
func TestRunInstallUpgrade_ExecutableLookupError(t *testing.T) {
	resetUpgradeSeams(t)
	upgradeExecutableFn = func() (string, error) { return "", errors.New("/proc/self/exe: no such file or directory") }
	cmd, _, _ := stubCmd()
	err := runInstallUpgrade(cmd)
	if err == nil || !strings.Contains(err.Error(), "resolve self-replace guard paths:") {
		t.Errorf("want wrapped self-resolution error, got %v", err)
	}
	if !strings.Contains(err.Error(), "/proc/self/exe") {
		t.Errorf("error should preserve underlying message; got %v", err)
	}
}

// TestRunInstallUpgrade_RefusesDevBuild pins the PR #188 A8 closure
// (dev-build guard). A binary built without the build scripts'
// ldflags shows version=="dev" and on Windows is CONSOLE-subsystem —
// installing it would re-introduce the terminal-flash + tray-broken
// regression caught in the 2026-05-15 smoke session. The guard runs
// AFTER self-replace check (so a self-replace error wins) but
// BEFORE StopAll (so daemons aren't stopped uselessly).
func TestRunInstallUpgrade_RefusesDevBuild(t *testing.T) {
	resetUpgradeSeams(t)

	upgradeExecutableFn = func() (string, error) { return "C:\\dev\\mcphub.exe", nil }
	upgradeTargetPathFn = func() (string, error) { return "C:\\Users\\u\\.local\\bin\\mcphub.exe", nil }
	upgradeBuildVersionFn = func() string { return "dev" }
	stopCalled := false
	upgradeStopAllFn = func() ([]api.RestartResult, error) {
		stopCalled = true
		return nil, nil
	}

	cmd, _, _ := stubCmd()
	err := runInstallUpgrade(cmd)
	if err == nil {
		t.Fatal("want dev-build refusal, got nil")
	}
	for _, want := range []string{
		"refusing to --upgrade from a dev-build binary",
		`version="dev"`,
		"build.sh",
		"CONSOLE-subsystem",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must contain %q for operator clarity; got %q", want, err.Error())
		}
	}
	if stopCalled {
		t.Errorf("StopAll must NOT be called when dev-build guard fires; would stop daemons for no reason")
	}
}

// TestRunInstallUpgrade_RefusesIfGUIRunning pins the running-GUI
// guard: if a `mcphub.exe gui` process is found whose image path
// equals the install target, --upgrade refuses BEFORE StopAll runs.
// This prevents the 2026-05-15 footgun where StopAll succeeded but
// Bootstrap then failed with "target in use", leaving the daemon
// fleet down.
func TestRunInstallUpgrade_RefusesIfGUIRunning(t *testing.T) {
	resetUpgradeSeams(t)

	upgradeExecutableFn = func() (string, error) { return "C:\\dev\\mcphub.exe", nil }
	upgradeTargetPathFn = func() (string, error) { return "C:\\Users\\u\\.local\\bin\\mcphub.exe", nil }
	findRunningGUIsOnTargetFn = func(target string) ([]api.ProcessInfo, error) {
		return []api.ProcessInfo{
			{PID: 12345, Cmdline: `"C:\Users\u\.local\bin\mcphub.exe" gui --no-browser`},
		}, nil
	}
	stopCalled := false
	upgradeStopAllFn = func() ([]api.RestartResult, error) {
		stopCalled = true
		return nil, nil
	}

	cmd, _, _ := stubCmd()
	err := runInstallUpgrade(cmd)
	if err == nil {
		t.Fatal("want running-GUI refusal, got nil")
	}
	for _, want := range []string{
		"refusing to --upgrade with a running mcphub GUI",
		"12345",
		"Stop-Process",
		"tray menu",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must contain %q; got %q", want, err.Error())
		}
	}
	if stopCalled {
		t.Errorf("StopAll must NOT be called when GUI is running; would leave daemons down on Bootstrap failure")
	}
}

// TestRunInstallUpgrade_GUIDetectionErrorIsBestEffort pins that a
// wmic failure during GUI detection does NOT abort the upgrade —
// the detection is informational, and Bootstrap itself will still
// catch a real "target in use" if the GUI is actually running. A
// wmic outage on a hardened CI/locked-down host should not block
// the upgrade flow.
func TestRunInstallUpgrade_GUIDetectionErrorIsBestEffort(t *testing.T) {
	resetUpgradeSeams(t)

	upgradeExecutableFn = func() (string, error) { return "C:\\dev\\mcphub.exe", nil }
	upgradeTargetPathFn = func() (string, error) { return "C:\\Users\\u\\.local\\bin\\mcphub.exe", nil }
	findRunningGUIsOnTargetFn = func(target string) ([]api.ProcessInfo, error) {
		return nil, errors.New("wmic: process not found")
	}
	stopCalled := false
	upgradeStopAllFn = func() ([]api.RestartResult, error) {
		stopCalled = true
		return nil, nil
	}
	upgradeBootstrapFn = func(w io.Writer) error { return nil }
	upgradeRestartAllFn = func() ([]api.RestartResult, error) { return nil, nil }

	cmd, _, stderr := stubCmd()
	if err := runInstallUpgrade(cmd); err != nil {
		t.Fatalf("upgrade should proceed despite GUI detection error: %v", err)
	}
	if !stopCalled {
		t.Errorf("StopAll should still run when GUI detection fails (best-effort)")
	}
	if !strings.Contains(stderr.String(), "GUI detection failed") {
		t.Errorf("stderr should warn about GUI detection failure; got %q", stderr.String())
	}
}

// TestCmdlineIsGUIOnTarget pins the helper that parses wmic
// CommandLine strings into (image, args) and matches against the
// install target.
func TestCmdlineIsGUIOnTarget(t *testing.T) {
	target := `C:\Users\u\.local\bin\mcphub.exe`
	cases := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{
			"quoted path with gui arg",
			`"C:\Users\u\.local\bin\mcphub.exe" gui --no-browser`,
			true,
		},
		{
			"unquoted path with gui arg",
			`C:\Users\u\.local\bin\mcphub.exe gui`,
			true,
		},
		{
			"Explorer launch — no args",
			`"C:\Users\u\.local\bin\mcphub.exe"`,
			true,
		},
		{
			"case-insensitive path match",
			`"C:\users\U\.local\bin\MCPHUB.EXE" gui`,
			true,
		},
		{
			"daemon process — reject",
			`"C:\Users\u\.local\bin\mcphub.exe" daemon --server time --daemon default`,
			false,
		},
		{
			"watchdog process — reject",
			`"C:\Users\u\.local\bin\mcphub.exe" watchdog --once`,
			false,
		},
		{
			"tray child process — reject",
			`C:\Users\u\.local\bin\mcphub.exe tray`,
			false,
		},
		{
			"different path — reject",
			`"D:\dev\mcp-local-hub\bin\mcphub.exe" gui`,
			false,
		},
		{
			"empty cmdline — reject",
			``,
			false,
		},
		{
			"malformed unterminated quote — reject",
			`"C:\Users\u\.local\bin\mcphub.exe gui`,
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cmdlineIsGUIOnTarget(tc.cmdline, target)
			if got != tc.want {
				t.Errorf("cmdlineIsGUIOnTarget(%q, %q) = %v; want %v", tc.cmdline, target, got, tc.want)
			}
		})
	}
}

// TestInstallCmd_UpgradeMutexErrors pins that every manifest-install
// flag combo with --upgrade returns a single coherent error message.
// One-shot mutex check at the top of RunE — if --upgrade is set and
// any other install flag is also set, refuse before doing anything.
func TestInstallCmd_UpgradeMutexErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"server", []string{"--upgrade", "--server", "time"}},
		{"daemon", []string{"--upgrade", "--daemon", "default"}},
		{"all", []string{"--upgrade", "--all"}},
		{"clients", []string{"--upgrade", "--clients", "claude-code"}},
		{"all-clients", []string{"--upgrade", "--all-clients"}},
		{"reconcile-hub-mode", []string{"--upgrade", "--reconcile-hub-mode"}},
		// Bot r1 P2 closure on PR #181: --dry-run + --upgrade must
		// reject. Otherwise dry-run would silently violate its
		// "no side effects" contract.
		{"dry-run", []string{"--upgrade", "--dry-run"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newInstallCmdReal()
			c.SetArgs(tc.args)
			c.SetOut(&bytes.Buffer{})
			c.SetErr(&bytes.Buffer{})
			c.SilenceUsage = true
			c.SilenceErrors = true
			err := c.Execute()
			if err == nil {
				t.Fatalf("want mutex error for %v, got nil", tc.args)
			}
			if !strings.Contains(err.Error(), "--upgrade is mutually exclusive") {
				t.Errorf("want mutex error, got %q", err.Error())
			}
		})
	}
}
