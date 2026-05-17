//go:build !linux && !windows

package process

import "time"

// ProcessStartTime is unavailable on non-Linux POSIX preview targets.
func ProcessStartTime(pid int) (time.Time, bool) {
	_ = pid
	return time.Time{}, false
}
