//go:build !windows && !linux

package api

import "golang.org/x/sys/unix"

func chmodStateFileAt(parentFd int, base string, mode uint32) error {
	return unix.Fchmodat(parentFd, base, mode, unix.AT_SYMLINK_NOFOLLOW)
}
