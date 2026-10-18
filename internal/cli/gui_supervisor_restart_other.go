//go:build !windows

package cli

import (
	"context"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/process"
)

func (m *supervisorManager) pinRestartExternalAdmission(*supervisorOwner) (*supervisorManagerOperation, error) {
	return nil, process.ErrProcessIdentityUnsupported
}

func (m *supervisorManager) runSupervisorRestart(context.Context, *supervisorManagerOperation) (gui.SupervisorRestartResult, error) {
	// The existing strict kernel-peer and held-target ports refuse these
	// platforms. No legacy PID-only kill/spawn substitutes for that proof.
	return restartRefusal("RESTART_IDENTITY_UNCONFIRMED", process.ErrProcessIdentityUnsupported)
}
