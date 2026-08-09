//go:build !windows

package boundedio

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// OpenRegular uses nonblocking, close-on-exec, no-follow admission so opening
// a replaced FIFO cannot block and a symlink cannot redirect the operation.
// Validation and later reads use the same descriptor.
func OpenRegular(path string) (RegularFile, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("boundedio: create file handle for %q", path)
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if statErr != nil {
			return nil, statErr
		}
		return nil, fmt.Errorf("boundedio: refuse non-regular file %q (%s)", path, info.Mode().Type())
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
