//go:build windows

package process

import (
	"os/exec"
	"testing"
	"time"
)

// TestStartWithJob_AssignsAtCreate proves that StartWithJob spawns a
// child process that is *already* a member of the supervisor's Job
// Object at create time — closing the v0.4.x Start-then-Assign race
// documented at internal/process/jobobject_windows.go:65-71.
//
// The child is `cmd.exe /c exit 0` — short-lived so the test ends
// quickly, but long-enough to be observable in IsProcessInJob before
// the kernel reaps the process handle.
func TestStartWithJob_AssignsAtCreate(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()

	// Spawn a short-lived child. We use ping -n 2 (≈1s) instead of
	// `cmd.exe /c exit 0` so the process handle is still openable
	// for the IsProcessInJob probe — `exit 0` races the kernel reaper.
	cmd := exec.Command("ping.exe", "-n", "2", "127.0.0.1")
	pid, err := StartWithJob(job, cmd)
	if err != nil {
		t.Fatalf("StartWithJob: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("invalid pid: %d", pid)
	}
	defer func() {
		// Defensive: kill the child so we don't leak it if the test
		// fails before the natural exit.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	// Verify the child is associated with the Job Object.
	if !job.HasMember(pid) {
		t.Fatalf("child PID %d not in Job Object", pid)
	}

	// Wait for child exit (best-effort; ping should finish in ~1s).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid, t) {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Child still running after 5s — odd for a 1s ping but not a
	// failure of the assign-at-create contract; the test goal was
	// the HasMember check, which already passed.
}

// TestStartWithJob_NilJob verifies the misuse guard.
func TestStartWithJob_NilJob(t *testing.T) {
	cmd := exec.Command("ping.exe", "-n", "1", "127.0.0.1")
	if _, err := StartWithJob(nil, cmd); err == nil {
		t.Error("StartWithJob(nil job) returned nil error; want error")
	}
}

// TestStartWithJob_NilCmd verifies the misuse guard.
func TestStartWithJob_NilCmd(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()
	if _, err := StartWithJob(job, nil); err == nil {
		t.Error("StartWithJob(nil cmd) returned nil error; want error")
	}
}

// TestJob_HandleReturnsUnderlyingHandle exercises Job.Handle() — used
// by StartWithJob to thread the job handle through the attribute list.
func TestJob_HandleReturnsUnderlyingHandle(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()
	if job.Handle() == 0 {
		t.Error("Handle() returned 0; want non-zero")
	}
	// nil-receiver safety.
	var nilJob *Job
	if nilJob.Handle() != 0 {
		t.Error("nil receiver Handle() returned non-zero")
	}
}

// TestJob_HasMemberNilSafe exercises Job.HasMember nil-receiver and
// invalid-pid paths so the helper can be called defensively without
// crashing.
func TestJob_HasMemberNilSafe(t *testing.T) {
	var nilJob *Job
	if nilJob.HasMember(1) {
		t.Error("nil receiver HasMember returned true")
	}
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()
	// PID 0 is the system idle process — OpenProcess will fail; the
	// helper must return false rather than panic.
	if job.HasMember(0) {
		t.Error("HasMember(0) returned true; want false")
	}
}
