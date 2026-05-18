package cli

import (
	"os"
	"time"

	"mcp-local-hub/internal/api"
)

func supervisorIPCOwnerForHello(ownerOpt ...api.SupervisorLockOwner) api.SupervisorLockOwner {
	if len(ownerOpt) > 0 && ownerOpt[0].PID > 0 && ownerOpt[0].StartedAt != "" {
		return ownerOpt[0]
	}
	return api.SupervisorLockOwner{
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
}
