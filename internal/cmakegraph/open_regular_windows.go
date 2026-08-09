//go:build windows

package cmakegraph

import (
	"fmt"
	"os"
)

func openRootRegular(root *os.Root, path string) (*os.File, error) {
	file, err := root.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil { return nil, err }
		return nil, fmt.Errorf("cmakegraph: %s is not a regular file", path)
	}
	return file, nil
}
