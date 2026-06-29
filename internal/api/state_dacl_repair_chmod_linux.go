//go:build linux

package api

import "golang.org/x/sys/unix"

func chmodStateFileAt(parentFd int, base string, mode uint32) error {
	fd, err := unix.Openat(parentFd, base, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if err := validatePosixStateFileRepairInode(base, &st); err != nil {
		return err
	}
	return unix.Fchmodat(fd, "", mode, unix.AT_EMPTY_PATH)
}
