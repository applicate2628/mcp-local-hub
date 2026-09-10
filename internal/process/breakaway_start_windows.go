//go:build windows

package process

import (
	"errors"
	"fmt"
	"os/exec"

	"golang.org/x/sys/windows"
)

// StartWindowsBreakaway is the sole required/optional Windows breakaway
// enforcement owner. Callers supply fresh equivalent commands and handles;
// this owner applies attempt flags, classifies CreateProcess failures, and
// decides whether another attempt is admitted.
func StartWindowsBreakaway(spec BreakawayStartSpec) (BreakawayStartResult, error) {
	result := BreakawayStartResult{}
	if spec.Policy != BreakawayRequired && spec.Policy != BreakawayOptional {
		return result, errors.New("invalid Windows breakaway policy")
	}
	if spec.Build == nil {
		return result, errors.New("Windows breakaway command builder is required")
	}
	start := spec.Start
	if start == nil {
		start = func(cmd *exec.Cmd) error { return cmd.Start() }
	}
	attempts := []BreakawayAttempt{BreakawayAttemptInitial}
	if spec.Policy == BreakawayOptional {
		switch spec.OptionalFallback {
		case BreakawayFallbackFlagless:
			attempts = append(attempts, BreakawayAttemptFlagless)
		case BreakawayFallbackFlaglessThenMinimal:
			attempts = append(attempts, BreakawayAttemptFlagless, BreakawayAttemptMinimal)
		default:
			return result, errors.New("invalid Windows optional breakaway fallback")
		}
	}
	var failures []error
	for index, attempt := range attempts {
		cmd := spec.Build(attempt)
		result.Command, result.Attempt, result.Attempts = cmd, attempt, index+1
		if cmd == nil {
			return result, errors.New("Windows breakaway command builder returned nil")
		}
		configureWindowsBreakawayAttempt(cmd, attempt)
		err := start(cmd)
		if index == 0 {
			result.InitialError = err
		}
		if err == nil {
			return result, nil
		}
		failures = append(failures, fmt.Errorf("%s attempt: %w", breakawayAttemptName(attempt), err))
		if index == 0 {
			if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				return result, err
			}
			if spec.Policy == BreakawayRequired {
				return result, errors.Join(ErrWindowsBreakawayRequired, failures[0])
			}
			continue
		}
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || index == len(attempts)-1 {
			return result, errors.Join(failures...)
		}
	}
	return result, errors.Join(failures...)
}

func configureWindowsBreakawayAttempt(cmd *exec.Cmd, attempt BreakawayAttempt) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &windows.SysProcAttr{}
	}
	switch attempt {
	case BreakawayAttemptInitial:
		cmd.SysProcAttr.CreationFlags |= windows.CREATE_BREAKAWAY_FROM_JOB
	case BreakawayAttemptFlagless:
		cmd.SysProcAttr.CreationFlags &^= windows.CREATE_BREAKAWAY_FROM_JOB
	case BreakawayAttemptMinimal:
		cmd.SysProcAttr.CreationFlags = windows.CREATE_NO_WINDOW
		cmd.SysProcAttr.HideWindow = true
	}
}

func breakawayAttemptName(attempt BreakawayAttempt) string {
	switch attempt {
	case BreakawayAttemptInitial:
		return "breakaway"
	case BreakawayAttemptFlagless:
		return "flagless"
	case BreakawayAttemptMinimal:
		return "minimal"
	default:
		return "unknown"
	}
}
