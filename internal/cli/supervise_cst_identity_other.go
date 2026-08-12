//go:build !windows

package cli

import (
	"fmt"
	"net"

	"mcp-local-hub/internal/api"
)

func supervisorCstDaemonPeerIdentity(net.Conn) (api.SupervisorProcessIdentityV1, api.SupervisorCstIdentityPolicyV1, bool, error) {
	return api.SupervisorProcessIdentityV1{}, api.SupervisorCstIdentityPolicyV1{}, false, fmt.Errorf("CST supervisor identity status is Windows-only")
}
