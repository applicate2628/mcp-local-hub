//go:build linux

package process

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestStartWithJob_SetsParentDeathSignal(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	cmd := exec.Command("sh", "-c", "exit 0")
	if _, err := StartWithJob(job, cmd); err != nil {
		t.Fatalf("StartWithJob: %v", err)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("StartWithJob must initialize SysProcAttr on Linux")
	}
	if cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("Pdeathsig=%v, want SIGKILL", cmd.SysProcAttr.Pdeathsig)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait child: %v", err)
	}
}
