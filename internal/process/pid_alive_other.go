//go:build !windows && !linux

package process

import (
	"errors"
	"syscall"
)

// IsPidAlive reports whether pid currently refers to a live process.
// On macOS/BSD this remains a kill(0)-only check for v0.5.0 preview
// scope; Linux excludes zombies via /proc in pid_alive_linux.go.
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
