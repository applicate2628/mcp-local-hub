//go:build windows

package process

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/windows"
)

func TestHeldOriginalBorrowExcludesTerminalAndPreservesOwnerCloseContract(t *testing.T) {
	for _, mode := range []string{"generation-close", "generation-terminate", "observation-close"} {
		t.Run(mode, func(t *testing.T) {
			deadline, ok := t.Deadline()
			if !ok {
				t.Fatal("test runner deadline required")
			}
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			callbackFailure, closeFailure := errors.New("original verification failure"), errors.New("original close failure")
			var closes, kills atomic.Int32
			generation := &heldPIDGeneration{pid: 7, handle: windows.Handle(101), ops: heldPIDGenerationOps{
				close:     func(windows.Handle) error { closes.Add(1); return closeFailure },
				terminate: func(windows.Handle, uint32) error { kills.Add(1); return nil },
				wait:      func(windows.Handle, uint32) (uint32, error) { return windows.WAIT_OBJECT_0, nil },
			}}
			observation := &heldPIDJobObservation{pid: 7, handle: windows.Handle(102), close: func(windows.Handle) error { closes.Add(1); return closeFailure }}
			var borrower HeldOriginalProcessHandleBorrower = generation
			requested := &generation.terminalRequested
			terminal := func() error { return generation.Close() }
			if mode == "generation-terminate" {
				terminal = func() error {
					committed, err := generation.Terminate()
					if err == nil && !committed {
						return errors.New("termination commitment lost")
					}
					return err
				}
			}
			if mode == "observation-close" {
				borrower = observation
				requested = &observation.terminalRequested
				terminal = observation.Close
			}
			entered, release := make(chan struct{}), make(chan struct{})
			borrowDone := make(chan error, 1)
			go func() {
				borrowDone <- borrower.BorrowOriginalProcessHandle(func(handle uintptr) error {
					if handle != 101 && handle != 102 {
						return errors.New("foreign handle substituted")
					}
					close(entered)
					select {
					case <-release:
						return callbackFailure
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			terminalDone := make(chan error, 1)
			go func() { terminalDone <- terminal() }()
			for !requested.Load() {
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				default:
					runtime.Gosched()
				}
			}
			if closes.Load() != 0 || kills.Load() != 0 {
				t.Fatal("terminal native operation entered while original handle was borrowed")
			}
			if err := borrower.BorrowOriginalProcessHandle(func(uintptr) error { t.Error("new borrow entered after retirement request"); return nil }); !errors.Is(err, ErrHeldOriginalProcessRetiring) {
				t.Fatal("retirement fence did not refuse a new borrow")
			}
			close(release)
			if err := <-borrowDone; !errors.Is(err, callbackFailure) || !errors.Is(err, ErrHeldOriginalProcessRetiring) {
				t.Fatalf("callback/retirement outcomes lost: %v", err)
			}
			terminalErr := <-terminalDone
			if mode == "generation-terminate" {
				if terminalErr != nil || kills.Load() != 1 || closes.Load() != 0 {
					t.Fatalf("original terminate result changed: %v", terminalErr)
				}
				if err := generation.Close(); !errors.Is(err, closeFailure) {
					t.Fatal("post-terminate original close cause lost")
				}
			} else if !errors.Is(terminalErr, closeFailure) || closes.Load() != 1 {
				t.Fatalf("original terminal close result changed: %v", terminalErr)
			}
			if mode == "observation-close" {
				if err := observation.Close(); err != nil {
					t.Fatal("query owner must retain its established nil-after-first-close behavior")
				}
			} else if err := generation.Close(); !errors.Is(err, closeFailure) {
				t.Fatal("generation owner must retain its cached first-close failure")
			}
			if closes.Load() != 1 {
				t.Fatal("original handle close was repeated")
			}
		})
	}
}
