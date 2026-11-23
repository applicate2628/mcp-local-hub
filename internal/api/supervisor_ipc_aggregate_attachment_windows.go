//go:build windows

package api

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

func dialRouteAggregateAttachment(ctx context.Context, address string) (net.Conn, error) {
	return acquireSupervisorIPCPipe(ctx, address)
}

func closeRouteAggregateAttachmentObserved(ctx context.Context, conn net.Conn) (routeAggregateAttachmentCloseReceipt, error) {
	original, ok := conn.(interface {
		CloseObserved(context.Context) (*winio.CloseReceipt, error)
	})
	if !ok {
		// Attempt ordinary transport cleanup once. A legacy result cannot
		// qualify this attachment for retirement or reattach.
		return nil, conn.Close()
	}
	return original.CloseObserved(ctx)
}
