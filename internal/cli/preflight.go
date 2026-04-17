package cli

import (
	"fmt"
	"net"
	"os/exec"
	"time"

	"mcp-local-hub/internal/config"
)

// Preflight verifies install preconditions. Returns first error found.
// Called by install before any side effects.
//
// daemonFilter must match the same filter used by BuildPlan — only daemons
// that the install will actually (re)create have their ports checked. Without
// this alignment, a partial install would fail preflight whenever sibling
// daemons (already running from a prior install) occupy their assigned ports,
// even though those ports are not being touched by the current invocation.
func Preflight(m *config.ServerManifest, daemonFilter string) error {
	// 1. Command available.
	if _, err := exec.LookPath(m.Command); err != nil {
		return fmt.Errorf("command %q not found on PATH: %w", m.Command, err)
	}
	// 2. Ports free — only for daemons in the filtered scope.
	for _, d := range m.Daemons {
		if daemonFilter != "" && d.Name != daemonFilter {
			continue
		}
		if portInUse(d.Port) {
			return fmt.Errorf("port %d already in use (needed for daemon %s/%s)", d.Port, m.Name, d.Name)
		}
	}
	return nil
}

// portInUse returns true if a listener on the given port accepts connections.
func portInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
