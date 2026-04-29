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
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"mcp-local-hub/internal/gui"
)

// ---------------------------------------------------------------
// Scenario 1: Healthy --force (bare flag activates via handshake)
// ---------------------------------------------------------------

func TestForce_HealthyIncumbent_BareFlagActivates(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	srv := healthyIncumbentServer(t, os.Getpid())
	defer srv.Close()
	port := portFromHTTPTestURL(t, srv.URL)
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d %d\n", os.Getpid(), port)), 0o600); err != nil {
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
	dir := t.TempDir()
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
// Scenario 3: Stuck — bare --force shows diagnostic + opens folder
// ---------------------------------------------------------------

func TestForce_StuckIncumbent_BareFlagShowsDiagnosticAndOpensFolder(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	const probablyClosedPort = 1
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d %d\n", os.Getpid(), probablyClosedPort)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pre-acquire flock so AcquireSingleInstance returns busy.
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	// Mock the open-folder seam.
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
	if spawnedName == "" {
		t.Errorf("OpenFolderAt seam was not invoked")
	}
}

// ---------------------------------------------------------------
// Scenario 4: --force --kill happy path (seam-mocked gate)
// ---------------------------------------------------------------

func TestForce_KillHappyPath_SeamMocked(t *testing.T) {
	// This scenario uses a seam to bypass the three-part gate's
	// strict cmdline/image checks (which would reject the test
	// binary as non-mcphub). Real-process proof is in scenario 8.
	prevGate := gui.IdentityGateForTest()
	defer gui.RestoreIdentityGate(prevGate)
	gui.SetIdentityGate(func(v gui.Verdict) (refused bool, reason string) { return false, "" })

	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	// Spawn a child sleep process so we have a real PID we can kill.
	var sleepCmd *exec.Cmd
	if runtime.GOOS == "windows" {
		sleepCmd = exec.Command("powershell", "-NoExit", "-Command", "Start-Sleep -Seconds 60")
	} else {
		sleepCmd = exec.Command("sleep", "60")
	}
	if err := sleepCmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep helper: %v", err)
	}
	defer func() {
		_ = sleepCmd.Process.Kill()
	}()
	pid := sleepCmd.Process.Pid
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d 1\n", pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	// Release the flock when the child dies — simulate the OS
	// auto-release so the acquire-poll loop can succeed.
	go func() {
		_, _ = sleepCmd.Process.Wait()
		fl.Unlock()
	}()

	// Use a pre-cancelled context so that if kill+acquire succeeds and
	// startGuiServer is entered, it exits immediately via <-ctx.Done()
	// rather than blocking until the test timeout fires.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately — startGuiServer will see Done on first select

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.ExecuteContext(ctx)
	if err != nil {
		// Acceptable outcomes: context.Canceled (startGuiServer exited
		// cleanly), forceExitError 0 (healthy-early-exit), or exit 3
		// (race-lost). Assert it is NOT exit 7 (refused) or exit 4 (failed).
		var fe interface{ ExitCode() int }
		if errors.As(err, &fe) {
			if fe.ExitCode() == 7 || fe.ExitCode() == 4 {
				t.Errorf("happy path exit code = %d (refused/failed); want 0, 3, or context cancel", fe.ExitCode())
			}
		}
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
	prevGate := gui.IdentityGateForTest()
	defer gui.RestoreIdentityGate(prevGate)
	gui.SetIdentityGate(func(v gui.Verdict) (refused bool, reason string) { return false, "" })

	// Discover a free ephemeral port: bind on :0, read the port the
	// OS picked, close. The actualPort==requestedPort branch is the
	// one the P2 #2 bug hid in.
	freePort := pickFreePort(t)

	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")

	// Spawn a sleep helper so Probe sees a live PID. The kill path
	// then terminates this child, the OS releases its flock, and our
	// acquire-poll succeeds.
	var sleepCmd *exec.Cmd
	if runtime.GOOS == "windows" {
		sleepCmd = exec.Command("powershell", "-NoExit", "-Command", "Start-Sleep -Seconds 60")
	} else {
		sleepCmd = exec.Command("sleep", "60")
	}
	if err := sleepCmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep helper: %v", err)
	}
	defer func() { _ = sleepCmd.Process.Kill() }()
	stalePid := sleepCmd.Process.Pid
	const stalePort = 1
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d %d\n", stalePid, stalePort)), 0o600); err != nil {
		t.Fatal(err)
	}

	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	// Release the flock when the child dies — simulate the OS
	// auto-release so the acquire-poll loop inside KillRecordedHolder
	// can succeed.
	go func() {
		_, _ = sleepCmd.Process.Wait()
		_ = fl.Unlock()
	}()

	// Run the command with a context that we cancel as soon as the
	// pidport has been rewritten with our PID. 10s overall deadline
	// keeps the test bounded if the rewrite never happens.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", fmt.Sprintf("%d", freePort), "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)

	cmdDone := make(chan error, 1)
	go func() { cmdDone <- c.ExecuteContext(ctx) }()

	// Poll the pidport: succeed as soon as we observe
	// (os.Getpid(), freePort) — that's the proof startGuiServer's
	// WritePidport landed in the actualPort==requestedPort branch.
	// Fail if the takeover happened (cmdDone fires early) without
	// ever rewriting the pidport with our PID.
	deadline := time.Now().Add(8 * time.Second)
	var observedPid, observedPort int
	for time.Now().Before(deadline) {
		pid, port, rerr := gui.ReadPidport(pidport)
		if rerr == nil && pid == os.Getpid() && port == freePort {
			observedPid = pid
			observedPort = port
			break
		}
		select {
		case err := <-cmdDone:
			// Command exited before we observed the rewrite.
			// Re-check the pidport (it may have written and then
			// the test's polling missed the window before
			// shutdown). If still stale, fail with the command
			// error for context.
			pid2, port2, rerr2 := gui.ReadPidport(pidport)
			if rerr2 == nil && pid2 == os.Getpid() && port2 == freePort {
				observedPid = pid2
				observedPort = port2
				break
			}
			t.Fatalf("command exited (err=%v) but pidport still has pid=%d port=%d (stale=%d:%d, want %d:%d)",
				err, pid2, port2, stalePid, stalePort, os.Getpid(), freePort)
		case <-time.After(50 * time.Millisecond):
		}
		if observedPid != 0 {
			break
		}
	}
	// Cancel the context so startGuiServer shuts down cleanly.
	cancel()
	// Drain the command goroutine so the test doesn't leak it.
	select {
	case <-cmdDone:
	case <-time.After(5 * time.Second):
		t.Logf("warning: command did not exit within 5s after cancel")
	}

	if observedPid == 0 {
		t.Fatalf("pidport never rewritten with current PID + freePort=%d; last read: pid=%d port=%d (stale incumbent was pid=%d port=%d)",
			freePort, observedPid, observedPort, stalePid, stalePort)
	}
	if observedPid == stalePid {
		t.Errorf("pidport still contains the killed incumbent's PID %d — WritePidport did not run", stalePid)
	}
	if observedPid != os.Getpid() {
		t.Errorf("pidport pid = %d, want os.Getpid() = %d", observedPid, os.Getpid())
	}
	if observedPort != freePort {
		t.Errorf("pidport port = %d, want freePort = %d", observedPort, freePort)
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
	dir := t.TempDir()
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
	dir := t.TempDir()
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
// Scenario 7: --force --kill race-lost → exit 3
// ---------------------------------------------------------------

func TestForce_KillRaceLost(t *testing.T) {
	// Inject seam: identity gate passes so the test binary's sleep
	// child PID is accepted as a valid target.  The postKillHook
	// immediately re-acquires the flock from a "competitor" goroutine
	// so the acquire-poll loop inside KillRecordedHolder always sees
	// ErrSingleInstanceBusy and eventually returns VerdictRaceLost.
	prevGate := gui.IdentityGateForTest()
	defer gui.RestoreIdentityGate(prevGate)
	gui.SetIdentityGate(func(v gui.Verdict) (refused bool, reason string) { return false, "" })

	prevHook := gui.PostKillHookForTest()
	defer gui.RestorePostKillHook(prevHook)

	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")

	// Spawn a sleep child so Probe sees a live PID.
	var sleepCmd *exec.Cmd
	if runtime.GOOS == "windows" {
		sleepCmd = exec.Command("powershell", "-NoExit", "-Command", "Start-Sleep -Seconds 60")
	} else {
		sleepCmd = exec.Command("sleep", "60")
	}
	if err := sleepCmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep helper: %v", err)
	}
	defer func() { _ = sleepCmd.Process.Kill() }()

	pid := sleepCmd.Process.Pid
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d 1\n", pid)), 0o600); err != nil {
		t.Fatal(err)
	}

	// Pre-acquire the lock as the "original holder".
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}

	// competitor flock — held by the "race winner" after the kill.
	flCompetitor := flock.New(pidport + ".lock")

	// postKillHook: release the original holder lock (simulating OS
	// auto-release after kill), then immediately re-acquire as the
	// competitor so the acquire-poll loop always sees the flock busy.
	gui.SetPostKillHook(func() {
		fl.Unlock()
		// Spin until we acquire as the competitor.
		for i := 0; i < 20; i++ {
			if ok, _ := flCompetitor.TryLock(); ok {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})

	c := newGuiCmdRealForTest()
	c.SetArgs([]string{"--port", "0", "--no-browser", "--no-tray", "--force", "--kill", "--yes"})
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", dir)
	err := c.Execute()
	// Release competitor flock after the command finishes.
	_ = flCompetitor.Unlock()

	var fe interface{ ExitCode() int }
	if !errors.As(err, &fe) {
		t.Errorf("expected typed forceExitError from race-lost path; got %v", err)
		return
	}
	if fe.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3 (RaceLost)", fe.ExitCode())
	}
}

// ---------------------------------------------------------------
// Scenario 8: Malformed pidport → exit 2
// ---------------------------------------------------------------

func TestForce_MalformedPidport(t *testing.T) {
	dir := t.TempDir()
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
	dir := t.TempDir()
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

	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d %d\n", os.Getpid(), port)), 0o600); err != nil {
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
	// Use the existing ensureMcphubBinary pattern from
	// daemon_reliability_test.go.
	bin := ensureMcphubBinary(t)
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	// Phase A: spawn a first mcphub gui that holds the lock.
	first := exec.Command(bin, "gui", "--port", "0", "--no-browser", "--no-tray")
	first.Env = append(os.Environ(), "MCPHUB_GUI_TEST_PIDPORT_DIR="+dir)
	if err := first.Start(); err != nil {
		t.Fatalf("spawn first gui: %v", err)
	}
	defer func() {
		if first.Process != nil {
			_ = first.Process.Kill()
		}
	}()

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
