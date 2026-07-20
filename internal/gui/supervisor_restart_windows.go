//go:build windows

package gui

import (
	"os/exec"
	"syscall"
)

// configureDetached sets Windows-specific flags on cmd so the spawned
// process becomes its own process group AND does not INHERIT the parent
// console. The combination is required for the spawned supervisor to
// outlive the GUI process that started it.
//
// NOT SUFFICIENT ON ITS OWN for an mcphub child. These flags stop console
// inheritance at create time; they do not stop the child from calling
// AttachConsole(ATTACH_PARENT_PROCESS) afterwards, which mcphub's own
// main() does as its first statement. A caller spawning a SUPERVISOR must
// therefore also apply process.SuppressConsoleAttach — see
// newDetachedSupervisorCmd in supervisor_restart.go. newRestartV3GUICmd
// deliberately does not, because it spawns a replacement GUI whose
// --foreground semantics require the console; the note there explains why.
//
// DETACHED_PROCESS                 — child does not inherit this console
//
//	(it can still attach one itself; see above).
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

func configureDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: winDetachedProcess | winCreateNewProcessGroup | winCreateBreakawayFromJob,
	}
}
