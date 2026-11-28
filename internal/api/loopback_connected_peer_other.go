//go:build !linux && !windows

package api

import (
	"context"
	"errors"
	"net/netip"
)

func verifyLoopbackConnectedPeer(context.Context, netip.AddrPort, netip.AddrPort, int) error {
	return errors.New("established loopback peer verification unavailable")
}
