//go:build windows

package process

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

type heldGenerationTestOps struct {
	verifyErr      error
	terminateErr   error
	waitEvent      uint32
	waitErr        error
	verifyCalls    int
	terminateCalls int
	waitCalls      int
	closeCalls     int
}

func (o *heldGenerationTestOps) operations() heldPIDGenerationOps {
	return heldPIDGenerationOps{
		verifyIdentity: func(PIDIdentityProof, windows.Handle) error {
			o.verifyCalls++
			return o.verifyErr
		},
		terminate: func(windows.Handle, uint32) error {
			o.terminateCalls++
			return o.terminateErr
		},
		wait: func(windows.Handle, uint32) (uint32, error) {
			o.waitCalls++
			return o.waitEvent, o.waitErr
		},
		close: func(windows.Handle) error {
			o.closeCalls++
			return nil
		},
	}
}

func heldGenerationForTest(t *testing.T, pid int, ops *heldGenerationTestOps) HeldPIDGeneration {
	t.Helper()
	held, err := holdPIDForTermination(pid, func(access uint32, inherit bool, openedPID uint32) (windows.Handle, error) {
		wantAccess := uint32(windows.PROCESS_TERMINATE | windows.SYNCHRONIZE | windows.PROCESS_QUERY_LIMITED_INFORMATION)
		if access != wantAccess || inherit || openedPID != uint32(pid) {
			t.Fatalf("OpenProcess args=(%#x,%v,%d), want=(%#x,false,%d)", access, inherit, openedPID, wantAccess, pid)
		}
		return windows.Handle(0x1234), nil
	}, ops.operations())
	if err != nil {
		t.Fatalf("holdPIDForTermination: %v", err)
	}
	return held
}

func TestHeldPIDGenerationClosesOnEveryTerminatePath(t *testing.T) {
	tests := []struct {
		name          string
		ops           heldGenerationTestOps
		wantErr       error
		wantErrText   string
		wantTerminate int
	}{
		{
			name:          "success",
			ops:           heldGenerationTestOps{waitEvent: windows.WAIT_OBJECT_0},
			wantTerminate: 1,
		},
		{
			name:    "verify failure",
			ops:     heldGenerationTestOps{verifyErr: ErrProcessIdentityMismatch},
			wantErr: ErrProcessIdentityMismatch,
		},
		{
			name:          "terminate failure",
			ops:           heldGenerationTestOps{terminateErr: windows.ERROR_GEN_FAILURE},
			wantErr:       windows.ERROR_GEN_FAILURE,
			wantTerminate: 1,
		},
		{
			name: "already exited",
			ops: heldGenerationTestOps{
				terminateErr: windows.ERROR_ACCESS_DENIED,
				waitEvent:    windows.WAIT_OBJECT_0,
			},
			wantErr:       ErrProcessAlreadyExited,
			wantTerminate: 1,
		},
		{
			name:          "wait error",
			ops:           heldGenerationTestOps{waitErr: windows.ERROR_GEN_FAILURE},
			wantErr:       windows.ERROR_GEN_FAILURE,
			wantTerminate: 1,
		},
		{
			name:          "wait timeout",
			ops:           heldGenerationTestOps{waitEvent: uint32(windows.WAIT_TIMEOUT)},
			wantErrText:   "timeout waiting for PID 4321 to exit after terminate",
			wantTerminate: 1,
		},
		{
			name:          "unexpected wait result",
			ops:           heldGenerationTestOps{waitEvent: 0xdeadbeef},
			wantErrText:   "returned unexpected code 3735928559",
			wantTerminate: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ops := tc.ops
			held := heldGenerationForTest(t, 4321, &ops)
			err := terminatePIDWithIdentity(PIDIdentityProof{PID: 4321}, func(int) (HeldPIDGeneration, error) {
				return held, nil
			})
			if tc.wantErr == nil && tc.wantErrText == "" && err != nil {
				t.Fatalf("terminatePIDWithIdentity: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want errors.Is(_, %v)", err, tc.wantErr)
			}
			if tc.wantErrText != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrText)) {
				t.Fatalf("error = %v, want text %q", err, tc.wantErrText)
			}
			if ops.closeCalls != 1 {
				t.Fatalf("close calls = %d, want 1", ops.closeCalls)
			}
			if ops.terminateCalls != tc.wantTerminate {
				t.Fatalf("terminate calls = %d, want %d", ops.terminateCalls, tc.wantTerminate)
			}
		})
	}
}

func TestHeldPIDGenerationAccessDeniedFailsClosedUnlessAlreadyExited(t *testing.T) {
	for _, tc := range []struct {
		name        string
		waitEvent   uint32
		wantAlready bool
	}{
		{name: "live or unreadable", waitEvent: uint32(windows.WAIT_TIMEOUT)},
		{name: "already exited", waitEvent: windows.WAIT_OBJECT_0, wantAlready: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := &heldGenerationTestOps{terminateErr: windows.ERROR_ACCESS_DENIED, waitEvent: tc.waitEvent}
			held := heldGenerationForTest(t, 4321, ops)
			committed, err := held.Terminate()
			if committed {
				t.Fatal("ACCESS_DENIED must never report a committed termination")
			}
			if tc.wantAlready {
				if !errors.Is(err, ErrProcessAlreadyExited) {
					t.Fatalf("error = %v, want ErrProcessAlreadyExited", err)
				}
			} else {
				if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, ErrProcessAlreadyExited) {
					t.Fatalf("error = %v, want ACCESS_DENIED without already-exited sentinel", err)
				}
			}
			if closeErr := held.Close(); closeErr != nil {
				t.Fatalf("Close: %v", closeErr)
			}
			if ops.closeCalls != 1 {
				t.Fatalf("close calls = %d, want 1", ops.closeCalls)
			}
		})
	}
}

func TestHoldPIDForTerminationMapsOpenErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		openErr error
		wantErr error
	}{
		{name: "already exited", openErr: windows.ERROR_INVALID_PARAMETER, wantErr: ErrProcessAlreadyExited},
		{name: "access denied", openErr: windows.ERROR_ACCESS_DENIED, wantErr: windows.ERROR_ACCESS_DENIED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := holdPIDForTermination(4321, func(uint32, bool, uint32) (windows.Handle, error) {
				return 0, tc.openErr
			}, productionHeldPIDGenerationOps)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want errors.Is(_, %v)", err, tc.wantErr)
			}
		})
	}
}

func TestTerminatePIDWithIdentityPreservesSentinels(t *testing.T) {
	for _, sentinel := range []error{ErrProcessIdentityMismatch, ErrProcessAlreadyExited} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			ops := &heldGenerationTestOps{}
			held := heldGenerationForTest(t, 4321, ops)
			if sentinel == ErrProcessIdentityMismatch {
				ops.verifyErr = sentinel
			} else {
				ops.terminateErr = windows.ERROR_ACCESS_DENIED
				ops.waitEvent = windows.WAIT_OBJECT_0
			}
			err := terminatePIDWithIdentity(PIDIdentityProof{PID: 4321}, func(int) (HeldPIDGeneration, error) {
				return held, nil
			})
			if !errors.Is(err, sentinel) {
				t.Fatalf("error = %v, want sentinel %v", err, sentinel)
			}
			if ops.closeCalls != 1 {
				t.Fatalf("close calls = %d, want 1", ops.closeCalls)
			}
		})
	}
}
