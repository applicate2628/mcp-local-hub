//go:build windows

package process

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestJob_KillsAssignedProcessOnClose proves the actual contract:
// when the last handle to a job with KILL_ON_JOB_CLOSE is closed, the
// kernel terminates every process still in the job. Without this, the
// fix is theater — the syscalls succeed but orphans still survive.
//
// Spawns `timeout /T 30` (Windows builtin), assigns it to a job,
// closes the job, asserts the process is dead within ~1s.
func TestJob_KillsAssignedProcessOnClose(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skipf("timeout.exe not on PATH: %v", err)
	}
	cmd := exec.Command("timeout", "/T", "30", "/NOBREAK")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		// Defensive: if the test failed before Close fired, kill the
		// child so we don't leak it across runs.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	if err := job.Assign(cmd); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	// Verify process is alive immediately after assignment — orphan
	// protection should not pre-emptively kill anyone.
	if !processAlive(pid, t) {
		t.Fatalf("process pid=%d already dead before Close — assignment must not pre-kill", pid)
	}

	// Closing the job's last handle should cause the kernel to kill
	// the assigned process via KILL_ON_JOB_CLOSE.
	if err := job.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Poll for process death — kernel cleanup is fast but not instant.
	// 2s is generous; if it takes longer the contract is broken.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid, t) {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process pid=%d still alive 2s after Job.Close — KILL_ON_JOB_CLOSE did not fire", pid)
}

// TestJob_AssignNilCmdReturnsError guards against a misuse of the API
// where a caller forgets to Start() the cmd before assigning. Without
// this check, the underlying OpenProcess(0) syscall returns a
// confusing error from the kernel layer instead of an actionable one.
func TestJob_AssignNilCmdReturnsError(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()

	if err := job.Assign(nil); err == nil {
		t.Error("Assign(nil) returned nil error; want error")
	}
	cmd := exec.Command("notepad.exe")
	// cmd.Process is nil here — Start was not called.
	if err := job.Assign(cmd); err == nil {
		t.Error("Assign(cmd-not-started) returned nil error; want error")
	}
}

// TestJob_CloseIdempotent guards against a double-Close panic. The
// daemon Stop path closes the job after killProcessTree — if a future
// refactor accidentally calls Close twice, the second call must not
// panic or return a misleading "handle invalid" error.
func TestJob_CloseIdempotent(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	if err := job.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := job.Close(); err != nil {
		t.Errorf("second Close: %v (must be no-op)", err)
	}
}

// TestJob_TerminateAllWaitsForExit is the load-bearing regression
// guard for ADR-#239 (per-task Job Object). Pre-ADR the TerminateAll
// stub returned immediately after TerminateJobObject, racing the
// kernel teardown — supervisor backoff respawn could rebind the
// port before the orphan socket was released. This test proves the
// post-ADR contract: TerminateAll(timeoutMs > 0) must NOT return
// until ActiveProcesses reaches zero OR the deadline expires.
//
// Method: spawn timeout /T 30 child, assign to its own Job (mirrors
// the per-spawn allocation in runSupervise), call TerminateAll(5000),
// assert (a) call returned without timeout error, (b) wall-clock
// elapsed under the deadline, (c) the PID is no longer alive.
func TestJob_TerminateAllWaitsForExit(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skipf("timeout.exe not on PATH: %v", err)
	}
	cmd := exec.Command("timeout", "/T", "30", "/NOBREAK")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()

	if err := job.Assign(cmd); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if !processAlive(pid, t) {
		t.Fatalf("pid=%d already dead before TerminateAll — assignment must not pre-kill", pid)
	}

	start := time.Now()
	if err := job.TerminateAll(5000); err != nil {
		t.Fatalf("TerminateAll: %v (regression: real polling broken OR ActiveProcesses query failed)", err)
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("TerminateAll took %v; should return well within 5s for one timeout.exe child (regression: returned timeout when it should have completed)", elapsed)
	}
	if processAlive(pid, t) {
		t.Errorf("pid=%d still alive after TerminateAll returned; the wait-for-exit contract is broken (caller will race respawn against still-tearing-down orphan)", pid)
	}
}

// TestJob_TerminateAllZeroTimeoutSkipsWait guards the explicit
// opt-out-of-wait path documented in jobobject_windows.go. Callers
// that pass timeoutMs=0 want the pre-ADR fire-and-return semantic
// (e.g. shutdown paths where the kernel will reap on process exit
// anyway). A future refactor accidentally making zero-timeout wait
// forever would deadlock supervisor shutdown — this test catches that.
func TestJob_TerminateAllZeroTimeoutSkipsWait(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()

	start := time.Now()
	if err := job.TerminateAll(0); err != nil {
		t.Errorf("TerminateAll(0) on empty job: %v; want nil (opt-out-of-wait path)", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("TerminateAll(0) took %v; want near-instant return (regression: zero-timeout path is waiting when it should not)", elapsed)
	}
}

// processAlive returns true if a process with pid is still in the OS
// process table. Uses windows.OpenProcess with SYNCHRONIZE — sufficient
// for liveness probe and tolerates the fast post-exit window where the
// process object exists but is signaled.
func processAlive(pid int, t *testing.T) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// Most likely "no such process" once the kernel has reaped it.
		return false
	}
	defer windows.CloseHandle(h)
	// WaitForSingleObject with timeout 0 returns WAIT_OBJECT_0 if the
	// process has exited (process handle is signaled on exit).
	ev, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	return ev != uint32(windows.WAIT_OBJECT_0) // alive if NOT signaled
}

// silence unused-import on builds that strip the strconv usage.
var _ = strconv.Itoa
var _ = os.Stderr
