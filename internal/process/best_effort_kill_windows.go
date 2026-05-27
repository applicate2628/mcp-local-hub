//go:build windows

package process

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// BestEffortKillByPID attempts to terminate a process by PID using
// platform primitives. Used by the supervisor when StartWithJob
// returns ErrSpawnPostCreate (Windows post-CreateProcess orphan):
// the OS child exists at pi.ProcessId but the Go-side handle is
// unavailable, so the supervisor cannot use cmd.Process.Kill. This
// helper opens an independent handle via OpenProcess and calls
// TerminateProcess directly.
//
// Returns nil on success or if the process was already gone
// (ERROR_INVALID_PARAMETER from OpenProcess maps to "PID has been
// recycled / process never existed"). Returns a wrapped error for
// any other failure so the caller can audit-log it.
//
// Closes bot finding on PR #237 a646148: orphan branch in
// supervise.go was leaving daemon stuck in StSpawning forever
// because no synthetic EvChildExit was posted and the SM has no
// StSpawning + EvStart transition for reconcile-driven retries.
// With BestEffortKillByPID + wrap-as-errSpawnPreChild, the orphan
// is killed and the SM routes through StBackoffWaiting + backoff
// timer for retry; either the next spawn binds a fresh PID
// successfully OR fails with port-in-use (natural duplicate cap).
func BestEffortKillByPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		// ERROR_INVALID_PARAMETER means the PID does not refer to a
		// live process - treat as success (the orphan already died).
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return fmt.Errorf("OpenProcess(PROCESS_TERMINATE, pid=%d): %w", pid, err)
	}
	defer windows.CloseHandle(h)

	if err := windows.TerminateProcess(h, 1); err != nil {
		// ERROR_ACCESS_DENIED can occur if the process is already
		// in the middle of exiting (Windows reports the PID as still
		// alive briefly after ExitProcess but rejects new operations).
		// Treat as success since the goal (process is dead/dying)
		// is already met.
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil
		}
		return fmt.Errorf("TerminateProcess(pid=%d): %w", pid, err)
	}
	return nil
}
