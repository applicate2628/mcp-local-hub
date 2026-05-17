//go:build windows

package cli

import (
	"mcp-local-hub/internal/api"
)

func finishProductionTerminate(_ int, _ api.SupervisorDaemon, _ *api.SupervisorEventLog) error {
	return nil
}
