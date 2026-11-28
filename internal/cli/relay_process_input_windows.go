//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var relayCancelSynchronousIO = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")

// Only an explicitly granted Windows pipe input receives this owner. Regular
// files and consoles retain their borrowed, non-interrupting behavior.
func processRelayInputInterrupt(input *relayProcessInput) (func() error, error) {
	info, err := input.file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect inherited relay input: %w", err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		return nil, nil
	}
	synchronous, err := relayPipeIsSynchronous(input.file)
	if err != nil {
		return nil, fmt.Errorf("inspect inherited relay pipe mode: %w", err)
	}
	if !synchronous {
		return nil, nil // an overlapped handle needs a different cancellation owner
	}
	if err := relayCancelSynchronousIO.Find(); err != nil {
		return nil, fmt.Errorf("resolve synchronous relay input cancellation: %w", err)
	}
	owner := &relayWindowsPipeInput{
		reader: input.file, closeInput: input.file.Close,
		cancelRead: cancelRelaySynchronousRead,
	}
	input.reader = owner
	return owner.interrupt, nil
}

// FileModeInformation (16) returns FILE_MODE_INFORMATION, a single ULONG.
// Query through the original File's controlled handle before any relay read;
// File.Fd would unnecessarily alter a pollable handle's runtime association.
func relayPipeIsSynchronous(file *os.File) (bool, error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return false, err
	}
	var mode uint32
	var status windows.IO_STATUS_BLOCK
	var queryErr error
	err = raw.Control(func(handle uintptr) {
		queryErr = windows.NtQueryInformationFile(windows.Handle(handle), &status, (*byte)(unsafe.Pointer(&mode)), uint32(unsafe.Sizeof(mode)), 16)
	})
	if err = errors.Join(err, queryErr); err != nil {
		return false, err
	}
	return mode&(windows.FILE_SYNCHRONOUS_IO_ALERT|windows.FILE_SYNCHRONOUS_IO_NONALERT) != 0, nil
}

func cancelRelaySynchronousRead(thread windows.Handle) error {
	ok, _, err := relayCancelSynchronousIO.Call(uintptr(thread))
	if ok == 0 {
		return err
	}
	return nil
}

// Inherited stdin is synchronous on the supported Go toolchain. File.Close
// alone can cancel before ReadFile is submitted and then wait forever for that
// later read. Keep one read's thread alive until cancellation and completion
// have met; File.Close is only resource cleanup after the read has returned.
type relayWindowsPipeInput struct {
	reader     io.Reader
	closeInput func() error
	cancelRead func(windows.Handle) error

	readMu   sync.Mutex
	mu       sync.Mutex // active/thread lifetime and cancellation calls
	stopping bool
	active   *relayWindowsPipeRead
}

type relayWindowsPipeRead struct {
	thread          windows.Handle
	done            chan struct{}
	cancelRequested bool // guarded by the input owner's mu
}

func (r *relayWindowsPipeInput) Read(p []byte) (int, error) {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var thread windows.Handle
	if err := windows.DuplicateHandle(windows.CurrentProcess(), windows.CurrentThread(), windows.CurrentProcess(), &thread, windows.THREAD_TERMINATE, false, 0); err != nil {
		return 0, fmt.Errorf("retain relay input thread: %w", err)
	}
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return 0, errors.Join(context.Canceled, windows.CloseHandle(thread))
	}
	read := &relayWindowsPipeRead{thread: thread, done: make(chan struct{})}
	r.active = read
	r.mu.Unlock()

	n, err := r.reader.Read(p)

	r.mu.Lock()
	// A cancellation call holds mu too. Removing this record before unlocking
	// prevents a late cancel from targeting another operation on this thread.
	r.active = nil
	closeErr := windows.CloseHandle(thread)
	if closeErr != nil {
		closeErr = fmt.Errorf("release relay input thread: %w", closeErr)
	}
	// File.Read wraps its single syscall error in PathError. Do not collapse
	// an error tree merely because one branch contains the expected abort.
	nativeErr := err
	if pathErr, ok := err.(*os.PathError); ok {
		nativeErr = pathErr.Err
	}
	if read.cancelRequested && nativeErr == windows.ERROR_OPERATION_ABORTED {
		err = context.Canceled
	}
	close(read.done)
	r.mu.Unlock()
	return n, errors.Join(err, closeErr)
}

func (r *relayWindowsPipeInput) interrupt() error {
	r.mu.Lock()
	r.stopping = true // no later Read may submit native input
	read := r.active
	for read != nil && r.active == read {
		err := r.cancelRead(read.thread)
		if err == nil {
			read.cancelRequested = true
		}
		r.mu.Unlock()
		if err != windows.ERROR_NOT_FOUND {
			// Request success is not completion. A native refusal also does not
			// authorize abandoning the read; its owner must still release it.
			<-read.done
			return errors.Join(err, r.closeInput())
		}
		// No pending request can mean the registered read has not reached
		// ReadFile yet. Retry only while that exact read remains active.
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-read.done:
			timer.Stop()
		case <-timer.C:
		}
		r.mu.Lock()
	}
	r.mu.Unlock()
	return r.closeInput()
}
