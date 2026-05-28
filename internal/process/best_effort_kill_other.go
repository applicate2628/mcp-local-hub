//go:build !windows

package process

import (
	"errors"
	"fmt"
	"syscall"
)

// BestEffortKillByPID attempts to terminate a process by PID using
// platform primitives. On POSIX (Linux/macOS) this sends SIGKILL via
// syscall.Kill. POSIX never returns ErrSpawnPostCreate from
// StartWithJob (the Windows-specific FindProcess-after-CreateProcess
// race does not exist on POSIX), so this code path is defensive
// only - if a future caller needs to kill a non-cmd.Process PID on
// POSIX, the primitive is here.
//
// Returns nil on success or if the process was already gone
// (syscall.ESRCH = "no such process"). Returns a wrapped error for
// any other failure so the caller can audit-log it.
func BestEffortKillByPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		// ESRCH = "no such process" - treat as success (the orphan
		// already died).
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("syscall.Kill(pid=%d, SIGKILL): %w", pid, err)
	}
	return nil
}
