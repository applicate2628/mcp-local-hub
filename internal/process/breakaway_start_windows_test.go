//go:build windows

package process

import (
	"errors"
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStartWindowsBreakawayPolicyMatrix(t *testing.T) {
	otherErr := errors.New("synthetic other start error")
	finalErr := errors.New("synthetic final start error")
	for _, tc := range []struct {
		name        string
		policy      BreakawayPolicy
		fallback    BreakawayOptionalFallback
		startErrors []error
		wantAttempt BreakawayAttempt
		wantStarts  int
		wantBuilds  int
		wantErr     error
		wantInitial error
	}{
		{name: "initial success", policy: BreakawayRequired, fallback: BreakawayFallbackFlagless, startErrors: []error{nil}, wantAttempt: BreakawayAttemptInitial, wantStarts: 1, wantBuilds: 1},
		{name: "required access denied", policy: BreakawayRequired, fallback: BreakawayFallbackFlagless, startErrors: []error{windows.ERROR_ACCESS_DENIED}, wantAttempt: BreakawayAttemptInitial, wantStarts: 1, wantBuilds: 1, wantErr: ErrWindowsBreakawayRequired, wantInitial: windows.ERROR_ACCESS_DENIED},
		{name: "required other error", policy: BreakawayRequired, fallback: BreakawayFallbackFlagless, startErrors: []error{otherErr}, wantAttempt: BreakawayAttemptInitial, wantStarts: 1, wantBuilds: 1, wantErr: otherErr, wantInitial: otherErr},
		{name: "optional flagless success", policy: BreakawayOptional, fallback: BreakawayFallbackFlagless, startErrors: []error{windows.ERROR_ACCESS_DENIED, nil}, wantAttempt: BreakawayAttemptFlagless, wantStarts: 2, wantBuilds: 2, wantInitial: windows.ERROR_ACCESS_DENIED},
		{name: "optional flagless other failure", policy: BreakawayOptional, fallback: BreakawayFallbackFlagless, startErrors: []error{windows.ERROR_ACCESS_DENIED, finalErr}, wantAttempt: BreakawayAttemptFlagless, wantStarts: 2, wantBuilds: 2, wantErr: finalErr, wantInitial: windows.ERROR_ACCESS_DENIED},
		{name: "optional GUI minimal success", policy: BreakawayOptional, fallback: BreakawayFallbackFlaglessThenMinimal, startErrors: []error{windows.ERROR_ACCESS_DENIED, windows.ERROR_ACCESS_DENIED, nil}, wantAttempt: BreakawayAttemptMinimal, wantStarts: 3, wantBuilds: 3, wantInitial: windows.ERROR_ACCESS_DENIED},
		{name: "optional GUI stops on non-access flagless", policy: BreakawayOptional, fallback: BreakawayFallbackFlaglessThenMinimal, startErrors: []error{windows.ERROR_ACCESS_DENIED, finalErr}, wantAttempt: BreakawayAttemptFlagless, wantStarts: 2, wantBuilds: 2, wantErr: finalErr, wantInitial: windows.ERROR_ACCESS_DENIED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builds, starts := 0, 0
			var built []BreakawayAttempt
			result, err := StartWindowsBreakaway(BreakawayStartSpec{
				Policy: tc.policy, OptionalFallback: tc.fallback,
				Build: func(attempt BreakawayAttempt) *exec.Cmd {
					builds++
					built = append(built, attempt)
					return exec.Command("cmd", "/c", "exit", "0")
				},
				Start: func(*exec.Cmd) error {
					defer func() { starts++ }()
					return tc.startErrors[starts]
				},
			})
			if tc.wantErr == nil && err != nil {
				t.Fatalf("error=%v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error=%v want %v", err, tc.wantErr)
			}
			if result.Attempt != tc.wantAttempt || result.Attempts != tc.wantStarts || builds != tc.wantBuilds || starts != tc.wantStarts || !errors.Is(result.InitialError, tc.wantInitial) {
				t.Fatalf("result=%+v builds=%d starts=%d built=%v", result, builds, starts, built)
			}
		})
	}
}

func TestStartWindowsBreakawayOwnsAttemptFlags(t *testing.T) {
	var flags []uint32
	_, err := StartWindowsBreakaway(BreakawayStartSpec{
		Policy: BreakawayOptional, OptionalFallback: BreakawayFallbackFlaglessThenMinimal,
		Build: func(BreakawayAttempt) *exec.Cmd { return exec.Command("cmd", "/c", "exit", "0") },
		Start: func(cmd *exec.Cmd) error {
			flags = append(flags, cmd.SysProcAttr.CreationFlags)
			if len(flags) < 3 {
				return windows.ERROR_ACCESS_DENIED
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(flags) != 3 || flags[0]&windows.CREATE_BREAKAWAY_FROM_JOB == 0 || flags[1]&windows.CREATE_BREAKAWAY_FROM_JOB != 0 || flags[2] != windows.CREATE_NO_WINDOW {
		t.Fatalf("attempt flags=%#v", flags)
	}
}
