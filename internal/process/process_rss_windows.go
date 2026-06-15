//go:build windows

package process

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GetProcessMemoryInfo is exported by psapi.dll but NOT surfaced by
// golang.org/x/sys/windows v0.43.0, so resolve it lazily — the same
// pattern jobobject_windows.go uses for IsProcessInJob. psapi.dll is a
// stable system DLL present on every supported Windows version; the
// kernel32 forwarder K32GetProcessMemoryInfo exists too but psapi is the
// documented export name.
var (
	procGetProcessMemoryInfo = syscall.NewLazyDLL("psapi.dll").NewProc("GetProcessMemoryInfo")
)

// processMemoryCounters mirrors the Win32 PROCESS_MEMORY_COUNTERS struct.
// golang.org/x/sys/windows does not define it, so we lay it out by hand
// per Microsoft docs (winnt.h / psapi.h):
//
//	https://learn.microsoft.com/windows/win32/api/psapi/ns-psapi-process_memory_counters
//
// cb (DWORD) + PageFaultCount (DWORD) + 8 × SIZE_T. On 64-bit Windows
// SIZE_T is 8 bytes; the leading two DWORDs (4 bytes each) pack into the
// first 8 bytes with natural alignment. WorkingSetSize is the resident
// set size (RSS) — the same value wmic's WorkingSetSize column reports,
// so the supervisor IPC RAM figure stays consistent with the legacy
// scheduler-scan enrichment path (internal/api/processes.go).
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// ResidentSetSizeByPID returns the resident set size (working-set bytes)
// of pid via OpenProcess + GetProcessMemoryInfo. It is the Windows
// implementation of the per-daemon RAM lookup the supervisor IPC status
// producer uses (internal/cli/supervise_status.go).
//
// Returns (0, false) on any failure — invalid pid, OpenProcess denied
// (PID recycled / insufficient rights), or the GetProcessMemoryInfo
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

	var counters processMemoryCounters
	counters.CB = uint32(unsafe.Sizeof(counters))
	r1, _, _ := procGetProcessMemoryInfo.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&counters)),
		uintptr(counters.CB),
	)
	if r1 == 0 {
		// GetProcessMemoryInfo returned FALSE — syscall failed.
		return 0, false
	}
	return uint64(counters.WorkingSetSize), true
}
