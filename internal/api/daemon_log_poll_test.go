package api

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPollDaemonLogTransitions(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	dir := DefaultLogDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "demo-worker.log")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(offset int64, prime bool, want DaemonLogPoll) {
		t.Helper()
		got, err := PollDaemonLog("demo", "worker", offset, prime)
		if err != nil || got != want {
			t.Fatalf("offset=%d prime=%t: got %+v, %v; want %+v", offset, prime, got, err, want)
		}
	}

	check(0, true, DaemonLogPoll{})
	check(11, false, DaemonLogPoll{Offset: 11})
	placeholder := LogPlaceholderFor("demo", "worker")
	write(placeholder)
	check(0, true, DaemonLogPoll{})
	check(int64(len(placeholder))+1, false, DaemonLogPoll{Offset: int64(len(placeholder)) + 1})

	near := strings.TrimSuffix(placeholder, "\n") + "x"
	write(near)
	check(0, true, DaemonLogPoll{Offset: int64(len(near))})
	check(0, false, DaemonLogPoll{Offset: int64(len(near)), Suffix: near})
	write(placeholder + "extra\n")
	check(0, false, DaemonLogPoll{Offset: int64(len(placeholder) + 6), Suffix: placeholder + "extra\n"})

	write("former")
	check(0, true, DaemonLogPoll{Offset: 6})
	write("change")
	check(6, false, DaemonLogPoll{Offset: 6})
	write("change\nnew\n")
	check(6, false, DaemonLogPoll{Offset: 11, Suffix: "\nnew\n"})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	check(11, false, DaemonLogPoll{Offset: 11})
	write("short")
	check(11, false, DaemonLogPoll{Offset: 5})
	write("short add\n")
	check(5, false, DaemonLogPoll{Offset: 10, Suffix: " add\n"})
}

func TestPollDaemonLogSampledEOF(t *testing.T) {
	readErr := errors.New("injected read error")
	for _, tc := range []struct {
		name       string
		read       string
		readErr    error
		nextSize   int64
		want       DaemonLogPoll
		wantRestat bool
	}{
		{name: "short positive moves only by bytes read", read: "xyz", readErr: io.EOF, want: DaemonLogPoll{Offset: 9, Suffix: "xyz"}},
		{name: "zero and shrink", readErr: io.EOF, nextSize: 3, wantRestat: true, want: DaemonLogPoll{Offset: 3}},
		{name: "zero and equal", readErr: io.EOF, nextSize: 6, wantRestat: true, want: DaemonLogPoll{Offset: 6}},
		{name: "zero and concurrent regrowth", readErr: io.EOF, nextSize: 80, wantRestat: true, want: DaemonLogPoll{Offset: 6}},
		{name: "non-EOF error", read: "x", readErr: readErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := &logEOFFile{t: t, read: tc.read, readErr: tc.readErr, nextSize: tc.nextSize}
			got, err := pollOpenedDaemonLog(file, "sentinel", 6, false)
			wantErr := error(nil)
			if tc.readErr == readErr {
				wantErr = readErr
			}
			if file.restatted != tc.wantRestat || got != tc.want || err != wantErr {
				t.Fatalf("poll=%+v err=%v restatted=%t; want %+v, %v, %t", got, err, file.restatted, tc.want, wantErr, tc.wantRestat)
			}
		})
	}
}

type logEOFFile struct {
	t          *testing.T
	read       string
	readErr    error
	nextSize   int64
	readCalled bool
	restatted  bool
}

func (*logEOFFile) Name() string             { return "synthetic.log" }
func (*logEOFFile) Read([]byte) (int, error) { panic("unexpected sequential read") }
func (f *logEOFFile) ReadAt(p []byte, offset int64) (int, error) {
	if f.readCalled || len(p) != 6 || offset != 6 {
		f.t.Fatalf("unexpected read length=%d offset=%d repeat=%t", len(p), offset, f.readCalled)
	}
	f.readCalled = true
	return copy(p, f.read), f.readErr
}
func (f *logEOFFile) Stat() (os.FileInfo, error) {
	if f.restatted {
		f.t.Fatal("repeated restat")
	}
	if f.readCalled {
		f.restatted = true
		return logEOFInfo{size: f.nextSize}, nil
	}
	return logEOFInfo{size: 12}, nil
}

type logEOFInfo struct {
	os.FileInfo
	size int64
}

func (i logEOFInfo) Size() int64     { return i.size }
func (logEOFInfo) Mode() os.FileMode { return 0600 }
