//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

type relayPrivateMemory struct {
	WorkingSet, PrivateWorkingSet, PrivateCommit uint64
}

// PROCESS_MEMORY_COUNTERS_EX2: minimum Windows 10/11 22H2 + Sep 2023 CU.
// This opt-in observer is a test capability, not a new Hub platform requirement.
// PrivateUsage is committed memory; PrivateWorkingSetSize is resident private
// memory. Neither equals summed total working sets, which count shared pages.
type relayMemoryCountersEX2 struct {
	CB, PageFaultCount                                                    uint32
	PeakWorkingSetSize, WorkingSetSize                                    uintptr
	QuotaPeakPagedPoolUsage, QuotaPagedPoolUsage                          uintptr
	QuotaPeakNonPagedPoolUsage, QuotaNonPagedPoolUsage                    uintptr
	PagefileUsage, PeakPagefileUsage, PrivateUsage, PrivateWorkingSetSize uintptr
	SharedCommitUsage                                                     uint64
}

var relayGetProcessMemoryInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

func observeRelayPrivateMemory(pid int) (sample relayPrivateMemory, resultErr error) {
	if pid <= 0 {
		return sample, fmt.Errorf("invalid owned PID")
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return sample, err
	}
	defer func() {
		if err := windows.CloseHandle(h); err != nil {
			sample = relayPrivateMemory{}
			resultErr = errors.Join(resultErr, err)
		}
	}()
	var counters relayMemoryCountersEX2
	counters.CB = uint32(unsafe.Sizeof(counters))
	// The handle stays open and the exact writable EX2 buffer stays live for the
	// synchronous call. No arbitrary address, VM_READ or process-control right.
	ok, _, callErr := relayGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&counters)), uintptr(counters.CB))
	if ok == 0 {
		return sample, fmt.Errorf("EX2 memory observation failed: %w", callErr)
	}
	if counters.PrivateWorkingSetSize == 0 || counters.PrivateWorkingSetSize > counters.WorkingSetSize || counters.PrivateUsage == 0 {
		return sample, fmt.Errorf("EX2 unsupported or invalid on this host: WS=%d privateWS=%d commit=%d", counters.WorkingSetSize, counters.PrivateWorkingSetSize, counters.PrivateUsage)
	}
	return relayPrivateMemory{uint64(counters.WorkingSetSize), uint64(counters.PrivateWorkingSetSize), uint64(counters.PrivateUsage)}, nil
}

func TestRelayPrivateMemoryProbeSelf(t *testing.T) {
	if os.Getenv("MCPHUB_RX_RELAY_PRIVATE_MEMORY") != "1" {
		t.Skip("opt-in Windows EX2 measurement")
	}
	if unsafe.Sizeof(uintptr(0)) == 8 && unsafe.Sizeof(relayMemoryCountersEX2{}) != 96 {
		t.Fatal("incorrect x64 EX2 structure layout")
	}
	sample, err := observeRelayPrivateMemory(os.Getpid())
	if err != nil {
		t.Fatalf("private memory observation unavailable on this host: %v", err)
	}
	if sample.PrivateWorkingSet == 0 || sample.PrivateWorkingSet > sample.WorkingSet || sample.PrivateCommit == 0 {
		t.Fatalf("invalid private memory observation: %+v", sample)
	}
	if _, err := observeRelayPrivateMemory(0); err == nil {
		t.Fatal("invalid PID accepted")
	}
}
