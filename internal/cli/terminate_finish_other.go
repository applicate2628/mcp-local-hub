//go:build !windows

package cli

import (
	"fmt"
	"syscall"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

const (
	productionTerminateGracePeriod = 2 * time.Second
	productionTerminatePollPeriod  = 100 * time.Millisecond
)

func finishProductionTerminate(pid int, d api.SupervisorDaemon, events *api.SupervisorEventLog) error {
	deadline := time.Now().Add(productionTerminateGracePeriod)
	for time.Now().Before(deadline) {
		if !process.IsPidAlive(pid) {
			return nil
		}
		time.Sleep(productionTerminatePollPeriod)
	}

	if !pidMatchesMcphub(pid) {
		_ = events.Emit(api.SupervisorEvent{
			Severity: api.SupervisorEventSeverityWarn,
			Source:   "lifecycle",
			Event:    "daemon-terminate-escalation-aborted-pid-reuse",
			TaskName: d.TaskName,
			Body: map[string]any{
				"pid":    pid,
				"reason": "PID does not match mcphub identity after grace window - possible OS PID reuse, refusing SIGKILL",
			},
		})
		return nil
	}

	_ = events.Emit(api.SupervisorEvent{
		Severity: api.SupervisorEventSeverityWarn,
		Source:   "lifecycle",
		Event:    "daemon-terminate-escalated",
		TaskName: d.TaskName,
		Body: map[string]any{
			"pid":     pid,
			"signal":  "SIGKILL",
			"timeout": productionTerminateGracePeriod.String(),
		},
	})
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("send SIGKILL to pid %d after SIGTERM timeout: %w", pid, err)
	}
	return nil
}
