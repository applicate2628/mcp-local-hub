//go:build !windows

package api

import (
	"fmt"
	"net"
)

func ObserveSupervisorStatusServerIdentityV1(net.Conn) (SupervisorProcessIdentityV1, error) {
	return SupervisorProcessIdentityV1{}, fmt.Errorf("supervisor kernel identity proof is Windows-only")
}

func observeSupervisorStatusServerIdentityV1(net.Conn, bool) (SupervisorProcessIdentityV1, error) {
	return SupervisorProcessIdentityV1{}, ErrSupervisorControlPeerUnsupported
}

func ObserveSupervisorProcessIdentityV1(int) (SupervisorProcessIdentityV1, error) {
	return SupervisorProcessIdentityV1{}, ErrSupervisorControlPeerUnsupported
}
