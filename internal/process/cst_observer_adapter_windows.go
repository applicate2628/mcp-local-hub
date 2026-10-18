//go:build windows && cst_vptr_probe

package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

// CSTObserverAtCreateHooks are test-only, per-launch inputs. The caller owns
// each allowlisted handle and, after a successful callback, the original
// process handle; this adapter neither launches by PID nor duplicates a Job.
type CSTObserverAtCreateHooks struct {
	ChildHandles   []windows.Handle
	RetainOriginal func(windows.Handle, uint32) error
	FindProcess    func(int) (*os.Process, error)
}

// StartCSTObserverAtCreateForTest uses the ordinary Windows at-create owner.
// It is compiled only for the explicitly tagged fake diagnostic.
func StartCSTObserverAtCreateForTest(job *Job, cmd *exec.Cmd, hooks CSTObserverAtCreateHooks) (int, error) {
	if hooks.RetainOriginal == nil {
		return 0, startWithJobError(StartWithJobInvalid, errors.New("missing original-process retention callback"))
	}
	return startWithJobFilesObserved(job, cmd, nil, nil, nil, &startWithJobObserverOptions{
		retainOriginal: hooks.RetainOriginal,
		findProcess:    hooks.FindProcess,
		childHandles:   hooks.ChildHandles,
	})
}

// StartCSTObserverTargetAtCreateForTest supplies all three custodian-owned
// target files to the same at-create launcher. The separate F4 parent still
// calls StartCSTObserverAtCreateForTest with nil standard files.
func StartCSTObserverTargetAtCreateForTest(job *Job, cmd *exec.Cmd, stdin, stdout, stderr *os.File, hooks CSTObserverAtCreateHooks) (int, error) {
	if hooks.RetainOriginal == nil || stdin == nil || stdout == nil || stderr == nil {
		return 0, startWithJobError(StartWithJobInvalid, errors.New("target requires original retention and all three standard files"))
	}
	return startWithJobFilesObserved(job, cmd, stdin, stdout, stderr, &startWithJobObserverOptions{
		retainOriginal: hooks.RetainOriginal,
		findProcess:    hooks.FindProcess,
		childHandles:   hooks.ChildHandles,
	})
}

// CSTObserverOriginalMemberForTest checks the retained original handle, not a
// reopened PID. A syscall error is distinct from a negative membership fact.
func CSTObserverOriginalMemberForTest(job *Job, original windows.Handle) (bool, uint32, []uint32, error) {
	if job == nil || job.Handle() == 0 || original == 0 {
		return false, 0, nil, errors.New("missing original process or Job handle")
	}
	member, err := jobMemberFromHandle(original, job)
	if err != nil {
		return false, 0, nil, fmt.Errorf("IsProcessInJob(original): %w", err)
	}
	active, pids, err := CSTObserverJobCensusForTest(job)
	if err != nil {
		return false, 0, nil, err
	}
	return member, active, pids, nil
}

// CSTObserverJobCensusForTest keeps Job accounting separate from any one
// original process, which may already be signaled on the final parent poll.
func CSTObserverJobCensusForTest(job *Job) (uint32, []uint32, error) {
	if job == nil || job.Handle() == 0 {
		return 0, nil, errors.New("missing live Job handle")
	}
	info, err := job.queryAccounting()
	if err != nil {
		return 0, nil, fmt.Errorf("query guardian accounting: %w", err)
	}
	pids, err := job.processIDs()
	if err != nil {
		return 0, nil, fmt.Errorf("query guardian members: %w", err)
	}
	return info.ActiveProcesses, pids, nil
}
