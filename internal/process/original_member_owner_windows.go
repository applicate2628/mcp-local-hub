//go:build windows

package process

import (
	"context"
	"errors"
	"os/exec"
)

type originalMemberPolicyResult struct {
	terminate, afterExit bool
	observation          jobTerminationObservation
	err                  error
}

func (j *Job) enterMemberTerminal(ctx context.Context) (*originalMemberTerminalPermit, bool, error) {
	if j == nil || j.memberCustody == nil {
		return nil, false, nil
	}
	if ctx != nil {
		if p, _ := ctx.Value(originalMemberTerminalContextKey{}).(*originalMemberTerminalPermit); p != nil {
			return p, false, p.validate(j.memberCustody)
		}
	}
	p, err := j.memberCustody.beginTerminal(ctx)
	return p, true, err
}
func (j *Job) observeCurrentJobPolicyEntered(ms uint32, original containedJobOriginal, ctx context.Context, terminate, afterExit bool, permit *originalMemberTerminalPermit) (jobTerminationObservation, error) {
	if err := permit.validate(j.memberCustody); err != nil {
		return jobTerminationObservation{}, err
	}
	g := j.memberCustody
	g.mu.Lock()
	prior := j.memberPolicy
	g.mu.Unlock()
	if prior != nil {
		if prior.terminate != terminate || prior.afterExit != afterExit {
			return jobTerminationObservation{}, errors.New("original Job policy already entered with a different projection")
		}
		g.mu.Lock()
		permit.nativeEntered = true
		g.mu.Unlock()
		result := prior.observation
		result.native = copyCreatorJobObservation(result.native)
		return result, prior.err
	}
	result, err := j.observeCurrentJobPolicyCore(ms, original, ctx, terminate, afterExit, permit)
	if !permit.nativeWasEntered() {
		return result, err
	}
	retained := result
	retained.native = copyCreatorJobObservation(result.native)
	g.mu.Lock()
	j.memberPolicy = &originalMemberPolicyResult{terminate: terminate, afterExit: afterExit, observation: retained, err: err}
	g.mu.Unlock()
	return result, err
}
func newProvisionalWindowsChild(job *Job, cmd *exec.Cmd) (*provisionalWindowsChild, error) {
	child := &provisionalWindowsChild{windowsContainedChild: &windowsContainedChild{cmd: cmd, job: job}}
	source, err := installOriginalMemberSource(job, func() (originalMemberRootBinding, error) {
		if child.creationErr != nil {
			return originalMemberRootBinding{}, child.creationErr
		}
		return originalMemberRootBinding{job: job, original: child.original, startedAt: child.creationTime}, nil
	})
	child.memberSource = source
	return child, err
}
func (c *provisionalWindowsChild) originalMemberSourceCapability(ctx context.Context) (OriginalJobMemberSource, error) {
	if c == nil || c.windowsContainedChild == nil {
		return nil, errors.New("original child missing")
	}
	return originalSourceAvailable(ctx, c.memberSource)
}
func (c *provisionalWindowsChild) originalMemberTerminal(ctx context.Context) (*originalMemberTerminalPermit, error) {
	if c == nil || c.memberSource == nil {
		return nil, nil
	}
	return c.memberSource.gate.beginTerminal(ctx)
}
func (c *provisionalWindowsChild) closeOriginalWithPermit(permit *originalMemberTerminalPermit) error {
	err := c.windowsContainedChild.closeOriginalWithPermit(permit)
	if errors.Is(err, ErrOriginalMemberBorrowPending) {
		return err
	}
	if c.cmd.Process != nil && c.cmd.ProcessState == nil {
		err = errors.Join(err, c.cmd.Process.Release())
	}
	return err
}
func (j *Job) closeOriginalWithPermit(permit *originalMemberTerminalPermit) error {
	return j.closeWithMemberPermit(permit)
}

func (c *provisionalWindowsChild) terminateOriginalWithPermit(ctx context.Context) error {
	_, err := c.terminateCreatorMembers(ctx)
	return err
}

func (c *provisionalWindowsChild) originalMemberCloseReady(permit *originalMemberTerminalPermit) error {
	if c == nil || c.memberSource == nil {
		return nil
	}
	g := c.memberSource.gate
	if err := permit.validate(g); err != nil {
		return err
	}
	g.mu.Lock()
	waiting := c.process != 0 && !c.rootWaitReturned
	g.mu.Unlock()
	if waiting {
		return &OriginalMemberBorrowPendingError{Stage: "original-root-wait"}
	}
	return nil
}

func (c *windowsContainedChild) originalMemberSourceCapability(ctx context.Context) (OriginalJobMemberSource, error) {
	if c == nil {
		return nil, errors.New("original child missing")
	}
	return originalSourceAvailable(ctx, c.memberSource)
}
func (c *windowsContainedChild) originalMemberTerminal(ctx context.Context) (*originalMemberTerminalPermit, error) {
	if c == nil || c.memberSource == nil {
		return nil, nil
	}
	return c.memberSource.gate.beginTerminal(ctx)
}
func (c *windowsContainedChild) originalMemberCloseReady(permit *originalMemberTerminalPermit) error {
	if c == nil || c.memberSource == nil {
		return nil
	}
	g := c.memberSource.gate
	if err := permit.validate(g); err != nil {
		return err
	}
	g.mu.Lock()
	waiting := c.process != 0 && !c.rootWaitReturned
	g.mu.Unlock()
	if waiting {
		return &OriginalMemberBorrowPendingError{Stage: "original-root-wait"}
	}
	return nil
}
