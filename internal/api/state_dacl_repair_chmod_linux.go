//go:build linux

package api

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

var fchmodatEmptyPathForRepair = func(fd int, mode uint32) error {
	return unix.Fchmodat(fd, "", mode, unix.AT_EMPTY_PATH)
}

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
	if err := fchmodatEmptyPathForRepair(fd, mode); err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) {
			return chmodStateFileAtLinuxFallback(fd, mode, &st)
		}
		return err
	}
	return nil
}

func chmodStateFileAtLinuxFallback(anchoredFd int, mode uint32, before *unix.Stat_t) error {
	procPath := fmt.Sprintf("/proc/self/fd/%d", anchoredFd)
	if err := unix.Chmod(procPath, mode); err != nil {
		return err
	}
	var after unix.Stat_t
	if err := unix.Fstat(anchoredFd, &after); err != nil {
		return err
	}
	if before.Dev != after.Dev || before.Ino != after.Ino {
		return fmt.Errorf("%w: repair target changed during chmod fallback", ErrIrregularFile)
	}
	if uint32(after.Mode&0o777) != mode {
		return fmt.Errorf("procfd chmod fallback left mode %#o, want %#o", uint32(after.Mode&0o777), mode)
	}
	return nil
}
