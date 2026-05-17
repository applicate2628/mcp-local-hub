//go:build !windows

package process

import (
	"errors"
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
	if cmd == nil {
		return 0, errors.New("StartWithJob: nil cmd")
	}
	SetParentDeathSignal(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	if cmd.Process == nil {
		return 0, errors.New("StartWithJob: cmd.Start returned no Process")
	}
	return cmd.Process.Pid, nil
}
