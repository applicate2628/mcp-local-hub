package boundedio

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"testing"
)

type testFS struct {
	file       io.ReadCloser
	dir        DirReader
	openErr    error
	openDirErr error
}

func (f testFS) Open(string) (io.ReadCloser, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return f.file, nil
}

func (f testFS) OpenDir(string) (DirReader, error) {
	if f.openDirErr != nil {
		return nil, f.openDirErr
	}
	return f.dir, nil
}

type recordingFile struct {
	data        []byte
	requests    []int
	returned    []int
	closeCount  int
	readErr     error
	closeErr    error
	cancelAfter context.CancelFunc
}

func (f *recordingFile) Read(buffer []byte) (int, error) {
	f.requests = append(f.requests, len(buffer))
	if f.readErr != nil {
		err := f.readErr
		f.readErr = nil
		f.returned = append(f.returned, 0)
		return 0, err
	}
	if len(f.data) == 0 {
		f.returned = append(f.returned, 0)
		return 0, io.EOF
	}
	n := min(len(buffer), len(f.data))
	copy(buffer, f.data[:n])
	f.data = f.data[n:]
	f.returned = append(f.returned, n)
	if f.cancelAfter != nil {
		f.cancelAfter()
		f.cancelAfter = nil
	}
	return n, nil
}

func (f *recordingFile) Close() error {
	f.closeCount++
	return f.closeErr
}

type testDirEntry string

func (entry testDirEntry) Name() string         { return string(entry) }
func (testDirEntry) IsDir() bool                { return false }
func (testDirEntry) Type() os.FileMode          { return 0 }
func (testDirEntry) Info() (os.FileInfo, error) { return nil, nil }

type recordingDir struct {
	entries     []os.DirEntry
	requests    []int
	returned    []int
	closeCount  int
	readErr     error
	closeErr    error
	cancelAfter context.CancelFunc
}

func (d *recordingDir) ReadDir(n int) ([]os.DirEntry, error) {
	d.requests = append(d.requests, n)
	if d.readErr != nil {
		err := d.readErr
		d.readErr = nil
		d.returned = append(d.returned, 0)
		return nil, err
	}
	if len(d.entries) == 0 {
		d.returned = append(d.returned, 0)
		return nil, io.EOF
	}
	count := min(n, len(d.entries))
	page := append([]os.DirEntry(nil), d.entries[:count]...)
	d.entries = d.entries[count:]
	d.returned = append(d.returned, count)
	if d.cancelAfter != nil {
		d.cancelAfter()
		d.cancelAfter = nil
	}
	return page, nil
}

func (d *recordingDir) Close() error {
	d.closeCount++
	return d.closeErr
}

func TestBoundedFileIngressRequests(t *testing.T) {
	reader := &recordingFile{data: []byte("abcdef")}
	result, err := ReadFile(context.Background(), testFS{file: reader}, "file", 5, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Limited || string(result.Data) != "abcde" {
		t.Fatalf("result=%+v, want five-byte limited prefix", result)
	}
	assertTerminalRequests(t, reader.requests, reader.returned, 6)
	if !slices.Equal(reader.requests, []int{4, 2}) {
		t.Fatalf("requests=%v, want [4 2]", reader.requests)
	}
	if reader.closeCount != 1 {
		t.Fatalf("close count=%d, want 1", reader.closeCount)
	}
}

func TestBoundedDirectoryIngressRequests(t *testing.T) {
	reader := &recordingDir{entries: dirEntries("d", "c", "b", "a")}
	result, err := ReadDirComplete(context.Background(), testFS{dir: reader}, "dir", 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Limited || result.TotalKnown || result.Entries != nil {
		t.Fatalf("result=%+v, want whole-directory omission with unknown total", result)
	}
	assertTerminalRequests(t, reader.requests, reader.returned, 4)
	if !slices.Equal(reader.requests, []int{2, 2}) {
		t.Fatalf("requests=%v, want [2 2]", reader.requests)
	}
	if reader.closeCount != 1 {
		t.Fatalf("close count=%d, want 1", reader.closeCount)
	}
}

func TestBoundedDirectoryAdmissionNoArbitraryPrefix(t *testing.T) {
	for _, order := range [][]string{
		{"d", "c", "b", "a"},
		{"a", "b", "c", "d"},
	} {
		reader := &recordingDir{entries: dirEntries(order...)}
		result, err := ReadDirComplete(context.Background(), testFS{dir: reader}, "dir", 3, 2)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Limited || len(result.Entries) != 0 {
			t.Fatalf("order=%v result=%+v, overflowing directory exposed a prefix", order, result)
		}
	}
}

func TestBoundedIngressCancellationCheckpoints(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &recordingFile{data: []byte("abcdef"), cancelAfter: cancel}
		_, err := ReadFile(ctx, testFS{file: reader}, "file", 5, 2)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v, want context cancellation", err)
		}
		if len(reader.requests) != 1 || reader.closeCount != 1 {
			t.Fatalf("requests=%v close count=%d, want one read and one close", reader.requests, reader.closeCount)
		}
	})

	t.Run("directory", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &recordingDir{entries: dirEntries("a", "b", "c"), cancelAfter: cancel}
		_, err := ReadDirComplete(ctx, testFS{dir: reader}, "dir", 3, 2)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v, want context cancellation", err)
		}
		if len(reader.requests) != 1 || reader.closeCount != 1 {
			t.Fatalf("requests=%v close count=%d, want one read and one close", reader.requests, reader.closeCount)
		}
	})
}

func TestBoundedIngressClosesEveryReturnPath(t *testing.T) {
	readFailure := errors.New("read failure")
	for _, test := range []struct {
		name string
		file *recordingFile
		max  int64
	}{
		{name: "success", file: &recordingFile{data: []byte("abc")}, max: 5},
		{name: "overflow", file: &recordingFile{data: []byte("abcdef")}, max: 5},
		{name: "read error", file: &recordingFile{readErr: readFailure}, max: 5},
	} {
		t.Run("file "+test.name, func(t *testing.T) {
			_, _ = ReadFile(context.Background(), testFS{file: test.file}, "file", test.max, 2)
			if test.file.closeCount != 1 {
				t.Fatalf("close count=%d, want 1", test.file.closeCount)
			}
		})
	}

	for _, test := range []struct {
		name string
		dir  *recordingDir
		max  int
	}{
		{name: "success", dir: &recordingDir{entries: dirEntries("a")}, max: 2},
		{name: "overflow", dir: &recordingDir{entries: dirEntries("a", "b", "c")}, max: 2},
		{name: "read error", dir: &recordingDir{readErr: readFailure}, max: 2},
	} {
		t.Run("directory "+test.name, func(t *testing.T) {
			_, _ = ReadDirComplete(context.Background(), testFS{dir: test.dir}, "dir", test.max, 2)
			if test.dir.closeCount != 1 {
				t.Fatalf("close count=%d, want 1", test.dir.closeCount)
			}
		})
	}
}

func TestBoundedIngressJoinsOperationAndCloseErrors(t *testing.T) {
	readFailure := errors.New("read failure")
	closeFailure := errors.New("close failure")
	reader := &recordingFile{readErr: readFailure, closeErr: closeFailure}
	result, err := ReadFile(context.Background(), testFS{file: reader}, "file", 5, 2)
	if !errors.Is(err, readFailure) || !errors.Is(err, closeFailure) {
		t.Fatalf("error=%v, want joined read and close failures", err)
	}
	if result.Data != nil || result.Limited {
		t.Fatalf("result=%+v, close failure must suppress the result", result)
	}
	if reader.closeCount != 1 {
		t.Fatalf("close count=%d, want 1", reader.closeCount)
	}
}

func assertTerminalRequests(t *testing.T, requests, returned []int, sentinelLimit int) {
	t.Helper()
	if len(requests) != len(returned) {
		t.Fatalf("requests=%v returned=%v, lengths differ", requests, returned)
	}
	admitted := 0
	requested := 0
	for index, request := range requests {
		remaining := sentinelLimit - admitted
		if request > remaining {
			t.Fatalf("request[%d]=%d exceeds remaining sentinel budget %d", index, request, remaining)
		}
		requested += request
		admitted += returned[index]
		if admitted > sentinelLimit {
			t.Fatalf("admitted=%d exceeds sentinel limit %d", admitted, sentinelLimit)
		}
	}
	if requested > sentinelLimit {
		t.Fatalf("cumulative requested=%d exceeds sentinel limit %d", requested, sentinelLimit)
	}
}

func dirEntries(names ...string) []os.DirEntry {
	entries := make([]os.DirEntry, len(names))
	for index, name := range names {
		entries[index] = testDirEntry(name)
	}
	return entries
}
