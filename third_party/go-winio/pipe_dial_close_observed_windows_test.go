//go:build windows

package winio

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestPipeDialNativeMetadataRefusalClosesOriginal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "ordinary-file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := pipeDialCloseFile
	t.Cleanup(func() { pipeDialCloseFile = previous })
	var original *win32File
	var opened windows.Handle
	closes := 0
	pipeDialCloseFile = func(owner *win32File, ctx context.Context) (*CloseReceipt, error) {
		original, opened = owner, owner.handle
		closes++
		return previous(owner, ctx)
	}
	// Actual CreateFile succeeds on this private file; actual GetNamedPipeInfo
	// refuses its type. No metadata or native-close result is fabricated here.
	conn, err := DialPipeContext(ctx, path)
	var failure *PipeDialError
	if conn != nil || !errors.As(err, &failure) || original == nil || opened == 0 || opened == windows.InvalidHandle || closes != 1 {
		t.Fatalf("original post-open refusal lost: conn=%v owner=%p handle=%v closes=%d error=%v", conn, original, opened, closes, err)
	}
	receipt, observerErr := failure.CloseResult()
	if observerErr != nil || receipt != original.closeReceipt || failure.owner != original {
		t.Fatalf("dial error substituted its original close: receipt=%p error=%v", receipt, observerErr)
	}
	if nativeErr, observed := receipt.NativeResult(); !observed || nativeErr != nil || original.Fd() != 0 || original.handle != 0 || receipt.failedHandle != 0 {
		t.Fatalf("actual close did not settle: native=%v observed=%t fd=%d", nativeErr, observed, original.Fd())
	}
	again, err := original.CloseObserved(ctx)
	if err != nil || again != receipt || closes != 1 {
		t.Fatal("re-observation replaced or retried the original close")
	}
	copy := *failure
	for _, invalid := range []*PipeDialError{nil, {}, &copy} {
		if received, err := invalid.CloseResult(); received != nil || err == nil {
			t.Fatal("nil, unissued, or copied failure issued a close result")
		}
	}
}

func TestPipeDialFailurePointsRetainOwnerAndAllCauses(t *testing.T) {
	for _, stage := range []string{"metadata", "initialization", "initialization-and-close", "timeout-and-close"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(t.TempDir(), "ordinary-file")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			oldInfo, oldInit, oldClose := pipeDialGetInfo, pipeDialInitFile, pipeDialCloseFile
			t.Cleanup(func() { pipeDialGetInfo, pipeDialInitFile, pipeDialCloseFile = oldInfo, oldInit, oldClose })
			primary := errors.New("injected pipe initialization refusal")
			if stage == "timeout-and-close" {
				primary = context.DeadlineExceeded
			}
			closeFailure := errors.New("modeled native close report failure")
			var opened windows.Handle
			var initialized, closing *win32File
			closes, nativeCalls := 0, 0
			fdDuringClose := uintptr(1)
			pipeDialGetInfo = func(handle windows.Handle, flags, _, _, _ *uint32) error {
				opened = handle
				if stage == "metadata" {
					return primary
				}
				// Metadata success is modeled only to reach the next deterministic
				// failure with this same actual CreateFile handle and no worker.
				*flags = 0
				return nil
			}
			pipeDialInitFile = func(owner *win32File) error {
				initialized = owner
				return primary
			}
			pipeDialCloseFile = func(owner *win32File, _ context.Context) (*CloseReceipt, error) {
				closing = owner
				closes++
				receipt := owner.closeHandle(func(handle windows.Handle) error {
					nativeCalls++
					fdDuringClose = owner.Fd()
					// Close the actual test resource once. Only the added failure
					// report is modeled; this is not a live failed-CloseHandle claim.
					nativeErr := windows.Close(handle)
					if stage == "initialization-and-close" || stage == "timeout-and-close" {
						return errors.Join(nativeErr, closeFailure)
					}
					return nativeErr
				})
				return receipt, nil
			}
			var connErr error
			if stage == "timeout-and-close" {
				// Existing default timeout; the injected primary error, not a new
				// elapsed budget, exercises the wrapper's timeout receiving branch.
				_, connErr = DialPipe(path, nil)
			} else {
				_, connErr = DialPipeContext(ctx, path)
			}
			var failure *PipeDialError
			if !errors.As(connErr, &failure) || !errors.Is(connErr, primary) || closing == nil || closing != failure.owner || closes != 1 || nativeCalls != 1 || opened == 0 || opened == windows.InvalidHandle {
				t.Fatalf("original failure path differs: opened=%v owner=%p closes=%d native=%d error=%v", opened, closing, closes, nativeCalls, connErr)
			}
			if stage == "metadata" && initialized != nil || stage != "metadata" && initialized != closing {
				t.Fatal("initialization did not use the same original file owner")
			}
			if stage == "timeout-and-close" && !errors.Is(connErr, ErrTimeout) {
				t.Fatal("timeout wrapper lost its compatible timeout sentinel")
			}
			receipt, observerErr := failure.CloseResult()
			nativeErr, observed := receipt.NativeResult()
			wantFailed := stage == "initialization-and-close" || stage == "timeout-and-close"
			if observerErr != nil || !observed || errors.Is(nativeErr, closeFailure) != wantFailed || errors.Is(connErr, closeFailure) != wantFailed || fdDuringClose != 0 || closing.Fd() != 0 || closing.handle != 0 {
				t.Fatalf("close cause or active-handle consumption differs: observed=%t native=%v observer=%v fdDuring=%d fdAfter=%d", observed, nativeErr, observerErr, fdDuringClose, closing.Fd())
			}
			if wantFailed && receipt.failedHandle != opened || !wantFailed && receipt.failedHandle != 0 {
				t.Fatal("private failure custody does not name the original closed attempt")
			}
			if err := closing.Close(); err != nil {
				t.Fatal(err)
			}
			again, err := closing.CloseObserved(ctx)
			if err != nil || again != receipt || nativeCalls != 1 || closes != 1 {
				t.Fatal("cached failure granted another close attempt")
			}
		})
	}
}

func TestPipeDialPreAcquireRefusalsHaveNoCloseOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordinary-file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	oldInfo, oldClose := pipeDialGetInfo, pipeDialCloseFile
	t.Cleanup(func() { pipeDialGetInfo, pipeDialCloseFile = oldInfo, oldClose })
	metadataCalls, closeCalls := 0, 0
	pipeDialGetInfo = func(handle windows.Handle, flags, out, in, instances *uint32) error {
		metadataCalls++
		return oldInfo(handle, flags, out, in, instances)
	}
	pipeDialCloseFile = func(owner *win32File, ctx context.Context) (*CloseReceipt, error) {
		closeCalls++
		return oldClose(owner, ctx)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := DialPipeContext(canceled, path)
	var acquired *PipeDialError
	if conn != nil || !errors.Is(err, context.Canceled) || errors.As(err, &acquired) {
		t.Fatalf("pre-acquisition cancellation gained a resource receipt: %v", err)
	}
	var elapsed time.Duration
	conn, err = DialPipe(path, &elapsed)
	if conn != nil || err != ErrTimeout || errors.As(err, &acquired) || metadataCalls != 0 || closeCalls != 0 {
		t.Fatalf("clean pre-acquisition timeout changed: metadata=%d close=%d error=%v", metadataCalls, closeCalls, err)
	}
}
