//go:build windows

package gui

import (
	"os/exec"
	"syscall"

	"mcp-local-hub/internal/process"
)

// Windows detach flags shared by both configurators below.
//
// DETACHED_PROCESS                 — child does not inherit a console.
//
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

func applyDetachFlags(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: winDetachedProcess | winCreateNewProcessGroup | winCreateBreakawayFromJob,
	}
	process.NoConsole(cmd)
}

// configureDetachedSupervisor configures a spawn of `mcphub supervise`.
func configureDetachedSupervisor(cmd *exec.Cmd) {
	applyDetachFlags(cmd)
}

// configureDetachedGUI configures a console-free replacement GUI. Debug
// console intent is process-local and is never copied into restart argv.
func configureDetachedGUI(cmd *exec.Cmd) {
	applyDetachFlags(cmd)
}
