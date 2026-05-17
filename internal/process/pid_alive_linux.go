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
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}

	data, statErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if statErr != nil {
		return false
	}
	state, ok := procStatState(data)
	if !ok {
		return false
	}
	return state != 'Z' && state != 'X'
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
