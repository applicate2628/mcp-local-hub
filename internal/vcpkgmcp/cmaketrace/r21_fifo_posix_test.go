//go:build !windows

package cmaketrace

import (
	"io"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestR21SpecialTraceOpenIsNonblockingOnPOSIX(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "trace.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	type outcome struct {
		reader io.ReadCloser
		info   fs.FileInfo
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		reader, info, err := openRegularTraceFile(fifo)
		done <- outcome{reader: reader, info: info, err: err}
	}()
	select {
	case got := <-done:
		if got.reader != nil {
			defer got.reader.Close()
		}
		if got.err != nil {
			t.Fatalf("open FIFO for admission: %v", got.err)
		}
		if got.info == nil || got.info.Mode().IsRegular() {
			t.Fatalf("opened FIFO info=%v, want non-regular", got.info)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("FIFO admission blocked waiting for a writer")
	}
}
