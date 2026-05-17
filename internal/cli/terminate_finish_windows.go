//go:build windows

package cli

import (
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

func finishProductionTerminate(_ process.PIDIdentityProof, _ api.SupervisorDaemon, _ *api.SupervisorEventLog) error {
	return nil
}
