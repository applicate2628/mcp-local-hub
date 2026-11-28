package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/process"
)

// startForceKillTestSleeper keeps the fixture's original Cmd and one Wait.
// Owned stop and the watchdog use that Process; PID lookup is not authority.
func startForceKillTestSleeper(t *testing.T) (*exec.Cmd, func() error, <-chan struct{}) {
	t.Helper()
	name, args := "sleep", []string{"60"}
	if runtime.GOOS == "windows" {
		name = "powershell"
		args = []string{"-NoProfile", "-NonInteractive", "-NoExit", "-Command", "Start-Sleep -Seconds 60"}
	}
	image, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("resolve force-kill fixture %q: %v", name, err)
	}
	image, err = filepath.Abs(image)
	if err != nil {
		t.Fatalf("resolve absolute force-kill fixture image: %v", err)
	}
	childCtx, cancelChild := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(childCtx, image, args...)
	process.NoConsole(cmd)
	if err := cmd.Start(); err != nil {
		cancelChild()
		t.Fatalf("start force-kill fixture %q: %v", image, err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	var stopOnce sync.Once
	var stopErr error
	stopAndWait := func() error {
		stopOnce.Do(func() {
			killErr := cmd.Process.Kill()
			<-done
			if killErr != nil {
				stopErr = fmt.Errorf("terminate original fixture: %w", killErr)
			}
			var exitErr *exec.ExitError
			if !errors.As(waitErr, &exitErr) {
				stopErr = errors.Join(stopErr, fmt.Errorf("fixture must exit after owned kill; Wait returned %v", waitErr))
			}
			if cmd.ProcessState == nil || cmd.ProcessState.Pid() != cmd.Process.Pid {
				stopErr = errors.Join(stopErr, errors.New("original fixture Wait did not return its process state"))
			}
			if err := childCtx.Err(); err != nil {
				stopErr = errors.Join(stopErr, fmt.Errorf("fixture watchdog ended before owned settlement: %w", err))
			}
			cancelChild()
			t.Logf("force-kill fixture settled: image=%q pid=%d kill=%v wait=%v", image, cmd.Process.Pid, killErr, waitErr)
		})
		return stopErr
	}
	t.Cleanup(func() {
		if err := stopAndWait(); err != nil {
			t.Errorf("force-kill fixture cleanup: %v", err)
		}
	})
	t.Logf("force-kill fixture started: image=%q pid=%d", image, cmd.Process.Pid)
	return cmd, stopAndWait, done
}

// guiForceCancellationOnly accepts cancellation through joined cleanup errors
// without hiding another failure merely because errors.Is finds cancellation.
func guiForceCancellationOnly(err error) bool {
	if err == context.Canceled {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !guiForceCancellationOnly(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return guiForceCancellationOnly(wrapped.Unwrap())
	}
	return false
}

func TestGUIForceCancellationOnly(t *testing.T) {
	cleanupErr := errors.New("cleanup failed")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, true},
		{"wrapped", fmt.Errorf("command: %w", context.Canceled), true},
		{"joined cancellation", errors.Join(context.Canceled, context.Canceled), true},
		{"cleanup failure", cleanupErr, false},
		{"cancellation and cleanup failure", errors.Join(context.Canceled, cleanupErr), false},
		{"cancellation and deadline", errors.Join(context.Canceled, context.DeadlineExceeded), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := guiForceCancellationOnly(tc.err); got != tc.want {
				t.Errorf("cancellation-only = %v, want %v for %v", got, tc.want, tc.err)
			}
		})
	}
}

// ---------------------------------------------------------------
// Scenario 1: Healthy --force (bare flag activates via handshake)
// ---------------------------------------------------------------

func TestForce_HealthyIncumbent_BareFlagActivates(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")
	srv := healthyIncumbentServer(t, os.Getpid())
	defer srv.Close()
	port := portFromHTTPTestURL(t, srv.URL)
	if err := gui.WritePidport(pidport, os.Getpid(), port); err != nil {
		t.Fatal(err)
	}
	// Pre-acquire the flock so AcquireSingleInstance returns busy and
	// the --force path is exercised.
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	var buf bytes.Buffer
	c := newGuiCmdRealForTest()
	c.SetOut(&buf)
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	_ = c.Execute()
	out := buf.String()
	if !strings.Contains(out, "activated existing mcphub gui") {
		t.Errorf("expected 'activated existing mcphub gui'; got %q", out)
	}
}

// ---------------------------------------------------------------
// Scenario 2: Healthy --force --kill --yes prints notice and activates
// ---------------------------------------------------------------

func TestForce_HealthyIncumbent_KillFlagPrintsNoticeAndActivates(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")
	srv := healthyIncumbentServer(t, os.Getpid())
	defer srv.Close()
	port := portFromHTTPTestURL(t, srv.URL)
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d %d\n", os.Getpid(), port)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pre-acquire the flock so AcquireSingleInstance returns busy and
	// the --force --kill path is exercised rather than normal startup.
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	var buf bytes.Buffer
	c := newGuiCmdRealForTest()
	c.SetOut(&buf)
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	_ = c.Execute()
	out := buf.String()
	if !strings.Contains(out, "incumbent is healthy") {
		t.Errorf("expected 'incumbent is healthy' notice; got %q", out)
	}
	if !strings.Contains(out, "activating instead of killing") {
		t.Errorf("expected 'activating instead of killing' notice; got %q", out)
	}
}

// ---------------------------------------------------------------
// Scenario 3: Stuck — bare --force is PRINT-ONLY (Option C, bug
// 2026-06-22-explorer-folder-window-orphan-flood). The diagnostic block
// already prints the lock-folder path; auto-opening explorer.exe is the
// highest-frequency trigger of the reveal-window flood, so it is gated
// behind --reveal. Bare --force must NOT spawn the open-folder seam;
// --force --reveal must.
// ---------------------------------------------------------------

// setupStuckForceFixture writes a pidport whose recorded PID is alive
// (this test process) but whose port is closed, then pre-acquires the
// flock so AcquireSingleInstance returns busy and --force routes to the
// diagnostic path. Returns the temp dir for MCPHUB_GUI_TEST_PIDPORT_DIR
// and registers flock cleanup on t.
func setupStuckForceFixture(t *testing.T) string {
	t.Helper()
	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")
	const probablyClosedPort = 1
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d %d\n", os.Getpid(), probablyClosedPort)), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	t.Cleanup(func() { _ = fl.Unlock() })
	return dir
}

func TestForce_StuckIncumbent_BareFlagIsPrintOnly(t *testing.T) {
	dir := setupStuckForceFixture(t)

	// Mock the open-folder seam and assert it is NOT invoked.
	prevSpawn := gui.OpenFolderSpawnForTest()
	defer gui.RestoreOpenFolderSpawn(prevSpawn)
	spawned := false
	gui.SetOpenFolderSpawn(func(name string, args ...string) error {
		spawned = true
		return nil
	})

	var buf bytes.Buffer
	c := newGuiCmdRealForTest()
	c.SetOut(&buf)
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()
	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		t.Errorf("expected typed exit code error; got %v", err)
	} else if fe.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2", fe.ExitCode())
	}
	out := buf.String()
	if !strings.Contains(out, "Cannot acquire") {
		t.Errorf("expected diagnostic block; got %q", out)
	}
	if spawned {
		t.Errorf("bare --force must be print-only; open-folder seam was invoked")
	}
}

func TestForce_StuckIncumbent_RevealFlagOpensFolder(t *testing.T) {
	dir := setupStuckForceFixture(t)

	// Mock the open-folder seam and assert it IS invoked with --reveal.
	prevSpawn := gui.OpenFolderSpawnForTest()
	defer gui.RestoreOpenFolderSpawn(prevSpawn)
	var spawnedName string
	gui.SetOpenFolderSpawn(func(name string, args ...string) error {
		spawnedName = name
		return nil
	})

	var buf bytes.Buffer
	c := newGuiCmdRealForTest()
	c.SetOut(&buf)
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--reveal"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()
	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		t.Errorf("expected typed exit code error; got %v", err)
	} else if fe.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2", fe.ExitCode())
	}
	out := buf.String()
	if !strings.Contains(out, "Cannot acquire") {
		t.Errorf("expected diagnostic block; got %q", out)
	}
	if spawnedName == "" {
		t.Errorf("--force --reveal must open the lock folder; seam was not invoked")
	}
}

// ---------------------------------------------------------------
// Scenario 4: --force --kill happy path (seam-mocked gate)
// ---------------------------------------------------------------

func TestForce_KillHappyPath_SeamMocked(t *testing.T) {
	// Exercise the finite force-kill recovery operation through its acquired
	// ownership lock. Scenario 4b exercises full command startup and port
	// publication. The identity seam admits our non-mcphub sleep helper;
	// TestForce_KillRefusesNonMcphubImage separately tests real-image refusal.
	prevGate := gui.IdentityGateForTest()
	defer gui.RestoreIdentityGate(prevGate)
	gui.SetIdentityGate(func(v gui.Verdict) (refused bool, reason string) { return false, "" })

	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	// Spawn a child sleep process so we have a real PID we can kill.
	var sleepCmd *exec.Cmd
	if runtime.GOOS == "windows" {
		sleepCmd = exec.Command("powershell", "-NoProfile", "-NoExit", "-Command", "Start-Sleep -Seconds 60")
	} else {
		sleepCmd = exec.Command("sleep", "60")
	}
	process.NoConsole(sleepCmd)
	if err := sleepCmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep helper: %v", err)
	}
	// The original child has one waiter. Release the fixture lock only
	// after it exits, and join this waiter before temporary-file cleanup.
	holderDone := make(chan error, 1)
	go func() {
		_, err := sleepCmd.Process.Wait()
		holderDone <- errors.Join(err, fl.Unlock())
	}()
	defer func() {
		_ = sleepCmd.Process.Kill()
		if err := <-holderDone; err != nil {
			t.Errorf("reap sleep helper or release fixture lock: %v", err)
		}
	}()

	pid := sleepCmd.Process.Pid
	const stalePort = 1
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d %d\n", pid, stalePort)), 0o600); err != nil {
		t.Fatal(err)
	}

	// Recovery returns its acquired lock; no normal cancellation is needed.
	// The timeout is only a watchdog, and reaching it must fail this scenario.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := newGuiCmdRealForTest()
	recovered, exitCode := runForceKill(ctx, c, pidport, true)
	contextErr := ctx.Err()
	if recovered != nil {
		// Carry the acquisition-bound lifecycle through cleanup. Like the
		// production owner, never unlock an unsettled active record.
		defer func() {
			owner := recovered.OwnerLifecycle()
			if owner == nil {
				t.Error("recovered lock has no owner lifecycle")
				return
			}
			if err := owner.TerminalSettle(); err != nil {
				t.Errorf("settle recovered owner record: %v", err)
				return
			}
			if err := recovered.ReleaseErr(); err != nil {
				t.Errorf("release recovered lock: %v", err)
			}
		}()
	}
	if contextErr != nil {
		t.Fatalf("force-kill recovery reached watchdog: %v", contextErr)
	}
	if exitCode != 0 || recovered == nil {
		t.Fatalf("force-kill recovery returned exit=%d, acquired lock=%t; want exit 0 and an acquired lock", exitCode, recovered != nil)
	}

	currentPID, port, err := gui.ReadPidport(pidport)
	if err != nil {
		t.Fatalf("read recovered pidport: %v", err)
	}
	if currentPID != os.Getpid() || port != stalePort {
		t.Errorf("recovered pidport = %d:%d; want %d:%d before server startup", currentPID, port, os.Getpid(), stalePort)
	}
}

// ---------------------------------------------------------------
// Scenario 4b: --force --kill rewrites pidport with current PID + port
// (Codex PR #23 P2 #2)
// ---------------------------------------------------------------

// TestForce_KillRewritesPidportWithCurrentPID pins the Codex PR #23
// P2 #2 fix. After --force --kill takeover succeeds, startGuiServer
// must write a fresh pidport with the current process's PID + the
// actually-bound port — NOT leave the killed incumbent's stale PID +
// port in the file. Before the fix, the conditional
// RewritePidportPort(actualPort) only fired when actualPort !=
// requestedPort and only updated the port field; if the operator
// passed --port=N and the bind succeeded on N, the pidport kept the
// killed incumbent's PID forever.
//
// To exercise the actualPort==requestedPort case (where the bug
// silently elided the rewrite), discover a free ephemeral port by
// briefly binding+closing it, then pass that port via --port=N.
// There's a microsecond-wide race where another process could grab
// the port between close and re-bind; on a quiet test machine this
// is reliable, and a one-off bind failure is a clear test signal
// rather than a silent false-pass.
//
// Approach: simulate the takeover via the IdentityGateForTest seam
// (same pattern as scenario 4), spawn a real sleep child that the
// kill path actually terminates so the flock auto-releases, then run
// the gui command with --port=N and a deadline-bounded context.
// Poll the pidport until it contains os.Getpid() (proof the rewrite
// happened) OR the deadline expires, then assert.
func TestForce_KillRewritesPidportWithCurrentPID(t *testing.T) {
	// Exercise the real GUI server without launching a supervisor or scheduler.
	t.Setenv("MCPHUB_E2E_SUPERVISOR", "none")
	t.Setenv("MCPHUB_E2E_SCHEDULER", "none")
	t.Setenv(selfRestartHandoffEnv, "")

	// Keep the actualPort==requestedPort regression oracle.
	freePort := pickFreePort(t)
	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")
	sleepCmd, stopHolder, holderDone := startForceKillTestSleeper(t)
	stalePid := sleepCmd.Process.Pid
	const stalePort = 1
	fl := flock.New(pidport + ".lock")
	prevGate := gui.IdentityGateForTest()
	prevHook := gui.PostKillHookForTest()
	gui.SetPostKillHook(nil)
	gui.SetIdentityGate(func(v gui.Verdict) (bool, string) {
		return v.PID != stalePid, "force-kill test owns only its original child"
	})
	var killCalls atomic.Int32
	restoreKill := gui.SetKillProcessForTest(func(pid int) error {
		if pid != stalePid {
			return fmt.Errorf("force-kill requested unowned PID %d", pid)
		}
		killCalls.Add(1)
		if err := stopHolder(); err != nil {
			return err
		}
		// This flock is parent-owned: release only after the original Cmd.Wait.
		return fl.Unlock()
	})
	defer func() {
		if err := stopHolder(); err != nil {
			t.Errorf("settle original holder: %v", err)
		}
		if err := fl.Unlock(); err != nil {
			t.Errorf("release original fixture flock: %v", err)
		}
		restoreKill()
		gui.RestorePostKillHook(prevHook)
		gui.RestoreIdentityGate(prevGate)
	}()
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d %d\n", stalePid, stalePort)), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := fl.TryLock(); err != nil || !ok {
		t.Fatalf("pre-lock original holder: acquired=%v err=%v", ok, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", fmt.Sprintf("%d", freePort), "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	cmdDone := make(chan error, 1)
	go func() { cmdDone <- c.ExecuteContext(ctx) }()
	var cmdErr error
	cmdJoined := false
	joinCommand := func() error {
		if !cmdJoined {
			cmdErr = <-cmdDone
			cmdJoined = true
		}
		return cmdErr
	}
	defer func() {
		cancel()
		// Do not release fixtures or restore seams while this command is active.
		if err := joinCommand(); err != nil && !guiForceCancellationOnly(err) {
			t.Errorf("GUI command shutdown: %v", err)
		}
	}()

	deadline := time.Now().Add(8 * time.Second)
	var observedPid, observedPort int
	for time.Now().Before(deadline) {
		pid, port, rerr := gui.ReadPidport(pidport)
		if rerr == nil && pid == os.Getpid() && port == freePort {
			observedPid, observedPort = pid, port
			break
		}
		select {
		case cmdErr = <-cmdDone:
			cmdJoined = true
			pid, port, rerr = gui.ReadPidport(pidport)
			if rerr == nil && pid == os.Getpid() && port == freePort {
				observedPid, observedPort = pid, port
			} else {
				t.Fatalf("command exited (err=%v) before current PID + freePort publication: pid=%d port=%d stale=%d:%d want=%d:%d",
					cmdErr, pid, port, stalePid, stalePort, os.Getpid(), freePort)
			}
		case <-time.After(50 * time.Millisecond):
		}
		if observedPid != 0 {
			break
		}
	}
	if ctx.Err() != nil {
		t.Errorf("GUI command reached its deadline before cancellation: %v", ctx.Err())
	}
	cancel()
	_ = joinCommand()
	select {
	case <-holderDone:
	default:
		t.Error("original holder Cmd.Wait did not complete during takeover")
	}
	if killCalls.Load() != 1 {
		t.Errorf("owned kill calls = %d, want 1", killCalls.Load())
	}
	if observedPid != os.Getpid() || observedPort != freePort || observedPid == stalePid {
		t.Fatalf("active pidport = %d:%d, want current PID + requested freePort %d:%d (stale %d:%d)",
			observedPid, observedPort, os.Getpid(), freePort, stalePid, stalePort)
	}
}

// pickFreePort returns an ephemeral port discovered by binding :0 and
// immediately closing. There's a small race window before the next
// caller binds the same port; tests using this helper accept that on
// a quiet machine (and a port collision is loud, not silent).
func pickFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pickFreePort: listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if cerr := ln.Close(); cerr != nil {
		t.Fatalf("pickFreePort: close: %v", cerr)
	}
	return port
}

// ---------------------------------------------------------------
// Scenario 5: --force --kill refuses non-mcphub image
// ---------------------------------------------------------------

func TestForce_KillRefusesNonMcphubImage(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")
	// os.Getppid is the shell (or test runner parent) — image does NOT
	// match mcphub.exe/mcphub.
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d 1\n", os.Getppid())), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()
	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		t.Fatalf("expected typed exit code error; got %v", err)
	}
	if fe.ExitCode() != 7 {
		t.Errorf("exit code = %d, want 7 (KillRefused)", fe.ExitCode())
	}
}

// ---------------------------------------------------------------
// Scenario 6: --force --kill non-interactive without --yes → exit 6
// ---------------------------------------------------------------

func TestForce_KillNonInteractiveWithoutYesExits6(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d 1\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	c := newGuiCmdRealForTest()
	// `go test` runs without a TTY by default, so --force --kill without
	// --yes triggers the non-interactive guard (exit 6).
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()
	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		t.Fatalf("expected typed exit code error; got %v", err)
	}
	if fe.ExitCode() != 6 {
		t.Errorf("exit code = %d, want 6 (NonInteractive)", fe.ExitCode())
	}
}

// ---------------------------------------------------------------
// PreRunE flag validation (GitHub Codex bot review on PR #23, P2)
// ---------------------------------------------------------------

// TestForce_FlagValidation_KillRequiresForce, _YesRequiresKill cover
// the cobra PreRunE rejecting destructive-flag misuse before any
// runtime path runs. PreRunE errors surface as plain cobra errors
// (NOT typed forceExitError); cobra's default handling is exit 1
// after printing usage. We assert the error message text and that
// the error is NOT a forceExitError (the runtime exit-code map is
// reserved for runtime outcomes, not operator misuse).
func TestForce_FlagValidation_KillRequiresForce(t *testing.T) {
	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--no-browser", "--no-tray", "--kill"})
	err := c.Execute()
	if err == nil {
		t.Fatal("expected validation error for --kill without --force; got nil")
	}
	if !strings.Contains(err.Error(), "--kill requires --force") {
		t.Errorf("error = %q; want substring '--kill requires --force'", err)
	}
	var fe interface{ ExitCode() int }
	if errors.As(err, &fe) {
		t.Errorf("validation error should NOT be a forceExitError; got %T", err)
	}
}

func TestForce_FlagValidation_YesRequiresKill(t *testing.T) {
	for _, args := range [][]string{
		// --yes alone: covered by --yes && !kill (kill not set → trips)
		{"--no-browser", "--no-tray", "--yes"},
		// --force --yes (forgot --kill): the bug Codex bot flagged on PR #23.
		{"--no-browser", "--no-tray", "--force", "--yes"},
	} {
		c := newGuiCmdRealForTest()
		c.SetArgs(args)
		err := c.Execute()
		if err == nil {
			t.Errorf("args=%v: expected validation error; got nil", args)
			continue
		}
		if !strings.Contains(err.Error(), "--yes requires --force --kill") {
			t.Errorf("args=%v: error = %q; want substring '--yes requires --force --kill'", args, err)
		}
		var fe interface{ ExitCode() int }
		if errors.As(err, &fe) {
			t.Errorf("args=%v: validation error should NOT be a forceExitError; got %T", args, err)
		}
	}
}

// ---------------------------------------------------------------
// Codex iter-6 P2 #1: --force --kill against non-killable verdicts
// must NOT prompt the operator.
// ---------------------------------------------------------------

// TestForce_KillMalformedPidport_NoPromptExits4 pins the iter-6 fix
// (no destructive prompt for non-killable verdicts) AND the iter-8
// P2 #2 exit-code update: kill-mode malformed → exit 4 ("pidport
// unrecoverable" per memo §Exit codes), not exit 2 (which is
// reserved for bare --force diagnostic).
func TestForce_KillMalformedPidport_NoPromptExits4(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte("garbage not a pidport"), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	c := newGuiCmdRealForTest()
	// --yes ensures we don't depend on TTY check (which fires first
	// at exit 6 in go test). The iter-6 gating is between Probe and
	// the prompt and must fire even in --yes mode.
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()
	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		t.Fatalf("expected typed exit code error; got %v", err)
	}
	if fe.ExitCode() != 4 {
		t.Errorf("exit code = %d, want 4 (kill-mode pidport unrecoverable per iter-8 P2 #2)", fe.ExitCode())
	}
}

// TestForce_KillOutOfRangePort_ClassifiesMalformed pins the iter-8
// P2 #1 fix: a pidport with a live PID but a corrupt out-of-range
// port (e.g. 70000) must classify as VerdictMalformed and exit 4
// in kill mode — NOT pass through to the LiveUnreachable kill path
// where the otherwise-healthy GUI would be terminated for bad
// metadata alone.
func TestForce_KillOutOfRangePort_ClassifiesMalformed(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")
	// Use os.Getpid() so PID is alive (we want to assert the port
	// validation kicks in BEFORE the kill path; without the iter-8
	// fix this would have classified as LiveUnreachable and tried
	// to kill the test runner itself via the identity gate).
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d 70000\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()
	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		t.Fatalf("expected typed exit code error; got %v", err)
	}
	if fe.ExitCode() != 4 {
		t.Errorf("exit code = %d, want 4 (out-of-range port → Malformed in kill mode)", fe.ExitCode())
	}
}

// TestForce_KillDeadPID_NoPromptExits3 pins iter-6 P2 #1 for the
// VerdictDeadPID branch: a pidport pointing at a dead PID skips the
// prompt and returns exit 3 (race-lost / already-recovered).
func TestForce_KillDeadPID_NoPromptExits3(t *testing.T) {
	// Find a likely-dead PID: spawn a short-lived child, capture its
	// PID, wait for it to exit. PID may recycle on long-uptime
	// systems, but the probability over a sub-second test window is
	// negligible.
	var deadCmd *exec.Cmd
	if runtime.GOOS == "windows" {
		deadCmd = exec.Command("cmd", "/C", "exit", "0")
	} else {
		deadCmd = exec.Command("sh", "-c", "exit 0")
	}
	process.NoConsole(deadCmd)
	if err := deadCmd.Run(); err != nil {
		t.Skipf("cannot spawn short-lived child: %v", err)
	}
	deadPid := deadCmd.Process.Pid

	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d 1\n", deadPid)), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()
	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		t.Fatalf("expected typed exit code error; got %v", err)
	}
	if fe.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3 (DeadPID skips kill prompt)", fe.ExitCode())
	}
}

// ---------------------------------------------------------------
// Scenario 7: --force --kill race-lost → exit 3
// ---------------------------------------------------------------

func TestForce_KillRaceLost(t *testing.T) {
	t.Setenv("MCPHUB_E2E_SUPERVISOR", "none")
	t.Setenv("MCPHUB_E2E_SCHEDULER", "none")
	t.Setenv(selfRestartHandoffEnv, "")
	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")
	sleepCmd, stopHolder, holderDone := startForceKillTestSleeper(t)
	pid := sleepCmd.Process.Pid
	fl := flock.New(pidport + ".lock")
	flCompetitor := flock.New(pidport + ".lock")
	prevGate := gui.IdentityGateForTest()
	prevHook := gui.PostKillHookForTest()
	gui.SetIdentityGate(func(v gui.Verdict) (bool, string) {
		return v.PID != pid, "force-kill test owns only its original child"
	})
	var killCalls atomic.Int32
	restoreKill := gui.SetKillProcessForTest(func(target int) error {
		if target != pid {
			return fmt.Errorf("force-kill requested unowned PID %d", target)
		}
		killCalls.Add(1)
		return stopHolder()
	})
	defer func() {
		if err := stopHolder(); err != nil {
			t.Errorf("settle original holder: %v", err)
		}
		if err := flCompetitor.Unlock(); err != nil {
			t.Errorf("release competitor fixture flock: %v", err)
		}
		if err := fl.Unlock(); err != nil {
			t.Errorf("release original fixture flock: %v", err)
		}
		restoreKill()
		gui.RestorePostKillHook(prevHook)
		gui.RestoreIdentityGate(prevGate)
	}()
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d 1\n", pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := fl.TryLock(); err != nil || !ok {
		t.Fatalf("pre-lock original holder: acquired=%v err=%v", ok, err)
	}

	hookCalls, competitorAcquired := 0, false
	var hookErr error
	gui.SetPostKillHook(func() {
		hookCalls++
		select {
		case <-holderDone:
		default:
			hookErr = errors.New("post-kill hook ran before original holder Cmd.Wait")
			return
		}
		if err := fl.Unlock(); err != nil {
			hookErr = fmt.Errorf("release original holder in post-kill hook: %w", err)
			return
		}
		for i := 0; i < 20; i++ {
			ok, err := flCompetitor.TryLock()
			if err != nil {
				hookErr = fmt.Errorf("competitor acquire: %w", err)
				return
			}
			if ok {
				competitorAcquired = true
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		hookErr = errors.New("competitor did not acquire the fixture flock")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.ExecuteContext(ctx)
	if ctx.Err() != nil {
		t.Errorf("race-lost command reached its deadline: %v", ctx.Err())
	}
	select {
	case <-holderDone:
	default:
		t.Error("original holder Cmd.Wait did not complete during kill")
	}
	if killCalls.Load() != 1 {
		t.Errorf("owned kill calls = %d, want 1", killCalls.Load())
	}
	if hookCalls != 1 || !competitorAcquired || hookErr != nil {
		t.Errorf("race-lost setup: hook calls=%d competitor acquired=%v err=%v", hookCalls, competitorAcquired, hookErr)
	}
	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		t.Fatalf("expected typed forceExitError from race-lost path; got %v", err)
	}
	if fe.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3 (RaceLost)", fe.ExitCode())
	}
}

// ---------------------------------------------------------------
// Scenario 8: Malformed pidport → exit 2
// ---------------------------------------------------------------

func TestForce_MalformedPidport(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte("garbage not a pidport"), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()
	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		t.Fatalf("expected typed exit code error; got %v", err)
	}
	if fe.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2 (Malformed reaches the diagnostic-only path)", fe.ExitCode())
	}
}

// ---------------------------------------------------------------
// Scenario 8b: KillRecordedHolder re-probe sees Healthy after
// cli's first Probe saw LiveUnreachable (Codex PR #23 P2 #2 iter-2).
// ---------------------------------------------------------------

// TestForce_KillRecoveryHealthyBetweenProbes pins the iter-2 fix for
// the runForceKill switch's missing VerdictHealthy arm.
// KillRecordedHolder's internal probe runs AFTER cli's first Probe
// (and AFTER the user confirmation prompt — `--yes` skips the prompt
// but the timing window is real). If the incumbent recovers to
// healthy in between (e.g., Server.Start finally bound during the
// prompt), KillRecordedHolder honors "never kill healthy" and
// returns Verdict{Class: Healthy}. Without the iter-2 arm this
// fell to the default "internal: unrecognized verdict class" arm
// and exited 1.
//
// Setup: a counting ping handler returns {ok:false} on the first
// request and {ok:true,pid:ourPid} on subsequent requests. Pidport
// mtime is backdated past probeStartupWindow so neither cli's first
// Probe nor KillRecordedHolder's internal probe triggers the
// startup-window retry (ensures each call makes exactly one ping
// request and we control the transition timing). cli's first Probe
// → ok:false → LiveUnreachable; KillRecordedHolder's probe →
// ok:true → Healthy → cli's iter-2 switch arm fires →
// "incumbent recovered to healthy" + exit 0.
func TestForce_KillRecoveryHealthyBetweenProbes(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")

	// Counting ping handler: first request fails, subsequent succeed.
	// Activate-window handler returns 204 so TryActivateIncumbent
	// doesn't error.
	var pingCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/activate-window" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/api/ping" {
			http.NotFound(w, r)
			return
		}
		count := atomic.AddInt32(&pingCount, 1)
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			// First probe (cli) sees a failed ping.
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false})
			return
		}
		// Subsequent probes (KillRecordedHolder's internal probe)
		// see a healthy incumbent.
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": os.Getpid()})
	}))
	defer srv.Close()
	port := portFromHTTPTestURL(t, srv.URL)

	if err := gui.WritePidport(pidport, os.Getpid(), port); err != nil {
		t.Fatal(err)
	}
	// Backdate pidport mtime past probeStartupWindow (5s) so the
	// retry loop is disabled and each Probe call makes exactly one
	// ping request. Without this, cli's first Probe might retry-
	// then-succeed and we'd never reach KillRecordedHolder.
	oldMtime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(pidport, oldMtime, oldMtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Pre-acquire flock so AcquireSingleInstance returns busy and
	// the --force --kill path is exercised. The seam below makes
	// the identity gate pass so we reach KillRecordedHolder's
	// internal Probe (which is what populates the Healthy verdict).
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	prevGate := gui.IdentityGateForTest()
	defer gui.RestoreIdentityGate(prevGate)
	gui.SetIdentityGate(func(v gui.Verdict) (refused bool, reason string) { return false, "" })

	var buf bytes.Buffer
	c := newGuiCmdRealForTest()
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()

	// Expect exit code 0. The cli wraps the exit code in
	// forceExitError even on success (so cmd/mcphub/main.go can
	// route via os.Exit(code)), so a forceExitError with
	// ExitCode()==0 is the success signal here, not nil.
	if err == nil {
		// Surprising — cobra returned nil. Acceptable only if the
		// output contains the success notice; fall through to the
		// output check.
	} else {
		var fe interface{ ExitCode() int }
		if !errors.As(err, &fe) {
			t.Errorf("expected typed forceExitError with ExitCode()==0; got %v", err)
		} else if fe.ExitCode() != 0 {
			t.Errorf("exit code = %d, want 0 (Healthy re-verdict success)", fe.ExitCode())
		}
	}
	out := buf.String()
	if !strings.Contains(out, "incumbent recovered to healthy") {
		t.Errorf("expected 'incumbent recovered to healthy' notice; got %q", out)
	}
}

// ---------------------------------------------------------------
// Scenario 9: Real subprocess E2E
// ---------------------------------------------------------------

func TestForce_RealSubprocessE2E(t *testing.T) {
	requireGuiSpawnTests(t)
	// Use the existing ensureMcphubBinary pattern from
	// daemon_reliability_test.go.
	bin := ensureMcphubBinary(t)
	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")
	// Phase A: spawn a first mcphub gui that holds the lock.
	first := exec.Command(bin, "gui", "--port", "0", "--no-browser", "--no-tray")
	process.NoConsole(first)
	first.Env = append(os.Environ(), "MCPHUB_GUI_TEST_PIDPORT_DIR="+dir)
	if err := first.Start(); err != nil {
		t.Fatalf("spawn first gui: %v", err)
	}
	// t.Cleanup (NOT defer) so the process is reaped BEFORE t.TempDir
	// removes pidportDir — Wait closes the kernel handle to the
	// pidport.lock file; without it, RemoveAll fails on Windows with
	// "file in use by another process". Sonnet review on PR #23
	// codex P1 (TestForce_RealSubprocessE2E zombie + cleanup race).
	t.Cleanup(func() {
		if first.Process != nil {
			_ = first.Process.Kill()
		}
		_ = first.Wait()
	})

	// Wait for first gui to write pidport.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidport); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(pidport); err != nil {
		t.Fatalf("first gui did not write pidport within deadline: %v", err)
	}

	// Phase B: spawn a second mcphub gui --force --kill --yes.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	second := exec.CommandContext(ctx, bin, "gui", "--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes")
	process.NoConsole(second)
	second.Env = append(os.Environ(), "MCPHUB_GUI_TEST_PIDPORT_DIR="+dir)
	out, err := second.CombinedOutput()
	t.Logf("second gui output:\n%s", string(out))
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Logf("second gui exit code: %d", ee.ExitCode())
		}
	}
	// Acceptable outcomes from the second invocation:
	//   a) "force-killed ... and acquired lock"  — kill succeeded, E2E
	//      exercised the full KillRecordedHolder + acquire path.
	//   b) "race" / "Race"                       — kill succeeded but a
	//      competitor won the new flock (VerdictRaceLost, exit 3).
	//   c) "incumbent is healthy"                — the first gui was still
	//      healthy when probed; runForceKill hit the Healthy early-exit
	//      guard and correctly refused to kill. This is valid E2E coverage
	//      of the --force --kill entry point and the Healthy guard; the
	//      kill-path hardware proof lives in Scenario 4 (seam-mocked) and
	//      the real binary was invoked end-to-end here.
	outStr := string(out)
	if !strings.Contains(outStr, "force-killed") &&
		!strings.Contains(outStr, "Race") &&
		!strings.Contains(outStr, "race") &&
		!strings.Contains(outStr, "incumbent is healthy") {
		t.Errorf("expected force-killed, race, or healthy-guard output from second gui; got %q", outStr)
	}
}

// ---------------------------------------------------------------
// Codex iter-9 P2 #1: identity gate must run BEFORE the destructive
// confirmation prompt. Without this, the operator could be asked
// "Kill PID X (mcphub gui)?" for a PID that the gate later refuses
// (e.g. recorded PID is `mcphub daemon`, recycled PID, macOS
// unsupported), and the refusal arrives only AFTER consent.
// ---------------------------------------------------------------

// TestForce_KillNonGUIMcphubSubcommand_RefusedBeforePrompt pins the
// iter-9 P2 #1 fix. Setup: pidport refers to a live PID whose
// argv[1] is "daemon" (not "gui"). The image basename matches, so
// the basename gate passes; the argv-subcommand gate refuses.
//
// With --yes (which would normally skip the prompt), the cli's
// pre-prompt CheckIdentityGate must observe the refusal and exit 7
// without ever proceeding to KillRecordedHolder. The diagnostic on
// stderr must mention the argv-gate refusal so the operator knows
// why the kill didn't fire.
//
// Cross-platform: ProcessIDOverride seam injects the synthetic
// identity on every runner (linux/windows/macOS), so the test runs
// uniformly without needing a real `mcphub daemon` subprocess.
func TestForce_KillNonGUIMcphubSubcommand_RefusedBeforePrompt(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	pidport := filepath.Join(dir, "gui.pidport")

	// pidport must record a live PID so probeOnce reaches
	// LiveUnreachable (and the gate runs). Use os.Getpid() — the
	// processID override below replaces the result regardless.
	const probablyClosedPort = 1
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d %d\n", os.Getpid(), probablyClosedPort)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate mtime so probe doesn't retry the LiveUnreachable
	// classification.
	oldMtime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(pidport, oldMtime, oldMtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Pre-acquire flock so AcquireSingleInstance returns busy and
	// the --force --kill path is exercised.
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	// Inject a synthetic identity that mimics a `mcphub daemon`
	// subprocess: image basename passes (mcphub / mcphub.exe), but
	// argv[1] == "daemon" fails the argv-gate.
	prevProcessID := gui.ProcessIDForTest()
	defer gui.RestoreProcessID(prevProcessID)
	mcphubBinary := mcphubBinaryNameForCliTest()
	gui.SetProcessIDOverride(func(pid int) (gui.ProcessIdentity, error) {
		return gui.ProcessIdentity{
			Alive:     true,
			ImagePath: mcphubBinary,
			Cmdline:   []string{mcphubBinary, "daemon", "wolfram"},
			// Start time well before pidport mtime so the
			// start-time gate would also pass — the argv gate is
			// what we want to trip.
			StartTime: time.Now().Add(-2 * time.Hour),
		}, nil
	})

	// If the gate ran AFTER the prompt instead of before, the
	// destructive prompt would block reading from stdin (no TTY in
	// `go test`). The non-TTY guard fires earlier (exit 6) without
	// --yes, so we use --yes to confirm the gate runs in the
	// post-non-TTY path.

	// Buffer both stdout and stderr because cobra's OutOrStderr()
	// returns the OUT writer when set (which the test helper sets
	// to os.Stdout). Capturing both leaves the assertion robust
	// against minor stream-routing changes in the cli.
	var buf bytes.Buffer
	c := newGuiCmdRealForTest()
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()

	// Exit code 7 — VerdictKillRefused per the exit-code map.
	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		t.Fatalf("expected typed exit code error; got %v", err)
	}
	if fe.ExitCode() != 7 {
		t.Errorf("exit code = %d, want 7 (kill refused at argv gate before prompt)", fe.ExitCode())
	}

	// Refusal message names the argv gate so the operator can act
	// on the refusal.
	got := buf.String()
	if !strings.Contains(got, "kill refused") {
		t.Errorf("output = %q; want a 'kill refused' line so operator sees the refusal reason", got)
	}
	// Codex iter-10 P2 #2 redacted: format is now `argv subcommand
	// is "<token>", not 'gui'` (only the offending subcommand, not
	// the full argv, to avoid leaking secrets from commands like
	// `mcphub secrets set --value <SECRET>`).
	if !strings.Contains(got, `argv subcommand is "daemon", not 'gui'`) {
		t.Errorf("output = %q; want the argv-gate diagnose so operator knows WHICH gate refused", got)
	}
}

// mcphubBinaryNameForCliTest mirrors the in-package helper from
// internal/gui/single_instance_test.go for cli-side use. Kept
// duplicated rather than exported because matchBasename's per-OS
// rule is an implementation detail of the gui package.
func mcphubBinaryNameForCliTest() string {
	if runtime.GOOS == "windows" {
		return `C:\Program Files\mcphub\mcphub.exe`
	}
	return "/usr/local/bin/mcphub"
}

// ---------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------

// healthyIncumbentServer creates an httptest.Server that mimics a
// healthy mcphub gui incumbent:
//   - GET /api/ping  → 200 {"ok":true,"pid":<pid>}
//   - POST /api/activate-window → 204 NoContent (required by TryActivateIncumbent)
//   - everything else → 200 {"ok":true,"pid":<pid>}
//
// TryActivateIncumbent checks resp2.StatusCode != http.StatusNoContent
// and returns an error on mismatch, so the activate-window endpoint
// must return exactly 204.
func healthyIncumbentServer(t *testing.T, pid int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/activate-window" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": pid})
	}))
}

// portFromHTTPTestURL extracts the numeric port from an httptest URL
// like "http://127.0.0.1:12345".
func portFromHTTPTestURL(t *testing.T, u string) int {
	t.Helper()
	const prefix = "http://127.0.0.1:"
	if !strings.HasPrefix(u, prefix) {
		t.Fatalf("unexpected httptest URL %q", u)
	}
	rest := u[len(prefix):]
	var port int
	for _, c := range rest {
		if c < '0' || c > '9' {
			break
		}
		port = port*10 + int(c-'0')
	}
	return port
}

// newGuiCmdRealForTest builds a fresh gui cobra command with the real
// RunE wired so --force flows actually execute. The command's I/O is
// wired to the test process's standard streams.
func newGuiCmdRealForTest() *cobra.Command {
	c := newGuiCmdReal()
	c.SetIn(os.Stdin)
	c.SetOut(os.Stdout)
	c.SetErr(os.Stderr)
	return c
}

// ---------------------------------------------------------------
// Codex iter-5 P2: main.go's force-exit branch must only match the
// cli's own sentinel, not arbitrary errors that happen to satisfy
// `interface{ ExitCode() int }` such as *os/exec.ExitError.
// ---------------------------------------------------------------

// TestForceExitError_SatisfiesMcphubMarker pins the positive case:
// the cli's forceExitError implements the combined matcher main.go
// uses, so legitimate --force / --force --kill exit codes still
// route through os.Exit(code).
func TestForceExitError_SatisfiesMcphubMarker(t *testing.T) {
	err := forceExit(7)
	var fe interface {
		ExitCode() int
		IsMcphubForceExit() bool
	}
	if !errors.As(err, &fe) {
		t.Fatalf("forceExit(7) should satisfy the combined matcher; got %v", err)
	}
	if fe.ExitCode() != 7 {
		t.Errorf("ExitCode() = %d, want 7", fe.ExitCode())
	}
	if !fe.IsMcphubForceExit() {
		t.Errorf("IsMcphubForceExit() = false, want true")
	}
}

// TestExecExitError_DoesNotShortCircuitMain pins the iter-5 P2 fix.
// Cobra commands like `mcp manifest edit` and `mcp secrets edit`
// wrap subprocess failures with `fmt.Errorf("...: %w", err)`. The
// child's *exec.ExitError satisfies `interface{ ExitCode() int }`,
// so a bare ExitCode-only matcher in main.go would have routed those
// errors to `os.Exit(<child code>)` and silently dropped the
// contextual "error: ..." diagnostic.
//
// Post-fix: main.go matches the combined
// `interface{ ExitCode() int; IsMcphubForceExit() bool }`, which
// *exec.ExitError does NOT satisfy. This test runs a child process
// that exits non-zero, wraps the error the same way the editor
// callsites in cli/manifest.go and cli/secrets.go do, and asserts
// the combined matcher rejects it.
func TestExecExitError_DoesNotShortCircuitMain(t *testing.T) {
	// Generate a real *exec.ExitError. On Windows, `cmd /C exit 7`
	// reliably yields exit code 7 without needing a POSIX shell.
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", "exit 7")
	} else {
		cmd = exec.Command("sh", "-c", "exit 7")
	}
	process.NoConsole(cmd)
	runErr := cmd.Run()
	if runErr == nil {
		t.Fatal("child process exited 0; expected non-zero exit for *exec.ExitError")
	}
	var ee *exec.ExitError
	if !errors.As(runErr, &ee) {
		t.Fatalf("child error is not *exec.ExitError: %T (%v)", runErr, runErr)
	}
	if ee.ExitCode() != 7 {
		t.Fatalf("child exit code = %d, want 7", ee.ExitCode())
	}

	// Wrap exactly like cli/manifest.go ("editor %s: %w") and
	// cli/secrets.go ("editor: %w") — main.go's matcher must reject
	// the wrapped form.
	wrapped := fmt.Errorf("editor %s: %w", "vim", runErr)

	// Bare ExitCode-only match (the pre-fix matcher) MUST still
	// match — that demonstrates the bug we're guarding against.
	var feBare interface{ ExitCode() int }
	if !errors.As(wrapped, &feBare) {
		t.Fatalf("regression: pre-fix bare ExitCode matcher should still match wrapped *exec.ExitError; got false")
	}
	if feBare.ExitCode() != 7 {
		t.Errorf("bare matcher ExitCode = %d, want 7 (exec.ExitError)", feBare.ExitCode())
	}

	// Post-fix combined matcher — the one main.go now uses — MUST
	// NOT match the wrapped *exec.ExitError, because *exec.ExitError
	// does not implement IsMcphubForceExit().
	var feCombined interface {
		ExitCode() int
		IsMcphubForceExit() bool
	}
	if errors.As(wrapped, &feCombined) {
		t.Errorf(
			"combined matcher must reject wrapped *exec.ExitError; matched ExitCode=%d "+
				"(this would silently suppress the 'error: ...' diagnostic in main.go)",
			feCombined.ExitCode(),
		)
	}
}

// ---------------------------------------------------------------
// Verdict-class → exit-code mapping (review finding: the new
// gui.VerdictIndeterminate class was unhandled by both switches)
// ---------------------------------------------------------------

// TestForceKillVerdictExitMaps_CoverEveryVerdictClass pins BOTH runForceKill
// mappings against the FULL gui.VerdictClass enum, so a class can never again
// be added upstream and silently land in an "internal: unexpected/unrecognized
// verdict class" arm — which is what happened to gui.VerdictIndeterminate: a
// legitimate, expected outcome (an ambiguous platform identity probe) was
// reported to the operator as an mcphub bug with exit 1, and the Verdict's own
// retry guidance was dropped.
//
// The mappings are exercised directly because runForceKill has no probe seam
// (it runs a real gui.Probe against a real pidport), and gui.VerdictIndeterminate
// requires a platform-level OpenProcess/kill(2) failure that a test cannot
// manufacture on demand.
//
// MUTATION: delete the `case gui.VerdictIndeterminate` arm from either helper —
// that helper's Indeterminate subtest fails with "terminal/mapped=false, want a
// non-destructive exit 4".
func TestForceKillVerdictExitMaps_CoverEveryVerdictClass(t *testing.T) {
	// Every class the enum defines today. If a new class is appended upstream
	// without extending this list, the exhaustiveness check at the bottom
	// fails.
	allClasses := []gui.VerdictClass{
		gui.VerdictHealthy,
		gui.VerdictLiveUnreachable,
		gui.VerdictDeadPID,
		gui.VerdictMalformed,
		gui.VerdictKilledRecovered,
		gui.VerdictKillRefused,
		gui.VerdictKillFailed,
		gui.VerdictRaceLost,
		gui.VerdictIndeterminate,
	}

	t.Run("preKill", func(t *testing.T) {
		want := map[gui.VerdictClass]struct {
			code     int
			terminal bool
		}{
			// Non-destructive terminal outcomes: no kill is attempted.
			gui.VerdictMalformed:     {4, true},
			gui.VerdictIndeterminate: {4, true}, // review finding
			gui.VerdictDeadPID:       {3, true},
			// Proceeds to the kill path.
			gui.VerdictLiveUnreachable: {0, false},
			// Handled before the mapping, or genuinely unexpected from Probe.
			gui.VerdictHealthy:         {0, false},
			gui.VerdictKilledRecovered: {0, false},
			gui.VerdictKillRefused:     {0, false},
			gui.VerdictKillFailed:      {0, false},
			gui.VerdictRaceLost:        {0, false},
		}
		for _, class := range allClasses {
			exp, ok := want[class]
			if !ok {
				t.Fatalf("class %q has no expectation — extend this table when a VerdictClass is added", class)
			}
			code, terminal := preKillVerdictExit(class)
			if code != exp.code || terminal != exp.terminal {
				t.Errorf("preKillVerdictExit(%q) = (%d, %v), want (%d, %v)",
					class, code, terminal, exp.code, exp.terminal)
			}
		}
		// gui.VerdictIndeterminate must NEVER authorize the kill path, and must
		// never be reported as an internal error.
		if code, terminal := preKillVerdictExit(gui.VerdictIndeterminate); !terminal || code != 4 {
			t.Fatalf("preKillVerdictExit(Indeterminate) = (%d, %v), want a non-destructive exit 4 — an ambiguous liveness probe must not fall into the 'internal: unexpected verdict class' arm", code, terminal)
		}
	})

	t.Run("postKill", func(t *testing.T) {
		want := map[gui.VerdictClass]struct {
			code   int
			mapped bool
		}{
			gui.VerdictKillRefused:   {7, true},
			gui.VerdictKillFailed:    {4, true},
			gui.VerdictMalformed:     {4, true},
			gui.VerdictIndeterminate: {4, true}, // review finding
			gui.VerdictRaceLost:      {3, true},
			gui.VerdictDeadPID:       {3, true},
			// Handled by runForceKill before this mapping (both exit 0).
			gui.VerdictHealthy:         {0, false},
			gui.VerdictKilledRecovered: {0, false},
			// Never produced by KillRecordedHolder as a failure verdict.
			gui.VerdictLiveUnreachable: {0, false},
		}
		for _, class := range allClasses {
			exp, ok := want[class]
			if !ok {
				t.Fatalf("class %q has no expectation — extend this table when a VerdictClass is added", class)
			}
			code, mapped := postKillVerdictExit(class)
			if code != exp.code || mapped != exp.mapped {
				t.Errorf("postKillVerdictExit(%q) = (%d, %v), want (%d, %v)",
					class, code, mapped, exp.code, exp.mapped)
			}
		}
		if code, mapped := postKillVerdictExit(gui.VerdictIndeterminate); !mapped || code != 4 {
			t.Fatalf("postKillVerdictExit(Indeterminate) = (%d, %v), want a non-destructive exit 4 — a SKIPPED kill must not be reported as an unrecognized class", code, mapped)
		}
	})

	// Exhaustiveness: String() falls through to "VerdictClass(%d)" for any
	// value the enum does not name, so scanning past the last known class
	// proves the list above is complete.
	if next := gui.VerdictIndeterminate + 1; !strings.HasPrefix(next.String(), "VerdictClass(") {
		t.Fatalf("gui.VerdictClass gained a new member %q after VerdictIndeterminate; extend allClasses and both expectation tables", next)
	}
}
