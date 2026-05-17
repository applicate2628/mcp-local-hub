//go:build darwin

// supervise_reaper_starttime_darwin.go — Darwin no-op fallback for the
// per-PID process-start-time probe used by the cold-start reaper's
// StartedAt gate (Lane F P0 #4).
//
// macOS does NOT expose /proc; the libproc + sysctl(KERN_PROC_PID)
// path that would read `kp_proc.p_starttime` is deferred to the same
// follow-up that wires `mcphub gui --force --kill` on macOS (see
// phase-3b-ii-backlog.md F6). Until then, this stub returns
// (zero, false) which causes the reaper to record every PID under
// SkippedPIDs and not kill anything — the safe failure mode (no
// kills > wrong kills on a recycled-PID host).
//
// Consistent with the existing Darwin gap in procIdentityFromProc /
// /proc/<pid>/comm path: when /proc is unavailable the reaper has no
// way to verify a PID belongs to mcphub, so it must not kill.

package cli

import "time"

// processStartTime is a no-op stub on Darwin. See file header.
func processStartTime(pid int) (time.Time, bool) {
	_ = pid
	return time.Time{}, false
}
