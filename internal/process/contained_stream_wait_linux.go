//go:build linux

package process

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// wait observes direct-child exit without reaping it. The resulting zombie
// pins both PID and PGID until RunContainedStream has signaled the group,
// closing the recycled-numeric-identity race.
func observePlatformContainedExit(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return err
	}
}
