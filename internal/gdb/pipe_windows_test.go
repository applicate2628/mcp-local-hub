package gdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func TestWindowsDebuggerPipeInterruptsPendingIO(t *testing.T) {
	for _, tc := range []struct {
		name        string
		parentReads bool
	}{
		{"read", true},
		{"write", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, child, err := debuggerPipe(tc.parentReads)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := errors.Join(parent.Close(), child.Close()); err != nil {
					t.Error("close held pipe pair:", err)
				}
			})
			setDeadline := parent.SetWriteDeadline
			if tc.parentReads {
				setDeadline = parent.SetReadDeadline
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			finish, err := armPipeDeadline(ctx, setDeadline)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := finish(); err != nil {
					t.Error(err)
				}
			}()

			done := make(chan error, 1)
			go func() { done <- heldDebuggerPipeIO(parent, tc.parentReads) }()
			// Get a waitIO frame while the peer is held, then verify the
			// original OS operation returns a deadline error before the
			// absolute bound, with neither end closed to induce completion.
			if err := observeDebuggerPipePending(ctx, done); err != nil {
				t.Fatal(err)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, winio.ErrTimeout) {
					t.Errorf("pending I/O returned %v, want deadline error", err)
				} else {
					t.Log("observed Go poller wait and deadline completion while the synchronous peer was still open")
				}
			case <-time.After(time.Second):
				t.Error("cancellation did not return the original pending I/O")
			}
		})
	}
}

func heldDebuggerPipeIO(parent parentPipe, read bool) error {
	if read {
		var data [1]byte
		_, err := parent.Read(data[:])
		return err
	}
	// An unread payload larger than the default named-pipe buffer causes
	// the write to pend rather than finish by copying into that buffer.
	_, err := parent.Write(make([]byte, 1<<20))
	return err
}

func observeDebuggerPipePending(ctx context.Context, done <-chan error) error {
	stack := make([]byte, 256<<10)
	for {
		select {
		case err := <-done:
			return fmt.Errorf("I/O returned before its pending state was observed: %v", err)
		case <-ctx.Done():
			return fmt.Errorf("no native pending I/O observed before the fixture deadline: %w", ctx.Err())
		default:
		}
		n := runtime.Stack(stack, true)
		for _, goroutine := range bytes.Split(stack[:n], []byte("\n\n")) {
			if bytes.Contains(goroutine, []byte(".heldDebuggerPipeIO(")) &&
				(bytes.Contains(goroutine, []byte("internal/poll.(*FD).waitIO(")) || bytes.Contains(goroutine, []byte("go-winio.(*win32File).asyncIO("))) {
				return nil
			}
		}
		runtime.Gosched()
	}
}
