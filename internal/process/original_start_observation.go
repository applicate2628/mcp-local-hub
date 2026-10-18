package process

import (
	"context"
	"errors"
)

// This read-only capability comes from the SAME retained at-create builder.
// It contains no public constructor or wait/signal/close operation.
type OriginalStartObservation struct {
	owner         *AtCreateJobProcess
	identity      ContainedRootIdentity
	created, live bool
	err           error
}

func (p *AtCreateJobProcess) ObserveOriginalStart(ctx context.Context) (result *OriginalStartObservation, err error) {
	if p == nil || p.observer == nil || ctx == nil {
		return nil, ErrContainmentUnavailable
	}
	result = &OriginalStartObservation{owner: p, created: p.Created()}
	result.identity, err = p.Identity()
	if err == nil && result.created {
		result.live, err = p.OriginalLive(ctx)
	}
	result.err = err
	return result, err
}
func (o *OriginalStartObservation) Complete() bool {
	return o != nil && o.owner != nil && o.err == nil && o.created && o.live && o.identity.Known && o.identity.PID > 0 && o.identity.NativeBirthFILETIME != 0
}
func (o *OriginalStartObservation) Identity() ContainedRootIdentity {
	if o == nil {
		return ContainedRootIdentity{}
	}
	return o.identity
}
func (o *OriginalStartObservation) Validate(ctx context.Context, pid uint32, birth uint64) error {
	if ctx == nil || !o.Complete() {
		return ErrContainmentUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if pid != uint32(o.identity.PID) || birth != o.identity.NativeBirthFILETIME {
		return errors.New("original start binding differs")
	}
	return nil
}
