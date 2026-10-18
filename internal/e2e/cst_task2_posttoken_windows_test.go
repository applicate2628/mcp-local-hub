//go:build windows && cst_vptr_probe

package e2e

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/e2e/cstwatchdogwire"
	subprocess "mcp-local-hub/internal/process"
)

const cstObserverCauseContinueFailed uint32 = 1

func TestCSTPostTokenPhaseTwoRequiresMatchedSlotSequence(t *testing.T) {
	cellMap, err := windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE, 0, 64, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(cellMap)
	cellView, err := windows.MapViewOfFile(cellMap, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.UnmapViewOfFile(cellView)
	slotMap, err := windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE, 0, cstwatchdogwire.Task2SlotSize, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(slotMap)
	slotView, err := windows.MapViewOfFile(slotMap, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, cstwatchdogwire.Task2SlotSize)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.UnmapViewOfFile(slotView)
	var nonce [16]byte
	owned, held := true, 1
	observation := cstwatchdogwire.Task2Evidence{Barrier: "B2", ObserverState: "unsettled_owned",
		Target: &cstwatchdogwire.Task2Target{PID: 101, CreationTicks: 1, ImageSHA256: strings.Repeat("c", 64),
			OriginalRetained: &owned, StartClosed: &owned, InnerMember: &owned},
		Debugger: &cstwatchdogwire.Task2Debugger{HeldEvents: &held, OwnerBound: &owned,
			Continued: "unknown", Detached: "unknown", StopAck: "unknown"},
		Settlement: &cstwatchdogwire.Task2Settlement{TargetJobClose: "unknown", ChildWait: "unknown", StdinJoin: "unknown",
			StdoutEOF: "unknown", StderrEOF: "unknown", SettledExit: "unknown"}}
	writeSlot := func(sequence uint64) {
		t.Helper()
		slot, err := cstwatchdogwire.EncodeTask2Slot(nonce, sequence, observation)
		if err != nil {
			t.Fatal(err)
		}
		var copied uintptr
		if err := windows.WriteProcessMemory(windows.CurrentProcess(), slotView, &slot[0], uintptr(len(slot)), &copied); err != nil || copied != uintptr(len(slot)) {
			t.Fatalf("write exact Task 2 slot: copied=%d err=%v", copied, err)
		}
	}
	writeSlot(2)
	if err := cstWriteMappedCell(cellView, cstEncodeObserverCell(nonce, 2, 2, 0)); err != nil {
		t.Fatal(err)
	}
	got, first, second, err := cstFakeReadTask2Observation(cellView, slotView, nonce, 2, 2)
	if err != nil || got.ObserverState != "unsettled_owned" || got.F3Sequence != 2 ||
		got.Debugger == nil || *got.Debugger.HeldEvents != 1 || first != second {
		t.Fatalf("matched post-token slot/status rejected: got=%+v err=%v", got, err)
	}
	writeSlot(1)
	if _, _, _, err := cstFakeReadTask2Observation(cellView, slotView, nonce, 2, 2); err == nil {
		t.Fatal("stale Task 2 slot sequence admitted against valid phase-two F3 status")
	}
	observation.Debugger.Continued = "fail"
	writeSlot(3)
	if err := cstWriteMappedCell(cellView, cstEncodeObserverCell(nonce, 3, 4, cstObserverCauseContinueFailed)); err != nil {
		t.Fatal(err)
	}
	failed, firstFault, secondFault, err := cstFakeReadTask2Observation(cellView, slotView, nonce, 3, 4)
	if err != nil || firstFault != secondFault || failed.Debugger == nil || failed.Debugger.Continued != "fail" ||
		*failed.Debugger.HeldEvents != 1 || binary.LittleEndian.Uint32(firstFault[36:40]) != cstObserverCauseContinueFailed {
		t.Fatalf("matched phase-four Continue failure lost typed held state: value=%+v err=%v", failed, err)
	}
}

// cstUnsettledCustody is a private linear host obligation. It contains no
// target Job/debugger/pipe handle; settle invokes the host's already-owned
// guardian finalizer exactly once, never a second cleanup implementation.
type cstUnsettledCustody struct {
	guardian *subprocess.Job
	original windows.Handle
	admitted bool
	consumed bool
	settle   func(string)
}

func cstAdmitUnsettledCustody(guardian *subprocess.Job, original windows.Handle, expectedPID uint32,
	first, second []byte, nonce [16]byte, settle func(string)) (*cstUnsettledCustody, error) {
	if guardian == nil || guardian.Handle() == 0 || original == 0 || expectedPID == 0 || settle == nil {
		return nil, errors.New("post-token guardian/original/finalizer absent")
	}
	if err := cstValidateObserverCell(first, second, nonce, 2, 2); err != nil {
		return nil, fmt.Errorf("post-token phase-two F3 status: %w", err)
	}
	pid, err := windows.GetProcessId(original)
	if err != nil || pid != expectedPID {
		return nil, fmt.Errorf("post-token original process identity: pid=%d expected=%d err=%v", pid, expectedPID, err)
	}
	result, err := windows.WaitForSingleObject(original, 0)
	if err != nil || result != uint32(windows.WAIT_TIMEOUT) {
		return nil, fmt.Errorf("post-token original custodian not live: result=%d err=%v", result, err)
	}
	member, active, pids, err := subprocess.CSTObserverOriginalMemberForTest(guardian, original)
	if err != nil || !member || active == 0 || active != uint32(len(pids)) || !cstFakeContainsPID(pids, expectedPID) {
		return nil, fmt.Errorf("post-token original guardian membership uncertain: member=%v active=%d listed=%d err=%v", member, active, len(pids), err)
	}
	return &cstUnsettledCustody{guardian: guardian, original: original, admitted: true, settle: settle}, nil
}

func (c *cstUnsettledCustody) finishUnknown(cause string) bool {
	if c == nil || !c.admitted || c.consumed || c.settle == nil {
		return false
	}
	c.consumed = true
	c.settle(cause)
	return true
}

func TestCSTPostTokenAdmissionRejectsUnownedOriginalAndBadPhaseTwo(t *testing.T) {
	guardian, err := subprocess.NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := guardian.Close(); err != nil {
			t.Errorf("empty post-token test Job close: %v", err)
		}
	})
	var nonce [16]byte
	good := cstEncodeObserverCell(nonce, 2, 2, 0)
	bad := good
	bad[0] ^= 1
	for _, tc := range []struct {
		name          string
		first, second [64]byte
	}{
		{"self_outside_guardian", good, good},
		{"malformed_phase_two", bad, bad},
		{"torn_phase_two", good, bad},
		{"stale_sequence", cstEncodeObserverCell(nonce, 1, 2, 0), cstEncodeObserverCell(nonce, 1, 2, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if token, err := cstAdmitUnsettledCustody(guardian, windows.CurrentProcess(), uint32(os.Getpid()), tc.first[:], tc.second[:], nonce, func(string) {}); err == nil || token != nil {
				t.Fatal("post-token admitted a foreign original, malformed/torn or stale F3 phase two")
			}
		})
	}
}

func TestCSTPostTokenGuardianFinalizerIsLinear(t *testing.T) {
	calls := 0
	token := &cstUnsettledCustody{admitted: true, settle: func(string) { calls++ }}
	if !token.finishUnknown("observer_stop_deadline") || token.finishUnknown("late_status") || calls != 1 {
		t.Fatalf("one guardian finalizer violated: calls=%d", calls)
	}
	if (&cstUnsettledCustody{}).finishUnknown("missing_token") {
		t.Fatal("zero post-token custody executed a guardian finalizer")
	}
}
