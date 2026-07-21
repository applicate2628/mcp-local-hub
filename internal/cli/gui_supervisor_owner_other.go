//go:build !windows

package cli

import (
	"os/exec"
	"syscall"

	"mcp-local-hub/internal/process"
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
//
// The Windows sibling needs a SECOND half (process.SuppressConsoleAttach)
// because a Windows child can re-attach to its parent's console after
// being created detached. Setsid has no such loophole — the severance is
// unconditional and the child cannot re-acquire the controlling terminal
// — so Setsid alone is the effective detach here.
//
// The marker is nevertheless applied on POSIX too. It changes nothing at
// runtime (POSIX main() has no AttachConsole, so the env var is inert), but
// it keeps ONE answer to "is this child attach-suppressed?" instead of an
// answer that depends on which platform's file the reader happens to open.
// The internal/gui supervisor configurator does the same.
func configureSupervisorDetach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	process.SuppressConsoleAttach(cmd)
}
