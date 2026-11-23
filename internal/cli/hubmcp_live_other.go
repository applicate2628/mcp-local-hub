//go:build !windows

package cli

import (
	"context"
	"net"

	"mcp-local-hub/internal/api"
)

func queryInstalledAggregateStatus(context.Context, windowsCanonicalEntryCommandInput) (api.RouteAggregateStatus, error) {
	return api.RouteAggregateStatus{}, api.ErrSupervisorControlPeerUnsupported
}

func observeRouteAdmissionNativePipePeer(net.Conn) (api.SupervisorProcessIdentityV1, routeAdmissionNativeEvidence, error) {
	return api.SupervisorProcessIdentityV1{}, nil, api.ErrSupervisorControlPeerUnsupported
}

func holdRouteAdmissionOperator(context.Context, net.Conn, *routeAggregateSupervisor, *routeAdmissionNativeObservations) (*routeAdmissionOperator, error) {
	return nil, api.ErrSupervisorControlPeerUnsupported
}

func routeAdmissionPlatformClosedRead(error) bool { return false }

func observeRouteAdmissionNativePipeClose(context.Context, net.Conn) (routeAdmissionNativePipeReceipt, error) {
	return nil, errRouteAdmissionNativePipeCloseUnavailable
}
