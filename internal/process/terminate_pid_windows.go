//go:build windows

package process

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func TerminatePID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("process: invalid PID %d", pid)
	}
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("process: open PID %d for terminate: %w", pid, err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.TerminateProcess(handle, 1); err != nil {
		return fmt.Errorf("process: terminate PID %d: %w", pid, err)
	}
	return nil
}
