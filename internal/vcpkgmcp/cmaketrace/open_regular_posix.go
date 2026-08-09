//go:build !windows

package cmaketrace

import (
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

func openRegularTraceFile(path string) (io.ReadCloser, fs.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}
