//go:build windows

package cli

import (
	"golang.org/x/sys/windows"
)

// pidAliveImpl probes liveness via windows.OpenProcess + WaitForSingleObject(0).
//
// Why not os.FindProcess + Signal(0)? Because the stdlib's
// Process.Signal on Windows returns syscall.EWINDOWS for any
// non-Kill signal — including signal 0 — regardless of whether the
// process is alive. That makes the POSIX-style probe useless here.
//
// Probe details:
//   - windows.OpenProcess fails with ERROR_INVALID_PARAMETER when the
//     PID was never assigned, or with the OS's "no such process"
//     code once the kernel has reaped the process object.
//   - WaitForSingleObject with timeout 0 returns WAIT_OBJECT_0 when
//     the process handle is signaled — i.e. the process has exited
//     but the handle is still open (typical brief post-exit window
//     before kernel reap).
//
// Mirrors internal/process/jobobject_windows_test.go:108-127's
// processAlive() — duplicated here rather than imported because
// that helper takes a *testing.T (test-only) and lives in the
// internal/process package, not internal/cli.
//
// SYNCHRONIZE alone is enough access right for the probe;
// PROCESS_QUERY_LIMITED_INFORMATION would let us also call
// GetExitCodeProcess but the WaitForSingleObject result is already
// a perfect signaled/not-signaled answer.
func pidAliveImpl(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// Most common: ERROR_INVALID_PARAMETER (no such PID) or the
		// kernel has reaped the process object.
		return false
	}
	defer windows.CloseHandle(h)

	ev, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	// WAIT_OBJECT_0 = handle is signaled (process has exited).
	return ev != uint32(windows.WAIT_OBJECT_0)
}
