//go:build !windows

package gui

import (
	"os/exec"
	"syscall"
)

// configureDetached sets POSIX-specific flags on cmd so the spawned
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
func configureDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}
