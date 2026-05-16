//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// pidAliveImpl probes liveness via os.FindProcess + Process.Signal(0).
//
// Per Go stdlib docs at os/exec.go:249-252, on Unix os.FindProcess
// ALWAYS succeeds — it just wraps the PID in a *Process without
// consulting the kernel. The actual liveness probe must come from a
// signal-0 delivery via kill(2). kill(2) returns:
//
//   - 0 (nil err)        — process exists AND we may signal it
//   - errno=ESRCH        — no such PID (dead OR never existed)
//   - errno=EPERM        — process exists but is owned by another
//                          user and we cannot signal it
//
// For the supervisor's drain we treat both ESRCH and EPERM as "not
// ours to keep waiting on" — the supervisor only manages PIDs it
// spawned, so an EPERM means the PID got recycled to a different
// user's process and the original transient is gone.
//
// Mirrors internal/api/supervisor_lock.go:113-141 isOwnerLive()
// behavior, but works for ANY PID (not just the supervisor's own
// owner-sidecar PID).
func pidAliveImpl(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		// Unix: should never happen per docs, but be defensive.
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}
