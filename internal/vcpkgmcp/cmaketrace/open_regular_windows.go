//go:build windows

package cmaketrace

import (
	"io"
	"io/fs"
	"os"
)

func openRegularTraceFile(path string) (io.ReadCloser, fs.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}
