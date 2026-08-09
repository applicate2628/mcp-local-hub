//go:build !windows

package cmakegraph

import (
	"fmt"
	"os"
	"syscall"
)

func openRootRegular(root *os.Root, path string) (*os.File, error) {
	file, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil { return nil, err }
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil { return nil, err }
		return nil, fmt.Errorf("cmakegraph: %s is not a regular file", path)
	}
	if err := syscall.SetNonblock(int(file.Fd()), false); err != nil {
		_ = file.Close(); return nil, err
	}
	return file, nil
}
