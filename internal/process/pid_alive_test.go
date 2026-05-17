package process

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestIsPidAlive(t *testing.T) {
	if !IsPidAlive(os.Getpid()) {
		t.Fatalf("current process pid %d must be reported alive", os.Getpid())
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd.exe", "/c", "exit", "0")
	} else {
		cmd = exec.Command("sh", "-c", "exit 0")
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start short-lived child: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("short-lived child started without Process")
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait short-lived child: %v", err)
	}

	if IsPidAlive(pid) {
		t.Fatalf("exited child pid %d must be reported dead", pid)
	}
}

func TestTerminatePID(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestTerminatePID_HelperSleep")
	cmd.Env = append(os.Environ(), "MCPHUB_TERMINATE_PID_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper child: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("helper child started without Process")
	}
	pid := cmd.Process.Pid
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	if err := TerminatePID(pid); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("TerminatePID(%d): %v", pid, err)
	}

	select {
	case <-waitCh:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("helper child pid %d did not exit after TerminatePID", pid)
	}
	if IsPidAlive(pid) {
		t.Fatalf("helper child pid %d still reported alive after TerminatePID", pid)
	}
}

func TestTerminatePID_AlreadyExited(t *testing.T) {
	pid := exitedProcessPID(t)
	if IsPidAlive(pid) {
		t.Fatalf("test precondition failed: pid %d is still alive after Wait", pid)
	}

	err := TerminatePID(pid)
	if !errors.Is(err, ErrProcessAlreadyExited) {
		t.Fatalf("TerminatePID(%d) error = %v, want ErrProcessAlreadyExited", pid, err)
	}
}

func TestTerminatePID_HelperSleep(t *testing.T) {
	if os.Getenv("MCPHUB_TERMINATE_PID_HELPER") != "1" {
		return
	}
	time.Sleep(60 * time.Second)
}

func exitedProcessPID(t *testing.T) int {
	t.Helper()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd.exe", "/c", "exit", "0")
	} else {
		cmd = exec.Command("sh", "-c", "exit 0")
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start short-lived child: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("short-lived child started without Process")
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait short-lived child: %v", err)
	}
	return pid
}
