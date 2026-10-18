//go:build windows && cst_vptr_probe

package main

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/e2e/cstwatchdogwire"
)

func TestF4ParentRejectsReadyAfterDeadline(t *testing.T) {
	var nonce, token [16]byte
	binding := cstwatchdogwire.NewBinding(nonce, "silence", token)
	control, err := f4NewControl(binding)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := control.close(); err != nil {
			t.Errorf("control cleanup: %v", err)
		}
	}()
	raw, err := cstwatchdogwire.Encode(cstwatchdogwire.Frame{Binding: binding, Sequence: 1, Phase: cstwatchdogwire.Ready})
	if err != nil {
		t.Fatal(err)
	}
	var written uintptr
	if err := windows.WriteProcessMemory(windows.CurrentProcess(), control.view, &raw[0], uintptr(len(raw)), &written); err != nil || written != uintptr(len(raw)) {
		t.Fatalf("fixture write n=%d err=%v", written, err)
	}
	if err := windows.SetEvent(control.ready); err != nil {
		t.Fatal(err)
	}
	var original windows.Handle
	if err := windows.DuplicateHandle(windows.CurrentProcess(), windows.CurrentProcess(), windows.CurrentProcess(), &original, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(original)
	if _, err := control.poll(original, time.Now().Add(-time.Millisecond)); !errors.Is(err, errF4WaitDeadline) {
		t.Fatalf("late ready frame upgraded after deadline: %v", err)
	}
}

func TestF4ParentFaultAllowlist(t *testing.T) {
	for _, tc := range []struct {
		cell, fault string
		want        bool
	}{
		{"precreate", "", true},
		{"precreate", "omit_ready", true},
		{"precreate", "late_started", true},
		{"precreate", "wrong_cell_started", true},
		{"precreate", "fired_extra_barrier", true},
		{"precreate", "corrupt_ready", true},
		{"precreate", "evidence_missing", true},
		{"closed_control", "duplicate_fired", true},
		{"silence", "duplicate_fired", false},
		{"closed_control", "evidence_missing", false},
		{"closed_control", "fired_extra_barrier", false},
		{"precreate", "unknown", false},
	} {
		if got := f4AllowedFault(tc.cell, tc.fault); got != tc.want {
			t.Errorf("cell=%q fault=%q allowed=%v want=%v", tc.cell, tc.fault, got, tc.want)
		}
	}
}

func TestF4ParentB1CellRequiresCustodianBarrier(t *testing.T) {
	if !f4AllowedCell("b1_crash") {
		t.Fatal("B1 crash has no externally watched parent cell")
	}
	if !f4NeedsBarrier("b1_crash") {
		t.Fatal("B1 crash can start without parent-acknowledged custodian barrier")
	}
	cause, name := f4ExpectedCause("b1_crash", "")
	if cause != cstwatchdogwire.CauseCustodianLost || name != "custodian_lost" {
		t.Fatalf("B1 crash cause not bound to custodian loss: id=%d name=%q", cause, name)
	}
}

func TestF4ParentB2CellRequiresCustodianBarrier(t *testing.T) {
	if !f4AllowedCell("b2_crash") {
		t.Fatal("B2 crash has no externally watched parent cell")
	}
	if !f4NeedsBarrier("b2_crash") {
		t.Fatal("B2 crash can start without parent-acknowledged custodian barrier")
	}
	cause, name := f4ExpectedCause("b2_crash", "")
	if cause != cstwatchdogwire.CauseCustodianLost || name != "custodian_lost" {
		t.Fatalf("B2 crash cause not bound to custodian loss: id=%d name=%q", cause, name)
	}
}

func TestF4ParentPostTokenFaultAdmission(t *testing.T) {
	const cell = "posttoken_held_cancel"
	if !f4AllowedCell(cell) || !f4NeedsBarrier(cell) {
		t.Error("post-token fault has no external parent and custodian barrier owner")
	}
	if f4AllowedFault(cell, "") || !f4AllowedFault(cell, "observer_stop_deadline") || !f4AllowedFault(cell, "continue_failed") {
		t.Error("post-token held-cancel must select only admitted stop-deadline and continue-failure faults")
	}
	// The remaining accepted PAO faults stay explicitly non-runnable until
	// their own custodian/finalizer oracle is implemented and verified.
	for _, fault := range []string{
		"detach_failed", "target_job_terminate_failed",
		"target_job_close_failed", "child_wait_unsettled", "stdin_join_fault", "stdout_eof_fault",
		"stderr_eof_fault", "late_status", "guardian_close_failed",
	} {
		if f4AllowedFault(cell, fault) {
			t.Errorf("unimplemented post-token fault %q became runnable", fault)
		}
	}
}

func TestF4ParentPostTokenContinueFailureCauseIsExact(t *testing.T) {
	cause, name := f4ExpectedCause("posttoken_held_cancel", "continue_failed")
	if cause != cstwatchdogwire.CauseChildFault || name != "continue_failed" {
		t.Fatalf("continue failure cause not selected: id=%d name=%q", cause, name)
	}
}

func TestF4ParentRejectsValidlyEncodedWrongOrderAndCell(t *testing.T) {
	var nonce, token [16]byte
	binding := cstwatchdogwire.NewBinding(nonce, "precreate", token)
	started := cstwatchdogwire.Frame{Binding: binding, Sequence: 2, Phase: cstwatchdogwire.Started, Flags: cstwatchdogwire.Attempted}
	duplicate := started
	duplicate.Sequence = 3
	wrongCell := started
	wrongCell.Binding = cstwatchdogwire.NewBinding(nonce, "wrong-cell", token)
	for _, tc := range []struct {
		name       string
		control    f4Control
		frame      cstwatchdogwire.Frame
		wrongBound bool
	}{
		{"duplicate_started", f4Control{lastPhase: cstwatchdogwire.Started, lastSequence: 2, lastFlags: cstwatchdogwire.Attempted}, duplicate, false},
		{"late_terminal", f4Control{lastPhase: cstwatchdogwire.Finished, lastSequence: 4, lastFlags: cstwatchdogwire.Attempted | cstwatchdogwire.InjectionFired | cstwatchdogwire.FinishedSeen}, cstwatchdogwire.Frame{Binding: binding, Sequence: 5, Phase: cstwatchdogwire.Finished, Flags: cstwatchdogwire.Attempted | cstwatchdogwire.InjectionFired | cstwatchdogwire.FinishedSeen, Cause: cstwatchdogwire.CauseLaunchPrecreate}, false},
		{"wrong_cell_crc_valid", f4Control{lastPhase: cstwatchdogwire.Ready, lastSequence: 1}, wrongCell, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := cstwatchdogwire.Encode(tc.frame)
			if err != nil {
				t.Fatalf("fixture must be a valid encoded frame: %v", err)
			}
			if tc.wrongBound {
				if _, err := cstwatchdogwire.Decode(raw[:], raw[:], tc.frame.Binding); err != nil {
					t.Fatalf("wrong-cell frame must have valid CRC and shape: %v", err)
				}
				if _, err := cstwatchdogwire.Decode(raw[:], raw[:], binding); err == nil {
					t.Fatal("CRC-valid foreign cell admitted under parent binding")
				}
				return
			}
			decoded, err := cstwatchdogwire.Decode(raw[:], raw[:], binding)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.control.validateNext(decoded, false); err == nil {
				t.Fatal("duplicate or late receipt upgraded parent state")
			}
		})
	}
}

func TestF4ParentRejectsUnwitnessedPhaseBits(t *testing.T) {
	var nonce, token [16]byte
	binding := cstwatchdogwire.NewBinding(nonce, "precreate", token)
	for _, tc := range []struct {
		name           string
		lastPhase      uint32
		lastSequence   uint64
		lastFlags      uint32
		frame          cstwatchdogwire.Frame
		requireBarrier bool
	}{
		{
			name:      "fault_claims_unacknowledged_fired",
			lastPhase: cstwatchdogwire.Started, lastSequence: 2, lastFlags: cstwatchdogwire.Attempted,
			frame: cstwatchdogwire.Frame{Binding: binding, Sequence: 3, Phase: cstwatchdogwire.Fault,
				Flags: cstwatchdogwire.Attempted | cstwatchdogwire.InjectionFired, Cause: cstwatchdogwire.CauseChildFault},
		},
		{
			name:      "fault_claims_unacknowledged_barrier",
			lastPhase: cstwatchdogwire.Started, lastSequence: 2, lastFlags: cstwatchdogwire.Attempted,
			frame: cstwatchdogwire.Frame{Binding: binding, Sequence: 3, Phase: cstwatchdogwire.Fault,
				Flags: cstwatchdogwire.Attempted | cstwatchdogwire.BarrierSeen, Cause: cstwatchdogwire.CauseChildFault},
			requireBarrier: true,
		},
		{
			name:      "no_barrier_fired_claims_barrier",
			lastPhase: cstwatchdogwire.Started, lastSequence: 2, lastFlags: cstwatchdogwire.Attempted,
			frame: cstwatchdogwire.Frame{Binding: binding, Sequence: 3, Phase: cstwatchdogwire.Fired,
				Flags: cstwatchdogwire.Attempted | cstwatchdogwire.BarrierSeen | cstwatchdogwire.InjectionFired},
		},
		{
			name:      "no_barrier_finished_claims_barrier",
			lastPhase: cstwatchdogwire.Fired, lastSequence: 3,
			lastFlags: cstwatchdogwire.Attempted | cstwatchdogwire.InjectionFired,
			frame: cstwatchdogwire.Frame{Binding: binding, Sequence: 4, Phase: cstwatchdogwire.Finished,
				Flags: cstwatchdogwire.Attempted | cstwatchdogwire.BarrierSeen | cstwatchdogwire.InjectionFired | cstwatchdogwire.FinishedSeen,
				Cause: cstwatchdogwire.CauseLaunchPrecreate},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := cstwatchdogwire.Encode(tc.frame)
			if err != nil {
				t.Fatalf("fixture must be wire-valid: %v", err)
			}
			decoded, err := cstwatchdogwire.Decode(raw[:], raw[:], binding)
			if err != nil {
				t.Fatalf("fixture must have valid CRC and binding: %v", err)
			}
			control := f4Control{lastPhase: tc.lastPhase, lastSequence: tc.lastSequence, lastFlags: tc.lastFlags}
			if err := control.validateNext(decoded, tc.requireBarrier); err == nil {
				t.Fatal("parent accepted a receipt bit without its acknowledged phase")
			}
		})
	}
}

func TestF4ParentAcceptsExactWitnessedPhaseBits(t *testing.T) {
	var nonce, token [16]byte
	binding := cstwatchdogwire.NewBinding(nonce, "precreate", token)
	for _, tc := range []struct {
		name           string
		control        f4Control
		frame          cstwatchdogwire.Frame
		requireBarrier bool
	}{
		{
			name:    "fault_retains_only_started",
			control: f4Control{lastPhase: cstwatchdogwire.Started, lastSequence: 2, lastFlags: cstwatchdogwire.Attempted},
			frame: cstwatchdogwire.Frame{Binding: binding, Sequence: 3, Phase: cstwatchdogwire.Fault,
				Flags: cstwatchdogwire.Attempted, Cause: cstwatchdogwire.CauseChildFault},
		},
		{
			name:    "no_barrier_fired",
			control: f4Control{lastPhase: cstwatchdogwire.Started, lastSequence: 2, lastFlags: cstwatchdogwire.Attempted},
			frame: cstwatchdogwire.Frame{Binding: binding, Sequence: 3, Phase: cstwatchdogwire.Fired,
				Flags: cstwatchdogwire.Attempted | cstwatchdogwire.InjectionFired},
		},
		{
			name: "acknowledged_barrier_fired",
			control: f4Control{lastPhase: cstwatchdogwire.Barrier, lastSequence: 3,
				lastFlags: cstwatchdogwire.Attempted | cstwatchdogwire.BarrierSeen},
			frame: cstwatchdogwire.Frame{Binding: binding, Sequence: 4, Phase: cstwatchdogwire.Fired,
				Flags: cstwatchdogwire.Attempted | cstwatchdogwire.BarrierSeen | cstwatchdogwire.InjectionFired},
			requireBarrier: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := cstwatchdogwire.Encode(tc.frame)
			if err != nil {
				t.Fatalf("fixture must be wire-valid: %v", err)
			}
			decoded, err := cstwatchdogwire.Decode(raw[:], raw[:], binding)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.control.validateNext(decoded, tc.requireBarrier); err != nil {
				t.Fatalf("parent rejected exact witnessed progression: %v", err)
			}
		})
	}
}
