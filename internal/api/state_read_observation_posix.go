//go:build !windows

package api

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"syscall"
)

func stateReadNativeError(err error) (string, *uint64) {
	var native syscall.Errno
	if errors.As(err, &native) {
		code := uint64(native)
		return "go", &code
	}
	return "go", nil
}

func observedStateReadPOSIXIdentity(info unix.Stat_t) string {
	return stateFileReadObservationDigest([]byte(fmt.Sprintf("posix-file-object-v1|%d|%d", info.Dev, info.Ino)))
}
