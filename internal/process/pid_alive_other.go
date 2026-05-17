//go:build !windows

package process

import (
	"errors"
	"syscall"
)

// IsPidAlive reports whether pid currently refers to a live process.
func IsPidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
