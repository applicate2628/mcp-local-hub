//go:build !windows && !linux

package process

import (
	"errors"
	"fmt"
	"syscall"
)

// IsPidAlive reports whether pid currently refers to a live process.
// On macOS/BSD this remains a kill(0)-only check for v0.5.0 preview
// scope; Linux excludes zombies via /proc in pid_alive_linux.go.
func IsPidAlive(pid int) bool {
	state, err := QueryPIDState(pid)
	return err == nil && state == PIDStateAlive
}

func QueryPIDState(pid int) (PIDState, error) {
	if pid <= 0 {
		return PIDStateDead, nil
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return PIDStateAlive, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return PIDStateDead, nil
	}
	if errors.Is(err, syscall.EPERM) {
		return PIDStateAlive, nil
	}
	return PIDStateUnknown, fmt.Errorf("kill(%d, 0): %w", pid, err)
}
