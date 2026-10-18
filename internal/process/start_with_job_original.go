package process

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// AtCreateJobProcess is the capability transferred by the existing at-create
// builder to its caller. It starts no watcher, timer, dispatcher or process.
// The host owns the watcher and its supplied standard files.
type AtCreateJobProcess struct {
	child        containedChild
	observer     containedObservedChild
	waitOnce     sync.Once
	waitDone     chan struct{}
	waitResult   containedWaitResult
	settleOnce   sync.Once
	settleReturned atomic.Bool
	settleResult ContainedNativeSnapshot
	settleError  error
	closeOnce    sync.Once
	closeError   error
}
type OriginalRootWait struct {
	Known    bool
	ExitCode int
	Error    error
}

func (p *AtCreateJobProcess) Identity() (ContainedRootIdentity, error) {
	if p == nil || p.observer == nil {
		return ContainedRootIdentity{}, ErrContainmentUnavailable
	}
	return p.observer.containedRootIdentity()
}
func (p *AtCreateJobProcess) Created() bool {
	if p == nil || p.observer == nil {
		return false
	}
	return p.observer.containedCreated()
}
func (p *AtCreateJobProcess) OriginalLive(ctx context.Context) (bool, error) {
	if p == nil || p.observer == nil {
		return false, ErrContainmentUnavailable
	}
	return p.observer.containedStartLive(ctx)
}
func (p *AtCreateJobProcess) WaitOriginal() OriginalRootWait {
	if p == nil || p.child == nil {
		return OriginalRootWait{Error: ErrContainmentUnavailable}
	}
	p.waitOnce.Do(func() { p.waitResult = p.child.wait(); close(p.waitDone) })
	<-p.waitDone
	return OriginalRootWait{Known: p.waitResult.exited, ExitCode: p.waitResult.exitCode, Error: p.waitResult.err}
}

// ObserveOriginal receives the same Job before handle release. Every call
// receives the cached action; it never renews the supplied absolute deadline.
func (p *AtCreateJobProcess) ObserveOriginal(ctx context.Context, natural bool) (ContainedNativeSnapshot, error) {
	if p == nil || p.observer == nil {
		return ContainedNativeSnapshot{}, ErrContainmentUnavailable
	}
	if !p.settleReturned.Load() {
		permit, err := beginOriginalOwnerTerminal(ctx, p.child)
		if err != nil {
			return ContainedNativeSnapshot{}, err
		}
		defer permit.finish()
		ctx = withOriginalMemberTerminalPermit(ctx, permit)
		result, settleErr := p.observer.settleContained(ctx, natural)
		if !permit.nativeWasEntered() {
			return result, settleErr
		}
		p.settleOnce.Do(func() { p.settleResult, p.settleError = result, settleErr; p.settleReturned.Store(true) })
	}
	v := p.settleResult
	v.CapturedMemberPIDs = append([]uint32(nil), v.CapturedMemberPIDs...)
	return v, p.settleError
}

// ReleaseOriginal is called only after the host receives its one waiter (or
// positive no-child release). Unreceived wait refuses release of live handles.
func (p *AtCreateJobProcess) ReleaseOriginal() (*ContainedProcessSettlement, error) {
	if p == nil || p.child == nil {
		return nil, ErrContainmentUnavailable
	}
	if !p.settleResult.NoChildKnown {
		select {
		case <-p.waitDone:
		default:
			return nil, errors.New("original root waiter not received")
		}
	}
	permit, err := beginOriginalOwnerTerminal(nil, p.child)
	if err != nil {
		return nil, err
	}
	defer permit.finish()
	original, ok := p.child.(originalMemberTerminalOwner)
	if !ok {
		return nil, ErrContainmentUnavailable
	}
	if err := original.originalMemberCloseReady(permit); err != nil {
		return nil, err
	}
	p.closeOnce.Do(func() { p.closeError = original.closeOriginalWithPermit(permit) })
	v := p.settleResult
	v.RootWaitKnown = p.waitResult.exited
	v.RootExitCode = p.waitResult.exitCode
	v.HandlesClosed = p.closeError == nil
	v.Error = errors.Join(v.Error, p.settleError, p.closeError)
	return &ContainedProcessSettlement{owner: p.child, snapshot: v}, v.Error
}

func (p *AtCreateJobProcess) OriginalMemberSource(ctx context.Context) (OriginalJobMemberSource, error) {
	if p == nil || p.child == nil {
		return nil, ErrContainmentUnavailable
	}
	provider, ok := p.child.(originalMemberSourceProvider)
	if !ok {
		return nil, ErrContainmentUnavailable
	}
	return provider.originalMemberSourceCapability(ctx)
}
func (p *AtCreateJobProcess) ObserveOriginalMembers(ctx context.Context) (*OriginalJobMemberObservation, error) {
	source, err := p.OriginalMemberSource(ctx)
	if err != nil {
		return nil, err
	}
	return observeOriginalJobMembers(ctx, source.originalMemberSourceBinding())
}
