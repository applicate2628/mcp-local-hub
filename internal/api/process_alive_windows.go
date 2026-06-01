//go:build windows

package api

import "golang.org/x/sys/windows"

func processAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER {
			return false, nil
		}
		if err == windows.ERROR_ACCESS_DENIED {
			return true, nil
		}
		return false, err
	}
	_ = windows.CloseHandle(h)
	return true, nil
}
