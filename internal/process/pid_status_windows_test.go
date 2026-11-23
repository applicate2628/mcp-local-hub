//go:build windows

package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Synthetic syscall outcomes exercise the production operation, not Windows
// access control. In particular, the second hypothetical open belongs to a
// different object: memory must be read through the first acquired handle.
func TestObservePIDStatusContract(t *testing.T) {
	identityFailure := errors.New("synthetic identity query failure")
	waitFailure := errors.New("synthetic wait failure")
	closeFailure := errors.New("synthetic close failure")
	type waitResult struct {
		code uint32
		err  error
	}
	alive := waitResult{code: uint32(windows.WAIT_TIMEOUT)}
	dead := waitResult{code: windows.WAIT_OBJECT_0}
	failed := waitResult{code: windows.WAIT_FAILED, err: waitFailure}
	for _, tc := range []struct {
		name       string
		openErr    error
		waits      []waitResult
		verifyErr  error
		ram        uint64
		known      bool
		closeErr   error
		wantState  PIDState
		wantErrors []error
		wantErr    bool
		wantRAM    bool
		wantCalls  []string
	}{
		{name: "same_object", waits: []waitResult{alive, alive}, ram: 42, known: true,
			wantState: PIDStateAlive, wantRAM: true, wantCalls: []string{"open", "wait", "verify", "rss", "wait", "close"}},
		{name: "optional_memory_unknown", waits: []waitResult{alive, alive},
			wantState: PIDStateAlive, wantCalls: []string{"open", "wait", "verify", "rss", "wait", "close"}},
		{name: "known_zero_memory", waits: []waitResult{alive, alive}, known: true,
			wantState: PIDStateAlive, wantRAM: true, wantCalls: []string{"open", "wait", "verify", "rss", "wait", "close"}},
		{name: "already_exited", waits: []waitResult{dead}, wantState: PIDStateDead,
			wantCalls: []string{"open", "wait", "close"}},
		{name: "exit_after_memory", waits: []waitResult{alive, dead}, ram: 42, known: true,
			wantState: PIDStateDead, wantCalls: []string{"open", "wait", "verify", "rss", "wait", "close"}},
		{name: "final_wait_failure_after_memory", waits: []waitResult{alive, failed}, ram: 42, known: true,
			wantState: PIDStateUnknown, wantErr: true, wantErrors: []error{waitFailure},
			wantCalls: []string{"open", "wait", "verify", "rss", "wait", "close"}},
		{name: "identity_mismatch", waits: []waitResult{alive, alive}, verifyErr: ErrProcessIdentityMismatch,
			wantState: PIDStateAlive, wantErr: true, wantErrors: []error{ErrProcessIdentityMismatch},
			wantCalls: []string{"open", "wait", "verify", "wait", "close"}},
		{name: "identity_query_failure", waits: []waitResult{alive, alive}, verifyErr: identityFailure,
			wantState: PIDStateAlive, wantErr: true, wantErrors: []error{identityFailure},
			wantCalls: []string{"open", "wait", "verify", "wait", "close"}},
		{name: "identity_final_wait_and_close_failures", waits: []waitResult{alive, failed}, verifyErr: identityFailure, closeErr: closeFailure,
			wantState: PIDStateUnknown, wantErr: true, wantErrors: []error{identityFailure, waitFailure, closeFailure},
			wantCalls: []string{"open", "wait", "verify", "wait", "close"}},
		{name: "initial_wait_and_close_failures", waits: []waitResult{failed}, closeErr: closeFailure,
			wantState: PIDStateUnknown, wantErr: true, wantErrors: []error{waitFailure, closeFailure},
			wantCalls: []string{"open", "wait", "close"}},
		{name: "unexpected_wait_result", waits: []waitResult{{code: windows.WAIT_ABANDONED}},
			wantState: PIDStateUnknown, wantErr: true, wantCalls: []string{"open", "wait", "close"}},
		{name: "failed_wait_without_error", waits: []waitResult{{code: windows.WAIT_FAILED}},
			wantState: PIDStateUnknown, wantErr: true, wantCalls: []string{"open", "wait", "close"}},
		{name: "denied_is_unknown", openErr: windows.ERROR_ACCESS_DENIED, wantState: PIDStateUnknown,
			wantErr: true, wantErrors: []error{windows.ERROR_ACCESS_DENIED}, wantCalls: []string{"open"}},
		{name: "invalid_pid_open_keeps_error", openErr: windows.ERROR_INVALID_PARAMETER, wantState: PIDStateDead,
			wantErr: true, wantErrors: []error{windows.ERROR_INVALID_PARAMETER}, wantCalls: []string{"open"}},
		{name: "successful_reads_failed_close", waits: []waitResult{alive, alive}, ram: 42, known: true, closeErr: closeFailure,
			wantState: PIDStateAlive, wantErr: true, wantErrors: []error{closeFailure},
			wantCalls: []string{"open", "wait", "verify", "rss", "wait", "close"}},
		{name: "exit_does_not_erase_close_failure", waits: []waitResult{alive, dead}, ram: 42, known: true, closeErr: closeFailure,
			wantState: PIDStateDead, wantErr: true, wantErrors: []error{closeFailure},
			wantCalls: []string{"open", "wait", "verify", "rss", "wait", "close"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proof := PIDIdentityProof{PID: 42, ExecutablePath: `D:\fixture\daemon.exe`, StartedAt: "2026-01-01T00:00:00Z"}
			var calls []string
			opens, waits := 0, 0
			checkHandle := func(h windows.Handle) {
				if h != 0x41 {
					t.Fatalf("operation changed process object: handle=%x", h)
				}
			}
			obs, err := observePIDStatus(proof, pidStatusOps{
				open: func(access uint32, inherit bool, pid uint32) (windows.Handle, error) {
					calls = append(calls, "open")
					opens++
					if access != windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE || inherit || pid != 42 {
						t.Fatal("changed query rights, inheritance, or PID")
					}
					if tc.openErr != nil {
						return 0, tc.openErr
					}
					return windows.Handle(0x40 + opens), nil
				},
				wait: func(h windows.Handle, timeout uint32) (uint32, error) {
					calls = append(calls, "wait")
					checkHandle(h)
					if timeout != 0 || waits >= len(tc.waits) {
						t.Fatal("unexpected blocking/repeated wait")
					}
					result := tc.waits[waits]
					waits++
					return result.code, result.err
				},
				verify: func(got PIDIdentityProof, h windows.Handle) error {
					calls = append(calls, "verify")
					checkHandle(h)
					if got != proof {
						t.Fatal("changed persisted identity proof")
					}
					return tc.verifyErr
				},
				rss: func(h windows.Handle) (uint64, bool) {
					calls = append(calls, "rss")
					checkHandle(h)
					return tc.ram, tc.known
				},
				close: func(h windows.Handle) error {
					calls = append(calls, "close")
					checkHandle(h)
					return tc.closeErr
				},
			})
			if obs.State != tc.wantState || (err != nil) != tc.wantErr {
				t.Fatalf("observation=%+v error=%v", obs, err)
			}
			for _, cause := range tc.wantErrors {
				if !errors.Is(err, cause) {
					t.Errorf("lost independent error %v: %v", cause, err)
				}
			}
			wantBytes := uint64(0)
			if tc.wantRAM {
				wantBytes = tc.ram
			}
			if obs.ResidentBytesKnown != tc.wantRAM || obs.ResidentBytes != wantBytes {
				t.Errorf("unqualified/misattributed memory: %+v", obs)
			}
			if !reflect.DeepEqual(calls, tc.wantCalls) {
				t.Errorf("resource/field sequence=%v, want=%v", calls, tc.wantCalls)
			}
		})
	}
}

func TestObservePIDStatusRejectsUnrepresentablePID(t *testing.T) {
	pids := []int{0, -1}
	if strconv.IntSize == 64 {
		overflow := uint64(1) << 32
		pids = append(pids, int(overflow))
	}
	for _, pid := range pids {
		obs, err := observePIDStatus(PIDIdentityProof{PID: pid}, pidStatusOps{
			open: func(uint32, bool, uint32) (windows.Handle, error) {
				t.Fatal("invalid PID reached OpenProcess")
				return 0, nil
			},
		})
		if err == nil || obs.State != PIDStateUnknown || obs.ResidentBytesKnown {
			t.Fatalf("invalid PID %d: %+v / %v", pid, obs, err)
		}
	}
}

func TestObservePIDStatusNativeSelf(t *testing.T) {
	identity, err := LookupPIDHandleIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	proof := PIDIdentityProof{PID: identity.PID, ExecutablePath: identity.ExecutablePath, StartedAt: identity.StartedAt.Format(time.RFC3339Nano)}
	obs, err := ObservePIDStatus(proof)
	if err != nil || obs.State != PIDStateAlive || !obs.ResidentBytesKnown || obs.ResidentBytes == 0 {
		t.Fatalf("native self observation=%+v / %v", obs, err)
	}
	for _, tc := range []struct {
		name  string
		proof PIDIdentityProof
	}{
		{name: "image", proof: PIDIdentityProof{PID: proof.PID, ExecutablePath: proof.ExecutablePath + ".other", StartedAt: proof.StartedAt}},
		{name: "creation", proof: PIDIdentityProof{PID: proof.PID, ExecutablePath: proof.ExecutablePath, StartedAt: identity.StartedAt.Add(-3 * time.Second).Format(time.RFC3339Nano)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs, err := ObservePIDStatus(tc.proof)
			if !errors.Is(err, ErrProcessIdentityMismatch) || obs.State != PIDStateAlive || obs.ResidentBytesKnown || obs.ResidentBytes != 0 {
				t.Fatalf("mismatched native proof=%+v / %v", obs, err)
			}
		})
	}
}

func TestObservePIDStatusNativeExitDuringObservation(t *testing.T) {
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("native child requires a bounded test runner")
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline.Add(-time.Second))
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestRetainedProcessExitChild$")
	cmd.Env = append(os.Environ(), retainedExitChildEnv+"=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(errors.Join(err, input.Close()))
	}
	inputClosed, waited := false, false
	settle := func() error {
		var closeErr, waitErr error
		if !inputClosed {
			inputClosed = true
			closeErr = input.Close()
		}
		if !waited {
			waited = true
			waitErr = cmd.Wait() // Only the creator joins/reaps this owned child.
		}
		return errors.Join(closeErr, waitErr)
	}
	t.Cleanup(func() {
		if err := settle(); err != nil {
			t.Error(err)
		}
	})
	identity, err := LookupPIDHandleIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	proof := PIDIdentityProof{PID: identity.PID, ExecutablePath: identity.ExecutablePath, StartedAt: identity.StartedAt.Format(time.RFC3339Nano)}
	closes := 0
	obs, err := observePIDStatus(proof, pidStatusOps{
		open: windows.OpenProcess,
		wait: windows.WaitForSingleObject,
		verify: func(proof PIDIdentityProof, h windows.Handle) error {
			if err := verifyHandleIdentity(proof, h); err != nil {
				return err
			}
			return settle() // Exit while the observation still owns its handle.
		},
		rss: residentSetSizeFromHandle,
		close: func(h windows.Handle) error {
			closes++
			return windows.CloseHandle(h)
		},
	})
	if err != nil || obs.State != PIDStateDead || obs.ResidentBytesKnown || obs.ResidentBytes != 0 || closes != 1 {
		t.Fatalf("exited original observation=%+v / %v; close attempts=%d", obs, err, closes)
	}
	if !waited || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 0 {
		t.Fatal("creator did not receive the original child exit")
	}
}
