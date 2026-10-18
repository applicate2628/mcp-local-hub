//go:build windows

package process

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const retainedExitChildEnv = "MCPHUB_TEST_RETAINED_EXIT_CHILD"

func TestRetainedProcessExitChild(t *testing.T) {
	value := os.Getenv(retainedExitChildEnv)
	if value == "" {
		return
	}
	code, err := strconv.Atoi(value)
	if err != nil || (code != 0 && code != 7) {
		t.Fatal("invalid retained-exit test helper")
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
	os.Exit(code)
}

func TestHeldPIDExitWaiterNativeOriginalExitCodes(t *testing.T) {
	if os.Getenv(retainedExitChildEnv) != "" {
		t.Fatal("unexpected child selector")
	}
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("Root's bounded native runner must provide a deadline")
	}
	for _, code := range []int{0, 7} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestRetainedProcessExitChild$")
			cmd.Env = append(os.Environ(), retainedExitChildEnv+"="+strconv.Itoa(code))
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(errors.Join(err, input.Close()))
			}
			inputClosed := false
			t.Cleanup(func() {
				if !inputClosed {
					if err := input.Close(); err != nil {
						t.Error(err)
					}
				}
				waitErr := cmd.Wait() // The creator's only Wait; WaitExit never reaps.
				if code == 0 && waitErr != nil {
					t.Error(waitErr)
				}
				if code != 0 && (cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != code) {
					t.Errorf("original creator outcome: %v", waitErr)
				}
			})
			identity, err := LookupPIDHandleIdentity(cmd.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			held, err := HoldPIDForTermination(cmd.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := held.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := held.VerifyIdentity(PIDIdentityProof{PID: identity.PID, ExecutablePath: identity.ExecutablePath, StartedAt: identity.StartedAt.UTC().Format(time.RFC3339Nano)}); err != nil {
				t.Fatal(err)
			}
			waiter, ok := held.(HeldPIDExitWaiter)
			if !ok {
				t.Fatal("production held object lacks optional exit waiter")
			}
			inputClosed = true // Record the one attempt even if closure is non-clean.
			if err := input.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			result, err := waiter.WaitExit(ctx)
			if err != nil || !result.Exited || !result.ExitCodeAvailable || result.ExitCode != uint32(code) {
				t.Fatalf("same original exit=%+v error=%v", result, err)
			}
		})
	}
}

// These are labelled synthetic provider outcomes, not failed Windows syscalls.
func TestRetainedProcessExitPreservesFactsAndFailureCauses(t *testing.T) {
	primary, closing := errors.New("synthetic native primary"), errors.New("synthetic event close")
	for _, scenario := range []string{"partial-create", "wait", "unexpected-timeout", "exit-code", "late-close"} {
		t.Run(scenario, func(t *testing.T) {
			closes := 0
			ops := retainedProcessExitOps{
				createEvent: func() (windows.Handle, error) {
					if scenario == "partial-create" {
						return 0x22, primary
					}
					return 0x22, nil
				},
				setEvent: func(windows.Handle) error { t.Error("unexpected wake"); return nil },
				wait: func(handles []windows.Handle, all bool, timeout uint32) (uint32, error) {
					if len(handles) != 2 || handles[0] != 0x11 || handles[1] != 0x22 || all || timeout != windows.INFINITE {
						t.Error("borrowed original/wait policy changed")
					}
					if scenario == "wait" {
						return 0, primary
					}
					if scenario == "unexpected-timeout" {
						return uint32(windows.WAIT_TIMEOUT), nil
					}
					return windows.WAIT_OBJECT_0, nil
				},
				exitCode: func(handle windows.Handle, value *uint32) error {
					if handle != 0x11 {
						t.Error("exit read changed original")
					}
					if scenario == "exit-code" {
						return primary
					}
					*value = 7
					return nil
				},
				close: func(handle windows.Handle) error {
					closes++
					if handle != 0x22 {
						t.Error("closed borrowed process")
					}
					return closing
				},
			}
			result, err := waitRetainedProcessExit(context.Background(), 0x11, ops)
			if !errors.Is(err, closing) || closes != 1 {
				t.Fatalf("event close receiving=%v calls=%d", err, closes)
			}
			if scenario != "late-close" && scenario != "unexpected-timeout" && !errors.Is(err, primary) {
				t.Fatal("lost original native cause")
			}
			if scenario == "unexpected-timeout" && !strings.Contains(err.Error(), "without a deadline") {
				t.Fatal("impossible infinite timeout became a poll/retry")
			}
			wantExit := scenario == "exit-code" || scenario == "late-close"
			if result.Exited != wantExit || result.ExitCodeAvailable != (scenario == "late-close") {
				t.Fatalf("lost signal/code distinction: %+v", result)
			}
			if scenario == "late-close" && result.ExitCode != 7 {
				t.Fatal("late failure erased actual code")
			}
		})
	}
}

func TestRetainedProcessExitFailedWakeKeepsNativeInvocation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, wakeReturned, originalSignal := make(chan struct{}), make(chan struct{}), make(chan struct{})
	wakeFailure := errors.New("synthetic failed cancellation wake")
	var closes atomic.Int32
	ops := retainedProcessExitOps{
		createEvent: func() (windows.Handle, error) { return 0x22, nil },
		setEvent:    func(windows.Handle) error { defer close(wakeReturned); return wakeFailure },
		wait: func([]windows.Handle, bool, uint32) (uint32, error) {
			close(entered)
			<-originalSignal
			return windows.WAIT_OBJECT_0, nil
		},
		exitCode: func(windows.Handle, *uint32) error { return nil },
		close: func(h windows.Handle) error {
			if h != 0x22 {
				t.Error("borrowed original closed")
			}
			closes.Add(1)
			return nil
		},
	}
	type outcome struct {
		result HeldPIDExitResult
		err    error
	}
	done := make(chan outcome, 1)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(originalSignal) }) }
	var receiveOnce sync.Once
	var received outcome
	receive := func() outcome { receiveOnce.Do(func() { received = <-done }); return received }
	t.Cleanup(func() { cancel(); release(); receive() })
	go func() { result, err := waitRetainedProcessExit(ctx, 0x11, ops); done <- outcome{result, err} }()
	<-entered
	cancel()
	<-wakeReturned
	select {
	case early := <-done:
		received = early
		receiveOnce.Do(func() {})
		t.Fatal("failed wake detached/completed the pending native invocation")
	default:
	}
	if closes.Load() != 0 {
		t.Fatal("event closed while native wait remained pending")
	}
	release()
	actual := receive()
	if !actual.result.Exited || !actual.result.ExitCodeAvailable || !errors.Is(actual.err, wakeFailure) || closes.Load() != 1 {
		t.Fatalf("original outcome/callback/event receiving=%+v / %v", actual.result, actual.err)
	}
}

func TestHeldPIDExitWaiterBorrowAndFirstCloseError(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	closeFailure := errors.New("synthetic held close failure")
	waitFailure := errors.New("synthetic controlled wait outcome unavailable")
	closes := 0
	held := &heldPIDGeneration{pid: 9, handle: 0x11, ops: heldPIDGenerationOps{
		waitExit: func(context.Context, uintptr) (HeldPIDExitResult, error) {
			close(entered)
			<-release
			return HeldPIDExitResult{}, waitFailure
		},
		close: func(windows.Handle) error { closes++; return closeFailure },
	}}
	done := make(chan error, 1)
	go func() { _, err := held.WaitExit(context.Background()); done <- err }()
	<-entered
	if held.mu.TryLock() {
		held.mu.Unlock()
		close(release)
		<-done
		t.Fatal("native wait did not retain its Close borrowing guard")
	}
	close(release)
	if err := <-done; !errors.Is(err, waitFailure) {
		t.Fatal("controlled wait failure was lost")
	}
	for i := 0; i < 2; i++ {
		if err := held.Close(); !errors.Is(err, closeFailure) {
			t.Fatal("first held close cause lost")
		}
	}
	if closes != 1 || held.handle != 0x11 {
		t.Fatal("uncertain held close retried or erased original identity")
	}
	if _, err := held.WaitExit(context.Background()); err == nil {
		t.Fatal("reused close-attempted held owner")
	}
	legacyEffects := &heldPIDGeneration{pid: 9, handle: 0x11}
	if _, err := legacyEffects.WaitExit(context.Background()); !errors.Is(err, ErrProcessIdentityUnsupported) {
		t.Fatal("old effect fixture gained fake native support")
	}
}

func TestRetainedProcessExitNativeNoDeadlineCancellation(t *testing.T) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := windows.CloseHandle(handle); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	ops := retainedProcessExitOps{
		createEvent: func() (windows.Handle, error) { return windows.CreateEvent(nil, 1, 0, nil) },
		setEvent:    windows.SetEvent, exitCode: windows.GetExitCodeProcess, close: windows.CloseHandle,
		wait: func(handles []windows.Handle, all bool, timeout uint32) (uint32, error) {
			close(entered)
			return windows.WaitForMultipleObjects(handles, all, timeout)
		},
	}
	type outcome struct {
		result HeldPIDExitResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := waitRetainedProcessExit(ctx, uintptr(handle), ops)
		done <- outcome{result, err}
	}()
	select {
	case <-entered:
	case early := <-done:
		t.Fatalf("native wait failed before entry: %+v / %v", early.result, early.err)
	}
	cancel()
	actual := <-done
	if !errors.Is(actual.err, context.Canceled) || actual.result.Exited {
		t.Fatalf("cancellation became exit: %+v / %v", actual.result, actual.err)
	}
	if state, err := windows.WaitForSingleObject(handle, 0); err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		t.Fatal("cancellation released/affected original handle")
	}
	if _, err := WaitRetainedProcessExit(nil, uintptr(handle)); err == nil {
		t.Fatal("nil context accepted")
	}
	if _, err := WaitRetainedProcessExit(context.Background(), 0); err == nil {
		t.Fatal("missing handle accepted")
	}
	already, stop := context.WithCancel(context.Background())
	stop()
	if _, err := WaitRetainedProcessExit(already, uintptr(handle)); !errors.Is(err, context.Canceled) {
		t.Fatal("pre-entry cancellation lost")
	}
}

func TestLookupPIDHandleIdentityUsesKernelCreationTime(t *testing.T) {
	pid := os.Getpid()
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatalf("open current process: %v", err)
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		t.Fatalf("get current process times: %v", err)
	}
	wantStartedAt := time.Unix(0, creation.Nanoseconds()).UTC()
	got, err := LookupPIDHandleIdentity(pid)
	if err != nil {
		t.Fatalf("lookup current process handle identity: %v", err)
	}
	if got.PID != pid || got.ExecutablePath == "" || !got.StartedAt.Equal(wantStartedAt) || got.StartedAt.Location() != time.UTC {
		t.Fatalf("handle identity=%+v want pid=%d started_at=%s UTC", got, pid, wantStartedAt.Format(time.RFC3339Nano))
	}
}

func TestLookupPIDHandleIdentityClosesOnSuccessAndIdentityError(t *testing.T) {
	for _, tc := range []struct {
		name        string
		identityErr error
	}{
		{name: "success"},
		{name: "identity_error", identityErr: errors.New("injected identity failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closeCalls := 0
			got, err := lookupPIDHandleIdentity(4321,
				func(access uint32, inherit bool, pid uint32) (windows.Handle, error) {
					wantAccess := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE)
					if access != wantAccess || inherit || pid != 4321 {
						t.Fatalf("OpenProcess args=(%#x,%v,%d)", access, inherit, pid)
					}
					return windows.Handle(0x1234), nil
				},
				func(pid int, h windows.Handle) (processHandleIdentity, error) {
					if pid != 4321 || h != windows.Handle(0x1234) {
						t.Fatalf("identity args=(%d,%#x)", pid, h)
					}
					if tc.identityErr != nil {
						return processHandleIdentity{}, tc.identityErr
					}
					return processHandleIdentity{ExecutablePath: `C:\\synthetic\\mcphub.exe`, CreationTime: time.Date(2031, 2, 3, 4, 5, 6, 700, time.UTC)}, nil
				},
				func(h windows.Handle) error {
					if h != windows.Handle(0x1234) {
						t.Fatalf("close handle=%#x", h)
					}
					closeCalls++
					return nil
				},
			)
			if closeCalls != 1 {
				t.Fatalf("close calls=%d, want 1", closeCalls)
			}
			if tc.identityErr != nil {
				if got != (PIDHandleIdentity{}) || !errors.Is(err, tc.identityErr) {
					t.Fatalf("identity=(%+v,%v), want error %v", got, err, tc.identityErr)
				}
				return
			}
			if err != nil || got.PID != 4321 || got.ExecutablePath == "" || got.StartedAt.IsZero() {
				t.Fatalf("identity=(%+v,%v)", got, err)
			}
		})
	}
}

func TestHeldPIDJobObservationIsQueryOnlyAndFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		member   bool
		queryErr error
	}{
		{name: "independent"},
		{name: "job owned", member: true},
		{name: "membership unknown", queryErr: windows.ERROR_ACCESS_DENIED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed := 0
			held, err := holdPIDForJobObservation(4321,
				func(access uint32, inherit bool, pid uint32) (windows.Handle, error) {
					if access != windows.PROCESS_QUERY_LIMITED_INFORMATION || inherit || pid != 4321 {
						t.Fatalf("OpenProcess=(%#x,%v,%d), want query-only PID 4321", access, inherit, pid)
					}
					return windows.Handle(0x1234), nil
				},
				func(proof PIDIdentityProof, h windows.Handle) error {
					if proof.PID != 4321 || h != windows.Handle(0x1234) {
						t.Fatalf("identity proof=%+v handle=%#x", proof, h)
					}
					return nil
				},
				func(h windows.Handle) (bool, error) {
					if h != windows.Handle(0x1234) {
						t.Fatalf("query handle=%#x", h)
					}
					return tc.member, tc.queryErr
				},
				func(h windows.Handle) error {
					if h != windows.Handle(0x1234) {
						t.Fatalf("close handle=%#x", h)
					}
					closed++
					return nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := held.InAnyJob(); err == nil {
				t.Fatal("membership query before identity verification succeeded")
			}
			if err := held.VerifyIdentity(PIDIdentityProof{PID: 4321}); err != nil {
				t.Fatal(err)
			}
			member, err := held.InAnyJob()
			if tc.queryErr != nil {
				if member || !errors.Is(err, tc.queryErr) {
					t.Fatalf("unknown membership=(%v,%v)", member, err)
				}
			} else if err != nil || member != tc.member {
				t.Fatalf("membership=(%v,%v), want (%v,nil)", member, err, tc.member)
			}
			if err := held.Close(); err != nil {
				t.Fatal(err)
			}
			if closed != 1 {
				t.Fatalf("close calls=%d, want 1", closed)
			}
		})
	}
}

func TestHeldPIDJobObservationQueriesCurrentProcessWithoutTerminationRights(t *testing.T) {
	identity, err := LookupPIDHandleIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	held, err := HoldPIDForJobObservation(identity.PID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := held.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	heldIdentity, ok := held.(HeldPIDIdentity)
	if !ok {
		t.Fatal("query-only hold lacks held identity")
	}
	observed, err := heldIdentity.Identity()
	if err != nil || observed != identity {
		t.Fatalf("query-only held identity=(%+v,%v), want %+v", observed, err, identity)
	}
	if err := held.VerifyIdentity(PIDIdentityProof{PID: identity.PID, ExecutablePath: identity.ExecutablePath, StartedAt: identity.StartedAt.Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	if _, err := held.InAnyJob(); err != nil {
		t.Fatalf("query own exact process Job membership: %v", err)
	}
}

func TestHeldPIDJobObservationExactGeneration(t *testing.T) {
	held, err := HoldPIDForJobObservation(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	reader, ok := held.(HeldPIDIdentity)
	if !ok {
		t.Fatal("query-only hold lacks same-handle identity")
	}
	if _, canTerminate := held.(interface{ Terminate() (bool, error) }); canTerminate {
		t.Fatal("query-only Job hold exposes process termination")
	}
	identity, err := reader.Identity()
	if err != nil || identity.PID != os.Getpid() || identity.ExecutablePath == "" || identity.StartedAt.IsZero() {
		t.Fatalf("held self identity=(%+v,%v)", identity, err)
	}
	wrong := PIDIdentityProof{PID: identity.PID, ExecutablePath: identity.ExecutablePath, StartedAt: identity.StartedAt.Add(500 * time.Millisecond).Format(time.RFC3339Nano), StartTolerance: time.Nanosecond}
	if err := held.VerifyIdentity(wrong); !errors.Is(err, ErrProcessIdentityMismatch) {
		t.Fatalf("wrong 500ms sidecar accepted: %v", err)
	}
	if _, err := held.InAnyJob(); err == nil {
		t.Fatal("Job query succeeded after wrong sidecar")
	}
	correct := PIDIdentityProof{PID: identity.PID, ExecutablePath: identity.ExecutablePath, StartedAt: identity.StartedAt.Format(time.RFC3339Nano), StartTolerance: time.Nanosecond}
	if err := held.VerifyIdentity(correct); err != nil {
		t.Fatal(err)
	}
	if _, err := held.InAnyJob(); err != nil {
		t.Fatalf("same-handle query-only Job observation: %v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Identity(); err == nil {
		t.Fatal("closed Job hold returned identity")
	}
}

func TestHeldPIDTerminationIdentityOfCurrentProcessWithoutTerminating(t *testing.T) {
	want, err := LookupPIDHandleIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	held, err := HoldPIDForTermination(want.PID)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	reader, ok := held.(HeldPIDIdentity)
	if !ok {
		t.Fatal("termination hold lacks held identity")
	}
	got, err := reader.Identity()
	if err != nil || got != want {
		t.Fatalf("termination held identity=(%+v,%v), want %+v", got, err, want)
	}
}

func TestHeldPIDIdentityReadsTheOriginalHandle(t *testing.T) {
	started := time.Date(2026, 9, 28, 8, 0, 0, 500000000, time.UTC)
	identity := func(pid int, handle windows.Handle) (processHandleIdentity, error) {
		if pid != 4321 || handle != windows.Handle(0x1234) {
			t.Fatalf("identity used PID/handle (%d,%#x), want original hold", pid, handle)
		}
		return processHandleIdentity{ExecutablePath: `C:\product\mcphub.exe`, CreationTime: started}, nil
	}
	for _, tc := range []struct {
		name string
		hold func() (any, error)
	}{
		{name: "termination hold", hold: func() (any, error) {
			return holdPIDForTermination(4321, func(uint32, bool, uint32) (windows.Handle, error) { return windows.Handle(0x1234), nil }, heldPIDGenerationOps{identity: identity, close: func(windows.Handle) error { return nil }})
		}},
		{name: "query-only job hold", hold: func() (any, error) {
			held, err := holdPIDForJobObservation(4321, func(access uint32, _ bool, _ uint32) (windows.Handle, error) {
				if access != windows.PROCESS_QUERY_LIMITED_INFORMATION {
					t.Fatalf("access=%#x", access)
				}
				return windows.Handle(0x1234), nil
			}, nil, nil, func(windows.Handle) error { return nil })
			if err == nil {
				held.(*heldPIDJobObservation).identity = identity
			}
			return held, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held, err := tc.hold()
			if err != nil {
				t.Fatal(err)
			}
			identityHold, ok := held.(interface {
				Identity() (PIDHandleIdentity, error)
				Close() error
			})
			if !ok {
				t.Fatal("hold lacks shared Identity operation")
			}
			defer identityHold.Close()
			got, err := identityHold.Identity()
			if err != nil || got.PID != 4321 || got.ExecutablePath != `C:\product\mcphub.exe` || !got.StartedAt.Equal(started) {
				t.Fatalf("held identity=(%+v,%v)", got, err)
			}
			if err := identityHold.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := identityHold.Identity(); err == nil {
				t.Fatal("closed hold returned identity")
			}
		})
	}
}

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
