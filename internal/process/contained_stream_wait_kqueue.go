//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package process

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// observePlatformContainedExit uses a kernel process filter to observe exit
// without wait(2). The direct child therefore remains unreaped and pins its
// PID/PGID until terminateBy has signaled the group.
func observePlatformContainedExit(pid int) (resultErr error) {
	kqueue, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(kqueue)) }()

	change := unix.Kevent_t{}
	unix.SetKevent(&change, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ENABLE|unix.EV_ONESHOT)
	change.Fflags = unix.NOTE_EXIT
	if _, err := unix.Kevent(kqueue, []unix.Kevent_t{change}, nil, nil); err != nil {
		return err
	}
	events := make([]unix.Kevent_t, 1)
	for {
		count, err := unix.Kevent(kqueue, nil, events, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if count != 1 {
			continue
		}
		if events[0].Flags&unix.EV_ERROR != 0 {
			return syscall.Errno(events[0].Data)
		}
		return nil
	}
}
