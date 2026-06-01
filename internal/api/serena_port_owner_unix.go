//go:build !windows

package api

import "fmt"

// errPortOwnerUnsupported is the fail-closed sentinel returned by the POSIX
// loopbackPortOwnerPID stub. The serena reconcile is Windows-GA (POSIX is
// test-only per the repo CLAUDE.md), and the OS-level port→owner proof is
// implemented only via Windows `netstat -ano`. Rather than silently fall back
// to the FORGEABLE self-reported HTTP-PID check (the exact weakness PR #252's
// P1 rejected), the POSIX path refuses: a platform that cannot prove socket
// ownership must NOT trust a stale-port listener.
var errPortOwnerUnsupported = fmt.Errorf("OS-level port-owner verification not implemented on this platform (serena reconcile is Windows-GA; refusing to trust a self-reported PID)")

// loopbackPortOwnerPID is the POSIX fail-closed stub. It never resolves an
// owner and never falls through to an HTTP self-report — it returns
// errPortOwnerUnsupported so defaultGUIPidportIdentityCheck fails closed and
// the reconcile refuses to rewrite client configs. A real POSIX implementation
// (e.g. /proc/net/tcp socket-inode → PID, or `ss -ltnp`) is deferred until
// serena reconcile ships on POSIX; until then, fail-closed is mandatory.
func loopbackPortOwnerPID(port int) (int, bool, error) {
	return 0, false, errPortOwnerUnsupported
}

// guiImageForPID is the POSIX counterpart to the Windows image lookup. It is
// unreachable in production because loopbackPortOwnerPID fails closed first,
// but it is defined so the cross-platform build compiles and so the
// guiImageForPIDFn seam has a real default on every platform. It fails closed
// (no image resolvable without the Windows process tooling).
func guiImageForPID(pid int) (string, bool) {
	return "", false
}
