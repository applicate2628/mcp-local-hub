//go:build windows

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"mcp-local-hub/internal/process"
)

// These are new per-worker retained-memory budgets, not native Task Scheduler
// XML/schema limits. They are independent of the bytes a faulty worker emits. XML export/list and ordinary schtasks share this budget;
// an oversized response fails explicitly, never as an absent task or empty list.
const schedulerStdoutLimit = 16 << 20
const schedulerStderrLimit = 64 << 10

var errSchedulerOutputLimit = errors.New("scheduler: command output exceeds capture budget")
var errSchedulerUnsettled = errors.New("scheduler: command settlement unconfirmed")

// schedulerCommandNotDispatchedError is evidence owned by the boundary before
// RunStrictlyContained is called. No result or missing cmd.Process after runner
// admission can mint it: Windows can create a child before retaining its handle.
type schedulerCommandNotDispatchedError struct{ cause error }

func (e *schedulerCommandNotDispatchedError) Error() string { return e.cause.Error() }
func (e *schedulerCommandNotDispatchedError) Unwrap() error { return e.cause }

func schedulerCommandWasNotDispatched(err error) bool {
	// Only the current command boundary can issue this outer evidence. A
	// cancellation cause can contain another command's early-refusal error.
	_, notDispatched := err.(*schedulerCommandNotDispatchedError)
	return notDispatched
}

// runSchedulerCommand owns no second process lifecycle. The existing contained
// runner retains the original child/Job, finite input and both bounded drains.
// The caller supplies the operation deadline; cancellation is checked before
// this command can acquire native resources or begin a scheduler mutation.
func runSchedulerCommand(ctx context.Context, cmd *exec.Cmd, input []byte) (process.StrictRunResult, error) {
	if ctx == nil {
		return process.StrictRunResult{}, &schedulerCommandNotDispatchedError{cause: fmt.Errorf("%w: nil command context", ErrUnavailable)}
	}
	if err := ctx.Err(); err != nil {
		return process.StrictRunResult{}, &schedulerCommandNotDispatchedError{cause: fmt.Errorf("%w: %w", ErrUnavailable, errors.Join(err, context.Cause(ctx)))}
	}
	process.NoConsole(cmd)
	result, err := process.RunStrictlyContained(ctx, process.StrictRunInvocation{
		Command: cmd, Input: input, InputLimit: max(1, len(input)),
		StdoutLimit: schedulerStdoutLimit, StderrLimit: schedulerStderrLimit,
	})
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err(), context.Cause(ctx))
	}
	if result.Stdout.Truncated || result.Stderr.Truncated {
		err = errors.Join(err, errSchedulerOutputLimit)
	}
	if result.SettledExit == nil {
		err = errors.Join(err, errSchedulerUnsettled)
	}
	if err != nil {
		return result, fmt.Errorf("%w: %w", ErrUnavailable, errors.Join(err, context.Cause(ctx)))
	}
	return result, nil
}

func (w *windowsScheduler) BindContext(ctx context.Context) (Scheduler, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil operation context", ErrUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, errors.Join(err, context.Cause(ctx)))
	}
	bound := *w
	bound.ctx = ctx
	return &bound, nil
}

func (w *windowsScheduler) operationContext() context.Context {
	if w.ctx != nil {
		return w.ctx
	}
	return context.Background()
}

var _ ContextBinder = (*windowsScheduler)(nil)

// A settled nonzero exit is a command failure, not proof of task absence. Loss
// of the acknowledgement or settlement additionally makes the mutation outcome
// unknown. Preserve every original cause for callers' errors.Is/errors.As checks.
func schedulerCommandMutationError(operation string, result process.StrictRunResult, err error) error {
	if err == nil {
		return nil
	}
	if !schedulerCommandWasNotDispatched(err) && (result.SettledExit == nil || result.SettledExit.TimedOut || result.Stdout.Truncated || result.Stderr.Truncated || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		err = errors.Join(ErrMutationOutcomeUnknown, err)
	}
	return fmt.Errorf("schtasks %s: %w", operation, err)
}
