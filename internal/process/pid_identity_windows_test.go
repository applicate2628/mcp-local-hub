//go:build windows

package process

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

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
