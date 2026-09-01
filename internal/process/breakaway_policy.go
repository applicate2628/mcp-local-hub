package process

import (
	"errors"
	"os/exec"
)

type BreakawayPolicy uint8

const (
	BreakawayRequired BreakawayPolicy = iota + 1
	BreakawayOptional
)

var ErrWindowsBreakawayRequired = errors.New("E_WINDOWS_BREAKAWAY_REQUIRED")

type BreakawayAttempt uint8

const (
	BreakawayAttemptInitial BreakawayAttempt = iota + 1
	BreakawayAttemptFlagless
	BreakawayAttemptMinimal
)

type BreakawayOptionalFallback uint8

const (
	BreakawayFallbackFlagless BreakawayOptionalFallback = iota + 1
	BreakawayFallbackFlaglessThenMinimal
)

type BreakawayStartSpec struct {
	Policy           BreakawayPolicy
	OptionalFallback BreakawayOptionalFallback
	Build            func(BreakawayAttempt) *exec.Cmd
	Start            func(*exec.Cmd) error
}

type BreakawayStartResult struct {
	Command      *exec.Cmd
	Attempt      BreakawayAttempt
	Attempts     int
	InitialError error
}

// BreakawayPolicyForCurrentProcess fails closed: a current process proven not
// to be in a kill-on-close Job may use the optional compatibility fallback;
// an owned or ambiguous Job disposition requires breakaway.
func BreakawayPolicyForCurrentProcess() BreakawayPolicy {
	killOnClose, err := currentProcessKillOnCloseJob()
	if err != nil || killOnClose {
		return BreakawayRequired
	}
	return BreakawayOptional
}
