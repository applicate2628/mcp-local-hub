package api

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

var errLoopbackPeerNotPublished = errors.New("established loopback peer not yet observable")

// VerifyLoopbackConnectedPeer requires the known server PID to own the server
// side of this particular established IPv4 loopback connection. It never
// returns a newly discovered process identity or a listener-only result.
func VerifyLoopbackConnectedPeer(ctx context.Context, local, remote netip.AddrPort, pid int) error {
	loopback := netip.AddrFrom4([4]byte{127, 0, 0, 1})
	if ctx == nil || local.Addr() != loopback || remote.Addr() != loopback || local.Port() == 0 || remote.Port() == 0 || local == remote || pid <= 0 {
		return errors.New("invalid established loopback peer expectation")
	}
	// Accept and socket publication are asynchronous on a newly dialed TCP
	// socket. Bound only this observation, not the caller's Quit settlement.
	ctx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := verifyLoopbackConnectedPeer(ctx, local, remote, pid)
		if !errors.Is(err, errLoopbackPeerNotPublished) {
			return err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
