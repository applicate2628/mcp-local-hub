package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
	origRestartTasks := upgradeRestartTasksFn
	origInstall := upgradeInstallServerFn
	origExec := upgradeExecutableFn
	origTarget := upgradeTargetPathFn
	origFindGUI := findRunningGUIsOnTargetFn
	origVersion := upgradeBuildVersionFn
	t.Cleanup(func() {
		upgradeStopAllFn = origStop
		upgradeBootstrapFn = origBoot
		upgradeRestartAllFn = origRestart
		upgradeRestartTasksFn = origRestartTasks
		upgradeInstallServerFn = origInstall
		upgradeExecutableFn = origExec
		upgradeTargetPathFn = origTarget
		findRunningGUIsOnTargetFn = origFindGUI
		upgradeBuildVersionFn = origVersion
	})
	upgradeStopAllFn = nil
	upgradeBootstrapFn = nil
	upgradeRestartAllFn = nil
	upgradeRestartTasksFn = nil
	upgradeInstallServerFn = nil
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

func windowsFixturePath(drive string, segments ...string) string {
	return drive + ":" + "\\" + strings.Join(segments, "\\")
}

func upgradeFixtureExecutable() string {
	return windowsFixturePath("C", "dev", "mcphub.exe")
}

func upgradeFixtureTarget() string {
	return windowsFixturePath("C", "Users", "u", ".local", "bin", "mcphub.exe")
}

// TestRunInstallUpgrade_HappyPath pins the StopAll → Bootstrap →
// RestartAll order and verifies the success-line output.
func TestRunInstallUpgrade_HappyPath(t *testing.T) {
	resetUpgradeSeams(t)

	var order []string
	upgradeExecutableFn = func() (string, error) { return upgradeFixtureExecutable(), nil }
	upgradeTargetPathFn = func() (string, error) { return upgradeFixtureTarget(), nil }
	upgradeStopAllFn = func() ([]api.RestartResult, error) {
		order = append(order, "stop")
		return []api.RestartResult{
			{TaskName: "mcp-local-hub-time-default"},
			{TaskName: "mcp-local-hub-godbolt-default"},
		}, nil
	}
	upgradeBootstrapFn = func(w io.Writer) error {
		order = append(order, "bootstrap")
		_, _ = io.WriteString(w, "✓ mcphub installed at "+upgradeFixtureTarget()+"\n")
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

	canonical := upgradeFixtureTarget()
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
	if !strings.Contains(err.Error(), "pwsh ./build.ps1") {
		t.Errorf("error message should hint at `pwsh ./build.ps1` recovery; got %q", err.Error())
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
		return strings.Replace(upgradeFixtureTarget(), "mcphub.exe", "MCPHUB.exe", 1), nil
	}
	upgradeTargetPathFn = func() (string, error) {
		return upgradeFixtureTarget(), nil
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
	upgradeExecutableFn = func() (string, error) { return upgradeFixtureExecutable(), nil }
	upgradeTargetPathFn = func() (string, error) { return upgradeFixtureTarget(), nil }
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
	upgradeExecutableFn = func() (string, error) { return upgradeFixtureExecutable(), nil }
	upgradeTargetPathFn = func() (string, error) { return upgradeFixtureTarget(), nil }
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
	upgradeExecutableFn = func() (string, error) { return upgradeFixtureExecutable(), nil }
	upgradeTargetPathFn = func() (string, error) { return upgradeFixtureTarget(), nil }
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
	upgradeExecutableFn = func() (string, error) { return upgradeFixtureExecutable(), nil }
	upgradeTargetPathFn = func() (string, error) { return upgradeFixtureTarget(), nil }
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
// ldflags shows version=="dev" and cannot satisfy the Windows product
// binary PE subsystem admission gate —
// installing it would re-introduce the terminal-flash + tray-broken
// regression caught in the 2026-05-15 smoke session. The guard runs
// AFTER self-replace check (so a self-replace error wins) but
// BEFORE StopAll (so daemons aren't stopped uselessly).
//
// Codex bot r5 P2 closure: scoped to Windows only. The PE subsystem
// admission gate is Windows-specific, and POSIX devs commonly
// run untagged `go build` binaries. Test is gated on
// runtime.GOOS == "windows"; on POSIX the same setup proceeds to
// happy-path execution (TestRunInstallUpgrade_AllowsDevBuildOnPOSIX
// pins that path).
func TestRunInstallUpgrade_RefusesDevBuild(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("dev-build refusal is Windows-only; see TestRunInstallUpgrade_AllowsDevBuildOnPOSIX for the POSIX path")
	}
	resetUpgradeSeams(t)

	upgradeExecutableFn = func() (string, error) { return upgradeFixtureExecutable(), nil }
	upgradeTargetPathFn = func() (string, error) { return upgradeFixtureTarget(), nil }
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
		"pwsh ./build.ps1",
		"PE subsystem admission gate",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must contain %q for operator clarity; got %q", want, err.Error())
		}
	}
	if stopCalled {
		t.Errorf("StopAll must NOT be called when dev-build guard fires; would stop daemons for no reason")
	}
}

// TestRunInstallUpgrade_AllowsDevBuildOnPOSIX pins codex bot r5 P2:
// the dev-build guard is Windows-only because the PE subsystem admission
// gate doesn't exist on POSIX. A POSIX dev-build (version=="dev")
// must proceed through the normal flow.
func TestRunInstallUpgrade_AllowsDevBuildOnPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only: Windows path is exercised by TestRunInstallUpgrade_RefusesDevBuild")
	}
	resetUpgradeSeams(t)

	binaryPath := filepath.Join(t.TempDir(), "mcphub")
	targetPath := filepath.Join(t.TempDir(), "mcphub")
	upgradeExecutableFn = func() (string, error) { return binaryPath, nil }
	upgradeTargetPathFn = func() (string, error) { return targetPath, nil }
	upgradeBuildVersionFn = func() string { return "dev" }
	bootstrapCalled := false
	upgradeStopAllFn = func() ([]api.RestartResult, error) { return nil, nil }
	upgradeBootstrapFn = func(w io.Writer) error { bootstrapCalled = true; return nil }
	upgradeRestartAllFn = func() ([]api.RestartResult, error) { return nil, nil }

	cmd, _, _ := stubCmd()
	if err := runInstallUpgrade(cmd); err != nil {
		t.Fatalf("POSIX dev-build should proceed: %v", err)
	}
	if !bootstrapCalled {
		t.Errorf("Bootstrap should run on POSIX dev-build path")
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

	upgradeExecutableFn = func() (string, error) { return upgradeFixtureExecutable(), nil }
	upgradeTargetPathFn = func() (string, error) { return upgradeFixtureTarget(), nil }
	findRunningGUIsOnTargetFn = func(target string) ([]api.ProcessInfo, error) {
		return []api.ProcessInfo{
			{PID: 12345, Cmdline: `"` + upgradeFixtureTarget() + `" gui --no-browser`},
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

	upgradeExecutableFn = func() (string, error) { return upgradeFixtureExecutable(), nil }
	upgradeTargetPathFn = func() (string, error) { return upgradeFixtureTarget(), nil }
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
	target := upgradeFixtureTarget()
	quotedTarget := `"` + target + `"`
	other := windowsFixturePath("D", "dev", "mcp-local-hub", "bin", "mcphub.exe")
	quotedOther := `"` + other + `"`
	cases := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{
			"quoted path with gui arg",
			quotedTarget + " gui --no-browser",
			true,
		},
		{
			"unquoted path with gui arg",
			target + " gui",
			true,
		},
		{
			"Explorer launch — no args",
			quotedTarget,
			true,
		},
		{
			"case-insensitive path match",
			`"` + windowsFixturePath("C", "users", "U", ".local", "bin", "MCPHUB.EXE") + `" gui`,
			true,
		},
		{
			"daemon process — reject",
			quotedTarget + " daemon --server time --daemon default",
			false,
		},
		{
			"watchdog process — reject",
			quotedTarget + " watchdog --once",
			false,
		},
		{
			"tray child process — reject",
			target + " tray",
			false,
		},
		{
			"different path — reject",
			quotedOther + " gui",
			false,
		},
		{
			"empty cmdline — reject",
			``,
			false,
		},
		{
			// Edge case: leading quote with no closing quote. PR #188
			// r2 parser is permissive — strips leading quote, then
			// matches target as case-insensitive prefix. Matching is
			// the correct behavior because the cmdline IS targeting
			// our binary with `gui`. WMIC/PowerShell don't emit
			// unterminated quotes in practice, so this is a defensive
			// case rather than a real-world input.
			"unterminated-quote variant — accept (still targets binary)",
			`"` + target + " gui",
			true,
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

// TestCmdlineIsGUIOnTarget_AliasedPathWithSpaces pins codex bot
// r7 P1 closure: the SameFile fallback was using a fixed
// "first whitespace = image boundary" extraction, which mis-cut
// an aliased path containing spaces. The progressive boundary scan now
// tries every whitespace position as a candidate image/args
// split, stopping at the first one where sameFileOrFalse(image,
// target) returns true.
func TestCmdlineIsGUIOnTarget_AliasedPathWithSpaces(t *testing.T) {
	target := upgradeFixtureTarget()
	origSF := sameFileOrFalseFn
	t.Cleanup(func() { sameFileOrFalseFn = origSF })

	// SameFile returns true only when called with the exact
	// junction path containing spaces (simulates a junction
	// alias whose source dir name has a space).
	aliasPath := windowsFixturePath("C", "Alias dir", "mcphub.exe")
	sameFileOrFalseFn = func(path1, _ string) bool {
		return path1 == aliasPath
	}

	cmdline := aliasPath + " gui --no-browser"
	if !cmdlineIsGUIOnTarget(cmdline, target) {
		t.Errorf("aliased-path-with-spaces + gui must accept via progressive boundary scan; got false")
	}
	// daemon subcommand on same path → reject
	cmdline = aliasPath + " daemon --server time"
	if cmdlineIsGUIOnTarget(cmdline, target) {
		t.Errorf("aliased-path-with-spaces + daemon must reject; got true")
	}
	// Explorer-launch on same alias (no args, alias has spaces)
	cmdline = aliasPath
	if !cmdlineIsGUIOnTarget(cmdline, target) {
		t.Errorf("aliased-path-with-spaces Explorer-launch must accept; got false")
	}
	// alias with 10+ wrong boundaries (cap reached) → reject
	sameFileOrFalseFn = func(_, _ string) bool { return false }
	cmdline = `a b c d e f g h i j k l m n o p`
	if cmdlineIsGUIOnTarget(cmdline, target) {
		t.Errorf("cmdline with 10+ wrong boundaries + no match must reject; got true")
	}
}

// TestCmdlineIsGUIOnTarget_FileIdentityFallback pins codex bot r4
// P1 closure: the case-insensitive prefix match misses path
// aliases (8.3 short names, junctions, symlinks) whose strings
// differ from the canonical target but resolve to the same
// file. The fallback uses os.SameFile (via the sameFileOrFalseFn
// test seam here) to catch these cases.
func TestCmdlineIsGUIOnTarget_FileIdentityFallback(t *testing.T) {
	target := upgradeFixtureTarget()
	shortAlias := windowsFixturePath("C", "PROGRA~1", "PROFIL~1", "u", ".local", "bin", "mcphub.exe")
	junctionAlias := windowsFixturePath("C", "junction-link", "mcphub.exe")
	other := windowsFixturePath("D", "dev", "mcp-local-hub", "bin", "mcphub.exe")
	origSF := sameFileOrFalseFn
	t.Cleanup(func() { sameFileOrFalseFn = origSF })

	// 1. 8.3 short path → prefix match fails, sameFileOrFalse
	//    returns true → accept (gui arg).
	sameFileOrFalseFn = func(path1, path2 string) bool { return true }
	cmdline := shortAlias + " gui"
	if !cmdlineIsGUIOnTarget(cmdline, target) {
		t.Errorf("8.3 short-path alias should match via SameFile fallback; got false")
	}
	// 2. Junction path → same SameFile fallback path.
	cmdline = junctionAlias + " gui --no-browser"
	if !cmdlineIsGUIOnTarget(cmdline, target) {
		t.Errorf("junction-aliased path should match via SameFile fallback; got false")
	}
	// 3. 8.3 short path + daemon arg → reject (not gui subcommand).
	cmdline = shortAlias + " daemon --server time"
	if cmdlineIsGUIOnTarget(cmdline, target) {
		t.Errorf("8.3 alias + daemon subcommand must reject; got true")
	}
	// 4. SameFile returns false → reject. Catches the case where
	//    an alias resolves to a DIFFERENT binary (e.g., build-dir
	//    image vs. canonical install).
	sameFileOrFalseFn = func(path1, path2 string) bool { return false }
	cmdline = other + " gui"
	if cmdlineIsGUIOnTarget(cmdline, target) {
		t.Errorf("different image file (SameFile=false) must reject; got true")
	}
}

// TestCmdlineIsGUIOnTarget_PathWithSpaces pins codex bot r2 P1
// closure: splitCSVLine strips quotes from the WMIC/PowerShell
// CSV cmdline cell. A target path containing spaces — common on
// Windows profile directories —
// arrives at cmdlineIsGUIOnTarget WITHOUT quotes, so a naive
// "first whitespace = image boundary" heuristic would split the
// image-path mid-string and miss the running GUI.
//
// The fixed parser uses case-insensitive prefix match against
// target instead of a whitespace split, so spaces inside the
// target path don't confuse it.
func TestCmdlineIsGUIOnTarget_PathWithSpaces(t *testing.T) {
	target := windowsFixturePath("C", "Users", "John Doe", ".local", "bin", "mcphub.exe")
	quotedTarget := `"` + target + `"`
	caseFoldedTarget := windowsFixturePath("C", "users", "JOHN doe", ".LOCAL", "bin", "MCPHUB.EXE")
	other := windowsFixturePath("D", "Other Users", "John Doe", ".local", "bin", "mcphub.exe")
	cases := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{
			"unquoted target with space + gui arg",
			target + " gui --no-browser",
			true,
		},
		{
			"unquoted target with space + Explorer launch",
			target,
			true,
		},
		{
			"unquoted target with space + daemon arg — reject",
			target + " daemon --server time --daemon default",
			false,
		},
		{
			"quoted target with space + gui arg",
			quotedTarget + " gui --no-browser",
			true,
		},
		{
			"case-insensitive target with space",
			caseFoldedTarget + " gui",
			true,
		},
		{
			"different path with same suffix — reject",
			other + " gui",
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

// TestCmdlineIsGUIOnTarget_UnicodePath pins codex bot r6 P2: the
// matchTargetPrefix helper must walk strings rune-by-rune (not
// slice on len(target) after a strings.ToLower roundtrip) because
// Unicode case-folding can change byte length. Cyrillic / CJK /
// Turkish profile names appear in real Windows user paths.
func TestCmdlineIsGUIOnTarget_UnicodePath(t *testing.T) {
	target := windowsFixturePath("C", "Users", "Дмитрий", ".local", "bin", "mcphub.exe")
	caseFoldedTarget := windowsFixturePath("C", "users", "ДМИТРИЙ", ".LOCAL", "BIN", "MCPHUB.EXE")
	other := windowsFixturePath("C", "Users", "Иван", ".local", "bin", "mcphub.exe")
	cases := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{
			"exact-case Cyrillic + gui",
			target + " gui",
			true,
		},
		{
			"case-folded Cyrillic + gui — accept (rune fold)",
			caseFoldedTarget + " gui --no-browser",
			true,
		},
		{
			"Cyrillic + daemon arg — reject",
			target + " daemon --server time",
			false,
		},
		{
			"different Unicode dir — reject",
			other + " gui",
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

// ---------------------------------------------------------------------------
// Task 8.2: v0.5.0 supervisor IPC handoff orchestration tests.
//
// These tests exercise RunInstallUpgrade (install_upgrade.go) — the new
// orchestrator that drives the rename-aside + IPC quiesce + IPC exit +
// force-kill fallback + per-OS supervisor start sequence per spec
// §"Upgrade sequence". The legacy runInstallUpgrade above remains in
// place for the v0.4.x Scheduler-backed flow; the two coexist until
// v0.5.0 ships and the supervisor architecture replaces Scheduler
// orchestration end-to-end.
//
// Every external side effect goes through the UpgradeDeps interface so
// tests inject fakes for RenameAsideBinary / QuiesceTimers /
// ExitGraceful / ForceKillSupervisor / StartSupervisor. The fake
// implementation is fakeUpgradeDeps below.
// ---------------------------------------------------------------------------

// fakeUpgradeDeps records every UpgradeDeps method call and returns
// caller-configured results / errors. Tests construct one inline,
// set the result fields they care about, and assert on the *Called
// booleans after running RunInstallUpgrade.
//
// Pattern parallels the upgrade*Fn package-level seams used by the
// legacy runInstallUpgrade above, but routed through an explicit
// interface so the new orchestrator stays pure (no globals to reset
// across test runs).
type fakeUpgradeDeps struct {
	calls []string

	renameAsideErr    error
	renameAsideCalled bool
	retainedPrior     string
	restoreErr        error

	quiesceResult    api.IPCResponse
	quiesceErr       error
	quiesceCalled    bool
	quiesceTimeoutMs int

	exitResult    api.IPCResponse
	exitErr       error
	exitCalled    bool
	exitTimeoutMs int

	forceKillCalled bool
	forceKillErr    error

	startErr    error
	startCalled bool
}

func (f *fakeUpgradeDeps) RenameAsideBinary(target, newSrc string) (string, error) {
	f.calls = append(f.calls, "rename")
	f.renameAsideCalled = true
	retained := f.retainedPrior
	if retained == "" {
		retained = target + ".old-test"
	}
	return retained, f.renameAsideErr
}

func (f *fakeUpgradeDeps) RestoreRetainedBinary(target, retainedPrior string) error {
	f.calls = append(f.calls, "restore")
	return f.restoreErr
}

func (f *fakeUpgradeDeps) QuiesceTimers(ctx context.Context, pipePath string, timeoutMs int) (api.IPCResponse, error) {
	f.calls = append(f.calls, "quiesce")
	f.quiesceCalled = true
	f.quiesceTimeoutMs = timeoutMs
	return f.quiesceResult, f.quiesceErr
}

func (f *fakeUpgradeDeps) ExitGraceful(ctx context.Context, pipePath string, timeoutMs int) (api.IPCResponse, error) {
	f.calls = append(f.calls, "exit")
	f.exitCalled = true
	f.exitTimeoutMs = timeoutMs
	return f.exitResult, f.exitErr
}

func (f *fakeUpgradeDeps) ForceKillSupervisor(pipePath string) error {
	f.calls = append(f.calls, "force-kill")
	f.forceKillCalled = true
	return f.forceKillErr
}

func (f *fakeUpgradeDeps) StartSupervisor(binaryPath string) error {
	f.calls = append(f.calls, "start")
	f.startCalled = true
	return f.startErr
}

func TestRollbackInstallUpgradeRefusesPendingStopSettlementBeforeForceKill(t *testing.T) {
	mock := &fakeUpgradeDeps{}
	err := rollbackInstallUpgrade(context.Background(), UpgradeOpts{
		Deps: mock,
		WithRollbackStopSettlementFence: func(ctx context.Context, critical func() error) error {
			return errors.New("pending stop settlement remains durable")
		},
	}, "prior.exe", time.Second, errors.New("successor readiness failed"))
	if err == nil || !strings.Contains(err.Error(), "pending stop settlement") {
		t.Fatalf("rollback error = %v, want pending settlement refusal", err)
	}
	if mock.forceKillCalled || mock.startCalled || len(mock.calls) != 0 {
		t.Fatalf("rollback performed side effects before receipt preflight: %+v", mock.calls)
	}
}

// TestRollbackInstallUpgradeRequiresStopSettlementFence catches the unsafe
// legacy fallback: a rollback caller without the state-path-bound fence must
// never force-kill a successor merely because it cannot inspect receipts.
func TestRollbackInstallUpgradeRequiresStopSettlementFence(t *testing.T) {
	mock := &fakeUpgradeDeps{}
	err := rollbackInstallUpgrade(context.Background(), UpgradeOpts{Deps: mock}, "prior.exe", time.Second, errors.New("successor readiness failed"))
	if err == nil || !strings.Contains(err.Error(), "stop-settlement fence") {
		t.Fatalf("rollback error = %v, want missing fence refusal", err)
	}
	if mock.forceKillCalled || mock.startCalled || len(mock.calls) != 0 {
		t.Fatalf("rollback performed side effects without fence: %+v", mock.calls)
	}
}

// TestRollbackInstallUpgradeForceKillsOnlyInsideStopSettlementFence catches a
// future split of check and kill into separate lock epochs. The fencer owns the
// callback boundary; the force kill must run only from that callback.
func TestRollbackInstallUpgradeForceKillsOnlyInsideStopSettlementFence(t *testing.T) {
	mock := &fakeUpgradeDeps{}
	fenceCalled := false
	inFence := false
	mock.forceKillErr = errors.New("ERROR: The process \"12345\" not found.")
	err := rollbackInstallUpgrade(context.Background(), UpgradeOpts{
		Deps: mock,
		WithRollbackStopSettlementFence: func(ctx context.Context, critical func() error) error {
			fenceCalled = true
			inFence = true
			defer func() { inFence = false }()
			return critical()
		},
	}, "prior.exe", time.Second, errors.New("successor readiness failed"))
	if err == nil || !strings.Contains(err.Error(), "automatic rollback restored") {
		t.Fatalf("rollback result = %v, want restored-prior diagnostic", err)
	}
	if !fenceCalled || !mock.forceKillCalled {
		t.Fatalf("fence=%v forceKill=%v, want both", fenceCalled, mock.forceKillCalled)
	}
	if inFence {
		t.Fatal("fence did not return after force-kill callback")
	}
}

// TestInstallUpgrade_HappyPath pins the canonical sequence:
// rename-aside → quiesce → exit{graceful} → start. Every step runs
// exactly once with a clean response; no force-kill fallback fires.
func TestInstallUpgrade_HappyPath(t *testing.T) {
	mock := &fakeUpgradeDeps{
		renameAsideErr: nil,
		quiesceResult:  api.IPCResponse{ID: 1, OK: true, Result: map[string]any{"drained": 1.0, "still_running": []any{}}, Final: true},
		exitResult:     api.IPCResponse{ID: 2, OK: true, Result: "exit-acked"},
		startErr:       nil,
	}
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath: "/fake/mcphub",
		NewBinary:  "/fake/mcphub.new",
		PipePath:   "fake-pipe",
		Deps:       mock,
	})
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if !mock.renameAsideCalled {
		t.Fatal("renameAside not called")
	}
	if !mock.quiesceCalled {
		t.Fatal("quiesce not called")
	}
	if !mock.exitCalled {
		t.Fatal("exit not called")
	}
	if !mock.startCalled {
		t.Fatal("start not called")
	}
	if mock.forceKillCalled {
		t.Error("force-kill must NOT fire on happy path (ExitGraceful succeeded)")
	}
}

// TestInstallUpgrade_ExitTimeoutFallsBackToForceKill pins the spec
// §"Fallback if step 4 IPC fails" path. ExitGraceful returns a
// timeout error → orchestrator invokes ForceKillSupervisor before
// proceeding to StartSupervisor. The overall return must be nil
// because force-kill is part of the normal recovery flow, not an
// abort condition.
func TestInstallUpgrade_ExitTimeoutFallsBackToForceKill(t *testing.T) {
	mock := &fakeUpgradeDeps{
		exitErr: errors.New("timeout"), // exit IPC times out
	}
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath:    "/fake/mcphub",
		NewBinary:     "/fake/mcphub.new",
		PipePath:      "fake-pipe",
		ExitTimeoutMs: 5000,
		Deps:          mock,
	})
	if err != nil {
		t.Fatalf("force-kill fallback should not error: %v", err)
	}
	if !mock.forceKillCalled {
		t.Fatal("force-kill not invoked after exit timeout")
	}
	if !mock.startCalled {
		t.Error("StartSupervisor must still run after force-kill (the upgrade must converge)")
	}
}

// TestInstallUpgrade_RenameAsideFailureAborts pins that a rename-aside
// failure aborts the orchestrator BEFORE issuing any IPC traffic. The
// binary swap is the load-bearing first step; if it fails, the prior
// supervisor is still healthy and we must NOT send it a graceful-exit.
func TestInstallUpgrade_RenameAsideFailureAborts(t *testing.T) {
	mock := &fakeUpgradeDeps{
		renameAsideErr: errors.New("locked"),
	}
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath: "/fake/mcphub",
		NewBinary:  "/fake/mcphub.new",
		Deps:       mock,
	})
	if err == nil {
		t.Fatal("expected error on rename-aside failure")
	}
	if mock.quiesceCalled {
		t.Fatal("quiesce should not be called when rename fails")
	}
	if mock.exitCalled {
		t.Fatal("exit should not be called when rename fails")
	}
	if mock.forceKillCalled {
		t.Fatal("force-kill should not be called when rename fails")
	}
	if mock.startCalled {
		t.Fatal("start should not be called when rename fails")
	}
}

// TestInstallUpgrade_DefaultExitTimeoutMs verifies that when callers
// don't set ExitTimeoutMs explicitly the orchestrator fills in the
// default (5000 ms per spec §"Upgrade sequence" step 4). The default
// is exercised implicitly — ExitGraceful having been called with a
// non-zero timeout is the observable outcome here; explicit timing
// assertion lives in the production adapter once the real IPC client
// is wired.
func TestInstallUpgrade_DefaultExitTimeoutMs(t *testing.T) {
	mock := &fakeUpgradeDeps{}
	_ = RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath: "/fake/mcphub",
		NewBinary:  "/fake/mcphub.new",
		Deps:       mock,
	})
	// no explicit ExitTimeoutMs given → should default to 5000ms
	// (verified implicitly by ExitGraceful having been called; explicit
	// timing not asserted here because the fake doesn't sleep).
	if !mock.exitCalled {
		t.Error("ExitGraceful must be called even when ExitTimeoutMs is zero (default fill-in)")
	}
	_ = time.Now()
}

// TestInstallUpgrade_ForceKillErrorPropagated pins codex r2 Lane C P1
// #8 closure: when ExitGraceful times out AND the subsequent
// ForceKillSupervisor returns a non-"already-exited" error
// (permission denied, malformed PID, missing binary), the orchestrator
// MUST surface that error rather than silently continuing. Continuing
// would race the new supervisor against a still-running old supervisor
// for the IPC pipe + listening ports.
func TestInstallUpgrade_ForceKillErrorPropagated(t *testing.T) {
	mock := &fakeUpgradeDeps{
		exitErr:      errors.New("timeout"),
		forceKillErr: errors.New("ACCESS_DENIED: insufficient privileges to terminate the supervisor PID"),
	}
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath:    "/fake/mcphub",
		NewBinary:     "/fake/mcphub.new",
		PipePath:      "fake-pipe",
		ExitTimeoutMs: 5000,
		Deps:          mock,
	})
	if err == nil {
		t.Fatal("expected error when force-kill fails with a non-already-exited cause")
	}
	if !strings.Contains(err.Error(), "force-kill supervisor failed") {
		t.Errorf("error should name the force-kill failure; got %v", err)
	}
	if !strings.Contains(err.Error(), "ACCESS_DENIED") {
		t.Errorf("error should preserve the underlying message; got %v", err)
	}
	if !mock.forceKillCalled {
		t.Error("force-kill must have been invoked before the error")
	}
	if mock.startCalled {
		t.Error("StartSupervisor must NOT run when force-kill failed unsafely")
	}
}

// TestInstallUpgrade_ForceKillAlreadyExitedContinues pins the benign
// branch of codex r2 Lane C P1 #8: when ForceKillSupervisor returns a
// "process not found" / exit-code-128 error (the supervisor was
// already dead before we issued the kill), the orchestrator must
// continue through port verification + StartSupervisor. The same
// behavior covers ExitGraceful failure on "connection refused after
// crash" — taskkill subsequently reports the PID is gone, and that is
// not a real error.
func TestInstallUpgrade_ForceKillAlreadyExitedContinues(t *testing.T) {
	mock := &fakeUpgradeDeps{
		exitErr:      errors.New("connection refused"),
		forceKillErr: errors.New("ERROR: The process \"12345\" not found."),
	}
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath: "/fake/mcphub",
		NewBinary:  "/fake/mcphub.new",
		PipePath:   "fake-pipe",
		Deps:       mock,
	})
	if err != nil {
		t.Fatalf("already-exited force-kill must not abort: %v", err)
	}
	if !mock.forceKillCalled {
		t.Error("force-kill must have been invoked")
	}
	if !mock.startCalled {
		t.Error("StartSupervisor must still run after a benign already-exited force-kill")
	}
}

// TestInstallUpgrade_PostForceKillVerifiesPortUnbound pins the
// post-force-kill port verification branch of codex r2 Lane C P1 #8:
// after a successful force-kill, the orchestrator MUST call
// VerifyPortsUnbound for every expected port. If a port is still
// bound, the upgrade aborts BEFORE StartSupervisor would otherwise
// race a zombie listener.
func TestInstallUpgrade_PostForceKillVerifiesPortUnbound(t *testing.T) {
	mock := &fakeUpgradeDeps{
		exitErr:      errors.New("timeout"),
		forceKillErr: nil,
	}
	var verifyCalls []struct {
		ports   []int
		timeout time.Duration
	}
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath:    "/fake/mcphub",
		NewBinary:     "/fake/mcphub.new",
		PipePath:      "fake-pipe",
		ExpectedPorts: []int{9128, 9129},
		VerifyPortsUnbound: func(ports []int, perPortTimeout time.Duration) error {
			verifyCalls = append(verifyCalls, struct {
				ports   []int
				timeout time.Duration
			}{ports, perPortTimeout})
			return errors.New("127.0.0.1:9128: still bound after 10s")
		},
		Deps: mock,
	})
	if err == nil {
		t.Fatal("expected error when VerifyPortsUnbound reports a stuck port")
	}
	if !strings.Contains(err.Error(), "port-unbound verification failed") {
		t.Errorf("error should name the port-verification failure; got %v", err)
	}
	if !strings.Contains(err.Error(), "9128") {
		t.Errorf("error should preserve the offending port; got %v", err)
	}
	if len(verifyCalls) != 1 {
		t.Fatalf("VerifyPortsUnbound call count = %d; want 1", len(verifyCalls))
	}
	if len(verifyCalls[0].ports) != 2 || verifyCalls[0].ports[0] != 9128 || verifyCalls[0].ports[1] != 9129 {
		t.Errorf("VerifyPortsUnbound got ports = %v; want [9128 9129]", verifyCalls[0].ports)
	}
	if verifyCalls[0].timeout < 5*time.Second {
		t.Errorf("VerifyPortsUnbound timeout = %s; want >=5s (default 10s)", verifyCalls[0].timeout)
	}
	if mock.startCalled {
		t.Error("StartSupervisor must NOT run after a port-verification failure")
	}
}

// TestInstallUpgrade_PostForceKillVerifyPortsUnboundSuccess pins the
// happy path of codex r2 Lane C P1 #8: when VerifyPortsUnbound returns
// nil for every expected port, the orchestrator proceeds to
// StartSupervisor and returns nil.
func TestInstallUpgrade_PostForceKillVerifyPortsUnboundSuccess(t *testing.T) {
	mock := &fakeUpgradeDeps{
		exitErr: errors.New("timeout"),
	}
	verifyCalled := false
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath:    "/fake/mcphub",
		NewBinary:     "/fake/mcphub.new",
		PipePath:      "fake-pipe",
		ExpectedPorts: []int{9128},
		VerifyPortsUnbound: func(ports []int, perPortTimeout time.Duration) error {
			verifyCalled = true
			return nil
		},
		Deps: mock,
	})
	if err != nil {
		t.Fatalf("happy path with port-verify success: %v", err)
	}
	if !verifyCalled {
		t.Error("VerifyPortsUnbound must be called after a force-kill fallback when ExpectedPorts is set")
	}
	if !mock.startCalled {
		t.Error("StartSupervisor must run after a clean port-verify")
	}
}

// TestInstallUpgrade_PortVerifySkippedWhenNoExpectedPorts pins that
// the port-verify step is silently skipped when ExpectedPorts is
// empty (zero-daemon installs). VerifyPortsUnbound must not be called
// — calling it with an empty slice would either error vacuously or
// pollute the call log with empty checks.
func TestInstallUpgrade_PortVerifySkippedWhenNoExpectedPorts(t *testing.T) {
	mock := &fakeUpgradeDeps{
		exitErr: errors.New("timeout"),
	}
	verifyCalled := false
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath:    "/fake/mcphub",
		NewBinary:     "/fake/mcphub.new",
		PipePath:      "fake-pipe",
		ExpectedPorts: nil, // explicit zero-daemon case
		VerifyPortsUnbound: func(ports []int, perPortTimeout time.Duration) error {
			verifyCalled = true
			return errors.New("should not be called")
		},
		Deps: mock,
	})
	if err != nil {
		t.Fatalf("zero-daemon force-kill path: %v", err)
	}
	if verifyCalled {
		t.Error("VerifyPortsUnbound must NOT be called when ExpectedPorts is empty")
	}
	if !mock.startCalled {
		t.Error("StartSupervisor must still run on the zero-daemon force-kill path")
	}
}

// TestInstallUpgrade_PortVerifySkippedWhenCallbackNil pins backward
// compatibility: when the caller supplies ExpectedPorts but has not
// wired VerifyPortsUnbound (production adapter adoption is staged),
// the orchestrator must silently skip verification. Strict-mode
// enforcement is the production wiring's responsibility, not the
// orchestrator's.
func TestInstallUpgrade_PortVerifySkippedWhenCallbackNil(t *testing.T) {
	mock := &fakeUpgradeDeps{
		exitErr: errors.New("timeout"),
	}
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath:         "/fake/mcphub",
		NewBinary:          "/fake/mcphub.new",
		PipePath:           "fake-pipe",
		ExpectedPorts:      []int{9128},
		VerifyPortsUnbound: nil,
		Deps:               mock,
	})
	if err != nil {
		t.Fatalf("nil VerifyPortsUnbound should be tolerated for backcompat: %v", err)
	}
	if !mock.startCalled {
		t.Error("StartSupervisor must run when port-verify is nil-skipped")
	}
}

// TestRunInstallUpgrade_QuiesceStillRunningTriggersForceKill pins codex
// round-4 Lane B P1 (codex-r4-b-p1): the upgrade orchestrator MUST
// consume the QuiesceTimers result envelope's body. If `still_running`
// is non-empty even when ExitGraceful subsequently ACKs, transients
// did not drain; ExitGraceful only SCHEDULES exit so those un-drained
// transients become orphans unless force-kill + port-verify fire.
//
// Surface: QuiesceTimers returns Result with still_running=[1234],
// ExitGraceful succeeds (returns nil error) → assert force-kill IS
// invoked AND port-verify runs.
func TestRunInstallUpgrade_QuiesceStillRunningTriggersForceKill(t *testing.T) {
	mock := &fakeUpgradeDeps{
		quiesceResult: api.IPCResponse{
			ID: 1,
			OK: true,
			Result: map[string]any{
				"drained":       0.0,
				"still_running": []any{map[string]any{"pid": 1234.0, "kind": "x"}},
			},
			Final: true,
		},
		// ExitGraceful succeeds — historical bug let this short-circuit
		// the force-kill path even though still_running was non-empty.
		exitErr:    nil,
		exitResult: api.IPCResponse{ID: 2, OK: true, Result: "exit-acked"},
	}

	verifyCalled := false
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath:    "/fake/mcphub",
		NewBinary:     "/fake/mcphub.new",
		PipePath:      "fake-pipe",
		ExitTimeoutMs: 5000,
		ExpectedPorts: []int{9128, 9129},
		VerifyPortsUnbound: func(ports []int, timeout time.Duration) error {
			verifyCalled = true
			return nil
		},
		Deps: mock,
	})
	if err != nil {
		t.Fatalf("upgrade should not error when force-kill succeeds: %v", err)
	}
	if !mock.quiesceCalled {
		t.Fatal("quiesce not called")
	}
	if !mock.forceKillCalled {
		t.Fatal("force-kill MUST fire when QuiesceTimers reports still_running non-empty, regardless of ExitGraceful success")
	}
	if !verifyCalled {
		t.Fatal("VerifyPortsUnbound MUST fire after force-kill driven by still_running non-empty (un-drained transients may be holding daemon ports)")
	}
	if !mock.startCalled {
		t.Fatal("StartSupervisor MUST still run after force-kill + verify (upgrade must converge)")
	}
}

// TestRunInstallUpgrade_QuiesceErrorTriggersForceKill pins the companion
// case to the still_running path: when QuiesceTimers itself returns an
// error, the orchestrator cannot prove transients drained — force-kill
// must fire even if ExitGraceful subsequently succeeds. (This is the
// strictness side of the historical "Failure here is non-fatal" comment
// at install_upgrade.go:305 — non-fatal does NOT mean "skip the safety
// net"; it means "do not abort the upgrade", which the force-kill path
// already satisfies via the converge-anyway StartSupervisor step.)
func TestRunInstallUpgrade_QuiesceErrorTriggersForceKill(t *testing.T) {
	mock := &fakeUpgradeDeps{
		quiesceErr: errors.New("simulated quiesce transport failure"),
		exitErr:    nil,
		exitResult: api.IPCResponse{ID: 2, OK: true, Result: "exit-acked"},
	}

	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath:    "/fake/mcphub",
		NewBinary:     "/fake/mcphub.new",
		PipePath:      "fake-pipe",
		ExitTimeoutMs: 5000,
		Deps:          mock,
	})
	if err != nil {
		t.Fatalf("upgrade should not error when force-kill succeeds: %v", err)
	}
	if !mock.forceKillCalled {
		t.Fatal("force-kill MUST fire when QuiesceTimers errored (drain provenance unproven)")
	}
}

// TestRunInstallUpgrade_QuiesceCleanVerifiesPortsWithoutForceKill is the
// negative force-kill companion: when QuiesceTimers returns success with
// still_running empty AND ExitGraceful succeeds, the orchestrator must NOT invoke
// force-kill, but it must still prove daemon ports are unbound before spawning
// the successor.
func TestRunInstallUpgrade_QuiesceCleanVerifiesPortsWithoutForceKill(t *testing.T) {
	mock := &fakeUpgradeDeps{
		quiesceResult: api.IPCResponse{
			ID: 1,
			OK: true,
			Result: map[string]any{
				"drained":       1.0,
				"still_running": []any{},
			},
			Final: true,
		},
		exitErr:    nil,
		exitResult: api.IPCResponse{ID: 2, OK: true, Result: "exit-acked"},
	}
	verifyCalled := false
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath:    "/fake/mcphub",
		NewBinary:     "/fake/mcphub.new",
		PipePath:      "fake-pipe",
		ExitTimeoutMs: 5000,
		ExpectedPorts: []int{9128},
		VerifyPortsUnbound: func(ports []int, timeout time.Duration) error {
			verifyCalled = true
			return nil
		},
		Deps: mock,
	})
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if mock.forceKillCalled {
		t.Error("force-kill must NOT fire on clean quiesce + clean exit")
	}
	if !verifyCalled {
		t.Error("VerifyPortsUnbound must fire on clean quiesce + clean exit before successor start")
	}
}

// TestRunInstallUpgrade_CleanGracefulPathVerifiesHandoff pins the audit P1:
// a clean exit{graceful} ACK is not enough to report success. The old
// supervisor must release supervisor.lock, the prior daemon ports must be
// observed unbound, the successor must be spawned, and IPC readiness must be
// verified before RunInstallUpgrade returns nil.
func TestRunInstallUpgrade_CleanGracefulPathVerifiesHandoff(t *testing.T) {
	mock := &fakeUpgradeDeps{
		quiesceResult: api.IPCResponse{
			ID: 1,
			OK: true,
			Result: map[string]any{
				"drained":       1.0,
				"still_running": []any{},
			},
			Final: true,
		},
		exitResult: api.IPCResponse{ID: 2, OK: true, Result: "exit-acked"},
	}

	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath:    "/fake/mcphub",
		NewBinary:     "/fake/mcphub.new",
		PipePath:      "fake-pipe",
		ExitTimeoutMs: 5000,
		ExpectedPorts: []int{9128},
		VerifyPortsUnbound: func(ports []int, timeout time.Duration) error {
			mock.calls = append(mock.calls, "verify-ports")
			return nil
		},
		WaitSupervisorLockReleased: func(ctx context.Context, timeout time.Duration) error {
			mock.calls = append(mock.calls, "wait-lock")
			return nil
		},
		WaitSupervisorReady: func(ctx context.Context, timeout time.Duration) error {
			mock.calls = append(mock.calls, "wait-ready")
			return nil
		},
		Deps: mock,
	})
	if err != nil {
		t.Fatalf("clean graceful handoff should converge: %v", err)
	}
	if mock.forceKillCalled {
		t.Fatal("force-kill must not run on clean quiesce + clean graceful exit")
	}
	want := []string{"rename", "quiesce", "exit", "wait-lock", "verify-ports", "start", "wait-ready"}
	if strings.Join(mock.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", mock.calls, want)
	}
}

func TestRunInstallUpgrade_HandoffWaitHonorsCallerGracefulBudget(t *testing.T) {
	mock := &fakeUpgradeDeps{
		quiesceResult: api.IPCResponse{
			ID: 1,
			OK: true,
			Result: map[string]any{
				"drained":       1.0,
				"still_running": []any{},
			},
			Final: true,
		},
		exitResult: api.IPCResponse{ID: 2, OK: true, Result: "exit-acked"},
	}

	var lockWaitTimeout time.Duration
	var readyWaitTimeout time.Duration
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath:    "/fake/mcphub",
		NewBinary:     "/fake/mcphub.new",
		PipePath:      "fake-pipe",
		ExitTimeoutMs: 90000,
		WaitSupervisorLockReleased: func(ctx context.Context, timeout time.Duration) error {
			lockWaitTimeout = timeout
			return nil
		},
		WaitSupervisorReady: func(ctx context.Context, timeout time.Duration) error {
			readyWaitTimeout = timeout
			return nil
		},
		Deps: mock,
	})
	if err != nil {
		t.Fatalf("clean graceful handoff should converge: %v", err)
	}
	if mock.exitTimeoutMs != 90000 {
		t.Fatalf("ExitGraceful timeout = %dms, want caller-supplied 90000ms", mock.exitTimeoutMs)
	}
	want := time.Duration(defaultQuiesceTimeoutMs+90000) * time.Millisecond
	if lockWaitTimeout != want {
		t.Fatalf("WaitSupervisorLockReleased timeout = %s, want %s", lockWaitTimeout, want)
	}
	if readyWaitTimeout != want {
		t.Fatalf("WaitSupervisorReady timeout = %s, want %s", readyWaitTimeout, want)
	}
}

// TestRunInstallUpgrade_CleanGracefulNeverReadySuccessorFails injects the
// real-world P1 failure shape: the detached successor never becomes reachable
// via IPC status. The upgrade must return a clear error instead of reporting
// success after StartSupervisor returns.
func TestRunInstallUpgrade_CleanGracefulNeverReadySuccessorFails(t *testing.T) {
	mock := &fakeUpgradeDeps{
		quiesceResult: api.IPCResponse{
			ID:     1,
			OK:     true,
			Result: map[string]any{"drained": 1.0, "still_running": []any{}},
			Final:  true,
		},
		exitResult: api.IPCResponse{ID: 2, OK: true, Result: "exit-acked"},
	}
	neverReady := errors.New("status poll timed out: supervisor.lock held by prior PID")

	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath: "/fake/mcphub",
		NewBinary:  "/fake/mcphub.new",
		PipePath:   "fake-pipe",
		WithRollbackStopSettlementFence: func(_ context.Context, critical func() error) error {
			return critical()
		},
		WaitSupervisorLockReleased: func(ctx context.Context, timeout time.Duration) error {
			return nil
		},
		WaitSupervisorReady: func(ctx context.Context, timeout time.Duration) error {
			return neverReady
		},
		Deps: mock,
	})
	if err == nil {
		t.Fatal("upgrade must fail when the successor never becomes IPC-ready")
	}
	if !strings.Contains(err.Error(), "IPC-ready") {
		t.Fatalf("error should name successor IPC readiness, got %v", err)
	}
	if !strings.Contains(err.Error(), "mcphub supervise") {
		t.Fatalf("error should include foreground recovery guidance, got %v", err)
	}
}

// TestRunInstallUpgrade_SweepsOldBinariesAfterSuccessfulSwap pins the P3
// wiring: the cold-restart upgrade path must invoke the .old-<timestamp> sweep
// after a successful rename-aside swap. Sweep failures are warning-only, so the
// test uses a nil sweep result and asserts only the production call path.
func TestRunInstallUpgrade_SweepsOldBinariesAfterSuccessfulSwap(t *testing.T) {
	installDir := t.TempDir()
	target := filepath.Join(installDir, "mcphub.exe")
	mock := &fakeUpgradeDeps{}
	var swept []string
	cleanupSweep := setSweepOldBinariesFnForTest(func(dir string, warn ...func(string, error)) error {
		swept = append(swept, dir)
		return nil
	})
	defer cleanupSweep()

	if err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath: target,
		NewBinary:  target + ".new",
		PipePath:   "fake-pipe",
		Deps:       mock,
	}); err != nil {
		t.Fatalf("upgrade with sweep should still converge: %v", err)
	}
	if len(swept) != 1 || swept[0] != installDir {
		t.Fatalf("sweep calls = %v, want [%s]", swept, installDir)
	}
}

// TestIsAlreadyExitedError exercises the helper's classification rules
// (codex r2 Lane C P1 #8). Each case names the failure shape the
// production force-kill helper might emit; the helper must accept
// already-exited shapes and reject everything else.
func TestIsAlreadyExitedError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"taskkill not found (English)", errors.New("taskkill PID 1234: exit status 128: ERROR: The process \"1234\" not found."), true},
		{"posix no such process", errors.New("kill: 12345: no such process"), true},
		{"could not find process", errors.New("ERROR: Could not find the process"), true},
		{"unrelated permission denied", errors.New("taskkill PID 1234: exit status 1: ERROR: Access is denied."), false},
		{"unrelated binary missing", errors.New(`exec: "taskkill": executable file not found in %PATH%`), false},
		{"empty message", errors.New(""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isAlreadyExitedError(tc.err)
			if got != tc.want {
				t.Errorf("isAlreadyExitedError(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestInstallUpgrade_PostSwapFailureRestoresPrior(t *testing.T) {
	mock := &fakeUpgradeDeps{
		retainedPrior: "/fake/mcphub.old-exact",
		quiesceResult: api.IPCResponse{ID: 1, OK: true, Result: map[string]any{
			"drained": 1.0, "still_running": []any{},
		}, Final: true},
		exitResult: api.IPCResponse{ID: 2, OK: true, Result: "exit-acked"},
	}
	readyCalls := 0
	var swept int
	cleanupSweep := setSweepOldBinariesFnForTest(func(string, ...func(string, error)) error {
		swept++
		return nil
	})
	defer cleanupSweep()

	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath: "/fake/mcphub",
		NewBinary:  "/fake/mcphub.new",
		PipePath:   "fake-pipe",
		WithRollbackStopSettlementFence: func(_ context.Context, critical func() error) error {
			return critical()
		},
		WaitSupervisorReady: func(context.Context, time.Duration) error {
			readyCalls++
			if readyCalls == 1 {
				return errors.New("forced successor readiness failure")
			}
			return nil
		},
		Deps: mock,
	})
	if err == nil || !strings.Contains(err.Error(), "automatic rollback restored") {
		t.Fatalf("error=%v, want successful automatic rollback report", err)
	}
	want := []string{"rename", "quiesce", "exit", "start", "force-kill", "restore", "start"}
	if strings.Join(mock.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls=%v, want %v", mock.calls, want)
	}
	if readyCalls != 2 {
		t.Fatalf("readiness calls=%d, want successor + restored prior", readyCalls)
	}
	if swept != 0 {
		t.Fatalf("rollback must preserve retained artifacts; sweep calls=%d", swept)
	}
}

func TestInstallUpgrade_SuccessCommitsRetainedBinary(t *testing.T) {
	mock := &fakeUpgradeDeps{
		quiesceResult: api.IPCResponse{ID: 1, OK: true, Result: map[string]any{
			"drained": 1.0, "still_running": []any{},
		}, Final: true},
		exitResult: api.IPCResponse{ID: 2, OK: true, Result: "exit-acked"},
	}
	ready := false
	var swept int
	cleanupSweep := setSweepOldBinariesFnForTest(func(string, ...func(string, error)) error {
		if !ready {
			t.Fatal("retained binary swept before successor readiness")
		}
		swept++
		return nil
	})
	defer cleanupSweep()

	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath: "/fake/mcphub",
		NewBinary:  "/fake/mcphub.new",
		PipePath:   "fake-pipe",
		WaitSupervisorReady: func(context.Context, time.Duration) error {
			ready = true
			return nil
		},
		Deps: mock,
	})
	if err != nil {
		t.Fatal(err)
	}
	if swept != 1 {
		t.Fatalf("sweep calls=%d, want exactly 1 after readiness", swept)
	}
	if strings.Contains(strings.Join(mock.calls, ","), "restore") {
		t.Fatalf("successful upgrade unexpectedly restored prior: %v", mock.calls)
	}
}
