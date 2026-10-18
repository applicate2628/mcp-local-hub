//go:build unix

package clients

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openRemovalSourceFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openRemovalDirectoryIdentity(path string) (string, uintptr, func(), error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", 0, nil, err
	}
	closeFD := func() { _ = unix.Close(fd) }
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		closeFD()
		return "", 0, nil, err
	}
	return fmt.Sprintf("%x:%x", info.Dev, info.Ino), uintptr(fd), closeFD, nil
}
