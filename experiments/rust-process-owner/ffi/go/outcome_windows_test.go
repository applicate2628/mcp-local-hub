//go:build windows && amd64 && cgo

package caller

import (
	"fmt"
	"testing"
)

func TestOutcomeAcceptsDefinedStatesWithoutCollapsingCleanup(t *testing.T) {
	cases := []struct {
		name   string
		status uint32
		out    Outcome
	}{
		{"no-owner-argument-error", argument, Outcome{}},
		{"nil-release", ok, Outcome{}},
		{"created", ok, Outcome{PID: 1}},
		{"denied", denied, Outcome{PID: 1}},
		{"native-failure", nativeFailure, Outcome{PID: 1, NativeError: 87}},
		{"pending-root", ok, Outcome{State: pending, PID: 1}},
		{"pending-descendant", ok, Outcome{State: pending, PID: 1, Flags: 3, RootExit: 7, Active: 1}},
		{"exit-zero", ok, Outcome{State: complete, PID: 1, Flags: 1}},
		{"real-exit-259", ok, Outcome{State: complete, PID: 1, Flags: 1, RootExit: 259}},
		{"terminal-observed-empty-cohort", ok, Outcome{State: complete, PID: 1, Flags: 3}},
	}
	for mask := uint32(1); mask < 8; mask++ {
		out := Outcome{State: cleanupFailed, PID: 1, Flags: 1, RootExit: 259, CleanupMask: mask}
		for i := range out.CleanupCodes {
			if mask&(1<<i) != 0 {
				out.CleanupCodes[i] = int32(5 + i)
			}
		}
		cases = append(cases, struct {
			name   string
			status uint32
			out    Outcome
		}{fmt.Sprintf("cleanup-mask-%d", mask), ok, out})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if reason := outcomeViolation(c.status, c.out); reason != "" {
				t.Fatalf("defined ABI1 value rejected: %s %+v", reason, c.out)
			}
		})
	}
}
func TestOutcomeRejectsIncoherentFields(t *testing.T) {
	cases := []struct {
		name   string
		status uint32
		out    Outcome
	}{
		{"unknown-status", 99, Outcome{}},
		{"unknown-state", ok, Outcome{State: 99, PID: 1}},
		{"unknown-flag", ok, Outcome{Flags: 4}},
		{"unknown-close-slot", ok, Outcome{CleanupMask: 8}},
		{"native-without-code", nativeFailure, Outcome{}},
		{"code-without-status", ok, Outcome{NativeError: 87}},
		{"unflagged-root", ok, Outcome{RootExit: 259}},
		{"unflagged-active", ok, Outcome{Active: 1}},
		{"missing-process", ok, Outcome{State: complete, Flags: 1}},
		{"missing-root", ok, Outcome{State: complete, PID: 1}},
		{"live-terminal", ok, Outcome{State: complete, PID: 1, Flags: 3, Active: 1}},
		{"empty-cleanup-failure", ok, Outcome{State: cleanupFailed, PID: 1, Flags: 1}},
		{"complete-with-close-failure", ok, Outcome{State: complete, PID: 1, Flags: 1, CleanupMask: 1, CleanupCodes: [3]int32{5}}},
		{"close-without-code", ok, Outcome{State: cleanupFailed, PID: 1, Flags: 1, CleanupMask: 1}},
		{"stray-close-code", ok, Outcome{State: cleanupFailed, PID: 1, Flags: 1, CleanupMask: 1, CleanupCodes: [3]int32{5, 0, 6}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if reason := outcomeViolation(c.status, c.out); reason == "" {
				t.Fatalf("contradictory outcome accepted: %+v", c.out)
			}
		})
	}
}
