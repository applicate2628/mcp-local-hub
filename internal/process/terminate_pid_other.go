//go:build !windows

package process

import (
	"errors"
	"fmt"
	"syscall"
)

func TerminatePID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("process: invalid PID %d", pid)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("process: PID %d already exited before SIGTERM: %w", pid, ErrProcessAlreadyExited)
		}
		return fmt.Errorf("process: send SIGTERM to PID %d: %w", pid, err)
	}
	return nil
}
