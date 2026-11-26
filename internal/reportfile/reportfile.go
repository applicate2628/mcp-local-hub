// Package reportfile reads bounded report prefixes while checking the entire stream.
package reportfile

import (
	"bytes"
	"io"
	"os"
)

// ReadPrefix reads a report through EOF and returns up to maxBytes+1 bytes.
// The extra byte lets the caller decide whether to append its truncation marker.
// Any open or read error, including in the discarded tail, returns no report.
func ReadPrefix(path string, maxBytes int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Size is an allocation hint only. The report can grow or shrink while read.
	var size int64
	if info, err := file.Stat(); err == nil {
		size = info.Size()
	}
	return readPrefix(file, size, maxBytes)
}

func readPrefix(reader io.Reader, size int64, maxBytes int) ([]byte, error) {
	limit := int64(maxBytes) + 1
	var prefix bytes.Buffer
	prefix.Grow(int(min(size, limit)) + bytes.MinRead)
	if _, err := prefix.ReadFrom(io.LimitReader(reader, limit)); err != nil {
		return nil, err
	}
	if prefix.Len() > maxBytes {
		if _, err := io.Copy(io.Discard, reader); err != nil {
			return nil, err
		}
	}
	return prefix.Bytes(), nil
}
