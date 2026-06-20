//go:build !windows

package process

import (
	"os/exec"
	"syscall"
)

// prepareProcessGroup puts the child in a NEW process group, making it the group
// leader (so its PID == the PGID). A wrapper command (sh -c, npm, uvx) forks the
// real server into this same group; CommandContext's deadline-kill targets only
// the direct leader, so without a group those forks would survive. The group lets
// killProcessGroup signal the WHOLE descendant tree.
//
// Windows has no analogue here — the KILL_ON_JOB_CLOSE Job Object owns tree-kill;
// see process_group_windows.go.
func prepareProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup SIGKILLs the child's process group to reap any forks the
// context-deadline kill of the leader missed (the orphan grandchild that keeps a
// server's port/pipe open). prepareProcessGroup made the child the group leader,
// so its PID is the PGID. Best-effort: a clean exit leaves an empty group and the
// signal is a harmless no-op.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
