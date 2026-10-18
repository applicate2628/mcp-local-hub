package process

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrOriginalMemberBorrowPending = errors.New("original member query or root waiter is pending before native entry")
var ErrOriginalMemberObservationBusy = errors.New("original creator already has a live member observation")

type OriginalMemberBorrowPendingError struct{ Stage string }

func (e *OriginalMemberBorrowPendingError) Error() string {
	return "original member custody pending: " + e.Stage
}
func (e *OriginalMemberBorrowPendingError) Unwrap() error { return ErrOriginalMemberBorrowPending }

// One per original creator/Job. Metadata is never locked across native calls,
// waits, finalizers or caller IO. The actual original waiter is separate.
type originalMemberCustody struct {
	mu               sync.Mutex
	sealed           bool
	entered          int
	drained          chan struct{}
	drainClosed      bool
	source           *originalMemberSource
	active           *OriginalJobMemberObservation
	terminalActive   bool
	terminalDeadline time.Time
	terminalBound    bool
}

type originalMemberTerminalPermit struct {
	gate          *originalMemberCustody
	ended         bool
	nativeEntered bool
}

func (p *originalMemberTerminalPermit) markNativeEntry(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx != nil {
		if err := creatorClosureContext(ctx); err != nil {
			return err
		}
	}
	if err := p.validate(p.gate); err != nil {
		return err
	}
	p.gate.mu.Lock()
	p.nativeEntered = true
	p.gate.mu.Unlock()
	return nil
}
func (p *originalMemberTerminalPermit) nativeWasEntered() bool {
	if p == nil {
		return true
	}
	p.gate.mu.Lock()
	defer p.gate.mu.Unlock()
	return p.nativeEntered
}

type originalMemberTerminalContextKey struct{}

func newOriginalMemberCustody() *originalMemberCustody {
	g := &originalMemberCustody{drained: make(chan struct{}), drainClosed: true}
	close(g.drained)
	return g
}

func (g *originalMemberCustody) updateDrainLocked() {
	ready := g.entered == 0 && g.active == nil
	if ready && !g.drainClosed {
		close(g.drained)
		g.drainClosed = true
	} else if !ready && g.drainClosed {
		g.drained = make(chan struct{})
		g.drainClosed = false
	}
}

func (g *originalMemberCustody) enterQuery(observation *OriginalJobMemberObservation) (func(), error) {
	if g == nil {
		return nil, errors.New("original member custody unavailable")
	}
	g.mu.Lock()
	if g.sealed || (observation != nil && (g.active != observation || observation.closing)) {
		g.mu.Unlock()
		return nil, errors.New("original member source is sealed or foreign")
	}
	g.entered++
	if observation != nil {
		observation.calls++
	}
	g.updateDrainLocked()
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.entered--
			run := false
			if observation != nil {
				observation.calls--
				if observation.closing && observation.calls == 0 && !observation.finalizerStarted {
					observation.finalizerStarted = true
					run = true
				}
			}
			g.updateDrainLocked()
			g.mu.Unlock()
			if run {
				observation.finalizeClose()
			}
		})
	}, nil
}

func (g *originalMemberCustody) reserve(observation *OriginalJobMemberObservation) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed || g.source == nil {
		return errors.New("original source cannot admit members")
	}
	if g.active != nil {
		return ErrOriginalMemberObservationBusy
	}
	g.active = observation
	g.updateDrainLocked()
	return nil
}

func (g *originalMemberCustody) release(observation *OriginalJobMemberObservation) {
	g.mu.Lock()
	if g.active == observation {
		g.active = nil
	}
	g.updateDrainLocked()
	g.mu.Unlock()
}

func (g *originalMemberCustody) beginTerminal(ctx context.Context) (*originalMemberTerminalPermit, error) {
	if g == nil {
		return nil, nil
	}
	if ctx != nil {
		if err := creatorClosureContext(ctx); err != nil {
			return nil, err
		}
	}
	g.mu.Lock()
	g.sealed = true
	if ctx != nil {
		deadline, _ := ctx.Deadline()
		if !g.terminalBound {
			g.terminalDeadline = deadline
			g.terminalBound = true
		} else if deadline.After(g.terminalDeadline) {
			g.mu.Unlock()
			return nil, errors.New("original member terminal deadline renewed")
		}
	}
	active := g.active
	g.mu.Unlock()
	// SAME observation owns its deferred close. An entered call exits and runs
	// that finalizer; no terminal caller becomes a second closer or worker.
	if active != nil {
		_, _ = active.CloseObserved()
	}
	for {
		if ctx != nil {
			if err := creatorClosureContext(ctx); err != nil {
				return nil, errors.Join(&OriginalMemberBorrowPendingError{Stage: "query-drain"}, err)
			}
		}
		g.mu.Lock()
		drained := g.drained
		ready := g.entered == 0 && g.active == nil && !g.terminalActive
		if ready {
			g.terminalActive = true
			permit := &originalMemberTerminalPermit{gate: g}
			g.mu.Unlock()
			return permit, nil
		}
		g.mu.Unlock()
		if ctx == nil {
			return nil, &OriginalMemberBorrowPendingError{Stage: "query-drain"}
		}
		select {
		case <-drained:
		case <-ctx.Done():
			return nil, errors.Join(&OriginalMemberBorrowPendingError{Stage: "query-drain"}, ctx.Err())
		}
		// A different entered terminal operation has no query notification. It
		// is a scoped pending refusal, never a spin or renewed waiting budget.
		g.mu.Lock()
		busy := g.terminalActive
		g.mu.Unlock()
		if busy {
			return nil, &OriginalMemberBorrowPendingError{Stage: "terminal-entry"}
		}
	}
}

func (p *originalMemberTerminalPermit) finish() {
	if p == nil || p.gate == nil {
		return
	}
	p.gate.mu.Lock()
	if !p.ended {
		p.ended = true
		p.gate.terminalActive = false
	}
	p.gate.mu.Unlock()
}
func (p *originalMemberTerminalPermit) validate(g *originalMemberCustody) error {
	if p == nil || p.gate != g || g == nil {
		return errors.New("same original terminal permit required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if p.ended || !g.terminalActive || !g.sealed || g.entered != 0 || g.active != nil {
		return &OriginalMemberBorrowPendingError{Stage: "terminal-permit"}
	}
	return nil
}
func withOriginalMemberTerminalPermit(ctx context.Context, p *originalMemberTerminalPermit) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, originalMemberTerminalContextKey{}, p)
}
func originalMemberPermitFromContext(ctx context.Context, g *originalMemberCustody) (*originalMemberTerminalPermit, error) {
	if g == nil {
		return nil, nil
	}
	if ctx == nil {
		return nil, errors.New("original terminal context required")
	}
	p, _ := ctx.Value(originalMemberTerminalContextKey{}).(*originalMemberTerminalPermit)
	if p == nil {
		return nil, errors.New("original terminal context has no same-owner permit")
	}
	return p, p.validate(g)
}
