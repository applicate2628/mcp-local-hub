//go:build windows && amd64

package vcpkgserver

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This opt-in profile requires PROCESS_MEMORY_COUNTERS_EX2 support. It does
// not add a product platform requirement. Private commit is not resident RAM;
// total working set includes shared pages. The handle names only this process.
type cmakeSDKMemoryCountersEX2 struct {
	CB, PageFaultCount                                                    uint32
	PeakWorkingSetSize, WorkingSetSize                                    uintptr
	QuotaPeakPagedPoolUsage, QuotaPagedPoolUsage                          uintptr
	QuotaPeakNonPagedPoolUsage, QuotaNonPagedPoolUsage                    uintptr
	PagefileUsage, PeakPagefileUsage, PrivateUsage, PrivateWorkingSetSize uintptr
	SharedCommitUsage                                                     uint64
}

type cmakeSDKMemorySample struct {
	HeapAlloc         uint64 `json:"heap_alloc_bytes"`
	HeapInuse         uint64 `json:"heap_inuse_bytes"`
	TotalAlloc        uint64 `json:"total_alloc_bytes"`
	Mallocs           uint64 `json:"mallocs"`
	NumGC             uint32 `json:"gc_cycles"`
	Goroutines        int    `json:"goroutines"`
	WorkingSet        uint64 `json:"working_set_bytes"`
	PrivateWorkingSet uint64 `json:"private_working_set_bytes"`
	PrivateCommit     uint64 `json:"private_commit_bytes"`
}

type cmakeSDKMemoryProbe struct {
	handle windows.Handle
	query  *windows.LazyProc
}

func newCMakeSDKMemoryProbe() (*cmakeSDKMemoryProbe, error) {
	if unsafe.Sizeof(cmakeSDKMemoryCountersEX2{}) != 96 {
		return nil, fmt.Errorf("incorrect x64 EX2 layout")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(os.Getpid()))
	if err != nil {
		return nil, err
	}
	query := windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")
	if err := query.Find(); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	return &cmakeSDKMemoryProbe{handle, query}, nil
}

func (p *cmakeSDKMemoryProbe) sample() (cmakeSDKMemorySample, error) {
	var counters cmakeSDKMemoryCountersEX2
	counters.CB = uint32(unsafe.Sizeof(counters))
	ok, _, err := p.query.Call(uintptr(p.handle), uintptr(unsafe.Pointer(&counters)), uintptr(counters.CB))
	if ok == 0 {
		return cmakeSDKMemorySample{}, fmt.Errorf("EX2 memory observation: %w", err)
	}
	if counters.PrivateWorkingSetSize == 0 || counters.PrivateWorkingSetSize > counters.WorkingSetSize || counters.PrivateUsage == 0 {
		return cmakeSDKMemorySample{}, fmt.Errorf("EX2 unsupported or invalid on this host")
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return cmakeSDKMemorySample{memory.HeapAlloc, memory.HeapInuse, memory.TotalAlloc, memory.Mallocs,
		memory.NumGC, runtime.NumGoroutine(), uint64(counters.WorkingSetSize),
		uint64(counters.PrivateWorkingSetSize), uint64(counters.PrivateUsage)}, nil
}
