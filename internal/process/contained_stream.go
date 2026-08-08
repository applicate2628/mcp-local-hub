package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"sync"
	"time"
)

// ContainedRunStage identifies the owner stage that produced a contained-run
// failure. The string values are stable internal diagnostics; Error never
// includes command, environment, path, or stream content.
type ContainedRunStage string

const (
	ContainedStageValidate    ContainedRunStage = "validate"
	ContainedStagePipeSetup   ContainedRunStage = "pipe_setup"
	ContainedStageContainment ContainedRunStage = "containment"
	ContainedStageStart       ContainedRunStage = "start"
	ContainedStageStdout      ContainedRunStage = "stdout"
	ContainedStageStderr      ContainedRunStage = "stderr"
	ContainedStageWait        ContainedRunStage = "wait"
	ContainedStageExit        ContainedRunStage = "exit"
	ContainedStageCleanup     ContainedRunStage = "cleanup"
)

var (
	ErrInvalidCleanupTimeout  = errors.New("invalid cleanup timeout")
	ErrContainmentUnavailable = errors.New("process containment unavailable")
	ErrCleanupTimeout         = errors.New("process cleanup timeout")

	errInvalidContainedRun = errors.New("invalid contained stream request")
	errContainedExit       = errors.New("contained process exited non-zero")
)

const maxContainedCleanupTimeout time.Duration = time.Duration(math.MaxUint32) * time.Millisecond

// ContainedStreamOptions configures one finite contained process invocation.
// Stderr is borrowed for the duration of RunContainedStream and is never
// closed by the runner.
type ContainedStreamOptions struct {
	CleanupTimeout time.Duration
	Stderr         io.Writer
}

// ContainedRunError preserves a primary lifecycle cause plus an optional
// ordered cleanup cause. Error is deliberately fixed-text and secret-free.
type ContainedRunError struct {
	Stage        ContainedRunStage
	Cause        error
	ExitCode     *int
	CleanupStage ContainedRunStage
	CleanupCause error
}

func (e *ContainedRunError) Error() string {
	if e == nil {
		return "contained process failed"
	}
	return "contained process " + string(e.Stage) + " failed"
}

// Unwrap keeps the primary cause first and the cleanup cause second.
func (e *ContainedRunError) Unwrap() []error {
	if e == nil {
		return nil
	}
	out := make([]error, 0, 2)
	if e.Cause != nil {
		out = append(out, e.Cause)
	}
	if e.CleanupCause != nil {
		out = append(out, e.CleanupCause)
	}
	return out
}

type containedReadFile interface {
	io.Reader
	io.Closer
	Fd() uintptr
}

type containedWriteFile interface {
	io.Writer
	io.Closer
	Fd() uintptr
}

type containedInputFile interface {
	io.Reader
	io.Closer
	Fd() uintptr
}

type containedWaitResult struct {
	exitCode int
	exited   bool
	err      error
}

type containedChild interface {
	start(*exec.Cmd, containedInputFile, containedWriteFile, containedWriteFile) error
	wait() containedWaitResult
	terminate(uint32) error
	close() error
}

// containedDeadlineChild is an additive private capability for platform
// children whose cleanup has multiple phases. RunContainedStream owns the one
// absolute cleanup deadline; implementations must not extend or resample it.
type containedDeadlineChild interface {
	terminateBy(time.Time) error
}

type containedStreamDependencies struct {
	newChild func(*exec.Cmd) (containedChild, error)
	pipe     func() (containedReadFile, containedWriteFile, error)
	openNull func() (containedInputFile, error)
}

func defaultContainedStreamDependencies() containedStreamDependencies {
	return containedStreamDependencies{
		newChild: newPlatformContainedChild,
		pipe: func() (containedReadFile, containedWriteFile, error) {
			return os.Pipe()
		},
		openNull: openContainedNull,
	}
}

type containedPlatformError struct {
	stage ContainedRunStage
	cause error
}

func (e *containedPlatformError) Error() string { return "contained platform operation failed" }
func (e *containedPlatformError) Unwrap() error { return e.cause }

type ownedCloser struct {
	mu     sync.Mutex
	closer io.Closer
	closed bool
}

func ownCloser(c io.Closer) *ownedCloser {
	return &ownedCloser{closer: c}
}

func (c *ownedCloser) close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.closer == nil {
		return nil
	}
	return c.closer.Close()
}

func closeOwned(closers ...*ownedCloser) error {
	var out error
	for _, closer := range closers {
		if err := closer.close(); err != nil {
			out = errors.Join(out, err)
		}
	}
	return out
}

func containedError(stage ContainedRunStage, cause error, cleanup error) error {
	if cause == nil && cleanup == nil {
		return nil
	}
	if cause == nil {
		return &ContainedRunError{
			Stage: ContainedStageCleanup,
			Cause: cleanup,
		}
	}
	out := &ContainedRunError{Stage: stage, Cause: cause}
	if cleanup != nil {
		out.CleanupStage = ContainedStageCleanup
		out.CleanupCause = cleanup
	}
	return out
}

func validateContainedStream(
	ctx context.Context,
	cmd *exec.Cmd,
	options ContainedStreamOptions,
	consumeStdout func(io.Reader) error,
) (uint32, error) {
	if ctx == nil || cmd == nil || consumeStdout == nil {
		return 0, &ContainedRunError{Stage: ContainedStageValidate, Cause: errInvalidContainedRun}
	}
	if err := ctx.Err(); err != nil {
		return 0, &ContainedRunError{Stage: ContainedStageValidate, Cause: err}
	}
	if cmd.Process != nil || cmd.ProcessState != nil ||
		cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil ||
		len(cmd.ExtraFiles) != 0 || cmd.Cancel != nil || cmd.WaitDelay != 0 {
		return 0, &ContainedRunError{Stage: ContainedStageValidate, Cause: errInvalidContainedRun}
	}
	if options.CleanupTimeout < time.Millisecond ||
		options.CleanupTimeout > maxContainedCleanupTimeout ||
		options.CleanupTimeout%time.Millisecond != 0 {
		return 0, &ContainedRunError{
			Stage: ContainedStageValidate,
			Cause: ErrInvalidCleanupTimeout,
		}
	}
	// This is the only duration-to-milliseconds conversion in the seam.
	return uint32(options.CleanupTimeout / time.Millisecond), nil
}

// RunContainedStream runs one finite command with fail-closed descendant
// containment while the caller consumes stdout incrementally.
func RunContainedStream(
	ctx context.Context,
	cmd *exec.Cmd,
	options ContainedStreamOptions,
	consumeStdout func(io.Reader) error,
) error {
	return runContainedStreamWithDependencies(
		ctx,
		cmd,
		options,
		consumeStdout,
		defaultContainedStreamDependencies(),
	)
}

func runContainedStreamWithDependencies(
	ctx context.Context,
	cmd *exec.Cmd,
	options ContainedStreamOptions,
	consumeStdout func(io.Reader) error,
	deps containedStreamDependencies,
) error {
	cleanupTimeoutMs, err := validateContainedStream(ctx, cmd, options, consumeStdout)
	if err != nil {
		return err
	}
	if deps.newChild == nil || deps.pipe == nil || deps.openNull == nil {
		return &ContainedRunError{Stage: ContainedStageValidate, Cause: errInvalidContainedRun}
	}

	child, err := deps.newChild(cmd)
	if err != nil {
		return &ContainedRunError{
			Stage: ContainedStageContainment,
			Cause: errors.Join(ErrContainmentUnavailable, err),
		}
	}
	if child == nil {
		return &ContainedRunError{
			Stage: ContainedStageContainment,
			Cause: errors.Join(ErrContainmentUnavailable, errInvalidContainedRun),
		}
	}
	childCloser := ownCloser(closerFunc(child.close))

	stdoutRead, stdoutWrite, err := deps.pipe()
	if err != nil {
		return containedError(
			ContainedStagePipeSetup,
			err,
			closeOwned(childCloser),
		)
	}
	stdoutReadCloser := ownCloser(stdoutRead)
	stdoutWriteCloser := ownCloser(stdoutWrite)

	stderrRead, stderrWrite, err := deps.pipe()
	if err != nil {
		return containedError(
			ContainedStagePipeSetup,
			err,
			closeOwned(stdoutWriteCloser, stdoutReadCloser, childCloser),
		)
	}
	stderrReadCloser := ownCloser(stderrRead)
	stderrWriteCloser := ownCloser(stderrWrite)

	stdin, err := deps.openNull()
	if err != nil {
		return containedError(
			ContainedStagePipeSetup,
			err,
			closeOwned(stderrWriteCloser, stderrReadCloser, stdoutWriteCloser, stdoutReadCloser, childCloser),
		)
	}
	stdinCloser := ownCloser(stdin)

	if err := child.start(cmd, stdin, stdoutWrite, stderrWrite); err != nil {
		stage := ContainedStageStart
		cause := err
		var platformErr *containedPlatformError
		if errors.As(err, &platformErr) {
			stage = platformErr.stage
			cause = platformErr.cause
			if stage == ContainedStageContainment {
				cause = errors.Join(ErrContainmentUnavailable, cause)
			}
		}
		return containedError(
			stage,
			cause,
			closeOwned(stdinCloser, stderrWriteCloser, stderrReadCloser, stdoutWriteCloser, stdoutReadCloser, childCloser),
		)
	}

	// The child owns duplicated standard handles now. Closing the parent's
	// copies is mandatory for EOF and never transfers reader ownership.
	cleanupErr := closeOwned(stdinCloser, stderrWriteCloser, stdoutWriteCloser)

	type streamResult struct {
		err error
	}
	stdoutDone := make(chan streamResult, 1)
	stderrDone := make(chan streamResult, 1)
	waitDone := make(chan containedWaitResult, 1)

	go func() {
		stdoutDone <- streamResult{err: consumeStdout(stdoutRead)}
	}()
	stderrTarget := options.Stderr
	if stderrTarget == nil {
		stderrTarget = io.Discard
	}
	go func() {
		_, copyErr := io.Copy(stderrTarget, stderrRead)
		stderrDone <- streamResult{err: copyErr}
	}()
	go func() {
		waitDone <- child.wait()
	}()

	var (
		stdoutResult            streamResult
		stderrResult            streamResult
		waitResult              containedWaitResult
		stdoutCompleted         bool
		stderrCompleted         bool
		waitCompleted           bool
		ctxErr                  error
		teardown                = cleanupErr != nil
		readersClosed           bool
		stdoutBeforeReaderClose bool
		stderrBeforeReaderClose bool
	)

	for !teardown {
		select {
		case stdoutResult = <-stdoutDone:
			stdoutCompleted = true
			stdoutBeforeReaderClose = !readersClosed
			if stdoutResult.err != nil {
				teardown = true
			}
		case stderrResult = <-stderrDone:
			stderrCompleted = true
			stderrBeforeReaderClose = !readersClosed
			if stderrResult.err != nil {
				teardown = true
			}
		case waitResult = <-waitDone:
			waitCompleted = true
			teardown = true
		case <-ctx.Done():
			ctxErr = ctx.Err()
			teardown = true
		}
	}

	cleanupDeadline := time.Now().Add(time.Duration(cleanupTimeoutMs) * time.Millisecond)
	_, deadlineAware := child.(containedDeadlineChild)
	var terminateErr error
	if deadlineChild, ok := child.(containedDeadlineChild); ok {
		terminateErr = deadlineChild.terminateBy(cleanupDeadline)
	} else {
		terminateErr = child.terminate(cleanupTimeoutMs)
	}
	if terminateErr != nil {
		cleanupErr = errors.Join(cleanupErr, terminateErr)
		// Kill-on-close is the final Windows backstop; on POSIX this is a
		// harmless no-op. Closing readers then releases owner-blocked I/O.
		cleanupErr = errors.Join(cleanupErr, childCloser.close())
		cleanupErr = errors.Join(cleanupErr, closeOwned(stdoutReadCloser, stderrReadCloser))
		readersClosed = true
	}

	drainReady := func() {
		for {
			drained := false
			if !stdoutCompleted {
				select {
				case stdoutResult = <-stdoutDone:
					stdoutCompleted = true
					stdoutBeforeReaderClose = !readersClosed
					drained = true
				default:
				}
			}
			if !stderrCompleted {
				select {
				case stderrResult = <-stderrDone:
					stderrCompleted = true
					stderrBeforeReaderClose = !readersClosed
					drained = true
				default:
				}
			}
			if !waitCompleted {
				select {
				case waitResult = <-waitDone:
					waitCompleted = true
					drained = true
				default:
				}
			}
			if !drained {
				return
			}
		}
	}

	// Completions published before termination returned outrank an already
	// expired deadline. Repeating this drain when the timer fires closes the
	// select race between a ready completion and a ready timer.
	drainReady()
	joinTimer := time.NewTimer(time.Until(cleanupDeadline))
	defer joinTimer.Stop()
	joinTimeout := joinTimer.C
	for !stdoutCompleted || !stderrCompleted || !waitCompleted {
		select {
		case stdoutResult = <-stdoutDone:
			stdoutCompleted = true
			stdoutBeforeReaderClose = !readersClosed
		case stderrResult = <-stderrDone:
			stderrCompleted = true
			stderrBeforeReaderClose = !readersClosed
		case waitResult = <-waitDone:
			waitCompleted = true
		case <-joinTimeout:
			drainReady()
			if stdoutCompleted && stderrCompleted && waitCompleted {
				joinTimeout = nil
				continue
			}
			if !readersClosed {
				joinErr := ErrCleanupTimeout
				if deadlineAware {
					joinErr = errors.Join(joinErr, errors.New("POSIX_JOIN_TIMEOUT"))
				}
				cleanupErr = errors.Join(cleanupErr, joinErr)
				cleanupErr = errors.Join(cleanupErr, childCloser.close())
				cleanupErr = errors.Join(cleanupErr, closeOwned(stdoutReadCloser, stderrReadCloser))
				readersClosed = true
			}
			// The direct child must still be reaped exactly once. After the
			// containment backstop and reader closure, keep joining rather
			// than returning a live child or goroutine.
			joinTimeout = nil
		}
	}

	if !readersClosed {
		cleanupErr = errors.Join(cleanupErr, closeOwned(stdoutReadCloser, stderrReadCloser))
	}
	cleanupErr = errors.Join(cleanupErr, childCloser.close())

	var primaryStage ContainedRunStage
	var primary error
	var exitCode *int

	// A genuine consumer failure outranks cancellation and process outcomes.
	if stdoutResult.err != nil && stdoutBeforeReaderClose {
		primaryStage = ContainedStageStdout
		primary = stdoutResult.err
	} else if ctxErr != nil {
		primaryStage = ContainedStageCleanup
		primary = ctxErr
	} else if stderrResult.err != nil && stderrBeforeReaderClose {
		primaryStage = ContainedStageStderr
		primary = stderrResult.err
	} else if waitResult.exited && waitResult.exitCode != 0 {
		code := waitResult.exitCode
		exitCode = &code
		primaryStage = ContainedStageExit
		primary = waitResult.err
		if primary == nil {
			primary = errContainedExit
		}
	} else if waitResult.err != nil {
		primaryStage = ContainedStageWait
		primary = waitResult.err
	}

	if primary == nil {
		return containedError(ContainedStageCleanup, nil, cleanupErr)
	}
	out := &ContainedRunError{
		Stage:    primaryStage,
		Cause:    primary,
		ExitCode: exitCode,
	}
	if cleanupErr != nil {
		out.CleanupStage = ContainedStageCleanup
		out.CleanupCause = cleanupErr
	}
	return out
}

type closerFunc func() error

func (f closerFunc) Close() error {
	if f == nil {
		return nil
	}
	return f()
}

func fixedContainedError(stage ContainedRunStage, err error) error {
	if err == nil {
		return nil
	}
	return &containedPlatformError{stage: stage, cause: fmt.Errorf("platform %s: %w", stage, err)}
}
