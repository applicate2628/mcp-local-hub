//go:build windows

package api

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/process"

	"golang.org/x/sys/windows"
)

// HANDLE_FLAG_PROTECT_FROM_CLOSE is 0x00000002 in the Windows SDK.
// The pinned x/sys/windows v0.47.0 does not export it; SetHandleInformation
// already accepts the SDK flag as a uint32 mask/value.
// https://learn.microsoft.com/en-us/windows/win32/api/handleapi/nf-handleapi-sethandleinformation
const preparedSerenaTestProtectFromClose uint32 = 0x00000002

// These fixtures exercise the private pin receiver with real file handles.
// Their owner state is synthetic: no acquisition, namespace, publisher, or
// generation observation is issued, and begin-use admission is not exercised.
func newPreparedSerenaBuildConstraintPinFixture(t *testing.T) (*preparedSerenaBuildConstraint, *windowsSerenaOwnedTree, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), preparedSerenaBuildConstraintName)
	if err := os.WriteFile(path, []byte(preparedSerenaBuildConstraintContent), 0600); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	owner := &windowsSerenaOwnedTree{
		buildConstraintReader: &serenaTreeHandle{handle: handle},
		buildConstraintInUse:  true,
	}
	constraint := &preparedSerenaBuildConstraint{tree: &SerenaOwnedTree{platform: owner}}
	owner.buildConstraint = constraint
	t.Cleanup(func() {
		if owner.buildConstraintReader.close.Closed {
			return
		}
		// The test knows no child received this fixture. Settle its actual
		// handle without rewriting the receiver's retained failure evidence.
		if err := windows.SetHandleInformation(handle, preparedSerenaTestProtectFromClose, 0); err != nil {
			t.Error("clear fixture close protection:", err)
		}
		if err := windows.CloseHandle(handle); err != nil {
			t.Error("close test-owned fixture handle:", err)
		}
	})
	return constraint, owner, path
}

func checkPreparedSerenaBuildConstraintPin(t *testing.T, path string, held bool) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Request access only; neither probe writes bytes nor marks the file for
	// deletion. Both requests conflict with the production read-only pin.
	for _, access := range []uint32{windows.FILE_WRITE_DATA, windows.DELETE} {
		handle, openErr := windows.CreateFile(name, access,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if openErr == nil {
			if err := windows.CloseHandle(handle); err != nil {
				t.Fatal("close access probe:", err)
			}
		}
		if held && !errors.Is(openErr, windows.ERROR_SHARING_VIOLATION) {
			t.Fatalf("access %#x while pin held: %v, want sharing violation", access, openErr)
		}
		if !held && openErr != nil {
			t.Fatalf("access %#x after pin release: %v", access, openErr)
		}
	}
}

func TestPreparedSerenaBuildConstraintPinSettlementNative(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	// An empty command fails before CreateProcess. The existing runner still
	// owns an actual empty Job and pipes and returns their original settlement;
	// the test does not construct a native receipt or launch any executable.
	settled, runErr := process.RunContainedStreamObserved(ctx, &exec.Cmd{}, process.ContainedStreamOptions{
		CleanupTimeout:          time.Second,
		AbsoluteCleanupDeadline: deadline.Add(time.Second),
		StdoutByteLimit:         1024,
		StderrByteLimit:         1024,
		Stderr:                  io.Discard,
	}, func(reader io.Reader) error { _, err := io.Copy(io.Discard, reader); return err })
	if runErr == nil || settled.PrimaryError == nil || !strings.Contains(settled.PrimaryError.Error(), "empty command path") ||
		settled.Stage != process.ContainedStageStart || !settled.StartAttempted ||
		!settled.NativeSettlement.Complete() || !settled.NativeSettlement.Snapshot().NoChildKnown ||
		!settled.IOSettlement.Complete() || settled.RootIdentity.Known || settled.RootWaitKnown ||
		settled.IOSettlement.Stdout.WorkerStarted || settled.IOSettlement.Stderr.WorkerStarted {
		t.Fatalf("original empty-command settlement unavailable: error=%v receipt=%+v native=%+v", runErr, settled, settled.NativeSettlement.Snapshot())
	}
	for _, slot := range []process.ContainedStreamSlot{settled.IOSettlement.Stdin,
		settled.IOSettlement.StdoutRead, settled.IOSettlement.StdoutWrite,
		settled.IOSettlement.StderrRead, settled.IOSettlement.StderrWrite} {
		if !slot.Acquired || !slot.CloseAttempted || !slot.Closed || slot.CloseError != nil {
			t.Fatalf("original no-child stream slot was not acquired and settled: %+v", slot)
		}
	}
	preflight, preflightErr := runPreparedSerenaInvocation(ctx, preparedSerenaInvocation{})
	if preflightErr == nil || preflight.containedAttempted {
		t.Fatal("fixture did not stop at the real local preflight", preflightErr)
	}
	// Only the I/O projection is deliberately degraded for the refusal case.
	// The complete native receipt above remains the original runner's value.
	pendingIO := settled
	pendingIO.IOSettlement.Stdout = process.ContainedIOStream{WorkerStarted: true}
	for _, tc := range []struct {
		name    string
		result  preparedSerenaCommandResult
		pending bool
	}{
		{"local-preflight-releases-unused-pin", preflight, false},
		{"missing-native-retains-pin", preparedSerenaCommandResult{containedAttempted: true}, true},
		{"incomplete-io-retains-pin", preparedSerenaCommandResult{containedAttempted: true, Receipt: pendingIO}, true},
		{"original-no-child-releases-pin", preparedSerenaCommandResult{containedAttempted: true, Receipt: settled}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			constraint, owner, path := newPreparedSerenaBuildConstraintPinFixture(t)
			checkPreparedSerenaBuildConstraintPin(t, path, true)
			err := finishPreparedSerenaBuildConstraintUse(constraint, tc.result)
			if tc.pending {
				if err == nil || !strings.Contains(err.Error(), "SERENA_BUILD_CONSTRAINT_USE_PENDING") ||
					!owner.buildConstraintInUse || owner.buildConstraintReader.close.Attempted {
					t.Fatalf("pending outcome released pin or lost debt: %v %+v", err, owner.buildConstraintReader.close)
				}
				if closeErr := closePreparedSerenaBuildConstraint(constraint); closeErr == nil ||
					!strings.Contains(closeErr.Error(), "SERENA_BUILD_CONSTRAINT_USE_PENDING") || owner.buildConstraintReader.close.Attempted {
					t.Fatalf("close overrode pending original outcome: %v", closeErr)
				}
			} else {
				if err != nil || owner.buildConstraintInUse || !owner.buildConstraintReader.close.Attempted ||
					!owner.buildConstraintReader.close.Closed || owner.buildConstraintReader.close.Error != nil {
					t.Fatalf("settled outcome did not close actual pin: %v %+v", err, owner.buildConstraintReader.close)
				}
				if err := closePreparedSerenaBuildConstraint(constraint); err != nil {
					t.Fatal("repeat close changed successful settlement:", err)
				}
			}
			checkPreparedSerenaBuildConstraintPin(t, path, tc.pending)
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != preparedSerenaBuildConstraintContent {
				t.Fatalf("pin settlement changed constraint bytes: %q %v", raw, err)
			}
		})
	}
}

func TestPreparedSerenaBuildConstraintPinCloseFailureNative(t *testing.T) {
	constraint, owner, path := newPreparedSerenaBuildConstraintPinFixture(t)
	handle := owner.buildConstraintReader.handle
	if err := windows.SetHandleInformation(handle, preparedSerenaTestProtectFromClose, preparedSerenaTestProtectFromClose); err != nil {
		t.Fatal(err)
	}
	// This is an actual native CloseHandle refusal, not a forged close result.
	// Only this test-owned handle is protected; no machine policy is changed.
	preflight, preflightErr := runPreparedSerenaInvocation(context.Background(), preparedSerenaInvocation{})
	if preflightErr == nil || preflight.containedAttempted {
		t.Fatal("fixture did not stop at local preflight", preflightErr)
	}
	err := finishPreparedSerenaBuildConstraintUse(constraint, preflight)
	first := owner.buildConstraintReader.close
	if err == nil || owner.buildConstraintInUse || !first.Attempted || first.Closed || first.Error == nil || !errors.Is(err, first.Error) {
		t.Fatalf("actual close failure was lost: error=%v close=%+v", err, first)
	}
	checkPreparedSerenaBuildConstraintPin(t, path, true)
	if err := windows.SetHandleInformation(handle, preparedSerenaTestProtectFromClose, 0); err != nil {
		t.Fatal(err)
	}
	if err := closePreparedSerenaBuildConstraint(constraint); !errors.Is(err, first.Error) || owner.buildConstraintReader.close != first {
		t.Fatalf("repeat close forgot original refusal: %v %+v", err, owner.buildConstraintReader.close)
	}
	checkPreparedSerenaBuildConstraintPin(t, path, true)
	closed, closeErr := owner.close()
	if !closed.Attempted || closed.Closed || !errors.Is(closeErr, first.Error) || !errors.Is(closed.Error, first.Error) {
		t.Fatalf("tree close hid original pin failure: %v %+v", closeErr, closed)
	}
	checkPreparedSerenaBuildConstraintPin(t, path, true)
}
