//go:build !windows

package gdb

import "os"

func debuggerPipe(parentReads bool) (parentPipe, *os.File, error) {
	reader, writer, err := os.Pipe()
	if parentReads {
		return reader, writer, err
	}
	return writer, reader, err
}
