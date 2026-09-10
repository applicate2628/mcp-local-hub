//go:build windows

package process

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	breakawayHelperModeEnv = "MCPHUB_JOB_BREAKAWAY_HELPER_MODE"
	breakawayPIDFileEnv    = "MCPHUB_JOB_BREAKAWAY_PID_FILE"
)

func TestNewJob_ConfiguresKillOnCloseAndBreakaway(t *testing.T) {
	job, err := NewJob(JobOptions{KillOnClose: true, BreakawayOK: true})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	defer job.Close()

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(
		job.Handle(),
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		nil,
	); err != nil {
		t.Fatalf("QueryInformationJobObject: %v", err)
	}
	want := uint32(windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK)
	if got := uint32(info.BasicLimitInformation.LimitFlags) & want; got != want {
		t.Fatalf("job limit flags=%#x, want both %#x", info.BasicLimitInformation.LimitFlags, want)
	}
}

func TestJob_BreakawayEnabledDescendantEscapesClose(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	job, err := NewJob(JobOptions{KillOnClose: true, BreakawayOK: true})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	jobClosed := false
	defer func() {
		if !jobClosed {
			_ = job.Close()
		}
	}()

	parent := exec.Command(exe, "-test.run=^TestJobBreakawayHelper$")
	parent.Env = append(os.Environ(),
		breakawayHelperModeEnv+"=parent",
		breakawayPIDFileEnv+"="+pidFile,
	)
	if _, err := StartWithJob(job, parent); err != nil {
		t.Fatalf("StartWithJob(parent): %v", err)
	}
	parentWaited := false
	defer func() {
		if parent.Process != nil && !parentWaited {
			_ = parent.Process.Kill()
			_ = parent.Wait()
		}
	}()

	descendantPID := waitForPIDFile(t, pidFile, 10*time.Second)
	descendantHandle, err := windows.OpenProcess(
		windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
		false,
		uint32(descendantPID),
	)
	if err != nil {
		t.Fatalf("OpenProcess(descendant=%d): %v", descendantPID, err)
	}
	defer func() {
		_ = windows.TerminateProcess(descendantHandle, 1)
		_, _ = windows.WaitForSingleObject(descendantHandle, 5000)
		_ = windows.CloseHandle(descendantHandle)
	}()

	if !job.HasMember(parent.Process.Pid) {
		t.Fatalf("direct child pid=%d is not a Job member", parent.Process.Pid)
	}
	if job.HasMember(descendantPID) {
		t.Fatalf("breakaway descendant pid=%d remained in the Job", descendantPID)
	}
	if err := job.Close(); err != nil {
		t.Fatalf("Job.Close: %v", err)
	}
	jobClosed = true
	waitForProcessExit(t, parent.Process.Pid, 5*time.Second)
	_ = parent.Wait()
	parentWaited = true

	if status, err := windows.WaitForSingleObject(descendantHandle, 0); err != nil {
		t.Fatalf("probe descendant: %v", err)
	} else if status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("breakaway descendant status=%#x, want WAIT_TIMEOUT (still alive)", status)
	}
}

func TestJobBreakawayHelper(t *testing.T) {
	switch os.Getenv(breakawayHelperModeEnv) {
	case "parent":
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		descendant := exec.Command(exe, "-test.run=^TestJobBreakawayHelper$")
		descendant.Env = append(os.Environ(), breakawayHelperModeEnv+"=descendant")
		descendant.SysProcAttr = &syscall.SysProcAttr{
			HideWindow:    true,
			CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB,
		}
		if err := descendant.Start(); err != nil {
			t.Fatalf("start breakaway descendant: %v", err)
		}
		pidFile := os.Getenv(breakawayPIDFileEnv)
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(descendant.Process.Pid)), 0o600); err != nil {
			_ = descendant.Process.Kill()
			t.Fatal(err)
		}
		time.Sleep(2 * time.Minute)
	case "descendant":
		time.Sleep(2 * time.Minute)
	}
}

func waitForPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(body)))
			if parseErr != nil || pid <= 0 {
				t.Fatalf("invalid PID file %q: %v", body, parseErr)
			}
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("PID file %s was not written within %v", path, timeout)
	return 0
}

func waitForProcessExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid, t) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("process pid=%d remained alive after %v", pid, timeout))
}
