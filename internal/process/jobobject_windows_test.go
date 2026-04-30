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
