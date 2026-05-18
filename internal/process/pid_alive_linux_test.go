//go:build linux

package process

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestProcStatState(t *testing.T) {
	tests := []struct {
		name string
		stat string
		want byte
		ok   bool
	}{
		{name: "running", stat: "123 (mcphub) R 1 2 3", want: 'R', ok: true},
		{name: "zombie", stat: "124 (mcphub helper) Z 1 2 3", want: 'Z', ok: true},
		{name: "comm contains paren", stat: "125 (mcphub)helper) S 1 2 3", want: 'S', ok: true},
		{name: "missing paren", stat: "126 mcphub S 1 2 3", ok: false},
		{name: "missing state", stat: "127 (mcphub)", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := procStatState([]byte(tc.stat))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("procStatState(%q) = (%q, %v), want (%q, %v)", tc.stat, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestIsPidAlive_RejectsZombie(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestIsPidAlive_ZombieHelper")
	cmd.Env = append(os.Environ(), "MCPHUB_PID_ALIVE_ZOMBIE_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start zombie helper: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("zombie helper started without Process")
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Wait()
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !IsPidAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	statPath := fmt.Sprintf("/proc/%d/stat", pid)
	data, _ := os.ReadFile(statPath)
	t.Fatalf("zombie helper pid %d was still reported alive; %s=%q", pid, statPath, string(data))
}

func TestIsPidAlive_ZombieHelper(t *testing.T) {
	if os.Getenv("MCPHUB_PID_ALIVE_ZOMBIE_HELPER") != "1" {
		return
	}
	os.Exit(0)
}
