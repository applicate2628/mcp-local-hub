//go:build !windows

package api

import (
	"context"
	"net"
)

func dialRouteAggregateAttachment(ctx context.Context, address string) (net.Conn, error) {
	return dialSupervisorIPC(ctx, address)
}

func closeRouteAggregateAttachmentObserved(_ context.Context, conn net.Conn) (routeAggregateAttachmentCloseReceipt, error) {
	return nil, conn.Close()
}
