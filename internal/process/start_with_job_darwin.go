//go:build darwin

package process

import (
	"errors"
	"os/exec"
	"syscall"
)

// StartWithJob is the Darwin preview spawn primitive. There is no Job Object
// equivalent, but children are started as process-group leaders so later
// lifecycle code can target the group. The kqueue NOTE_EXIT watcher remains a
// v0.6 follow-up; this function only closes the missing process-group portion.
func StartWithJob(job *Job, cmd *exec.Cmd) (int, error) {
	_ = job
	if cmd == nil {
		return 0, errors.New("StartWithJob: nil cmd")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	if cmd.Process == nil {
		return 0, errors.New("StartWithJob: cmd.Start returned no Process")
	}
	return cmd.Process.Pid, nil
}
