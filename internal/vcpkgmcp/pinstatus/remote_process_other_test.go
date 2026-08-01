//go:build !windows

package pinstatus

import (
	"errors"
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
	t.Fatalf("pid %d survived remote runner return", pid)
}
