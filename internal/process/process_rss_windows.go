//go:build windows

package process

import (
	winprocess "github.com/Microsoft/go-winio/pkg/process"
	"golang.org/x/sys/windows"
)

// ResidentSetSizeByPID returns the resident set size (working-set bytes)
// of pid via OpenProcess + GetProcessMemoryInfo. It is the Windows
// implementation of the optional per-PID RAM lookup. It does not verify a PID
// generation; identity-qualified status uses ObservePIDStatus instead.
//
// Returns (0, false) on any failure — invalid pid, OpenProcess denied
// (insufficient rights or the process no longer exists), or the GetProcessMemoryInfo
// syscall failing. RAM is a best-effort diagnostic, so a miss renders no
// RAM row rather than surfacing an error; callers MUST treat ok=false as
// "unknown" and omit the metric.
func ResidentSetSizeByPID(pid int) (uint64, bool) {
	if pid <= 0 {
		return 0, false
	}
	// PROCESS_QUERY_LIMITED_INFORMATION is sufficient for
	// GetProcessMemoryInfo and is the least-privilege right that succeeds
	// against same-user processes — mirrors process_start_time_windows.go.
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	return residentSetSizeFromHandle(h)
}

// residentSetSizeFromHandle borrows a query-capable handle for this synchronous
// read only. The caller retains ownership and decides when to close it.
func residentSetSizeFromHandle(h windows.Handle) (uint64, bool) {
	counters, err := winprocess.GetProcessMemoryInfo(h)
	if err != nil {
		return 0, false
	}
	return uint64(counters.WorkingSetSize), true
}
