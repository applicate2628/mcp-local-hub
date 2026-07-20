//go:build windows

package gui

import (
	"os/exec"
	"syscall"

	"mcp-local-hub/internal/process"
)

// Windows detach flags shared by both configurators below.
//
// DETACHED_PROCESS                 — child does not INHERIT this console
//
//	(it can still attach one itself — see configureDetachedSupervisor).
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
}

// configureDetachedSupervisor configures a spawn of `mcphub supervise`.
//
// The creation flags are NOT SUFFICIENT ON THEIR OWN for an mcphub child:
// they stop console inheritance at create time, but they do not stop the
// child from calling AttachConsole(ATTACH_PARENT_PROCESS) afterwards, which
// mcphub's own main() does as its first statement. A supervisor that
// re-attaches to the GUI's console dies with that terminal and takes every
// daemon under its Job Object with it, so the suppression marker is applied
// here — folded in, not left to each call site to remember.
func configureDetachedSupervisor(cmd *exec.Cmd) {
	applyDetachFlags(cmd)
	process.SuppressConsoleAttach(cmd)
}

// configureDetachedGUI configures a spawn of a replacement GUI
// (RestartV3 self-restart) and deliberately does NOT suppress the console
// attach.
//
// This is the one spawn in the tree whose console requirement is the
// OPPOSITE of the supervisor's: the child re-parses the same argv, so under
// --foreground / --no-tray the operator has explicitly asked for a
// console-attached GUI and suppression would silently remove its terminal
// output. In background mode the parent already released its console, so
// there is nothing to attach to and suppression would verify nothing.
//
// Splitting the two configurators is what makes that difference structural.
// While one shared helper served both, "is this spawn attach-suppressed?"
// depended on remembering to hand-apply the marker at the right call sites,
// and one new supervisor spawn written against the shared helper would have
// silently regressed the class with nothing in the build to object.
func configureDetachedGUI(cmd *exec.Cmd) {
	applyDetachFlags(cmd)
}
