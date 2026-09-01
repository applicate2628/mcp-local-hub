//go:build windows

package process

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func currentProcessKillOnCloseJob() (bool, error) {
	var inJob uint32
	r1, _, callErr := procIsProcessInJob.Call(uintptr(windows.CurrentProcess()), 0, uintptr(unsafe.Pointer(&inJob)))
	if r1 == 0 {
		return false, fmt.Errorf("IsProcessInJob(current): %w", callErr)
	}
	if inJob == 0 {
		return false, nil
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return false, fmt.Errorf("QueryInformationJobObject(current): %w", err)
	}
	return info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE != 0, nil
}
