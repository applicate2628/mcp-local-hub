//go:build !windows

package api

import (
	"context"
	"net"
)

func dialSupervisorIPC(ctx context.Context, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", address)
}
