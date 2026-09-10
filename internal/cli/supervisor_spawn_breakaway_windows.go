//go:build windows

package cli

import (
	"os/exec"

	processowner "mcp-local-hub/internal/process"
)

// startSupervisorDetachedBreakaway adapts CLI command construction and
// degraded diagnostics to the sole internal/process attempt-policy owner.
func startSupervisorDetachedBreakaway(cmd *exec.Cmd, rebuild func() *exec.Cmd, onDegrade func(error), policy processowner.BreakawayPolicy) (*exec.Cmd, error) {
	return startSupervisorDetachedBreakawayWithStart(cmd, rebuild, onDegrade, policy, func(next *exec.Cmd) error {
		return next.Start()
	})
}

// Tests inject Start; production passes exec.Cmd.Start. Attempt count, Windows
// flags, ERROR_ACCESS_DENIED classification, and fallback admission remain
// owned entirely by internal/process.
func startSupervisorDetachedBreakawayWithStart(cmd *exec.Cmd, rebuild func() *exec.Cmd, onDegrade func(error), policy processowner.BreakawayPolicy, start func(*exec.Cmd) error) (*exec.Cmd, error) {
	result, err := processowner.StartWindowsBreakaway(processowner.BreakawayStartSpec{
		Policy: policy, OptionalFallback: processowner.BreakawayFallbackFlagless,
		Build: func(attempt processowner.BreakawayAttempt) *exec.Cmd {
			if attempt == processowner.BreakawayAttemptInitial {
				return cmd
			}
			return rebuild()
		},
		Start: start,
	})
	if err == nil && result.Attempt != processowner.BreakawayAttemptInitial && onDegrade != nil {
		onDegrade(result.InitialError)
	}
	return result.Command, err
}
