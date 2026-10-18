package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
)

// StrictRunFailureKind classifies the single contained-worker lifecycle.
type StrictRunFailureKind string

const (
	StrictRunContainmentFailed StrictRunFailureKind = "containment_failed"
	StrictRunTimeout           StrictRunFailureKind = "timeout"
	StrictRunExecutionFailed   StrictRunFailureKind = "execution_failed"
	StrictRunInvalidInvocation StrictRunFailureKind = "invalid_invocation"
)

// Only the default Windows context-timeout path spends one Job polling quantum
// proving post-kill member settlement. It does not extend child execution.
const strictRunWindowsTimeoutJobProofMs uint32 = 50

// StrictRunError retains the exact lower-level cause for errors.Is/errors.As.
type StrictRunError struct {
	Kind  StrictRunFailureKind
	Cause error
}

func (e *StrictRunError) Error() string {
	if e == nil || e.Cause == nil {
		return "strict contained run: " + string(e.Kind)
	}
	return "strict contained run: " + string(e.Kind) + ": " + e.Cause.Error()
}
func (e *StrictRunError) Unwrap() error { return e.Cause }
func strictRunError(kind StrictRunFailureKind, cause error) error {
	return &StrictRunError{Kind: kind, Cause: cause}
}

// StrictRunCapture is an independently bounded stream observation. Prefix is
// retained only up to the requested limit; Bytes counts all drained bytes.
type StrictRunCapture struct {
	Prefix    []byte
	Bytes     int
	Truncated bool
}

// StrictRunResult is returned even when the child exits unsuccessfully so the
// composition owner can emit only safe stream metadata.
type StrictRunResult struct {
	Stdout      StrictRunCapture
	Stderr      StrictRunCapture
	SettledExit *StrictRunSettledExit `json:"-"`
}

// StrictRunSettledExit is set only after the child, Job, streams and pipes
// have all been positively settled. It is not part of any JSON protocol.
type StrictRunSettledExit struct {
	ExitCode int
	TimedOut bool
}

// StrictRunInvocation makes the strict runner the sole standard-stream owner.
// Input is finite and every limit must be positive.
type StrictRunInvocation struct {
	Command     *exec.Cmd
	Input       []byte
	InputLimit  int
	StdoutLimit int
	StderrLimit int
	pipe        func() (*os.File, *os.File, error) // per-call test seam
	started     func(*Job, int)                    // per-call test seam
	timeoutKill func(*Job, *exec.Cmd) error        // per-call test seam
}

func (i StrictRunInvocation) validate() error {
	if i.Command == nil {
		return errors.New("nil command")
	}
	if i.Command.Path == "" {
		return errors.New("command path is empty")
	}
	if i.InputLimit <= 0 || i.StdoutLimit <= 0 || i.StderrLimit <= 0 {
		return errors.New("strict stream limits must be positive")
	}
	if len(i.Input) > i.InputLimit {
		return fmt.Errorf("strict input exceeds limit: %d > %d", len(i.Input), i.InputLimit)
	}
	if i.Command.Stdin != nil || i.Command.Stdout != nil || i.Command.Stderr != nil {
		return errors.New("command standard streams must be nil; strict runner owns them")
	}
	return nil
}

// RunStrictlyContained establishes containment before the child starts, owns
// finite input and independent bounded output drains, and does not return until
// the child and those drains have been joined.
func RunStrictlyContained(ctx context.Context, invocation StrictRunInvocation) (StrictRunResult, error) {
	return runStrictlyContained(ctx, invocation, nil)
}

// RunStrictlyContainedWithJobCloseFaultForTest injects one close result only
// after the real Job is closed.
func RunStrictlyContainedWithJobCloseFaultForTest(ctx context.Context, invocation StrictRunInvocation, fault error) (StrictRunResult, error) {
	if fault == nil {
		return StrictRunResult{}, strictRunError(StrictRunInvalidInvocation, errors.New("nil test close fault"))
	}
	return runStrictlyContained(ctx, invocation, fault)
}

func runStrictlyContained(ctx context.Context, invocation StrictRunInvocation, closeFault error) (StrictRunResult, error) {
	if ctx == nil {
		return StrictRunResult{}, strictRunError(StrictRunInvalidInvocation, errors.New("nil context"))
	}
	if err := invocation.validate(); err != nil {
		return StrictRunResult{}, strictRunError(StrictRunInvalidInvocation, err)
	}
	job, err := NewKillOnCloseJob()
	if err != nil || job == nil {
		if err == nil {
			err = errors.New("job creation returned nil job")
		}
		return StrictRunResult{}, strictRunError(StrictRunContainmentFailed, fmt.Errorf("establish containment: %w", err))
	}
	return runStrictlyContainedWithJobCloseFault(ctx, invocation, job, closeFault)
}

// runStrictlyContainedWithJob is a per-call test seam for a deterministically
// invalid Job. It owns the supplied job's close lifetime exactly as production
// owns a newly-created one.
func runStrictlyContainedWithJob(ctx context.Context, invocation StrictRunInvocation, job *Job) (result StrictRunResult, err error) {
	return runStrictlyContainedWithJobCloseFault(ctx, invocation, job, nil)
}

func runStrictlyContainedWithJobCloseFault(ctx context.Context, invocation StrictRunInvocation, job *Job, closeFault error) (result StrictRunResult, err error) {
	if job == nil {
		return result, strictRunError(StrictRunContainmentFailed, errors.New("nil job"))
	}
	jobClosed := false
	jobCloseReported := false
	var jobCloseErr error
	closeOwnedJob := func() error {
		if !jobClosed {
			jobClosed = true
			jobCloseErr = job.Close()
			if closeFault != nil {
				jobCloseErr = errors.Join(jobCloseErr, closeFault)
			}
		}
		if jobCloseReported {
			return nil
		}
		jobCloseReported = true
		return jobCloseErr
	}
	defer func() {
		if closeErr := closeOwnedJob(); closeErr != nil {
			if err == nil {
				err = strictRunError(StrictRunExecutionFailed, closeErr)
				return
			}
			var strictErr *StrictRunError
			if errors.As(err, &strictErr) {
				strictErr.Cause = errors.Join(strictErr.Cause, closeErr)
				return
			}
			err = errors.Join(err, closeErr)
		}
	}()
	if ctx == nil {
		return result, strictRunError(StrictRunInvalidInvocation, errors.New("nil context"))
	}
	if err := invocation.validate(); err != nil {
		return result, strictRunError(StrictRunInvalidInvocation, err)
	}

	makePipe := os.Pipe
	if invocation.pipe != nil {
		makePipe = invocation.pipe
	}
	stdinChild, stdinParent, pipeErr := makePipe()
	if pipeErr != nil {
		return result, strictRunError(StrictRunExecutionFailed, fmt.Errorf("create stdin pipe: %w", pipeErr))
	}
	stdoutParent, stdoutChild, pipeErr := makePipe()
	if pipeErr != nil {
		_ = stdinChild.Close()
		_ = stdinParent.Close()
		return result, strictRunError(StrictRunExecutionFailed, fmt.Errorf("create stdout pipe: %w", pipeErr))
	}
	stderrParent, stderrChild, pipeErr := makePipe()
	if pipeErr != nil {
		_ = stdinChild.Close()
		_ = stdinParent.Close()
		_ = stdoutParent.Close()
		_ = stdoutChild.Close()
		return result, strictRunError(StrictRunExecutionFailed, fmt.Errorf("create stderr pipe: %w", pipeErr))
	}
	childFiles := []*os.File{stdinChild, stdoutChild, stderrChild}
	parentFiles := []*os.File{stdinParent, stdoutParent, stderrParent}
	closeFiles := func(files []*os.File) error {
		var joined error
		for _, f := range files {
			if f != nil {
				if closeErr := f.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
					joined = errors.Join(joined, closeErr)
				}
			}
		}
		return joined
	}
	closeChildren := func() error { return closeFiles(childFiles) }
	closeParents := func() error { return closeFiles(parentFiles) }

	prepareProcessGroup(invocation.Command)
	if _, startErr := startWithJobFiles(job, invocation.Command, stdinChild, stdoutChild, stderrChild); startErr != nil {
		return result, strictRunError(strictKindForStart(startErr), errors.Join(fmt.Errorf("start contained worker: %w", startErr), closeChildren(), closeParents(), closeOwnedJob()))
	}
	if invocation.started != nil {
		invocation.started(job, invocation.Command.Process.Pid)
	}
	// Parent must release child endpoints before I/O begins; descendants are
	// later terminated/closed with the Job or process group before draining joins.
	if closeErr := closeChildren(); closeErr != nil {
		terminateErr := job.TerminateAll(1)
		killProcessGroup(invocation.Command)
		if invocation.Command.Process != nil {
			_ = invocation.Command.Process.Kill()
		}
		waitErr := invocation.Command.Wait()
		return result, strictRunError(StrictRunExecutionFailed, errors.Join(closeErr, terminateErr, waitErr, closeParents(), closeOwnedJob()))
	}

	inputDone := make(chan error, 1)
	go func() {
		_, writeErr := stdinParent.Write(invocation.Input)
		inputDone <- errors.Join(writeErr, stdinParent.Close())
	}()
	stdoutDone := make(chan error, 1)
	stderrDone := make(chan error, 1)
	go func() {
		var e error
		result.Stdout, e = drainStrictCapture(stdoutParent, invocation.StdoutLimit)
		stdoutDone <- e
	}()
	go func() {
		var e error
		result.Stderr, e = drainStrictCapture(stderrParent, invocation.StderrLimit)
		stderrDone <- e
	}()

	waited := make(chan error, 1)
	go func() { waited <- waitStrictContainedCommand(invocation.Command) }()
	var waitErr, childWaitErr, unexpectedTerminationErr error
	var timeout bool
	select {
	case childWaitErr = <-waited:
		waitErr = childWaitErr
	case <-ctx.Done():
		timeout = true
		if invocation.timeoutKill != nil {
			waitErr = invocation.timeoutKill(job, invocation.Command)
			unexpectedTerminationErr = waitErr
		} else {
			proofMs := uint32(1)
			if runtime.GOOS == "windows" {
				proofMs = strictRunWindowsTimeoutJobProofMs
			}
			termErr := job.TerminateAll(proofMs)
			unexpectedTerminationErr = termErr
			killProcessGroup(invocation.Command)
			waitErr = termErr
			if invocation.Command.Process != nil {
				killErr := invocation.Command.Process.Kill()
				waitErr = errors.Join(waitErr, killErr)
				// A successful TerminateAll already proved the Job empty;
				// this direct kill is redundant and may report an exited PID.
				if termErr != nil && killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
					unexpectedTerminationErr = errors.Join(unexpectedTerminationErr, killErr)
				}
			}
		}
		// Closing a kill-on-close Job is the final containment primitive. It
		// must happen before joining Wait: TerminateAll and Process.Kill can
		// both fail, and waiting first would otherwise make the caller's
		// deadline unbounded while the still-owned Job could still reap the
		// worker tree.
		closeErr := closeOwnedJob()
		unexpectedTerminationErr = errors.Join(unexpectedTerminationErr, closeErr)
		waitErr = errors.Join(waitErr, closeErr)
		childWaitErr = <-waited
		waitErr = errors.Join(waitErr, childWaitErr)
	}
	// The output readers own the active drains until EOF. Closing their read
	// endpoints before joining them can convert a successful fast-exit worker
	// into os.ErrClosed and discard already-buffered protocol output.
	cleanupErr := errors.Join(closeOwnedJob(), <-inputDone, <-stdoutDone, <-stderrDone, closeParents())
	var exitErr *exec.ExitError
	if cleanupErr == nil && unexpectedTerminationErr == nil && (!timeout || (runtime.GOOS == "windows" && invocation.timeoutKill == nil)) &&
		(childWaitErr == nil || errors.As(childWaitErr, &exitErr)) &&
		invocation.Command.ProcessState != nil && invocation.Command.ProcessState.Exited() {
		result.SettledExit = &StrictRunSettledExit{ExitCode: invocation.Command.ProcessState.ExitCode(), TimedOut: timeout}
	}
	if timeout {
		return result, strictRunError(StrictRunTimeout, errors.Join(ctx.Err(), waitErr, cleanupErr))
	}
	if waitErr != nil || cleanupErr != nil {
		return result, strictRunError(StrictRunExecutionFailed, errors.Join(waitErr, cleanupErr))
	}
	return result, nil
}

func strictKindForStart(err error) StrictRunFailureKind {
	var startErr *StartWithJobError
	if errors.As(err, &startErr) {
		switch startErr.Phase {
		case StartWithJobContainment:
			return StrictRunContainmentFailed
		case StartWithJobInvalid:
			return StrictRunInvalidInvocation
		}
	}
	return StrictRunExecutionFailed
}

func drainStrictCapture(reader io.Reader, limit int) (StrictRunCapture, error) {
	var capture StrictRunCapture
	buf := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			capture.Bytes += n
			remaining := limit - len(capture.Prefix)
			if remaining > 0 {
				if remaining > n {
					remaining = n
				}
				capture.Prefix = append(capture.Prefix, buf[:remaining]...)
			}
			if capture.Bytes > limit {
				capture.Truncated = true
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return capture, nil
			}
			return capture, readErr
		}
	}
}
