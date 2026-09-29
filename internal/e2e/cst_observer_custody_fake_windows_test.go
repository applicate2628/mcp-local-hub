//go:build windows && cst_vptr_probe

package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/e2e/cstwatchdogwire"
	subprocess "mcp-local-hub/internal/process"
)

type cstFakeB0Outcome struct {
	cause             string
	attempted         int
	completed         int
	fired             int
	finalizers        int
	roots             int
	guardians         int
	custodians        int
	targets           int
	quarantined       bool
	settled           bool
	pendingRead       int
	elapsed           time.Duration
	foreignDrift      bool
	sourceDrift       bool
	rootDeleted       bool
	member            bool
	active            uint32
	originalPID       uint32
	memberPIDs        []uint32
	barriers          int
	membersClassified bool
	tokenReturned     int
	blocked           string
}

const cstObserverCustodyMagic = "CSTOBS01"
const cstFakeObservationDeadline = 250 * time.Millisecond
const cstFakeCloseAllowance = 500 * time.Millisecond

var cstObserverCustodyCRC = crc32.MakeTable(crc32.Castagnoli)

var cstFakeAllocationCounters struct {
	roots      atomic.Int64
	guardians  atomic.Int64
	custodians atomic.Int64
	targets    atomic.Int64
}

var cstFakeUnsettledOriginals struct {
	sync.Mutex
	rows []struct {
		handle windows.Handle
		root   string
		cmd    *exec.Cmd
	}
}

func TestCSTObserverCustodyFakeFault(t *testing.T) {
	if os.Getenv("MCPHUB_CST_F4_CHILD") == "helper" {
		raw, err := strconv.ParseUint(os.Getenv("MCPHUB_CST_F4_HELPER_RELEASE"), 10, 64)
		if err != nil || raw == 0 {
			os.Exit(91)
		}
		release := windows.Handle(raw)
		result, err := windows.WaitForSingleObject(release, windows.INFINITE)
		_ = windows.CloseHandle(release)
		if err != nil || result != windows.WAIT_OBJECT_0 {
			os.Exit(92)
		}
		return
	}
	if os.Getenv("MCPHUB_CST_OBSERVER_CHILD") == "custodian" {
		if cstTask2TargetMode(os.Getenv("MCPHUB_CST_OBSERVER_MODE")) {
			cstFakeTask2Custodian()
			return
		}
		cstFakeB0Custodian()
		return
	}
	if os.Getenv("MCPHUB_CST_OBSERVER_CHILD") == "b1_target" {
		cstFakeB1Target()
		return
	}
	if os.Getenv("MCPHUB_CST_OBSERVER_CHILD") == "foreign" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	if os.Getenv("MCPHUB_CST_VPTR_FAKE_RUN") != "1" {
		if cstFakeAllocationCounters.roots.Load() != 0 || cstFakeAllocationCounters.guardians.Load() != 0 || cstFakeAllocationCounters.custodians.Load() != 0 || cstFakeAllocationCounters.targets.Load() != 0 {
			t.Fatal("opt-out allocated root, guardian, custodian or target")
		}
		t.Log("opt-out allocations root=0 guardian=0 custodian=0 target=0")
		return
	}
	selected := os.Getenv("MCPHUB_CST_F4_CELL")
	var f4 *f4TestHost
	if selected != "" {
		approved := map[string]bool{"hang_after_fired": true, "watchdog_postcreate_findprocess": true,
			"crash_ready_tie": true, "closed_control": true, "silence": true, "bad_magic": true,
			"precreate": true, "postcreate_transfer": true, "find_process": true, "find_process_nil": true, "b1_crash": true, "b2_crash": true,
			"posttoken_held_cancel": true}
		if !approved[selected] {
			t.Fatalf("unapproved F4 fake cell %q", selected)
		}
		if selected == "watchdog_postcreate_findprocess" {
			t.Run(selected, func(t *testing.T) { t.Fatal("second-launch FindProcess fault must prevent test-host entry") })
			return
		}
		var err error
		f4, err = f4BindTestHost(selected)
		if err != nil {
			t.Run(selected, func(t *testing.T) { t.Fatal(err) })
			return
		}
		defer func() {
			if err := f4.close(); err != nil {
				t.Errorf("F4 host control close: %v", err)
			}
		}()
		if err := f4.publish(cstwatchdogwire.Ready, 0, 0, cstwatchdogwire.CauseNone); err != nil {
			t.Fatal(err)
		}
		if f4.fault == "late_started" {
			// Force a receipt after the parent's fixed D; no natural race selects it.
			time.Sleep(2 * cstFakeObservationDeadline)
		}
		if selected == "hang_after_fired" {
			t.Run(selected, func(t *testing.T) {
				if err := f4.publish(cstwatchdogwire.Started, cstwatchdogwire.Attempted, 0, cstwatchdogwire.CauseNone); err != nil {
					t.Fatal(err)
				}
				_ = cstRunFakeB0(t, selected, f4)
				t.Fatal("acknowledged F4 hang cell returned without external termination")
			})
			return
		}
		if cstTask2TargetMode(selected) {
			t.Run(selected, func(t *testing.T) {
				if err := f4.publish(cstwatchdogwire.Started, cstwatchdogwire.Attempted, 0, cstwatchdogwire.CauseNone); err != nil {
					t.Fatal(err)
				}
				got := cstRunFakeB0(t, selected, f4)
				wantCause := "custodian_lost"
				if selected == "posttoken_held_cancel" {
					wantCause = f4.fault
				}
				if got.cause != wantCause || got.targets != 1 || got.barriers != 1 || got.fired != 1 || got.finalizers != 1 || !got.quarantined || got.settled || got.pendingRead != 0 {
					t.Fatalf("Task 2 at-create target custody incomplete: %+v", got)
				}
				if selected == "posttoken_held_cancel" && (got.tokenReturned != 1 || got.blocked != "BLOCKED:probe_preservation_failed") {
					t.Fatalf("post-token linear custody did not return typed non-success before finalizer: %+v", got)
				}
			})
			return
		}
	}
	if f4 == nil {
		t.Run("cell_codec", func(t *testing.T) {
			var nonce [16]byte
			copy(nonce[:], []byte("known-nonce-1234"))
			valid := cstEncodeObserverCell(nonce, 1, 1, 0)
			if err := cstValidateObserverCell(valid[:], valid[:], nonce, 1, 1); err != nil {
				t.Fatalf("valid 64-byte armed specimen rejected: %v", err)
			}
			for _, tc := range []struct {
				name   string
				change func(*[64]byte)
			}{
				{"nonce", func(c *[64]byte) { c[8] ^= 1 }},
				{"reserved", func(c *[64]byte) { c[40] = 1 }},
				{"crc", func(c *[64]byte) { c[56] ^= 1 }},
				{"stale_sequence", func(c *[64]byte) { binary.LittleEndian.PutUint64(c[24:32], 2) }},
				{"illegal_phase", func(c *[64]byte) { binary.LittleEndian.PutUint32(c[32:36], 3) }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					bad := valid
					tc.change(&bad)
					if tc.name != "crc" {
						binary.LittleEndian.PutUint32(bad[56:60], crc32.Checksum(bad[:56], cstObserverCustodyCRC))
					}
					if err := cstValidateObserverCell(bad[:], bad[:], nonce, 1, 1); err == nil {
						t.Fatal("mutated custody cell accepted")
					}
				})
			}
			torn := valid
			torn[1] ^= 1
			if err := cstValidateObserverCell(valid[:], torn[:], nonce, 1, 1); err == nil {
				t.Fatal("torn copy accepted")
			}
		})
	}
	for _, tc := range []struct{ name, cause string }{
		{"crash_ready_tie", "custodian_lost"},
		{"closed_control", "custodian_channel_fault"},
		{"silence", "custodian_silent"},
		{"bad_magic", "custodian_channel_fault"},
		{"precreate", "launch_precreate_failed"},
		{"postcreate_transfer", "post_create_identity_uncertain"},
		{"find_process", "post_create_identity_uncertain"},
		{"find_process_nil", "post_create_identity_uncertain"},
	} {
		if f4 != nil && tc.name != selected {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			if f4 != nil {
				if err := f4.publish(cstwatchdogwire.Started, cstwatchdogwire.Attempted, 0, cstwatchdogwire.CauseNone); err != nil {
					t.Fatal(err)
				}
			}
			got := cstRunFakeB0(t, tc.name, f4)
			wantCustodians, wantBarriers := 1, 1
			if tc.name == "precreate" {
				wantCustodians, wantBarriers = 0, 0
			} else if tc.name == "postcreate_transfer" || tc.name == "find_process" || tc.name == "find_process_nil" {
				wantBarriers = 0
			}
			if got.cause != tc.cause || got.attempted != 1 || got.completed != 1 || got.fired != 1 || got.finalizers != 1 || !got.quarantined || got.settled || got.pendingRead != 0 || got.targets != 0 || got.foreignDrift || got.sourceDrift || got.rootDeleted || got.elapsed > cstFakeObservationDeadline+cstFakeCloseAllowance {
				t.Fatalf("B0 %s: got %+v, want cause=%s attempted=completed=fired=finalizers=1, quarantine, no settlement/read, bounded", tc.name, got, tc.cause)
			}
			if got.roots != 1 || got.guardians != 1 || got.custodians != wantCustodians || got.barriers != wantBarriers || (wantCustodians == 1 && (!got.member || got.active == 0 || got.active != uint32(len(got.memberPIDs)) || !cstFakeContainsPID(got.memberPIDs, got.originalPID) || !got.membersClassified)) {
				t.Fatalf("B0 custody/membership barrier: got %+v, want one root/guardian, custodian=%d, original in complete live Job census, acknowledged barriers=%d", got, wantCustodians, wantBarriers)
			}
			if f4 != nil {
				if f4.fault != "evidence_missing" {
					if err := f4.writeEvidence(f4ChildEvidence{Attempted: 1, Barrier: got.barriers, Fired: 1, Finished: 1, Cause: got.cause}); err != nil {
						t.Fatal(err)
					}
				}
				flags := cstwatchdogwire.Attempted | cstwatchdogwire.InjectionFired | cstwatchdogwire.FinishedSeen
				if got.barriers == 1 {
					flags |= cstwatchdogwire.BarrierSeen
				}
				if err := f4.publish(cstwatchdogwire.Finished, flags, 0, f4CauseForB0(got.cause)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func cstRunFakeB0(t *testing.T, mode string, f4 *f4TestHost) (got cstFakeB0Outcome) {
	t.Helper()
	started := time.Now()
	got.attempted = 1
	var root string
	var err error
	if f4 != nil {
		root = f4.root // parent created and bound this exact root before START
		if _, err := os.Lstat(root); err != nil {
			t.Fatal(err)
		}
	} else {
		root, err = os.MkdirTemp("", "mcphub-cst-observer-fake-")
		if err != nil {
			t.Fatal(err)
		}
		cstFakeAllocationCounters.roots.Add(1)
	}
	got.roots = 1
	marker := filepath.Join(root, "custody.marker")
	if err := os.WriteFile(marker, []byte("pending\n"), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "synthetic-source.bin")
	sourceBytes := []byte("CST observer fake source; no native project\n")
	if err := os.WriteFile(source, sourceBytes, 0600); err != nil {
		t.Fatal(err)
	}
	var foreign *exec.Cmd
	var foreignHandle windows.Handle
	if f4 == nil {
		foreign = exec.Command(os.Args[0], "-test.run=^TestCSTObserverCustodyFakeFault$")
		foreign.Env = append(os.Environ(), "MCPHUB_CST_OBSERVER_CHILD=foreign")
		foreignInput, err := foreign.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := foreign.Start(); err != nil {
			t.Fatal(err)
		}
		foreignHandle, err = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(foreign.Process.Pid))
		if err != nil {
			_ = foreign.Process.Kill()
			_ = foreign.Wait()
			t.Fatal(err)
		}
		defer func() {
			_ = foreignInput.Close()
			if killErr := foreign.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				t.Errorf("owned foreign helper kill: %v", killErr)
			}
			result, waitErr := windows.WaitForSingleObject(foreignHandle, 500)
			if waitErr != nil || result != windows.WAIT_OBJECT_0 {
				t.Errorf("owned foreign helper did not settle: result=%d err=%v", result, waitErr)
				return // retain the original foreign handle until this test host exits
			}
			_ = foreign.Wait() // nonzero exit after Kill is expected
			if err := windows.CloseHandle(foreignHandle); err != nil {
				t.Errorf("owned foreign helper handle close: %v", err)
			}
		}()
	}
	t.Logf("test-owned exact root (quarantine on uncertainty): %s", root)
	guardian, err := subprocess.NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	got.guardians = 1
	cstFakeAllocationCounters.guardians.Add(1)
	stopAt := started.Add(cstFakeObservationDeadline)
	var original windows.Handle
	var command *exec.Cmd
	var childKnown bool
	finalize := func(cause string) {
		got.finalizers++
		got.cause = cause
		if err := guardian.Close(); err != nil {
			got.cause = "guardian_close_failed_settlement_unknown"
			t.Errorf("guardian close: %v", err)
		}
		if original != 0 {
			waitResult, waitErr := windows.WaitForSingleObject(original, uint32(cstFakeCloseAllowance/time.Millisecond))
			if waitErr != nil || waitResult != windows.WAIT_OBJECT_0 {
				t.Errorf("original custodian handle not signaled after guardian close: result=%d err=%v", waitResult, waitErr)
				cstFakeUnsettledOriginals.Lock()
				cstFakeUnsettledOriginals.rows = append(cstFakeUnsettledOriginals.rows, struct {
					handle windows.Handle
					root   string
					cmd    *exec.Cmd
				}{original, root, command})
				cstFakeUnsettledOriginals.Unlock()
			} else if command != nil && command.Process != nil {
				_ = command.Wait() // nonzero synthetic fault exits are expected
			}
			if waitErr == nil && waitResult == windows.WAIT_OBJECT_0 {
				if err := windows.CloseHandle(original); err != nil {
					t.Errorf("original custodian close: %v", err)
				}
			}
		}
		if err := os.WriteFile(marker, []byte("termination_unknown\n"), 0600); err != nil {
			t.Errorf("quarantine marker: %v", err)
		} else {
			got.quarantined = true
		}
		if currentSource, readErr := os.ReadFile(source); readErr != nil || !bytes.Equal(currentSource, sourceBytes) {
			got.sourceDrift = true
			t.Errorf("synthetic source changed: %v", readErr)
		}
		if _, statErr := os.Stat(root); statErr != nil {
			got.rootDeleted = os.IsNotExist(statErr)
			t.Errorf("quarantine root unavailable: %v", statErr)
		}
		if f4 == nil {
			if waitResult, waitErr := windows.WaitForSingleObject(foreignHandle, 0); waitErr != nil || waitResult != uint32(windows.WAIT_TIMEOUT) {
				got.foreignDrift = true
				t.Errorf("foreign original process changed: result=%d err=%v", waitResult, waitErr)
			}
		}
		got.completed = 1
		got.elapsed = time.Since(started)
		if !childKnown && mode != "precreate" && mode != "postcreate_transfer" {
			t.Errorf("custodian identity not retained: mode=%s", mode)
		}
	}
	defer func() {
		if got.finalizers == 0 {
			finalize("custodian_channel_fault")
		}
	}()

	mapping, err := windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE, 0, 64, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(mapping)
	view, err := windows.MapViewOfFile(mapping, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.UnmapViewOfFile(view)
	var task2Mapping windows.Handle
	var task2View uintptr
	if cstTask2TargetMode(mode) {
		task2Mapping, err = windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE, 0, cstwatchdogwire.Task2SlotSize, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer windows.CloseHandle(task2Mapping)
		task2View, err = windows.MapViewOfFile(task2Mapping, windows.FILE_MAP_READ, 0, 0, cstwatchdogwire.Task2SlotSize)
		if err != nil {
			t.Fatal(err)
		}
		defer windows.UnmapViewOfFile(task2View)
	}
	ready, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(ready)
	ack, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(ack)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	allowlist := make([]windows.Handle, 0, 4)
	childSources := []windows.Handle{mapping, ready, ack}
	if cstTask2TargetMode(mode) {
		childSources = append(childSources, task2Mapping)
	}
	for _, source := range childSources {
		var duplicate windows.Handle
		if err := windows.DuplicateHandle(windows.CurrentProcess(), source, windows.CurrentProcess(), &duplicate, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			t.Fatal(err)
		}
		allowlist = append(allowlist, duplicate)
	}
	defer func() {
		for _, handle := range allowlist {
			if err := windows.CloseHandle(handle); err != nil {
				t.Errorf("allowlist handle close: %v", err)
			}
		}
	}()

	path := os.Args[0]
	if mode == "precreate" {
		path = filepath.Join(root, "nonexistent-custodian.exe")
	}
	command = exec.Command(path, "-test.run=^TestCSTObserverCustodyFakeFault$")
	command.Env = append(os.Environ(),
		"MCPHUB_CST_OBSERVER_CHILD=custodian",
		"MCPHUB_CST_OBSERVER_MODE="+mode,
		"MCPHUB_CST_OBSERVER_NONCE="+hex.EncodeToString(nonce[:]),
		fmt.Sprintf("MCPHUB_CST_OBSERVER_HANDLES=%d,%d,%d", allowlist[0], allowlist[1], allowlist[2]),
	)
	if cstTask2TargetMode(mode) {
		command.Env = append(command.Env, fmt.Sprintf("MCPHUB_CST_OBSERVER_TASK2_SLOT=%d", allowlist[3]))
	}
	hooks := subprocess.CSTObserverAtCreateHooks{
		ChildHandles: allowlist,
		RetainOriginal: func(handle windows.Handle, pid uint32) error {
			member, active, pids, membershipErr := subprocess.CSTObserverOriginalMemberForTest(guardian, handle)
			if membershipErr != nil {
				return membershipErr
			}
			got.member, got.active, got.originalPID, got.memberPIDs = member, active, pid, pids
			classified, classifyErr := cstClassifyB0GuardianMembers(pids, pid)
			if classifyErr != nil {
				return classifyErr
			}
			got.membersClassified = classified
			if mode == "postcreate_transfer" {
				return errors.New("injected original-handle transfer failure")
			}
			original = handle
			childKnown = true
			return nil
		},
	}
	if mode == "find_process" {
		hooks.FindProcess = func(int) (*os.Process, error) { return nil, errors.New("injected FindProcess failure") }
	} else if mode == "find_process_nil" {
		hooks.FindProcess = func(int) (*os.Process, error) { return nil, nil }
	}
	_, launchErr := subprocess.StartCSTObserverAtCreateForTest(guardian, command, hooks)
	if mode == "precreate" {
		got.fired = 1
		if launchErr == nil || original != 0 {
			t.Errorf("pre-create failure did not remain pre-create: err=%v original=%v", launchErr, original)
		}
		if f4 != nil {
			if err := f4.fired(false); err != nil {
				t.Fatal(err)
			}
		}
		finalize("launch_precreate_failed")
		return got
	}
	got.custodians = 1
	cstFakeAllocationCounters.custodians.Add(1)
	if mode == "postcreate_transfer" || mode == "find_process" || mode == "find_process_nil" {
		got.fired = 1
		if launchErr == nil || !errors.Is(launchErr, subprocess.ErrSpawnPostCreate) {
			t.Errorf("post-create failure lacked typed phase: %v", launchErr)
		}
		if f4 != nil {
			if err := f4.fired(false); err != nil {
				t.Fatal(err)
			}
		}
		finalize("post_create_identity_uncertain")
		return got
	}
	if launchErr != nil || original == 0 {
		t.Fatalf("unexpected custodian launch failure: %v original=%v", launchErr, original)
	}
	remaining := time.Until(stopAt)
	if remaining <= 0 {
		t.Fatal("B0 deadline expired before barrier")
	}
	barrier, waitErr := windows.WaitForMultipleObjects([]windows.Handle{original, ready}, false, uint32(remaining/time.Millisecond))
	if waitErr != nil || barrier != windows.WAIT_OBJECT_0+1 {
		t.Fatalf("B0 two-sided ready barrier absent: result=%d err=%v", barrier, waitErr)
	}
	member, active, pids, membershipErr := subprocess.CSTObserverOriginalMemberForTest(guardian, original)
	if membershipErr != nil || !member || active == 0 || active != uint32(len(pids)) || !cstFakeContainsPID(pids, got.originalPID) {
		t.Fatalf("B0 original membership/census changed: member=%v active=%d members=%v custodian=%d err=%v", member, active, pids, got.originalPID, membershipErr)
	}
	got.member, got.active, got.memberPIDs = member, active, pids
	if cstTask2TargetMode(mode) {
		got.membersClassified, membershipErr = cstClassifyTask2GuardianMembers(pids, got.originalPID)
	} else {
		got.membersClassified, membershipErr = cstClassifyB0GuardianMembers(pids, got.originalPID)
	}
	if membershipErr != nil || !got.membersClassified {
		t.Fatalf("B0 guardian has unclassified member: pids=%v custodian=%d err=%v", pids, got.originalPID, membershipErr)
	}
	var task2 *cstwatchdogwire.Task2Evidence
	var custody *cstUnsettledCustody
	if cstTask2TargetMode(mode) {
		observation, _, _, err := cstFakeReadTask2Observation(view, task2View, nonce, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		task2 = &observation
		got.targets = 1
		if mode == "posttoken_held_cancel" {
			if err := windows.SetEvent(ack); err != nil { // acknowledge phase one, not the held debugger event
				t.Fatal(err)
			}
			remaining = time.Until(stopAt)
			if remaining <= 0 {
				t.Fatal("post-token phase-two deadline expired")
			}
			result, waitErr := windows.WaitForMultipleObjects([]windows.Handle{original, ready}, false, uint32(remaining/time.Millisecond))
			if waitErr != nil || result != windows.WAIT_OBJECT_0+1 {
				t.Fatalf("post-token phase-two two-sided barrier absent: result=%d err=%v", result, waitErr)
			}
			secondObservation, firstCell, secondCell, err := cstFakeReadTask2Observation(view, task2View, nonce, 2, 2)
			if err != nil {
				t.Fatal(err)
			}
			custody, err = cstAdmitUnsettledCustody(guardian, original, got.originalPID, firstCell[:], secondCell[:], nonce, finalize)
			if err != nil || secondObservation.ObserverState != "unsettled_owned" || secondObservation.Barrier != "B2" ||
				secondObservation.Debugger == nil || *secondObservation.Debugger.HeldEvents != 1 || !*secondObservation.Debugger.OwnerBound {
				t.Fatalf("post-token linear custody was not admitted: err=%v", err)
			}
			got.tokenReturned = 1
			got.blocked = "BLOCKED:probe_preservation_failed"
			task2 = &secondObservation
		}
		if err := f4.writeEvidence(f4ChildEvidence{Attempted: 1, Barrier: 1, Cause: "barrier_checkpoint", Task2: task2}); err != nil {
			t.Fatal(err)
		}
	}
	if f4 != nil {
		if err := f4.publish(cstwatchdogwire.Barrier, cstwatchdogwire.Attempted|cstwatchdogwire.BarrierSeen, original, cstwatchdogwire.CauseNone); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "crash_ready_tie" && f4 == nil {
		// A corrupted census that includes the live foreign process must fail
		// even though the real guardian member set is classified.
		bogus := append(append([]uint32(nil), pids...), uint32(foreign.Process.Pid))
		if classified, _ := cstClassifyB0GuardianMembers(bogus, got.originalPID); classified {
			t.Fatal("unrelated foreign process was accepted as guardian descendant")
		}
	}
	if err := windows.SetEvent(ack); err != nil {
		t.Fatal(err)
	}
	got.barriers = 1
	// The custodian acknowledged B0 before the injected fault; no target exists.
	if mode == "posttoken_held_cancel" {
		if custody == nil || !custody.admitted {
			t.Fatal("post-token guardian obligation lost before fault")
		}
		if result, err := windows.WaitForSingleObject(original, 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
			t.Fatalf("post-token custodian died before held-stop fault: result=%d err=%v", result, err)
		}
		switch f4.fault {
		case "observer_stop_deadline":
			got.fired = 1 // injection: withhold the third F3 stop request
			if err := f4.fired(true); err != nil {
				t.Fatal(err)
			}
			remaining = time.Until(stopAt)
			if remaining > 0 {
				result, err := windows.WaitForMultipleObjects([]windows.Handle{original, ready}, false, uint32(remaining/time.Millisecond))
				if err != nil || result != uint32(windows.WAIT_TIMEOUT) {
					t.Errorf("post-token stop-deadline silence changed: result=%d err=%v", result, err)
				}
			}
		case "continue_failed":
			if err := windows.SetEvent(ack); err != nil { // exact owner requests one held-event Continue
				t.Fatal(err)
			}
			remaining = time.Until(stopAt)
			if remaining <= 0 {
				t.Fatal("post-token Continue fault deadline expired")
			}
			result, waitErr := windows.WaitForMultipleObjects([]windows.Handle{original, ready}, false, uint32(remaining/time.Millisecond))
			if waitErr != nil || result != windows.WAIT_OBJECT_0+1 {
				t.Fatalf("post-token Continue failure phase-four absent: result=%d err=%v", result, waitErr)
			}
			thirdObservation, firstCell, _, err := cstFakeReadTask2Observation(view, task2View, nonce, 3, 4)
			if err != nil || binary.LittleEndian.Uint32(firstCell[36:40]) != cstObserverCauseContinueFailed ||
				thirdObservation.Debugger == nil || thirdObservation.Debugger.Continued != "fail" ||
				*thirdObservation.Debugger.HeldEvents != 1 || thirdObservation.Settlement.SettledExit == "pass" {
				t.Fatalf("post-token Continue failure status/slot unproved: err=%v", err)
			}
			if result, err := windows.WaitForSingleObject(original, 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
				t.Fatalf("post-token Continue failure producer died before receipt: result=%d err=%v", result, err)
			}
			task2 = &thirdObservation
			if err := windows.SetEvent(ack); err != nil { // acknowledge phase four before guardian finalizer
				t.Fatal(err)
			}
			got.fired = 1
			if err := f4.fired(true); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("unadmitted post-token fault reached live guardian")
		}
		if !custody.finishUnknown(f4.fault) {
			t.Fatal("post-token guardian finalized twice or not at all")
		}
		if err := f4.writeEvidence(f4ChildEvidence{Attempted: 1, Barrier: 1, Fired: 1, Finished: 1, Cause: got.cause, Task2: task2}); err != nil {
			t.Fatal(err)
		}
		if err := f4.publish(cstwatchdogwire.Finished, cstwatchdogwire.Attempted|cstwatchdogwire.BarrierSeen|cstwatchdogwire.InjectionFired|cstwatchdogwire.FinishedSeen, 0, cstwatchdogwire.CauseChildFault); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if cstTask2CrashMode(mode) {
		if err := windows.TerminateProcess(original, 71); err != nil {
			t.Fatal(err)
		}
		got.fired = 1
		if err := f4.fired(true); err != nil {
			t.Fatal(err)
		}
		finalize("custodian_lost")
		if err := f4.writeEvidence(f4ChildEvidence{Attempted: 1, Barrier: 1, Fired: 1, Finished: 1, Cause: got.cause, Task2: task2}); err != nil {
			t.Fatal(err)
		}
		if err := f4.publish(cstwatchdogwire.Finished, cstwatchdogwire.Attempted|cstwatchdogwire.BarrierSeen|cstwatchdogwire.InjectionFired|cstwatchdogwire.FinishedSeen, 0, cstwatchdogwire.CauseCustodianLost); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if mode == "hang_after_fired" {
		if f4 == nil {
			t.Fatal("hang injection requires an external parent")
		}
		got.fired = 1
		if err := f4.fired(true); err != nil {
			t.Fatal(err)
		}
		select {} // parent has acknowledged FIRED and owns termination
	}
	if mode == "closed_control" {
		got.fired = 1 // synthetic former control-EOF fault; no blocking read exists
		if f4 != nil {
			if err := f4.fired(true); err != nil {
				t.Fatal(err)
			}
		}
		finalize("custodian_channel_fault")
		return got
	}
	if mode == "silence" {
		got.fired = 1 // deliberately withhold the one acknowledgment
		if f4 != nil {
			if err := f4.fired(true); err != nil {
				t.Fatal(err)
			}
		}
		remaining = time.Until(stopAt)
		if remaining > 0 {
			result, err := windows.WaitForMultipleObjects([]windows.Handle{original, ready}, false, uint32(remaining/time.Millisecond))
			if err != nil || result != uint32(windows.WAIT_TIMEOUT) {
				t.Errorf("silence did not reach absolute deadline: result=%d err=%v", result, err)
			}
		}
		finalize("custodian_silent")
		return got
	}
	got.fired = 1
	if mode == "crash_ready_tie" {
		remaining = time.Until(stopAt)
		if remaining <= 0 {
			t.Fatal("crash deadline expired")
		}
		result, err := windows.WaitForSingleObject(original, uint32(remaining/time.Millisecond))
		if err != nil || result != windows.WAIT_OBJECT_0 {
			t.Errorf("custodian did not exit at acknowledged crash barrier: result=%d err=%v", result, err)
		}
		// The custodian deliberately signaled ready just before exiting. Both
		// handles are now signaled; index zero must win, not the stale ready.
		result, err = windows.WaitForMultipleObjects([]windows.Handle{original, ready}, false, 0)
		if err != nil || result != windows.WAIT_OBJECT_0 {
			t.Errorf("process did not win ready/death tie: result=%d err=%v", result, err)
		}
		if f4 != nil {
			if err := f4.fired(true); err != nil {
				t.Fatal(err)
			}
		}
		finalize("custodian_lost")
		return got
	}
	remaining = time.Until(stopAt)
	if remaining <= 0 {
		t.Fatal("bad-cell deadline expired")
	}
	result, err := windows.WaitForMultipleObjects([]windows.Handle{original, ready}, false, uint32(remaining/time.Millisecond))
	if err != nil || result != windows.WAIT_OBJECT_0+1 {
		t.Errorf("bad-cell ready absent: result=%d err=%v", result, err)
	}
	first, err := cstReadMappedCell(view)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cstReadMappedCell(view)
	if err != nil {
		t.Fatal(err)
	}
	if err := cstValidateObserverCell(first[:], second[:], nonce, 1, 1); err == nil {
		t.Error("valid-CRC bad-magic custody cell accepted")
	}
	if f4 != nil {
		if err := f4.fired(true); err != nil {
			t.Fatal(err)
		}
	}
	finalize("custodian_channel_fault")
	return got
}

func cstFakeB0Custodian() {
	parts := []windows.Handle{}
	for _, token := range splitCSTFakeHandles(os.Getenv("MCPHUB_CST_OBSERVER_HANDLES")) {
		n, err := strconv.ParseUint(token, 10, 64)
		if err != nil {
			os.Exit(91)
		}
		parts = append(parts, windows.Handle(n))
	}
	if len(parts) != 3 {
		os.Exit(92)
	}
	view, err := windows.MapViewOfFile(parts[0], windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, 64)
	if err != nil {
		os.Exit(93)
	}
	defer windows.UnmapViewOfFile(view)
	if err := windows.SetEvent(parts[1]); err != nil {
		os.Exit(94)
	}
	result, err := windows.WaitForSingleObject(parts[2], windows.INFINITE)
	if err != nil || result != windows.WAIT_OBJECT_0 {
		os.Exit(95)
	}
	switch os.Getenv("MCPHUB_CST_OBSERVER_MODE") {
	case "closed_control", "silence", "hang_after_fired":
		_, _ = windows.WaitForSingleObject(parts[2], windows.INFINITE)
	case "crash_ready_tie":
		_ = windows.SetEvent(parts[1])
		os.Exit(71)
	case "bad_magic":
		nonceBytes, err := hex.DecodeString(os.Getenv("MCPHUB_CST_OBSERVER_NONCE"))
		if err != nil || len(nonceBytes) != 16 {
			os.Exit(97)
		}
		var nonce [16]byte
		copy(nonce[:], nonceBytes)
		frame := cstEncodeObserverCell(nonce, 1, 1, 0)
		frame[0] = 'X'
		binary.LittleEndian.PutUint32(frame[56:60], crc32.Checksum(frame[:56], cstObserverCustodyCRC))
		if err := cstWriteMappedCell(view, frame); err != nil {
			os.Exit(98)
		}
		_ = windows.SetEvent(parts[1])
		_, _ = windows.WaitForSingleObject(parts[2], windows.INFINITE)
	}
	os.Exit(96)
}

func splitCSTFakeHandles(s string) []string {
	var parts []string
	start := 0
	for i := range s {
		if s[i] == ',' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

func cstFakeContainsPID(pids []uint32, wanted uint32) bool {
	for _, pid := range pids {
		if pid == wanted {
			return true
		}
	}
	return false
}

func cstEncodeObserverCell(nonce [16]byte, sequence uint64, phase, cause uint32) [64]byte {
	var cell [64]byte
	copy(cell[:8], cstObserverCustodyMagic)
	copy(cell[8:24], nonce[:])
	binary.LittleEndian.PutUint64(cell[24:32], sequence)
	binary.LittleEndian.PutUint32(cell[32:36], phase)
	binary.LittleEndian.PutUint32(cell[36:40], cause)
	binary.LittleEndian.PutUint32(cell[56:60], crc32.Checksum(cell[:56], cstObserverCustodyCRC))
	return cell
}

func cstValidateObserverCell(first, second []byte, nonce [16]byte, sequence uint64, expectedPhase uint32) error {
	if len(first) != 64 || len(second) != 64 || !bytes.Equal(first, second) {
		return errors.New("torn or wrong-sized custody cell")
	}
	if string(first[:8]) != cstObserverCustodyMagic || !bytes.Equal(first[8:24], nonce[:]) {
		return errors.New("custody cell magic or nonce mismatch")
	}
	if binary.LittleEndian.Uint64(first[24:32]) != sequence {
		return errors.New("custody cell sequence mismatch")
	}
	phase := binary.LittleEndian.Uint32(first[32:36])
	cause := binary.LittleEndian.Uint32(first[36:40])
	if phase < 1 || phase > 4 || phase != expectedPhase || (phase != 4 && cause != 0) || (phase == 4 && cause == 0) {
		return errors.New("custody cell phase or cause invalid")
	}
	for _, value := range first[40:56] {
		if value != 0 {
			return errors.New("custody cell reserved bytes nonzero")
		}
	}
	for _, value := range first[60:64] {
		if value != 0 {
			return errors.New("custody cell trailer nonzero")
		}
	}
	if binary.LittleEndian.Uint32(first[56:60]) != crc32.Checksum(first[:56], cstObserverCustodyCRC) {
		return errors.New("custody cell checksum mismatch")
	}
	return nil
}

func cstReadMappedCell(view uintptr) ([64]byte, error) {
	var cell [64]byte
	var copied uintptr
	if err := windows.ReadProcessMemory(windows.CurrentProcess(), view, &cell[0], uintptr(len(cell)), &copied); err != nil {
		return cell, err
	}
	if copied != uintptr(len(cell)) {
		return cell, fmt.Errorf("short mapped-cell read: %d", copied)
	}
	return cell, nil
}

// cstFakeReadTask2Observation is the host's one matched F3/slot read owner
// for both armed and post-token status. It never acknowledges a torn pair.
func cstFakeReadTask2Observation(cellView, slotView uintptr, nonce [16]byte, sequence uint64, phase uint32) (cstwatchdogwire.Task2Evidence, [64]byte, [64]byte, error) {
	first, err := cstReadMappedCell(cellView)
	if err != nil {
		return cstwatchdogwire.Task2Evidence{}, first, [64]byte{}, err
	}
	second, err := cstReadMappedCell(cellView)
	if err != nil {
		return cstwatchdogwire.Task2Evidence{}, first, second, err
	}
	if err := cstValidateObserverCell(first[:], second[:], nonce, sequence, phase); err != nil {
		return cstwatchdogwire.Task2Evidence{}, first, second, err
	}
	var firstSlot, secondSlot [cstwatchdogwire.Task2SlotSize]byte
	for _, slot := range []*[cstwatchdogwire.Task2SlotSize]byte{&firstSlot, &secondSlot} {
		var copied uintptr
		if err := windows.ReadProcessMemory(windows.CurrentProcess(), slotView, &slot[0], uintptr(len(slot)), &copied); err != nil || copied != uintptr(len(slot)) {
			return cstwatchdogwire.Task2Evidence{}, first, second, fmt.Errorf("Task 2 mapped slot read: copied=%d err=%v", copied, err)
		}
	}
	observation, err := cstwatchdogwire.DecodeTask2Slot(firstSlot[:], secondSlot[:], nonce, sequence)
	if err != nil {
		return cstwatchdogwire.Task2Evidence{}, first, second, err
	}
	nonceDigest := sha256.Sum256(nonce[:])
	slotDigest := sha256.Sum256(firstSlot[:])
	observation.F3NonceDigest = hex.EncodeToString(nonceDigest[:])
	observation.F3Sequence = sequence
	observation.SlotSHA256 = hex.EncodeToString(slotDigest[:])
	return observation, first, second, nil
}

func cstWriteMappedCell(view uintptr, cell [64]byte) error {
	var copied uintptr
	if err := windows.WriteProcessMemory(windows.CurrentProcess(), view, &cell[0], uintptr(len(cell)), &copied); err != nil {
		return err
	}
	if copied != uintptr(len(cell)) {
		return fmt.Errorf("short mapped-cell write: %d", copied)
	}
	return nil
}

// cstClassifyB0GuardianMembers uses a process snapshot only to classify
// guardian members; custody and waits continue to use the original handle.
// An extra member counts as owned only when the live custodian is its recorded
// parent; a particular Windows console-helper executable name is not required.
func cstClassifyB0GuardianMembers(pids []uint32, custodianPID uint32) (bool, error) {
	if !cstFakeContainsPID(pids, custodianPID) {
		return false, nil
	}
	if len(pids) == 1 {
		return true, nil
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(snapshot)
	entries := make(map[uint32]windows.ProcessEntry32)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return false, err
	}
	for {
		entries[entry.ProcessID] = entry
		entry.Size = uint32(unsafe.Sizeof(entry))
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return false, err
		}
	}
	for _, pid := range pids {
		if pid == custodianPID {
			continue
		}
		extra, found := entries[pid]
		if !found || extra.ParentProcessID != custodianPID || windows.UTF16ToString(extra.ExeFile[:]) == "" {
			return false, fmt.Errorf("unclassified guardian member pid=%d found=%v parent=%d image=%q custodian=%d", pid, found, extra.ParentProcessID, windows.UTF16ToString(extra.ExeFile[:]), custodianPID)
		}
	}
	return true, nil
}
