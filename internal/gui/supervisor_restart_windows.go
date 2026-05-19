//go:build windows

package gui

import (
	"os/exec"
	"syscall"
)

// configureDetached sets Windows-specific flags on cmd so the spawned
// process becomes its own process group AND detaches from the parent
// console. The combination is required for the spawned supervisor to
// outlive the GUI process that started it.
//
// DETACHED_PROCESS                 — child has no console.
// CREATE_NEW_PROCESS_GROUP         — child gets its own Ctrl-C group;
//
//	a Ctrl-C in the parent terminal does NOT propagate.
//
// CREATE_BREAKAWAY_FROM_JOB        — when the parent is in a Job
//
//	Object with KILL_ON_JOB_CLOSE (the canonical Windows
//	supervisor lifecycle wiring), this flag lets the child
//	escape the Job's cascade-kill. Without it, the new
//	supervisor would die together with whatever Job-bearing
//	process spawned the GUI handler.
const (
	winDetachedProcess        = 0x00000008
	winCreateNewProcessGroup  = 0x00000200
	winCreateBreakawayFromJob = 0x01000000
)

func configureDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: winDetachedProcess | winCreateNewProcessGroup | winCreateBreakawayFromJob,
	}
}
