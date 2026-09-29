//go:build windows && cst_vptr_probe

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/e2e/cstwatchdogwire"
	subprocess "mcp-local-hub/internal/process"
)

func cstFakeB1Target() {
	raw, err := strconv.ParseUint(os.Getenv("MCPHUB_CST_B1_START_RELEASE"), 10, 64)
	if err != nil || raw == 0 {
		os.Exit(91)
	}
	release := windows.Handle(raw)
	result, err := windows.WaitForSingleObject(release, windows.INFINITE)
	_ = windows.CloseHandle(release)
	if err != nil || result != windows.WAIT_OBJECT_0 {
		os.Exit(92)
	}
}

func cstTask2CrashMode(mode string) bool { return mode == "b1_crash" || mode == "b2_crash" }
func cstTask2TargetMode(mode string) bool {
	return cstTask2CrashMode(mode) || mode == "posttoken_held_cancel"
}

// cstFakeTask2Custodian owns the sole target launcher, inner Job, start event
// and three target-only files. Its one matched observation precedes F3 ready.
func cstFakeTask2Custodian() {
	mode := os.Getenv("MCPHUB_CST_OBSERVER_MODE")
	if !cstTask2TargetMode(mode) {
		os.Exit(90)
	}
	if mode == "b2_crash" || mode == "posttoken_held_cancel" {
		runtime.LockOSThread() // includes the existing target CreateProcess and debug attach/wait
		defer runtime.UnlockOSThread()
	}
	parts := splitCSTFakeHandles(os.Getenv("MCPHUB_CST_OBSERVER_HANDLES"))
	if len(parts) != 3 {
		os.Exit(91)
	}
	var inherited [4]windows.Handle
	for i, token := range append(parts, os.Getenv("MCPHUB_CST_OBSERVER_TASK2_SLOT")) {
		n, err := strconv.ParseUint(token, 10, 64)
		if err != nil || n == 0 {
			os.Exit(92)
		}
		inherited[i] = windows.Handle(n)
	}
	cellView, err := windows.MapViewOfFile(inherited[0], windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, 64)
	if err != nil {
		os.Exit(93)
	}
	defer windows.UnmapViewOfFile(cellView)
	slotView, err := windows.MapViewOfFile(inherited[3], windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, cstwatchdogwire.Task2SlotSize)
	if err != nil {
		os.Exit(94)
	}
	defer windows.UnmapViewOfFile(slotView)
	nonceBytes, err := hex.DecodeString(os.Getenv("MCPHUB_CST_OBSERVER_NONCE"))
	if err != nil || len(nonceBytes) != 16 {
		os.Exit(95)
	}
	var nonce [16]byte
	copy(nonce[:], nonceBytes)
	inner, err := subprocess.NewKillOnCloseJob()
	if err != nil {
		os.Exit(96)
	}
	defer inner.Close()
	release, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		os.Exit(97)
	}
	defer windows.CloseHandle(release)
	var childRelease windows.Handle
	if err := windows.DuplicateHandle(windows.CurrentProcess(), release, windows.CurrentProcess(), &childRelease, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
		os.Exit(98)
	}
	defer windows.CloseHandle(childRelease)
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		os.Exit(99)
	}
	defer stdinRead.Close()
	defer stdinWrite.Close()
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		os.Exit(100)
	}
	defer stdoutRead.Close()
	defer stdoutWrite.Close()
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		os.Exit(101)
	}
	defer stderrRead.Close()
	defer stderrWrite.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCSTObserverCustodyFakeFault$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "MCPHUB_CST_OBSERVER_CHILD=b1_target", fmt.Sprintf("MCPHUB_CST_B1_START_RELEASE=%d", childRelease))
	var original windows.Handle
	var targetPID uint32
	_, err = subprocess.StartCSTObserverTargetAtCreateForTest(inner, cmd, stdinRead, stdoutWrite, stderrWrite, subprocess.CSTObserverAtCreateHooks{
		ChildHandles: []windows.Handle{childRelease},
		RetainOriginal: func(handle windows.Handle, pid uint32) error {
			original, targetPID = handle, pid
			return nil
		},
	})
	if err != nil || original == 0 || cmd.Process == nil || uint32(cmd.Process.Pid) != targetPID {
		os.Exit(102)
	}
	defer windows.CloseHandle(original)
	if err := stdinRead.Close(); err != nil {
		os.Exit(103)
	}
	if err := stdoutWrite.Close(); err != nil {
		os.Exit(104)
	}
	if err := stderrWrite.Close(); err != nil {
		os.Exit(105)
	}
	member, active, pids, err := subprocess.CSTObserverOriginalMemberForTest(inner, original)
	if err != nil || !member || active == 0 || !cstFakeContainsPID(pids, targetPID) {
		os.Exit(106)
	}
	if result, err := windows.WaitForSingleObject(release, 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
		os.Exit(107)
	}
	if result, err := windows.WaitForSingleObject(original, 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
		os.Exit(108)
	}
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(original, &created, &exited, &kernel, &user); err != nil || created.Nanoseconds() <= 0 {
		os.Exit(109)
	}
	binaryBytes, err := os.ReadFile(os.Args[0])
	if err != nil {
		os.Exit(110)
	}
	imageHash := sha256.Sum256(binaryBytes)
	truth := true
	observation := cstwatchdogwire.Task2Evidence{
		Barrier: "B1", ObserverState: "none",
		Target:     &cstwatchdogwire.Task2Target{PID: targetPID, CreationTicks: uint64(created.Nanoseconds()), ImageSHA256: hex.EncodeToString(imageHash[:]), OriginalRetained: &truth, StartClosed: &truth, InnerMember: &truth},
		Settlement: &cstwatchdogwire.Task2Settlement{TargetJobClose: "unknown", ChildWait: "unknown", StdinJoin: "unknown", StdoutEOF: "unknown", StderrEOF: "unknown", SettledExit: "unknown"},
	}
	var proof *cstB2HeldProof
	if mode == "b2_crash" || mode == "posttoken_held_cancel" {
		proof, err = cstB2AttachHeld(targetPID, cstFakeObservationDeadline)
		if err != nil || !proof.heldNow() {
			os.Exit(117)
		}
		held := 1
		observation.Barrier = "B2"
		observation.Debugger = &cstwatchdogwire.Task2Debugger{HeldEvents: &held, OwnerBound: &truth,
			Continued: "unknown", Detached: "unknown", StopAck: "unknown"}
	}
	slot, err := cstwatchdogwire.EncodeTask2Slot(nonce, 1, observation)
	if err != nil {
		os.Exit(111)
	}
	var copied uintptr
	if err := windows.WriteProcessMemory(windows.CurrentProcess(), slotView, &slot[0], uintptr(len(slot)), &copied); err != nil || copied != uintptr(len(slot)) {
		os.Exit(112)
	}
	if err := cstWriteMappedCell(cellView, cstEncodeObserverCell(nonce, 1, 1, 0)); err != nil {
		os.Exit(113)
	}
	if err := windows.SetEvent(inherited[1]); err != nil {
		os.Exit(114)
	}
	if result, err := windows.WaitForSingleObject(inherited[2], windows.INFINITE); err != nil || result != windows.WAIT_OBJECT_0 {
		os.Exit(115)
	}
	if mode == "posttoken_held_cancel" {
		fault := os.Getenv("MCPHUB_CST_F4_FAULT")
		if (fault != "observer_stop_deadline" && fault != "continue_failed") || !proof.heldNow() {
			os.Exit(118)
		}
		observation.ObserverState = "unsettled_owned"
		secondSlot, err := cstwatchdogwire.EncodeTask2Slot(nonce, 2, observation)
		if err != nil {
			os.Exit(119)
		}
		var secondCopied uintptr
		if err := windows.WriteProcessMemory(windows.CurrentProcess(), slotView, &secondSlot[0], uintptr(len(secondSlot)), &secondCopied); err != nil || secondCopied != uintptr(len(secondSlot)) {
			os.Exit(120)
		}
		if err := cstWriteMappedCell(cellView, cstEncodeObserverCell(nonce, 2, 2, 0)); err != nil {
			os.Exit(121)
		}
		if err := windows.SetEvent(inherited[1]); err != nil {
			os.Exit(122)
		}
		if result, err := windows.WaitForSingleObject(inherited[2], windows.INFINITE); err != nil || result != windows.WAIT_OBJECT_0 {
			os.Exit(123)
		}
		// Receipt of phase two is not permission to continue the held event.
		if result, err := windows.WaitForSingleObject(inherited[2], windows.INFINITE); err != nil || result != windows.WAIT_OBJECT_0 {
			os.Exit(124)
		}
		if fault == "continue_failed" {
			attempts := 0
			continueErr := proof.continueHeldWith(func(uint32, uint32, uint32) error {
				attempts++
				return errors.New("injected ContinueDebugEvent failure")
			})
			if continueErr == nil || attempts != 1 || !proof.heldNow() {
				os.Exit(125)
			}
			observation.Debugger.Continued = "fail"
			thirdSlot, err := cstwatchdogwire.EncodeTask2Slot(nonce, 3, observation)
			if err != nil {
				os.Exit(126)
			}
			var thirdCopied uintptr
			if err := windows.WriteProcessMemory(windows.CurrentProcess(), slotView, &thirdSlot[0], uintptr(len(thirdSlot)), &thirdCopied); err != nil || thirdCopied != uintptr(len(thirdSlot)) {
				os.Exit(127)
			}
			if err := cstWriteMappedCell(cellView, cstEncodeObserverCell(nonce, 3, 4, cstObserverCauseContinueFailed)); err != nil {
				os.Exit(128)
			}
			if err := windows.SetEvent(inherited[1]); err != nil {
				os.Exit(129)
			}
			if result, err := windows.WaitForSingleObject(inherited[2], windows.INFINITE); err != nil || result != windows.WAIT_OBJECT_0 {
				os.Exit(130)
			}
		}
		_, _ = windows.WaitForSingleObject(inherited[2], windows.INFINITE)
		os.Exit(124)
	}
	_, _ = windows.WaitForSingleObject(inherited[2], windows.INFINITE)
	os.Exit(116) // F3 receipt alone cannot release the target start event.
}

// Task 2 permits the one target and classified descendants in the same guardian;
// unlike B0 it does not require every extra member to be a direct child.
func cstClassifyTask2GuardianMembers(pids []uint32, custodianPID uint32) (bool, error) {
	if !cstFakeContainsPID(pids, custodianPID) || len(pids) < 2 {
		return false, errors.New("Task 2 target absent from guardian census")
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(snapshot)
	parents := map[uint32]uint32{}
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return false, err
	}
	for {
		parents[entry.ProcessID] = entry.ParentProcessID
		entry.Size = uint32(unsafe.Sizeof(entry))
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if err == windows.ERROR_NO_MORE_FILES {
				break
			}
			return false, err
		}
	}
	for _, pid := range pids {
		if pid == custodianPID {
			continue
		}
		seen := map[uint32]bool{}
		for current := pid; current != custodianPID; current = parents[current] {
			if current == 0 || seen[current] || len(seen) > len(pids) {
				return false, fmt.Errorf("Task 2 unclassified guardian member %d", pid)
			}
			seen[current] = true
		}
	}
	return true, nil
}
