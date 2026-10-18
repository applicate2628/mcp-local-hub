package process

import (
	"context"
	"errors"
)

type originalMemberSourceProvider interface {
	originalMemberSourceCapability(context.Context) (OriginalJobMemberSource, error)
}

type originalMemberTerminalOwner interface {
	originalMemberTerminal(context.Context) (*originalMemberTerminalPermit, error)
	closeOriginalWithPermit(*originalMemberTerminalPermit) error
	originalMemberCloseReady(*originalMemberTerminalPermit) error
}

func beginOriginalOwnerTerminal(ctx context.Context, owner containedChild) (*originalMemberTerminalPermit, error) {
	if original, ok := owner.(originalMemberTerminalOwner); ok {
		return original.originalMemberTerminal(ctx)
	}
	return nil, nil
}

func (p *ProvisionalProcess) closeWithMemberPermit(permit *originalMemberTerminalPermit) error {
	return p.closeWithMemberPermitMode(permit, false)
}
func (p *ProvisionalProcess) closeWithMemberPermitMode(permit *originalMemberTerminalPermit, legacyMetadataHeld bool) error {
	if p == nil {
		return nil
	}
	original, participating := p.owner.(originalMemberTerminalOwner)
	if participating {
		if err := original.originalMemberCloseReady(permit); err != nil {
			return err
		}
		if p.cmd != nil && p.cmd.Process != nil && p.waitDone != nil {
			select {
			case <-p.waitDone:
			default:
				return &OriginalMemberBorrowPendingError{Stage: "original-published-wait"}
			}
		}
	}
	if !legacyMetadataHeld {
		p.mu.Lock()
	}
	if p.closeAttempted {
		err := p.closeErr
		if !legacyMetadataHeld && !p.closeReturned {
			err = errors.Join(err, &OriginalMemberBorrowPendingError{Stage: "original-close-return"})
		}
		if !legacyMetadataHeld {
			p.mu.Unlock()
		}
		return err
	}
	p.closeAttempted = true
	if !legacyMetadataHeld {
		p.mu.Unlock()
	}
	var err error
	if participating {
		err = original.closeOriginalWithPermit(permit)
	} else if p.owner != nil {
		err = p.owner.close()
	}
	if job, ok := any(p.job).(interface {
		closeOriginalWithPermit(*originalMemberTerminalPermit) error
	}); ok {
		err = errors.Join(err, job.closeOriginalWithPermit(permit))
	} else {
		err = errors.Join(err, p.job.Close())
	}
	if !legacyMetadataHeld {
		p.mu.Lock()
	}
	p.closeErr = err
	p.closeReturned = true
	p.settled = err == nil
	if !legacyMetadataHeld {
		p.mu.Unlock()
	}
	return err
}

func (p *ProvisionalProcess) OriginalMemberSource(ctx context.Context) (OriginalJobMemberSource, error) {
	if err := creatorClosureContext(ctx); err != nil {
		return nil, err
	}
	if p == nil || p.owner == nil {
		return nil, errors.New("original provisional owner missing")
	}
	provider, ok := p.owner.(originalMemberSourceProvider)
	if !ok {
		return nil, errors.New("original provisional member source unsupported")
	}
	return provider.originalMemberSourceCapability(ctx)
}

func (p *ProvisionalProcess) ObserveOriginalMembers(ctx context.Context) (*OriginalJobMemberObservation, error) {
	source, err := p.OriginalMemberSource(ctx)
	if err != nil {
		return nil, err
	}
	return observeOriginalJobMembers(ctx, source.originalMemberSourceBinding())
}
