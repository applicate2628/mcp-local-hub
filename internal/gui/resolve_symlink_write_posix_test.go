// internal/gui/resolve_symlink_write_posix_test.go
//go:build !windows

package gui

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestResolveTargetOpenFlags_NonBlockingFIFOOpen is the Finding-2 mechanism
// guard. The production read path (readResolvedSymlinkTargetBytes) does
// os.Stat -> open -> fstat-IsRegular. If a local process swaps the resolved
// target for a FIFO in the stat->open WINDOW, the open must NOT block waiting
// for a writer before the post-open fstat re-check can refuse it. The fix is
// resolveTargetOpenFlags() returning O_RDONLY|O_NONBLOCK on POSIX.
//
// The stat->open window is a microscopic in-process race that cannot be hit
// deterministically by timing alone, so this test exercises the EXACT open
// mechanism the fix changes: it opens a steady FIFO directly with the
// production flags and asserts the open returns immediately, then fstat-refuses
// it as non-regular — the same two-step the production path runs once the
// window is lost. With the buggy plain-O_RDONLY flags this open BLOCKS forever
// (no writer); the timeout converts that hang into a test failure rather than a
// hung suite.
func TestResolveTargetOpenFlags_NonBlockingFIFOOpen(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "swapped.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported on this FS: %v", err)
	}

	type openResult struct {
		f   *os.File
		err error
	}
	done := make(chan openResult, 1)
	go func() {
		// EXACT production open call (resolve_symlink_write.go).
		f, err := os.OpenFile(fifo, resolveTargetOpenFlags(), 0)
		done <- openResult{f, err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			// A nonblocking open of a reader-side FIFO with no writer can
			// legitimately succeed on Linux (O_RDONLY|O_NONBLOCK returns an fd);
			// any error here would also be a non-hang refusal. Either way it did
			// not block. If it opened, the production path's fstat re-check is
			// what refuses it — assert that below when we have a handle.
			return
		}
		defer res.f.Close()
		// Production refusal step: fstat the OPEN handle, must be non-regular.
		fi, serr := res.f.Stat()
		if serr != nil {
			t.Fatalf("fstat opened FIFO handle: %v", serr)
		}
		if fi.Mode().IsRegular() {
			t.Fatalf("opened FIFO fstat reports IsRegular — the post-open gate would wrongly accept it")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("open of a FIFO with resolveTargetOpenFlags() BLOCKED — O_NONBLOCK missing; production read path would hang the handler goroutine")
	}
}

// TestReadResolvedSymlinkTargetBytes_FIFO_RefusesWithoutHang drives the FULL
// production read helper against a steady FIFO target. The leading os.Stat
// refuses it as non-regular here (the window was not lost), but the call is run
// under a timeout so that ANY regression that lets a non-regular target reach a
// blocking open — including a future refactor that drops the leading stat —
// fails the test instead of hanging. It is the integration twin of the
// mechanism test above.
func TestReadResolvedSymlinkTargetBytes_FIFO_RefusesWithoutHang(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "config.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported on this FS: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := readResolvedSymlinkTargetBytes(fifo)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("readResolvedSymlinkTargetBytes accepted a FIFO target — must refuse a non-regular file")
		}
		// Refused without hanging — the message names the non-regular refusal.
	case <-time.After(5 * time.Second):
		t.Fatal("readResolvedSymlinkTargetBytes HUNG on a FIFO target — a non-regular target reached a blocking open")
	}
}
