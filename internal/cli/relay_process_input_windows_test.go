//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestProcessRelayInputWindowsQualification(t *testing.T) {
	t.Run("regular_file_stays_uninterruptible", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "regular")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		owner := &relayProcessInput{file: file}
		interrupt, err := processRelayInputInterrupt(owner)
		if err != nil || interrupt != nil {
			t.Fatalf("regular file acquired unsupported stop action: %v", err)
		}
		if _, err := file.Stat(); err != nil {
			t.Fatalf("qualification closed regular input: %v", err)
		}
	})
	t.Run("closed_handle_refused", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "closed")
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		interrupt, err := processRelayInputInterrupt(&relayProcessInput{file: file})
		if err == nil || interrupt != nil {
			t.Fatal("invalid native handle qualified")
		}
	})
	t.Run("owned_pipe_has_explicit_stop", func(t *testing.T) {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		defer writer.Close()
		owner := &relayProcessInput{file: reader}
		interrupt, err := processRelayInputInterrupt(owner)
		if err != nil || interrupt == nil {
			t.Fatalf("owned pipe did not qualify: %v", err)
		}
		if err := interrupt(); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
			t.Fatalf("owner stop not normalized: %v", err)
		}
		if _, err := writer.Stat(); err != nil {
			t.Fatalf("foreign pipe end closed: %v", err)
		}
	})
	t.Run("overlapped_pipe_stays_borrowed", func(t *testing.T) {
		name, err := windows.UTF16PtrFromString(fmt.Sprintf("\\\\.\\pipe\\mcphub-relay-mode-%d-%d", os.Getpid(), time.Now().UnixNano()))
		if err != nil {
			t.Fatal(err)
		}
		handle, err := windows.CreateNamedPipe(name, windows.PIPE_ACCESS_INBOUND|windows.FILE_FLAG_OVERLAPPED, windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT, 1, 4096, 4096, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		file := os.NewFile(uintptr(handle), "owned-overlapped-test")
		defer file.Close()
		input := &relayProcessInput{file: file}
		interrupt, err := processRelayInputInterrupt(input)
		if err != nil || interrupt != nil || input.reader != nil {
			t.Fatalf("overlapped pipe received synchronous owner: %v", err)
		}
		if _, err := file.Stat(); err != nil {
			t.Fatalf("unsupported input was closed: %v", err)
		}
	})
}

type relayProcessReadFunc func([]byte) (int, error)

func (f relayProcessReadFunc) Read(p []byte) (int, error) { return f(p) }

// Force cancellation before the native request exists. The first real Windows
// cancel must report NOT_FOUND; the same read must then be canceled after its
// submission, while this test still owns an open write end.
func TestProcessRelayInputWindowsCancelBeforeNativeRead(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	input := &relayProcessInput{file: reader}
	interrupt, err := processRelayInputInterrupt(input)
	if err != nil || interrupt == nil {
		t.Fatalf("synchronous owned pipe did not qualify: %v", err)
	}
	owner := input.reader.(*relayWindowsPipeInput)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	owner.reader = relayProcessReadFunc(func(p []byte) (int, error) {
		close(entered)
		<-release
		return reader.Read(p)
	})
	var calls int
	var firstErr error
	owner.cancelRead = func(thread windows.Handle) error {
		calls++
		err := cancelRelaySynchronousRead(thread)
		if calls == 1 {
			firstErr = err
			releaseOnce.Do(func() { close(release) })
		}
		return err
	}
	readDone, stopDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := input.Read(make([]byte, 1)); readDone <- err }()
	<-entered
	go func() { stopDone <- interrupt() }()
	watchdog := time.NewTimer(3 * time.Second)
	defer watchdog.Stop()
	var readErr error
	timedOut := false
	select {
	case readErr = <-readDone:
	case <-watchdog.C:
		timedOut = true
		releaseOnce.Do(func() { close(release) })
		_ = writer.Close() // owner recovery before joining a regressed read
		readErr = <-readDone
	}
	stopErr := <-stopDone
	if timedOut {
		t.Fatalf("cancel before native submission failed to settle: read=%v stop=%v", readErr, stopErr)
	}
	if !errors.Is(firstErr, windows.ERROR_NOT_FOUND) || calls < 2 {
		t.Fatalf("missed-cancel boundary not exercised: first=%v calls=%d", firstErr, calls)
	}
	if !errors.Is(readErr, context.Canceled) || stopErr != nil {
		t.Fatalf("original native read/stop did not settle: read=%v stop=%v", readErr, stopErr)
	}
	if _, err := writer.Stat(); err != nil {
		t.Fatalf("interruption touched the foreign write end: %v", err)
	}
	if _, err := input.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("a later read was admitted after stop: %v", err)
	}
}

func TestProcessRelayInputWindowsCancellationOutcomes(t *testing.T) {
	readFailure, cancelFailure, closeFailure := errors.New("read failed"), errors.New("cancel failed"), errors.New("input close failed")
	for _, tc := range []struct {
		name      string
		readErr   error
		cancelErr error
		wantRead  error
	}{
		{"own_abort", &os.PathError{Op: "read", Path: "owned", Err: windows.ERROR_OPERATION_ABORTED}, nil, context.Canceled},
		{"joined_read_failure", errors.Join(windows.ERROR_OPERATION_ABORTED, readFailure), nil, readFailure},
		{"path_joined_read_failure", &os.PathError{Op: "read", Path: "owned", Err: errors.Join(windows.ERROR_OPERATION_ABORTED, readFailure)}, nil, readFailure},
		{"cancel_refusal", readFailure, cancelFailure, readFailure},
		{"foreign_abort", windows.ERROR_OPERATION_ABORTED, windows.ERROR_NOT_FOUND, windows.ERROR_OPERATION_ABORTED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered, release, cancelCalled := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var cancelOnce sync.Once
			var nativeDone, closedEarly atomic.Bool
			owner := &relayWindowsPipeInput{
				reader: relayProcessReadFunc(func([]byte) (int, error) {
					close(entered)
					<-release
					nativeDone.Store(true)
					return 0, tc.readErr
				}),
				cancelRead: func(windows.Handle) error {
					cancelOnce.Do(func() { close(cancelCalled) })
					return tc.cancelErr
				},
				closeInput: func() error {
					if !nativeDone.Load() {
						closedEarly.Store(true)
					}
					return closeFailure
				},
			}
			readDone, stopDone := make(chan error, 1), make(chan error, 1)
			go func() { _, err := owner.Read(make([]byte, 1)); readDone <- err }()
			<-entered
			go func() { stopDone <- owner.interrupt() }()
			<-cancelCalled
			close(release)
			readErr, stopErr := <-readDone, <-stopDone
			if !errors.Is(readErr, tc.wantRead) || !errors.Is(stopErr, closeFailure) || closedEarly.Load() {
				t.Fatalf("independent outcomes/settlement lost: read=%v stop=%v closedEarly=%v", readErr, stopErr, closedEarly.Load())
			}
			if strings.Contains(tc.name, "joined") && !errors.Is(readErr, windows.ERROR_OPERATION_ABORTED) {
				t.Fatalf("native abort branch was lost from combined read failure: %v", readErr)
			}
			if tc.cancelErr == cancelFailure && !errors.Is(stopErr, cancelFailure) {
				t.Fatalf("native cancellation refusal lost: %v", stopErr)
			}
			if tc.name != "own_abort" && errors.Is(readErr, context.Canceled) {
				t.Fatalf("independent read outcome relabeled as own cancellation: %v", readErr)
			}
		})
	}
}

// Completion of the reader cannot release its OS-thread handle while a cancel
// call still owns that handle. Channels keep cancellation inside the API seam.
func TestProcessRelayInputWindowsJoinsCancellationCall(t *testing.T) {
	entered, releaseRead, nativeReturned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	cancelEntered, releaseCancel := make(chan struct{}), make(chan struct{})
	owner := &relayWindowsPipeInput{
		reader: relayProcessReadFunc(func([]byte) (int, error) {
			close(entered)
			<-releaseRead
			close(nativeReturned)
			return 0, io.EOF
		}),
		cancelRead: func(handle windows.Handle) error {
			close(cancelEntered)
			<-releaseCancel
			var held windows.Handle
			if err := windows.DuplicateHandle(windows.CurrentProcess(), handle, windows.CurrentProcess(), &held, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
				return err
			}
			return windows.CloseHandle(held)
		},
		closeInput: func() error { return nil },
	}
	readDone, stopDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := owner.Read(make([]byte, 1)); readDone <- err }()
	<-entered
	go func() { stopDone <- owner.interrupt() }()
	<-cancelEntered
	close(releaseRead)
	<-nativeReturned
	close(releaseCancel)
	if err := <-readDone; !errors.Is(err, io.EOF) {
		t.Fatalf("read outcome changed: %v", err)
	}
	if err := <-stopDone; err != nil {
		t.Fatalf("thread handle released before cancellation call joined: %v", err)
	}
}
