//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ForegroundCreateAdmission is borrowed by the original creator. The hook
// validates the actual suspended process and releases its guards before return;
// a failure forbids ResumeThread. Close is cached by its owning implementation.
// RunForegroundAdmitted receives Close after native settlement on every exit.
type ForegroundCreateAdmission interface {
	AfterCreateBeforeResume(original *AtCreateJobProcess) error
	Close() error
}

// ForegroundResult preserves the acquired root result separately from any
// cancellation or settlement failure. Job and Original retain their actual
// capabilities, including partial acquisition and incomplete settlement.
// Caller-owned standard files are never closed by this operation.
type ForegroundResult struct {
	Root                    OriginalRootWait
	Native                  ContainedNativeSnapshot
	Settlement              *ContainedProcessSettlement
	Job                     *Job
	Original                *AtCreateJobProcess
	Cancelled               bool
	Outcome                 ForegroundOutcome
	Admission               ForegroundCreateAdmission
	AdmissionCloseAttempted bool
	AdmissionCloseError     error
}

// RunForeground forwards the supplied standard files without capture and waits
// on the original at-create Job process. ctx controls cancellation; it need not
// have a deadline. Only terminal cleanup uses the existing retained-process
// allowance. A child nonzero status is in Root, not converted into a new error.
// An incomplete result retains its original owners for the caller to inspect;
// this operation never retries creation or a failed native terminal action.
func RunForeground(ctx context.Context, cmd *exec.Cmd, stdin, stdout, stderr *os.File) (ForegroundResult, error) {
	return runForeground(ctx, cmd, stdin, stdout, stderr, foregroundDependencies{newJob: NewJobObserved, start: StartWithJobFilesOriginal})
}

// RunForegroundAdmitted uses the same foreground lifecycle with a required
// original-create admission. The caller retains it through this returned result.
func RunForegroundAdmitted(ctx context.Context, cmd *exec.Cmd, stdin, stdout, stderr *os.File, admission ForegroundCreateAdmission) (ForegroundResult, error) {
	if admission == nil {
		return ForegroundResult{}, errors.New("foreground creation admission required")
	}
	return runForeground(ctx, cmd, stdin, stdout, stderr, foregroundDependencies{newJob: NewJobObserved, admission: admission,
		start: func(job *Job, cmd *exec.Cmd, stdin, stdout, stderr *os.File) (*AtCreateJobProcess, error) {
			return startWithJobFilesOriginalAdmittedContext(ctx, job, cmd, stdin, stdout, stderr, admission)
		}})
}

// RetainedTerminationAllowance projects the existing native terminal budget;
// it is never an execution timeout or a renewed cleanup allowance.
func RetainedTerminationAllowance() time.Duration {
	return terminateWaitMilliseconds * time.Millisecond
}

// WaitOriginalContext waits on the original native handle without a waiter
// goroutine, then receives the existing single reap. It renews no deadline.
func (p *AtCreateJobProcess) WaitOriginalContext(ctx context.Context) OriginalRootWait {
	if p == nil || ctx == nil {
		return OriginalRootWait{Error: ErrContainmentUnavailable}
	}
	child, ok := p.child.(*windowsContainedChild)
	if !ok || child.original.handle == 0 {
		return OriginalRootWait{Error: ErrContainmentUnavailable}
	}
	waited, err := WaitRetainedProcessExit(ctx, uintptr(child.original.handle))
	if !waited.Exited {
		return OriginalRootWait{Error: errors.Join(err, errors.New("original foreground exit unproved"))}
	}
	result := p.WaitOriginal()
	result.Error = errors.Join(err, result.Error)
	return result
}

// BorrowOriginalProcessHandle lends the actual retained creation handle only
// synchronously. The consumer must neither retain nor close it. No PID reopen
// or handle reconstructed from observations can enter this path.
func (p *AtCreateJobProcess) BorrowOriginalProcessHandle(consume func(uintptr) error) error {
	if p == nil || consume == nil {
		return ErrContainmentUnavailable
	}
	child, ok := p.child.(*windowsContainedChild)
	if !ok || child == nil || child.memberSource == nil {
		return ErrContainmentUnavailable
	}
	release, err := child.memberSource.gate.enterQuery(nil)
	if err != nil {
		return err
	}
	defer release()
	if child.original.handle == 0 || child.processCloseAttempted {
		return ErrContainmentUnavailable
	}
	return consume(uintptr(child.original.handle))
}

// Per-call dependencies exercise acquisition failures without process-global hooks.
type foregroundDependencies struct {
	admission ForegroundCreateAdmission
	newJob    func(JobOptions) (*Job, error)
	start     func(*Job, *exec.Cmd, *os.File, *os.File, *os.File) (*AtCreateJobProcess, error)
}

func runForeground(ctx context.Context, cmd *exec.Cmd, stdin, stdout, stderr *os.File, deps foregroundDependencies) (result ForegroundResult, resultErr error) {
	defer func() { result.Outcome = projectForegroundOutcome(result) }()
	result.Admission = deps.admission
	if deps.admission != nil {
		defer func() {
			result.AdmissionCloseAttempted = true
			result.AdmissionCloseError = deps.admission.Close()
			resultErr = errors.Join(resultErr, result.AdmissionCloseError)
		}()
	}
	if ctx == nil || cmd == nil || cmd.Process != nil || !filepath.IsAbs(cmd.Path) || stdin == nil || stdout == nil || stderr == nil {
		return result, errors.New("foreground execution requires a context, fresh absolute command and three standard files")
	}
	if err := ctx.Err(); err != nil {
		result.Cancelled = true
		return result, err
	}
	result.Outcome.AcquisitionEntered = true
	job, err := deps.newJob(JobOptions{KillOnClose: true, BreakawayOK: true})
	result.Job = job
	if err != nil {
		return result, fmt.Errorf("foreground Job acquisition: %w", err)
	}
	original, startErr := deps.start(job, cmd, stdin, stdout, stderr)
	result.Original = original
	if original == nil {
		return result, errors.Join(startErr, job.Close())
	}
	if startErr == nil {
		result.Root = original.WaitOriginalContext(ctx)
		resultErr = result.Root.Error
		if !result.Root.Known && ctx.Err() != nil {
			result.Cancelled = true
			resultErr = errors.Join(resultErr, ctx.Err())
		}
	} else {
		resultErr = startErr
		if cancellationErr := ctx.Err(); cancellationErr != nil {
			result.Cancelled = true
			if !errors.Is(resultErr, cancellationErr) {
				resultErr = errors.Join(resultErr, cancellationErr)
			}
		}
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), RetainedTerminationAllowance())
	defer cancel()
	natural := startErr == nil && result.Root.Known && !result.Cancelled
	var settleErr, releaseErr error
	result.Native, settleErr = original.ObserveOriginal(cleanupCtx, natural)
	if original.Created() && !result.Root.Known {
		result.Root = original.WaitOriginalContext(cleanupCtx)
		resultErr = errors.Join(resultErr, result.Root.Error)
	}
	if result.Native.NoChildKnown || (result.Root.Known && result.Native.MembersExited) {
		result.Settlement, releaseErr = original.ReleaseOriginal()
		if result.Settlement != nil {
			result.Native = result.Settlement.Snapshot()
		}
	}
	resultErr = errors.Join(resultErr, settleErr, releaseErr)
	if !result.Settlement.Complete() {
		resultErr = errors.Join(resultErr, errors.New("foreground original process settlement incomplete; original capabilities retained"))
	}
	return result, resultErr
}
