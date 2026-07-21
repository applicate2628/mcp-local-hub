//go:build !windows

package gui

import (
	"os/exec"
	"syscall"

	"mcp-local-hub/internal/process"
)

// applyDetachFlags sets POSIX-specific flags on cmd so the spawned
// process detaches from the parent process group. Without
// Setpgid, the child would receive SIGHUP when the parent's
// controlling terminal closes (matters for cases where mcphub gui
// was started from an interactive shell + the user closes the
// terminal expecting the GUI to keep running).
//
// Setsid: not used here — the child is started by an HTTP handler
// in the GUI process, not by a daemon-pattern double-fork; Setpgid
// is sufficient for the lifecycle we need (child outlives the
// HTTP request that triggered it).
func applyDetachFlags(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

// configureDetachedSupervisor configures a spawn of `mcphub supervise`.
//
// The suppression marker is applied on POSIX too even though the attach it
// suppresses is a Windows-only mechanism (POSIX main() has no
// AttachConsole, and process.SuppressConsoleAttach is a plain env write
// there). Setting it unconditionally keeps ONE answer to "is this child
// attach-suppressed?" across platforms: a reader who opens either the
// _windows or the _other file sees the same contract, instead of having to
// know which platform's file carries the marker. It matches the
// internal/cli supervisor configurator, which does the same.
func configureDetachedSupervisor(cmd *exec.Cmd) {
	applyDetachFlags(cmd)
	process.SuppressConsoleAttach(cmd)
}

// configureDetachedGUI configures a spawn of a replacement GUI (RestartV3
// self-restart) and deliberately does NOT suppress the console attach —
// the child must keep the operator's terminal under --foreground. See the
// Windows sibling for the full rationale; the split exists so that
// difference is a choice of constructor rather than a note to remember.
func configureDetachedGUI(cmd *exec.Cmd) {
	applyDetachFlags(cmd)
}
