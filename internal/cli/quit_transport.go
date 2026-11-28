package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
)

type guiQuitDial func(context.Context, string, string) (net.Conn, error)
type guiQuitVerify func(context.Context, net.Conn) error

// Dial and verify synchronously before handing the connection to net/http.
// The retained GUI process observation never escapes into an asynchronous dial.
func newGUIQuitTransport(ctx context.Context, address string, dial guiQuitDial, verify guiQuitVerify) (*http.Transport, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	c, err := dial(ctx, "tcp4", address)
	if err != nil {
		return nil, nil, err
	}
	if c == nil {
		return nil, nil, errors.New("GUI_QUIT_CONNECTION_UNAVAILABLE")
	}
	if err = errors.Join(verify(ctx, c), ctx.Err()); err != nil {
		return nil, nil, errors.Join(err, c.Close())
	}
	var taken atomic.Bool
	t := &http.Transport{DisableKeepAlives: true,
		DialContext: func(_ context.Context, _ string, addr string) (net.Conn, error) {
			if addr != address || !taken.CompareAndSwap(false, true) {
				return nil, errors.New("GUI_QUIT_CONNECTION_ALREADY_USED_OR_ADDRESS_CHANGED")
			}
			return c, nil
		}}
	return t, func() { _ = c.Close() }, nil
}
