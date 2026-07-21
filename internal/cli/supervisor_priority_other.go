//go:build !windows

package cli

import "mcp-local-hub/internal/api"

// ensureSupervisorPriorityFloor is a no-op on non-Windows platforms.
//
// The inherited-IDLE defect is Windows-specific: it stems from the Windows
// Task Scheduler <Priority> element (0-10) mapping the liveness task to
// IDLE_PRIORITY_CLASS and the Win32 CreateProcess rule that a child
// inherits an IDLE/BELOW_NORMAL parent's class. Neither the scheduled-task
// priority element nor that inheritance rule exists on POSIX. The Linux
// (beta) autostart shim is a systemd user service and the macOS (preview)
// shim a LaunchAgent; neither defaults the supervisor to an idle scheduling
// class, so there is no inherited-IDLE fleet starvation to correct here.
//
// The analogous POSIX knob is nice(2) / a sched policy, which is a
// different mechanism with different semantics (a NICE value, not a
// class-inheritance rule). It is intentionally NOT wired speculatively:
// no POSIX starvation case has been measured, and the release posture is
// Windows GA / Linux beta / macOS preview. If one is ever observed, add a
// POSIX implementation here behind the same seam.
func ensureSupervisorPriorityFloor(events *api.SupervisorEventLog) {
	_ = events
}
