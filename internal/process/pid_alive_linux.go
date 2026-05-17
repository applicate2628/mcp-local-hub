//go:build linux

package process

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// IsPidAlive reports whether pid currently refers to a non-zombie process.
func IsPidAlive(pid int) bool {
	state, err := QueryPIDState(pid)
	return err == nil && state == PIDStateAlive
}

func QueryPIDState(pid int) (PIDState, error) {
	if pid <= 0 {
		return PIDStateDead, nil
	}
	err := syscall.Kill(pid, 0)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return PIDStateDead, nil
		}
		if !errors.Is(err, syscall.EPERM) {
			return PIDStateUnknown, fmt.Errorf("kill(%d, 0): %w", pid, err)
		}
	}

	data, statErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return PIDStateDead, nil
		}
		return PIDStateUnknown, fmt.Errorf("read /proc/%d/stat: %w", pid, statErr)
	}
	state, ok := procStatState(data)
	if !ok {
		return PIDStateUnknown, fmt.Errorf("parse /proc/%d/stat state", pid)
	}
	if state == 'Z' || state == 'X' {
		return PIDStateDead, nil
	}
	return PIDStateAlive, nil
}

func procStatState(data []byte) (byte, bool) {
	lastParen := bytes.LastIndexByte(data, ')')
	if lastParen < 0 {
		return 0, false
	}
	fields := bytes.Fields(data[lastParen+1:])
	if len(fields) == 0 || len(fields[0]) != 1 {
		return 0, false
	}
	return fields[0][0], true
}
