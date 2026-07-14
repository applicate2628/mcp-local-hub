//go:build !windows

package cbuild

import (
	"os/exec"
	"syscall"
)

// procGroup is the POSIX process-group seam used by runCommand to reap the whole
// child tree on timeout/cancel. The child is placed in its own process group at
// fork (Setpgid); a single kill to the negative pgid signals every process in
// the group, so ninja/make and their compiler grandchildren die together.
type procGroup struct{}

func newProcGroup() *procGroup { return &procGroup{} }

// configure puts the child in a new process group so the whole tree can be
// signaled at once. Must be called before cmd.Start.
func (p *procGroup) configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Setpgid with Pgid==0 makes the child the leader of a new group whose id
	// equals the child pid.
	cmd.SysProcAttr.Setpgid = true
}

// start is a no-op on POSIX: the process group is established by the kernel at
// fork via the Setpgid attribute above.
func (p *procGroup) start(cmd *exec.Cmd) {}

// kill sends SIGKILL to the entire process group (negative pid targets the
// group). Falls back to a single-process kill on any error.
func (p *procGroup) kill(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// close has nothing to release on POSIX.
func (p *procGroup) close() {}
