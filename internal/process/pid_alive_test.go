package process

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
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
