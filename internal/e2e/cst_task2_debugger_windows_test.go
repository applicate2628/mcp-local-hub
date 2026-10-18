//go:build windows && cst_vptr_probe

package e2e

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	subprocess "mcp-local-hub/internal/process"
)

// The installed Windows SDK 10.0.26100 AMD64 DEBUG_EVENT is 176 bytes with
// the union at offset 16. No debug handle is serialized to F3/F4 evidence.
type cstB2DebugEvent struct {
	Code      uint32
	ProcessID uint32
	ThreadID  uint32
	_         uint32
	Data      [20]uint64
}

const cstB2ExceptionEvent = 1
const cstB2CreateThreadEvent = 2
const cstB2CreateProcessEvent = 3
const cstB2LoadDLLEvent = 6
const cstB2UnloadDLLEvent = 7
const cstB2Breakpoint = 0x80000003
const cstB2ContinueStatus = 0x00010002

func TestCSTB2DebugEventLayout(t *testing.T) {
	var event cstB2DebugEvent
	if unsafe.Sizeof(event) != 176 || unsafe.Alignof(event) != 8 || unsafe.Offsetof(event.Data) != 16 ||
		cstB2CreateProcessEvent != 3 {
		t.Fatalf("installed AMD64 DEBUG_EVENT ABI differs: size=%d align=%d union=%d code=%d",
			unsafe.Sizeof(event), unsafe.Alignof(event), unsafe.Offsetof(event.Data), cstB2CreateProcessEvent)
	}
}

func TestCSTB2NoKernelEventCannotClaimHeld(t *testing.T) {
	var missing cstB2HeldProof
	if missing.heldNow() {
		t.Fatal("zero debugger proof claimed a held event")
	}
}

func TestCSTPostTokenInjectedContinueFailureRetainsHeldOwner(t *testing.T) {
	var event cstB2DebugEvent
	event.Code, event.ProcessID, event.ThreadID, event.Data[0] = cstB2ExceptionEvent, 101, 202, cstB2Breakpoint
	proof := &cstB2HeldProof{event: event, ownerThread: windows.GetCurrentThreadId(), targetPID: 101, held: true}
	called := 0
	err := proof.continueHeldWith(func(pid, thread, status uint32) error {
		called++
		if pid != 101 || thread != 202 || status != cstB2ContinueStatus {
			t.Fatalf("injected Continue saw wrong event identity: pid=%d thread=%d status=%#x", pid, thread, status)
		}
		return errors.New("injected ContinueDebugEvent failure")
	})
	if err == nil || called != 1 || !proof.heldNow() {
		t.Fatalf("failed Continue promoted or lost held event: called=%d held=%v err=%v", called, proof.heldNow(), err)
	}
}

// TestCSTB2TargetOnlyHeldEventPipeProbe uses the same target at-create owner
// as B1/B2. READY and both echo controls are worker output on its existing
// stdout; the target's independent main start event stays closed throughout.
func TestCSTB2TargetOnlyHeldEventPipeProbe(t *testing.T) {
	if os.Getenv("MCPHUB_CST_B2_PIPE_TARGET") == "1" {
		raw, err := strconv.ParseUint(os.Getenv("MCPHUB_CST_B2_START_RELEASE"), 10, 64)
		if err != nil || raw == 0 {
			os.Exit(91)
		}
		release := windows.Handle(raw)
		go func() {
			if _, err := os.Stdout.Write([]byte{'R'}); err != nil {
				return
			}
			var input [1]byte
			for {
				if _, err := io.ReadFull(os.Stdin, input[:]); err != nil {
					return
				}
				if _, err := os.Stdout.Write(input[:]); err != nil {
					return
				}
			}
		}()
		_, _ = windows.WaitForSingleObject(release, windows.INFINITE)
		_ = windows.CloseHandle(release)
		return
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var heldProof *cstB2HeldProof
	defer func() {
		if heldProof != nil && heldProof.heldNow() {
			if err := heldProof.continueHeld(); err != nil {
				t.Errorf("B2 probe held-event release on failure: %v", err)
			}
		}
	}()
	job, err := subprocess.NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	var original windows.Handle
	var cmd *exec.Cmd
	var files []*os.File
	var childRelease windows.Handle
	var readerDone chan error
	t.Cleanup(func() {
		if err := job.Close(); err != nil {
			t.Errorf("B2 probe inner Job close: %v", err)
		}
		settled := original == 0
		if original != 0 {
			result, err := windows.WaitForSingleObject(original, 500)
			settled = err == nil && result == windows.WAIT_OBJECT_0
			if !settled {
				t.Errorf("B2 probe original target wait unknown: result=%d err=%v", result, err)
			} else if err := windows.CloseHandle(original); err != nil {
				t.Errorf("B2 probe original target close: %v", err)
			}
		}
		if settled && cmd != nil && cmd.Process != nil {
			_ = cmd.Wait() // killed synthetic target is intentionally nonzero
		}
		for _, file := range files {
			if file != nil {
				_ = file.Close()
			}
		}
		if childRelease != 0 {
			_ = windows.CloseHandle(childRelease)
		}
		if readerDone != nil {
			select {
			case <-readerDone:
			case <-time.After(500 * time.Millisecond):
				t.Error("B2 probe stdout reader did not join after Job close")
			}
		}
	})
	release, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(release)
	if err := windows.DuplicateHandle(windows.CurrentProcess(), release, windows.CurrentProcess(), &childRelease, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
		t.Fatal(err)
	}
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, stdinRead, stdinWrite)
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, stdoutRead, stdoutWrite)
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, stderrRead, stderrWrite)
	cmd = exec.Command(os.Args[0], "-test.run=^TestCSTB2TargetOnlyHeldEventPipeProbe$", "-test.timeout=10s")
	cmd.Env = append(os.Environ(), "MCPHUB_CST_B2_PIPE_TARGET=1", fmt.Sprintf("MCPHUB_CST_B2_START_RELEASE=%d", childRelease))
	var targetPID uint32
	_, err = subprocess.StartCSTObserverTargetAtCreateForTest(job, cmd, stdinRead, stdoutWrite, stderrWrite, subprocess.CSTObserverAtCreateHooks{
		ChildHandles: []windows.Handle{childRelease},
		RetainOriginal: func(handle windows.Handle, pid uint32) error {
			original, targetPID = handle, pid
			return nil
		},
	})
	if err != nil || original == 0 || cmd.Process == nil || uint32(cmd.Process.Pid) != targetPID {
		t.Fatalf("B2 target at-create original unavailable: pid=%d err=%v", targetPID, err)
	}
	if err := stdinRead.Close(); err != nil {
		t.Fatal(err)
	}
	files[0] = nil
	if err := stdoutWrite.Close(); err != nil {
		t.Fatal(err)
	}
	files[3] = nil
	if err := stderrWrite.Close(); err != nil {
		t.Fatal(err)
	}
	files[5] = nil
	if err := windows.CloseHandle(childRelease); err != nil {
		t.Fatal(err)
	}
	childRelease = 0
	member, active, pids, err := subprocess.CSTObserverOriginalMemberForTest(job, original)
	if err != nil || !member || active == 0 || !cstFakeContainsPID(pids, targetPID) {
		t.Fatalf("B2 probe inner-Job original/membership: member=%v active=%d err=%v", member, active, err)
	}
	if result, err := windows.WaitForSingleObject(release, 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("B2 probe main start event not closed: result=%d err=%v", result, err)
	}
	responses := make(chan byte, 4)
	readerDone = make(chan error, 1)
	go func() {
		var value [1]byte
		for {
			_, err := io.ReadFull(stdoutRead, value[:])
			if err != nil {
				readerDone <- err
				return
			}
			responses <- value[0]
		}
	}()
	expectEcho := func(want byte, bound time.Duration) {
		t.Helper()
		select {
		case got := <-responses:
			if got != want {
				t.Fatalf("B2 probe echo=%q want=%q", got, want)
			}
		case <-time.After(bound):
			t.Fatalf("B2 probe echo %q absent within %s", want, bound)
		}
	}
	expectEcho('R', 500*time.Millisecond)
	if _, err := stdinWrite.Write([]byte{'N'}); err != nil {
		t.Fatal(err)
	}
	expectEcho('N', 500*time.Millisecond) // no-attach worker control
	heldProof, err = cstB2AttachHeld(targetPID, cstFakeObservationDeadline)
	if err != nil || !heldProof.heldNow() {
		t.Fatalf("B2 real held event absent: %v", err)
	}
	if _, err := stdinWrite.Write([]byte{'H'}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-responses:
		t.Fatalf("B2 worker ran while debug event held: echo=%q", got)
	case <-time.After(cstFakeObservationDeadline): // injected held interval, not a natural race
	}
	if err := heldProof.continueHeld(); err != nil {
		t.Fatal(err)
	}
	if heldProof.heldNow() {
		t.Fatal("continued B2 event still claimed held")
	}
	expectEcho('H', 500*time.Millisecond)
	if _, err := stdinWrite.Write([]byte{'C'}); err != nil {
		t.Fatal(err)
	}
	expectEcho('C', 500*time.Millisecond) // Continue-before-probe control
	t.Logf("B2 target-only one real held breakpoint, one held-event continuation, inner Job original PID=%d", targetPID)
}

var cstB2Kernel = syscall.NewLazyDLL("kernel32.dll")
var cstB2AttachProc = cstB2Kernel.NewProc("DebugActiveProcess")
var cstB2WaitProc = cstB2Kernel.NewProc("WaitForDebugEventEx")
var cstB2ContinueProc = cstB2Kernel.NewProc("ContinueDebugEvent")
var cstB2StopProc = cstB2Kernel.NewProc("DebugActiveProcessStop")

// cstB2HeldProof represents only the final, still-uncontinued breakpoint
// event. Earlier attach inventory events have already been continued on the
// same locked OS thread. A zero value cannot produce held-event evidence.
type cstB2HeldProof struct {
	event       cstB2DebugEvent
	ownerThread uint32
	targetPID   uint32
	held        bool
}

func (p *cstB2HeldProof) heldNow() bool {
	return p != nil && p.held && p.ownerThread != 0 && windows.GetCurrentThreadId() == p.ownerThread &&
		p.targetPID != 0 && p.event.Code == cstB2ExceptionEvent && p.event.ProcessID == p.targetPID &&
		p.event.ThreadID != 0 && uint32(p.event.Data[0]) == cstB2Breakpoint
}

// cstB2AttachHeld must run on the custodian's locked target-creating OS
// thread. Windows first enumerates process/thread/DLL state; only the final
// attach breakpoint remains held at the Task 2 barrier.
func cstB2AttachHeld(targetPID uint32, wait time.Duration) (proof *cstB2HeldProof, err error) {
	if targetPID == 0 || wait <= 0 || wait > cstFakeObservationDeadline {
		return nil, errors.New("invalid bounded B2 debug attach input")
	}
	for _, proc := range []*syscall.LazyProc{cstB2AttachProc, cstB2WaitProc, cstB2ContinueProc, cstB2StopProc} {
		if err := proc.Find(); err != nil {
			return nil, fmt.Errorf("missing installed B2 debug export %s: %w", proc.Name, err)
		}
	}
	ownerThread := windows.GetCurrentThreadId()
	deadline := time.Now().Add(wait)
	result, _, callErr := cstB2AttachProc.Call(uintptr(targetPID))
	if result == 0 {
		return nil, fmt.Errorf("DebugActiveProcess target=%d: %w", targetPID, callErr)
	}
	var pending cstB2DebugEvent
	defer func() {
		if proof != nil {
			return
		}
		if pending.ProcessID == targetPID && pending.ThreadID != 0 {
			if result, _, continueErr := cstB2ContinueProc.Call(uintptr(pending.ProcessID), uintptr(pending.ThreadID), cstB2ContinueStatus); result == 0 {
				err = errors.Join(err, fmt.Errorf("ContinueDebugEvent after failed held admission: %w", continueErr))
			}
		}
		result, _, stopErr := cstB2StopProc.Call(uintptr(targetPID))
		if result == 0 {
			err = errors.Join(err, fmt.Errorf("DebugActiveProcessStop after failed held admission: %w", stopErr))
		}
	}()
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, errors.New("B2 debug attach inventory deadline")
		}
		var event cstB2DebugEvent
		timeoutMS := uint32((remaining + time.Millisecond - 1) / time.Millisecond)
		result, _, callErr = cstB2WaitProc.Call(uintptr(unsafe.Pointer(&event)), uintptr(timeoutMS))
		if result == 0 {
			return nil, fmt.Errorf("WaitForDebugEventEx: %w", callErr)
		}
		pending = event
		if windows.GetCurrentThreadId() != ownerThread || event.ProcessID != targetPID || event.ThreadID == 0 {
			return nil, fmt.Errorf("B2 debug event owner/target mismatch: code=%d pid=%d tid=%d", event.Code, event.ProcessID, event.ThreadID)
		}
		switch event.Code {
		case cstB2CreateProcessEvent:
			if event.Data[1] == 0 || event.Data[2] == 0 {
				return nil, errors.New("B2 CREATE_PROCESS event lacks process/thread handles")
			}
			debugPID, idErr := windows.GetProcessId(windows.Handle(event.Data[1]))
			if idErr != nil || debugPID != targetPID {
				return nil, fmt.Errorf("B2 debug-event process handle identity: pid=%d err=%v", debugPID, idErr)
			}
			fallthrough
		case cstB2LoadDLLEvent:
			if event.Data[0] != 0 {
				if err := windows.CloseHandle(windows.Handle(event.Data[0])); err != nil {
					return nil, fmt.Errorf("B2 debug-event image/DLL handle close: %w", err)
				}
			}
		case cstB2CreateThreadEvent, cstB2UnloadDLLEvent:
			// Windows owns these event handles through the matching exit event.
		case cstB2ExceptionEvent:
			if uint32(event.Data[0]) != cstB2Breakpoint {
				return nil, fmt.Errorf("B2 first attach exception is not breakpoint: %#x", uint32(event.Data[0]))
			}
			proof = &cstB2HeldProof{event: event, ownerThread: ownerThread, targetPID: targetPID, held: true}
			if !proof.heldNow() {
				proof = nil
				return nil, errors.New("B2 final breakpoint lost owning-thread proof")
			}
			return proof, nil
		default:
			return nil, fmt.Errorf("unexpected B2 attach debug event %d", event.Code)
		}
		result, _, callErr = cstB2ContinueProc.Call(uintptr(event.ProcessID), uintptr(event.ThreadID), cstB2ContinueStatus)
		if result == 0 {
			return nil, fmt.Errorf("ContinueDebugEvent attach inventory: %w", callErr)
		}
		pending = cstB2DebugEvent{}
	}
}

// continueHeld is the exactly-one continuation of the final held event.
// It is used by the target-only echo oracle and continued-before-barrier cell.
func (p *cstB2HeldProof) continueHeld() error {
	return p.continueHeldWith(func(pid, thread, status uint32) error {
		result, _, callErr := cstB2ContinueProc.Call(uintptr(pid), uintptr(thread), uintptr(status))
		if result == 0 {
			return callErr
		}
		return nil
	})
}

// continueHeldWith is the tagged per-call Continue result injection seam.
// A failed call leaves the actual held event and sole debugger owner intact.
func (p *cstB2HeldProof) continueHeldWith(continueCall func(uint32, uint32, uint32) error) error {
	if !p.heldNow() || continueCall == nil {
		return errors.New("B2 debug continue lacks real held owner or call")
	}
	if err := continueCall(p.event.ProcessID, p.event.ThreadID, cstB2ContinueStatus); err != nil {
		return fmt.Errorf("ContinueDebugEvent: %w", err)
	}
	p.held = false
	result, _, callErr := cstB2StopProc.Call(uintptr(p.targetPID))
	if result == 0 {
		return fmt.Errorf("DebugActiveProcessStop after continue: %w", callErr)
	}
	return nil
}
