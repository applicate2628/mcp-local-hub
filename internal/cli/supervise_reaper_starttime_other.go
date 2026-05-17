//go:build !windows && !linux && !darwin

// supervise_reaper_starttime_other.go — fallback no-op for non-Linux,
// non-Darwin POSIX targets (FreeBSD, OpenBSD, NetBSD, illumos, etc.).
// Each of those platforms has its own per-process start-time probe
// (sysctl kern.proc or equivalent) that would need a per-OS impl, and
// none of them are production targets for the v0.5.0 supervisor. The
// no-op makes the reaper skip every PID on those hosts, which is the
// safe failure mode (no kills > wrong kills).

package cli

import "time"

// processStartTime is a no-op stub on non-Linux, non-Darwin POSIX.
// See file header.
func processStartTime(pid int) (time.Time, bool) {
	_ = pid
	return time.Time{}, false
}
