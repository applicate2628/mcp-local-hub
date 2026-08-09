//go:build darwin

package process

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// StartWithJob is the Darwin preview spawn primitive. There is no Job Object
// equivalent, but children are started as process-group leaders so later
// lifecycle code can target the group. The kqueue NOTE_EXIT watcher remains a
// v0.6 follow-up; this function only closes the missing process-group portion.
func StartWithJob(job *Job, cmd *exec.Cmd) (int, error) {
	return startWithJobFiles(job, cmd, nil, nil, nil)
}

func startWithJobFiles(job *Job, cmd *exec.Cmd, stdin, stdout, stderr *os.File) (int, error) {
	_ = job
	if cmd == nil {
		return 0, startWithJobError(StartWithJobInvalid, errors.New("StartWithJob: nil cmd"))
	}
	if (stdin == nil) != (stdout == nil) || (stdin == nil) != (stderr == nil) {
		return 0, startWithJobError(StartWithJobInvalid, errors.New("StartWithJob: standard files must be all nil or all present"))
	}
	if stdin != nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return 0, startWithJobError(StartWithJobLaunch, err)
	}
	if cmd.Process == nil {
		return 0, startWithJobError(StartWithJobLaunch, errors.New("StartWithJob: cmd.Start returned no Process"))
	}
	return cmd.Process.Pid, nil
}
