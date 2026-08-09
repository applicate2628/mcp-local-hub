//go:build !windows

package pinstatus

import (
	"errors"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func waitForRemoteProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	state, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	var waitStatus syscall.WaitStatus
	reaped, waitErr := syscall.Wait4(pid, &waitStatus, syscall.WNOHANG, nil)
	t.Fatalf("pid %d survived remote runner return (test pid %d, diagnostic wait=%d err=%v): %s", pid, os.Getpid(), reaped, waitErr, state)
}
