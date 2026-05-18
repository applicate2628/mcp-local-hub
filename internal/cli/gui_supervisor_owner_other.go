//go:build !windows

package cli

import (
	"os/exec"
	"syscall"
)

// configureSupervisorDetach is the POSIX variant: spawn the
// supervisor in a new process session via Setsid so it does NOT
// inherit the GUI's process group. The supervisor's own Job (or
// process-group on POSIX) then owns daemon children — the GUI's
// process tree stays a pure UI surface.
//
// Setsid (vs just Setpgid) additionally severs the controlling
// terminal so the supervisor can outlive a hangup that closes the
// GUI's tty. This matches the production autostart contract: the
// supervisor is detached background, not a foreground job.
func configureSupervisorDetach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
