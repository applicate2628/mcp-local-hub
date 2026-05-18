//go:build windows

package process

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// IsPidAlive reports whether pid currently refers to a live process.
func IsPidAlive(pid int) bool {
	state, err := QueryPIDState(pid)
	return err == nil && state == PIDStateAlive
}

func QueryPIDState(pid int) (PIDState, error) {
	if pid <= 0 {
		return PIDStateDead, nil
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return PIDStateDead, nil
		}
		return PIDStateUnknown, fmt.Errorf("open PID %d for liveness: %w", pid, err)
	}
	defer windows.CloseHandle(h)

	ev, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return PIDStateUnknown, fmt.Errorf("wait PID %d for liveness: %w", pid, err)
	}
	switch ev {
	case windows.WAIT_OBJECT_0:
		return PIDStateDead, nil
	case uint32(windows.WAIT_TIMEOUT):
		return PIDStateAlive, nil
	default:
		return PIDStateUnknown, fmt.Errorf("wait PID %d for liveness returned unexpected code %d", pid, ev)
	}
}
