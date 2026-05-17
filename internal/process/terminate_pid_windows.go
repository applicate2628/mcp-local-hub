//go:build windows

package process

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

func TerminatePID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("process: invalid PID %d", pid)
	}
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return fmt.Errorf("process: PID %d already exited before terminate open: %w", pid, ErrProcessAlreadyExited)
		}
		return fmt.Errorf("process: open PID %d for terminate: %w", pid, err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.TerminateProcess(handle, 1); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			if ev, waitErr := windows.WaitForSingleObject(handle, 0); waitErr == nil && ev == windows.WAIT_OBJECT_0 {
				return fmt.Errorf("process: PID %d already exited before terminate: %w", pid, ErrProcessAlreadyExited)
			}
		}
		return fmt.Errorf("process: terminate PID %d: %w", pid, err)
	}
	return nil
}
