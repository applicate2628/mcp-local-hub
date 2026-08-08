//go:build !windows && !darwin

package process

import (
	"errors"
	"os"
	"os/exec"
)

// StartWithJob is the POSIX shim of the Windows assign-at-create
// primitive. On POSIX there is no Job-Object analogue — orphan
// protection is split across PR_SET_PDEATHSIG on Linux and (planned)
// kqueue NOTE_EXIT on macOS/BSD. See jobobject_other.go for the
// rationale.
//
// The stub applies SetParentDeathSignal before cmd.Start() and returns
// the spawned PID so callers (notably the v0.5.0 supervisor reconcile
// loop) can stay platform-agnostic at the call site. The job argument
// is accepted but ignored — the POSIX Job is itself a no-op stub.
//
// Returns the child PID on success. The caller is responsible for
// reaping the child via cmd.Wait() / cmd.Process.Wait().
func StartWithJob(job *Job, cmd *exec.Cmd) (int, error) {
	return startWithJobFiles(job, cmd, nil, nil, nil)
}

func startWithJobFiles(job *Job, cmd *exec.Cmd, stdin, stdout, stderr *os.File) (int, error) {
	if cmd == nil {
		return 0, startWithJobError(StartWithJobInvalid, errors.New("StartWithJob: nil cmd"))
	}
	if (stdin == nil) != (stdout == nil) || (stdin == nil) != (stderr == nil) {
		return 0, startWithJobError(StartWithJobInvalid, errors.New("StartWithJob: standard files must be all nil or all present"))
	}
	if stdin != nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	}
	SetParentDeathSignal(cmd)
	if err := cmd.Start(); err != nil {
		return 0, startWithJobError(StartWithJobLaunch, err)
	}
	if cmd.Process == nil {
		return 0, startWithJobError(StartWithJobLaunch, errors.New("StartWithJob: cmd.Start returned no Process"))
	}
	return cmd.Process.Pid, nil
}
