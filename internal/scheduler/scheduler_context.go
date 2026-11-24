package scheduler

import (
	"context"
	"errors"
	"fmt"
)

// ContextBinder binds the legacy context-free Scheduler methods to one caller
// lifetime. A binding is a private view; it must not change a shared backend.
// Methods that explicitly accept a context continue to use that context.
type ContextBinder interface {
	BindContext(context.Context) (Scheduler, error)
}

// BindContext preserves the existing interface for older backends and test
// doubles. Only ContextBinder backends provide cancellation of in-flight work;
// the compatibility path checks admission but cannot preempt a legacy call.
func BindContext(ctx context.Context, backend Scheduler) (Scheduler, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil operation context", ErrUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, errors.Join(err, context.Cause(ctx)))
	}
	if backend == nil {
		return nil, fmt.Errorf("%w: nil scheduler", ErrUnavailable)
	}
	if binder, ok := backend.(ContextBinder); ok {
		return binder.BindContext(ctx)
	}
	return backend, nil
}

// NewContext constructs the usual backend and binds its context-free methods
// to the caller's operation. The Windows backend keeps its existing per-command
// ceiling within this shared lifetime, without renewing the caller's deadline.
func NewContext(ctx context.Context) (Scheduler, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil operation context", ErrUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, errors.Join(err, context.Cause(ctx)))
	}
	backend, err := New()
	if err != nil {
		return nil, err
	}
	return BindContext(ctx, backend)
}
