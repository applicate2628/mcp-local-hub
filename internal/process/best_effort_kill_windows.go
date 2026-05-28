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
// bestEffortKillWaitTimeoutMs is the maximum time BestEffortKillByPID
// blocks waiting for the killed process to fully exit (release its
// kernel handles, free its port bindings, etc.). 5s mirrors the
// shutdown timeout discipline used elsewhere in the supervisor
// (cmd.Process.Kill grace period).
var bestEffortKillWaitTimeoutMs uint32 = 5000

func BestEffortKillByPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	// Open the handle with BOTH PROCESS_TERMINATE (to call
	// TerminateProcess) AND SYNCHRONIZE (to call WaitForSingleObject
	// for actual exit signaling). Without SYNCHRONIZE, the helper
	// would return as soon as TerminateProcess succeeded but the
	// kernel might still be tearing down the process - any caller
	// that immediately tried to rebind the port (the supervisor's
	// backoff respawn does exactly this) would race against the
	// orphan's still-active socket and either succeed-with-races or
	// fail-with-port-in-use. Closes bot finding on PR #237 82f1899
	// (P2 wait-for-exit-before-retrying).
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// ERROR_INVALID_PARAMETER means the PID does not refer to a
		// live process - treat as success (the orphan already died).
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return fmt.Errorf("OpenProcess(PROCESS_TERMINATE|SYNCHRONIZE, pid=%d): %w", pid, err)
	}
	defer windows.CloseHandle(h)

	if err := windows.TerminateProcess(h, 1); err != nil {
		// ERROR_ACCESS_DENIED can occur if the process is already
		// in the middle of exiting (Windows reports the PID as still
		// alive briefly after ExitProcess but rejects new operations).
		// We still wait below for the handle to signal exit so the
		// caller knows the process is fully gone.
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("TerminateProcess(pid=%d): %w", pid, err)
		}
	}

	// Wait for the handle to signal (process has fully exited and
	// released its kernel resources). Bounded by
	// bestEffortKillWaitTimeoutMs so a hung-on-exit process doesn't
	// block the supervisor indefinitely; if the wait times out the
	// caller treats this as a kill failure.
	ev, err := windows.WaitForSingleObject(h, bestEffortKillWaitTimeoutMs)
	if err != nil {
		return fmt.Errorf("WaitForSingleObject(pid=%d): %w", pid, err)
	}
	switch ev {
	case windows.WAIT_OBJECT_0:
		// Process exited; orphan is truly gone. Safe for caller to
		// rebind the port via backoff respawn.
		return nil
	case uint32(windows.WAIT_TIMEOUT):
		return fmt.Errorf("WaitForSingleObject(pid=%d): timeout after %dms (process did not exit)", pid, bestEffortKillWaitTimeoutMs)
	default:
		return fmt.Errorf("WaitForSingleObject(pid=%d): unexpected event %d", pid, ev)
	}
}
