//go:build !windows

package cli

import (
	"errors"
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

func finishProductionTerminate(proof process.PIDIdentityProof, d api.SupervisorDaemon, events *api.SupervisorEventLog) error {
	pid := proof.PID
	deadline := time.Now().Add(productionTerminateGracePeriod)
	for time.Now().Before(deadline) {
		if !process.IsPidAlive(pid) {
			return nil
		}
		time.Sleep(productionTerminatePollPeriod)
	}

	if err := process.VerifyPIDIdentity(proof); err != nil {
		_ = events.Emit(api.SupervisorEvent{
			Severity: api.SupervisorEventSeverityWarn,
			Source:   "lifecycle",
			Event:    "daemon-terminate-escalation-aborted-pid-reuse",
			TaskName: d.TaskName,
			Body: map[string]any{
				"pid":    pid,
				"reason": err.Error(),
			},
		})
		return fmt.Errorf("terminate escalation aborted for pid %d: %w", pid, err)
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
	if err := process.KillPIDWithIdentity(proof, syscall.SIGKILL); err != nil {
		if errors.Is(err, process.ErrProcessAlreadyExited) {
			emitDaemonTerminateAlreadyExited(events, d, pid)
			return nil
		}
		return fmt.Errorf("send SIGKILL to pid %d after SIGTERM timeout: %w", pid, err)
	}
	return nil
}
