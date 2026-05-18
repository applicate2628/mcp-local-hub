//go:build windows

package api

import (
	"context"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

func dialSupervisorIPC(ctx context.Context, address string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	timeout := 5 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			timeout = time.Nanosecond
		}
	}
	conn, err := winio.DialPipe(address, &timeout)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}
