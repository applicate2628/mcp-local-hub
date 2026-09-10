//go:build windows

package gui

import (
	"os/exec"

	"mcp-local-hub/internal/api"
	processowner "mcp-local-hub/internal/process"
)

// startDetachedSupervisorTolerant is GUI command/result glue around the sole
// internal/process breakaway attempt owner. The GUI admits the optional
// flagless-then-minimal compatibility mode; required policy still performs one
// attempt and fails loud.
func startDetachedSupervisorTolerant(build func() *exec.Cmd, policy processowner.BreakawayPolicy) (*exec.Cmd, error) {
	return startDetachedSupervisorTolerantWithStart(build, policy, func(cmd *exec.Cmd) error { return cmd.Start() })
}

func startDetachedSupervisorTolerantWithStart(build func() *exec.Cmd, policy processowner.BreakawayPolicy, start func(*exec.Cmd) error) (*exec.Cmd, error) {
	result, err := processowner.StartWindowsBreakaway(processowner.BreakawayStartSpec{
		Policy: policy, OptionalFallback: processowner.BreakawayFallbackFlaglessThenMinimal,
		Build: func(processowner.BreakawayAttempt) *exec.Cmd { return build() },
		Start: start,
	})
	if err != nil {
		return result.Command, err
	}
	if result.Attempt == processowner.BreakawayAttemptMinimal {
		pid := 0
		if result.Command.Process != nil {
			pid = result.Command.Process.Pid
		}
		_ = api.LogHubMcpEvent("warn", "supervisor-restart-detach-flags-denied", map[string]any{
			"reason":    "CreateProcess denied breakaway and detached retry; spawned with minimal flag set",
			"spawned":   true,
			"degraded":  "no-detach-no-orphan-escape",
			"new_pid":   pid,
			"breakaway": "already-cleared",
		})
	}
	return result.Command, nil
}
