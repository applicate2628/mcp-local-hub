package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// ProvisionalProcess owns one contained process tree that exists only while a
// caller performs an admission probe. It is deliberately separate from the
// supervisor's long-lived child ownership: a successful probe MUST still reap
// the whole provisional tree before the caller may advertise or persist it.
type ProvisionalProcess struct {
	cmd *exec.Cmd
	job *Job

	waitOnce sync.Once
	waitDone chan error
}

// StartProvisional starts cmd in an at-create contained Job on Windows and as
// a process-group leader on POSIX. The returned owner must be settled with
// TerminateAndWait on every path.
func StartProvisional(cmd *exec.Cmd) (*ProvisionalProcess, error) {
	if cmd == nil || cmd.Path == "" {
		return nil, errors.New("start provisional process: command is nil or has an empty path")
	}
	job, err := NewKillOnCloseJob()
	if err != nil || job == nil {
		if err == nil {
			err = errors.New("job creation returned nil job")
		}
		return nil, fmt.Errorf("start provisional process containment: %w", err)
	}
	prepareProcessGroup(cmd)
	if _, err := StartWithJob(job, cmd); err != nil {
		closeErr := job.Close()
		return nil, errors.Join(fmt.Errorf("start provisional process: %w", err), closeErr)
	}
	return &ProvisionalProcess{cmd: cmd, job: job, waitDone: make(chan error, 1)}, nil
}

func (p *ProvisionalProcess) wait() <-chan error {
	if p == nil {
		ch := make(chan error, 1)
		ch <- errors.New("wait provisional process: nil owner")
		return ch
	}
	p.waitOnce.Do(func() {
		go func() { p.waitDone <- p.cmd.Wait() }()
	})
	return p.waitDone
}

// TerminateAndWait reaps the entire provisional tree before returning. A
// non-zero child exit after the requested termination is expected and does not
// make cleanup fail; failure to signal, wait, or close containment does.
func (p *ProvisionalProcess) TerminateAndWait(timeout time.Duration) error {
	if p == nil || p.cmd == nil || p.job == nil {
		return errors.New("terminate provisional process: invalid owner")
	}
	if timeout <= 0 {
		return errors.New("terminate provisional process: non-positive timeout")
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	var cleanup error
	cleanup = errors.Join(cleanup, p.job.TerminateAll(uint32(timeout.Milliseconds())))
	killProcessGroup(p.cmd)
	var directKillErr error
	if p.cmd.Process != nil {
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			directKillErr = err
		}
	}
	select {
	case err := <-p.wait():
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			cleanup = errors.Join(cleanup, err)
		}
		// A Job/process-group signal can win the race with os.Process.Kill;
		// a reaped direct child is the stronger completion proof, so an
		// AccessDenied-like late direct-kill result is not a cleanup failure.
		directKillErr = nil
	case <-deadline.C:
		cleanup = errors.Join(cleanup, directKillErr, errors.New("terminate provisional process: wait deadline exceeded"))
	}
	cleanup = errors.Join(cleanup, p.job.Close())
	return cleanup
}
