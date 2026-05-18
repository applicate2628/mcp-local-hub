//go:build linux

package cli

import (
	"time"

	"mcp-local-hub/internal/process"
)

// processStartTime returns the wall-clock start time of pid. The process
// package owns the /proc parsing so supervisor startup and reaper gates share
// one start-time proof implementation.
func processStartTime(pid int) (time.Time, bool) {
	return process.ProcessStartTime(pid)
}
