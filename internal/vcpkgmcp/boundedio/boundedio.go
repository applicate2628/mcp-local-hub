// Package boundedio owns paged filesystem admission for vcpkg MCP tools.
//
// The limit-plus-one byte or entry is a sentinel used only to prove overflow.
// Callers never receive more than the configured public limit, and directory
// overflow never exposes an operating-system-order prefix as evidence.
package boundedio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

// LineResult is one line admitted by LineReader. Limited means the line was
// longer than the configured semantic limit; Data must not be parsed then.
// EOF is returned as the error from ReadLine so callers retain normal stream
// termination semantics.
type LineResult struct {
	Data    []byte
	Limited bool
}

// LineReader is a persistent, delimiter-aware reader bounded by one semantic
// line limit plus one sentinel byte. Its buffer is retained across calls so
// bytes prefetched while finding one delimiter remain available for the next
// line.
type LineReader struct {
	reader   io.Reader
	buffer   []byte
	maxBytes int
	eof      bool
}

// NewLineReader creates a bounded line reader. maxBytes includes the newline
// delimiter when one is present.
func NewLineReader(r io.Reader, maxBytes int) (*LineReader, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("boundedio: max line bytes must be non-negative")
	}
	if maxBytes == int(^uint(0)>>1) {
		return nil, fmt.Errorf("boundedio: max line bytes leaves no sentinel capacity")
	}
	return &LineReader{reader: r, buffer: make([]byte, 0, maxBytes+1), maxBytes: maxBytes}, nil
}

// ReadLine returns one complete line, an EOF-complete final line, or Limited.
// It never accumulates fragments in a builder: every read is made directly
// into the remaining space of its exact maxBytes-plus-one buffer.
func (r *LineReader) ReadLine() (LineResult, error) {
	for {
		if delimiter := bytes.IndexByte(r.buffer, '\n'); delimiter >= 0 {
			line := r.buffer[:delimiter+1]
			if len(line) > r.maxBytes {
				return LineResult{Limited: true}, nil
			}
			out := append([]byte(nil), line...)
			r.buffer = append(r.buffer[:0], r.buffer[delimiter+1:]...)
			return LineResult{Data: out}, nil
		}
		if len(r.buffer) > r.maxBytes {
			return LineResult{Limited: true}, nil
		}
		if r.eof {
			if len(r.buffer) == 0 {
				return LineResult{}, io.EOF
			}
			out := append([]byte(nil), r.buffer...)
			r.buffer = r.buffer[:0]
			return LineResult{Data: out}, io.EOF
		}

		remaining := cap(r.buffer) - len(r.buffer)
		if remaining == 0 {
			return LineResult{Limited: true}, nil
		}
		scratch := r.buffer[len(r.buffer) : len(r.buffer)+remaining]
		n, err := r.reader.Read(scratch)
		if n < 0 || n > remaining {
			return LineResult{}, fmt.Errorf("boundedio: line reader returned %d bytes for a %d-byte request", n, remaining)
		}
		r.buffer = r.buffer[:len(r.buffer)+n]
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return LineResult{}, err
			}
			r.eof = true
		}
		if n == 0 && err == nil {
			return LineResult{}, io.ErrNoProgress
		}
	}
}

// FS is the filesystem surface required by bounded admission.
type FS interface {
	OpenRegular(path string) (RegularFile, error)
	Stat(path string) (os.FileInfo, error)
	Open(path string) (io.ReadCloser, error)
	OpenDir(path string) (DirReader, error)
}

// RegularFile is a file handle whose type is validated from that same open
// handle, eliminating path-based Stat/Open replacement races.
type RegularFile interface {
	io.ReadCloser
	Stat() (os.FileInfo, error)
}

// DirReader is the paged directory surface implemented by *os.File.
type DirReader interface {
	ReadDir(n int) ([]os.DirEntry, error)
	Close() error
}

// FileResult is a completely admitted file or a bounded prefix carrying an
// explicit overflow marker. A limited prefix must not be parsed as a complete
// file.
type FileResult struct {
	Data    []byte
	Limited bool
}

// DirResult contains entries only when the complete directory reached EOF
// within its admission budget. An overflowing directory is omitted as a whole.
type DirResult struct {
	Entries    []os.DirEntry
	Limited    bool
	TotalKnown bool
}

// ReadFile admits at most maxBytes plus one sentinel byte. Before every read,
// the requested size is min(pageBytes, maxBytes+1-admitted); after the sentinel
// is admitted no further read is issued.
func ReadFile(ctx context.Context, fsys FS, path string, maxBytes, pageBytes int64) (result FileResult, err error) {
	if err := ctx.Err(); err != nil {
		return FileResult{}, err
	}
	if err := validateFileLimits(maxBytes, pageBytes); err != nil {
		return FileResult{}, err
	}

	reader, err := fsys.OpenRegular(path)
	if err != nil {
		return FileResult{}, err
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			result = FileResult{}
			err = errors.Join(err, closeErr)
		}
	}()
	info, err := reader.Stat()
	if err != nil {
		return FileResult{}, err
	}
	if !info.Mode().IsRegular() {
		return FileResult{}, fmt.Errorf("boundedio: refuse non-regular file %q (%s)", path, info.Mode().Type())
	}

	if err := ctx.Err(); err != nil {
		return FileResult{}, err
	}

	sentinelLimit := maxBytes + 1
	initialCapacity := min(pageBytes, sentinelLimit)
	data := make([]byte, 0, int(initialCapacity))
	for int64(len(data)) < sentinelLimit {
		if err := ctx.Err(); err != nil {
			return FileResult{}, err
		}

		remaining := sentinelLimit - int64(len(data))
		requestSize := min(pageBytes, remaining)
		buffer := make([]byte, int(requestSize))
		n, readErr := reader.Read(buffer)
		if err := ctx.Err(); err != nil {
			return FileResult{}, err
		}
		if n < 0 || n > len(buffer) {
			return FileResult{}, fmt.Errorf("boundedio: reader returned %d bytes for a %d-byte request", n, len(buffer))
		}
		if n > 0 {
			data = append(data, buffer[:n]...)
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return FileResult{}, readErr
			}
			break
		}
		if n == 0 {
			return FileResult{}, io.ErrNoProgress
		}
	}

	if int64(len(data)) > maxBytes {
		return FileResult{Data: data[:maxBytes], Limited: true}, nil
	}
	return FileResult{Data: data}, nil
}

// ReadDirComplete admits at most maxEntries plus one sentinel entry. Before
// every read, the requested size is min(pageEntries, maxEntries+1-admitted);
// after the sentinel is admitted no further read is issued.
//
// Only a complete directory is sorted and returned. On overflow, Entries is
// nil so callers cannot mistake an arbitrary enumeration-order prefix for
// deterministic evidence.
func ReadDirComplete(ctx context.Context, fsys FS, path string, maxEntries, pageEntries int) (result DirResult, err error) {
	if err := ctx.Err(); err != nil {
		return DirResult{}, err
	}
	if maxEntries < 0 {
		return DirResult{}, fmt.Errorf("boundedio: max entries must be non-negative")
	}
	if pageEntries <= 0 {
		return DirResult{}, fmt.Errorf("boundedio: page entries must be positive")
	}
	if maxEntries == int(^uint(0)>>1) {
		return DirResult{}, fmt.Errorf("boundedio: max entries leaves no sentinel capacity")
	}

	reader, err := fsys.OpenDir(path)
	if err != nil {
		return DirResult{}, err
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			result = DirResult{}
			err = errors.Join(err, closeErr)
		}
	}()

	if err := ctx.Err(); err != nil {
		return DirResult{}, err
	}

	sentinelLimit := maxEntries + 1
	entries := make([]os.DirEntry, 0, min(pageEntries, sentinelLimit))
	for len(entries) < sentinelLimit {
		if err := ctx.Err(); err != nil {
			return DirResult{}, err
		}

		remaining := sentinelLimit - len(entries)
		requestSize := min(pageEntries, remaining)
		page, readErr := reader.ReadDir(requestSize)
		if err := ctx.Err(); err != nil {
			return DirResult{}, err
		}
		if len(page) > requestSize {
			return DirResult{}, fmt.Errorf("boundedio: directory reader returned %d entries for a %d-entry request", len(page), requestSize)
		}
		entries = append(entries, page...)
		if len(entries) == sentinelLimit {
			return DirResult{Limited: true, TotalKnown: false}, nil
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return DirResult{}, readErr
			}
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].Name() < entries[j].Name()
			})
			return DirResult{Entries: entries, TotalKnown: true}, nil
		}
		if len(page) == 0 {
			return DirResult{}, io.ErrNoProgress
		}
	}

	panic("boundedio: unreachable directory admission state")
}

func validateFileLimits(maxBytes, pageBytes int64) error {
	if maxBytes < 0 {
		return fmt.Errorf("boundedio: max bytes must be non-negative")
	}
	if pageBytes <= 0 {
		return fmt.Errorf("boundedio: page bytes must be positive")
	}
	maxInt := int64(^uint(0) >> 1)
	if maxBytes >= maxInt {
		return fmt.Errorf("boundedio: max bytes leaves no sentinel capacity")
	}
	if pageBytes > maxInt {
		return fmt.Errorf("boundedio: page bytes exceed platform capacity")
	}
	return nil
}
