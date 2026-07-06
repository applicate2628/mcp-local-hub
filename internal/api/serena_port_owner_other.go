//go:build !windows && !linux

package api

import (
	"context"
	"fmt"
)

// errPortOwnerUnsupported is the fail-closed sentinel returned on POSIX
// platforms without an OS-level socket-owner implementation. Linux has a
// /proc-backed implementation; this fallback is for other non-Windows targets.
// Rather than silently fall back to the FORGEABLE self-reported HTTP-PID check
// (the exact weakness PR #252's P1 rejected), unsupported platforms refuse: a
// platform that cannot prove socket ownership must NOT trust a stale-port
// listener.
var errPortOwnerUnsupported = fmt.Errorf("OS-level port-owner verification not implemented on this platform (serena reconcile is Windows-GA; refusing to trust a self-reported PID)")

// loopbackPortOwnerPID is the unsupported-POSIX fail-closed stub. It never
// resolves an owner and never falls through to an HTTP self-report — it returns
// errPortOwnerUnsupported so liveness/status surface an unverifiable owner
// instead of trusting any listener on the daemon port.
func loopbackPortOwnerPID(port int) (int, bool, error) {
	return 0, false, errPortOwnerUnsupported
}

// loopbackPortOwnerPIDContext is the context-bounded stub. Like the non-ctx
// form it fails closed with errPortOwnerUnsupported; it exists only so the
// cross-platform LoopbackPortOwnerPIDContext seam links on every target.
func loopbackPortOwnerPIDContext(ctx context.Context, port int) (int, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	return loopbackPortOwnerPID(port)
}

// loopbackPortOwnersSnapshot is the unsupported-POSIX fail-closed stub for the
// batch owner lookup. Like loopbackPortOwnerPID it refuses rather than guess,
// returning the same errPortOwnerUnsupported sentinel so a caller batching the
// snapshot gets the identical fail-closed signal as the per-port path.
func loopbackPortOwnersSnapshot() (map[int]int, error) {
	return nil, errPortOwnerUnsupported
}

// guiImageForPID is the POSIX counterpart to the Windows image lookup. It is
// unreachable in production because loopbackPortOwnerPID fails closed first,
// but it is defined so the cross-platform build compiles and so the
// guiImageForPIDFn seam has a real default on every platform. It fails closed
// (no image resolvable without the Windows process tooling).
func guiImageForPID(pid int) (string, bool) {
	return "", false
}
