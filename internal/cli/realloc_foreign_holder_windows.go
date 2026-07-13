//go:build windows

package cli

import (
	"context"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

// reallocForeignHolder best-effort resolves (pid, basename) of the foreign
// process holding a stolen loopback port, for the L3 daemon-bind-access-denied
// event's REDACTED foreign_holder field (PID + basename ONLY — never a command
// line / executable path / env, which can carry attacker-controlled bytes or
// secrets). Runs ONLY on the off-loop reallocation worker (it may spawn a
// WMI/PowerShell identity probe). A probe miss returns (0, "") and the event
// omits foreign_holder. Windows-only: process.LookupProcessIdentityContext has no
// POSIX implementation, and the self-heal collision class is Windows-specific.
func reallocForeignHolder(port int) (int, string) {
	probeCtx, cancel := context.WithTimeout(context.Background(), portGateProbeDeadline)
	defer cancel()
	pid, ok, probeErr := api.LoopbackPortOwnerPIDContext(probeCtx, port)
	if probeErr != nil || !ok || pid <= 0 {
		return 0, ""
	}
	idCtx, idCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer idCancel()
	id, idErr := process.LookupProcessIdentityContext(idCtx, pid)
	if idErr != nil {
		return pid, "" // PID known; basename unresolved
	}
	return pid, id.Basename
}
