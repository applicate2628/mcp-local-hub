package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

const lazyProxyReadinessAttemptTimeout = 5 * time.Second

type lazyProxyReadinessKey struct {
	epoch uint64
	rank  uint8
}

type lazyProxyReadinessRequest struct {
	event       api.LazyProxyReadinessEventV1
	backendExit <-chan struct{}
	done        chan struct{}
	once        sync.Once
	err         error
}

func (r *lazyProxyReadinessRequest) complete(err error) {
	r.once.Do(func() {
		r.err = err
		close(r.done)
	})
}

// lazyProxyReadinessPublisher is one serialized, bounded worker per proxy.
// Producers replace the single pending slot only with a greater state; IPC is
// therefore never performed under LazyProxy.mu and no transition creates a
// goroutine. A failed state can be submitted again by a later organic success.
type lazyProxyReadinessPublisher struct {
	cfg           LazyProxyReadinessConfig
	ctx           context.Context
	cancel        context.CancelFunc
	notify        chan struct{}
	done          chan struct{}
	onBackendExit func(uint64)

	mu           sync.Mutex
	closed       bool
	pending      *lazyProxyReadinessRequest
	inFlight     *lazyProxyReadinessRequest
	latest       lazyProxyReadinessKey
	hasLatest    bool
	delivered    lazyProxyReadinessKey
	hasDelivered bool
}

func newLazyProxyReadinessPublisher(cfg LazyProxyReadinessConfig, onBackendExit func(uint64)) *lazyProxyReadinessPublisher {
	if cfg.Commit == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &lazyProxyReadinessPublisher{
		cfg: cfg, ctx: ctx, cancel: cancel, notify: make(chan struct{}, 1),
		done: make(chan struct{}), onBackendExit: onBackendExit,
	}
	go p.run()
	return p
}

func lazyProxyReadinessTransitionRank(transition string) uint8 {
	switch transition {
	case "starting":
		return 1
	case "ready":
		return 2
	case "idle", "failed":
		return 3
	default:
		return 0
	}
}

func compareLazyProxyReadinessKey(a, b lazyProxyReadinessKey) int {
	if a.epoch < b.epoch {
		return -1
	}
	if a.epoch > b.epoch {
		return 1
	}
	if a.rank < b.rank {
		return -1
	}
	if a.rank > b.rank {
		return 1
	}
	return 0
}

// publish submits one state. waitLimit==0 is asynchronous, waitLimit<0 waits
// for its complete bounded retry budget, and waitLimit>0 bounds only the
// caller's wait while the worker continues settlement independently.
func (p *lazyProxyReadinessPublisher) publish(epoch uint64, transition string, backendExit <-chan struct{}, waitLimit time.Duration) error {
	if p == nil {
		return nil
	}
	rank := lazyProxyReadinessTransitionRank(transition)
	if epoch == 0 || rank == 0 {
		return fmt.Errorf("invalid lazy readiness state epoch=%d transition=%q", epoch, transition)
	}
	key := lazyProxyReadinessKey{epoch: epoch, rank: rank}
	req := &lazyProxyReadinessRequest{
		event: api.LazyProxyReadinessEventV1{
			ProtocolVersion: 1,
			TaskName:        p.cfg.TaskName, WrapperPID: p.cfg.WrapperPID,
			WrapperStartedAt: p.cfg.WrapperStartedAt.UTC().Format(time.RFC3339Nano),
			BackendEpoch:     epoch, Transition: transition,
			ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
		},
		backendExit: backendExit,
		done:        make(chan struct{}),
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return context.Canceled
	}
	if p.hasLatest {
		cmp := compareLazyProxyReadinessKey(key, p.latest)
		if cmp < 0 || (cmp == 0 && p.hasDelivered && p.delivered == key) {
			p.mu.Unlock()
			return nil
		}
		if cmp == 0 {
			if p.pending != nil && lazyProxyReadinessKeyFor(p.pending.event) == key {
				req = p.pending
				p.mu.Unlock()
				return p.wait(req, waitLimit)
			}
			if p.inFlight != nil && lazyProxyReadinessKeyFor(p.inFlight.event) == key {
				req = p.inFlight
				p.mu.Unlock()
				return p.wait(req, waitLimit)
			}
		}
	}
	if !p.hasLatest || compareLazyProxyReadinessKey(key, p.latest) > 0 {
		p.latest = key
		p.hasLatest = true
	}
	replaced := p.pending
	p.pending = req
	p.mu.Unlock()
	if replaced != nil {
		replaced.complete(nil)
	}
	select {
	case p.notify <- struct{}{}:
	default:
	}
	return p.wait(req, waitLimit)
}

func (p *lazyProxyReadinessPublisher) wait(req *lazyProxyReadinessRequest, waitLimit time.Duration) error {
	if waitLimit == 0 {
		return nil
	}
	if waitLimit < 0 {
		select {
		case <-req.done:
			return req.err
		case <-p.ctx.Done():
			return p.ctx.Err()
		}
	}
	timer := time.NewTimer(waitLimit)
	defer timer.Stop()
	select {
	case <-req.done:
		return req.err
	case <-p.ctx.Done():
		return p.ctx.Err()
	case <-timer.C:
		return context.DeadlineExceeded
	}
}

func lazyProxyReadinessKeyFor(event api.LazyProxyReadinessEventV1) lazyProxyReadinessKey {
	return lazyProxyReadinessKey{epoch: event.BackendEpoch, rank: lazyProxyReadinessTransitionRank(event.Transition)}
}

func (p *lazyProxyReadinessPublisher) takePending() *lazyProxyReadinessRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	req := p.pending
	p.pending = nil
	if req != nil {
		p.inFlight = req
	}
	return req
}

func (p *lazyProxyReadinessPublisher) isCurrent(key lazyProxyReadinessKey) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed && p.hasLatest && p.latest == key
}

func (p *lazyProxyReadinessPublisher) run() {
	defer close(p.done)
	var backendExit <-chan struct{}
	var backendEpoch uint64
	for {
		if req := p.takePending(); req != nil {
			key := lazyProxyReadinessKeyFor(req.event)
			receipt, err := p.send(req, key)
			p.mu.Lock()
			current := !p.closed && p.hasLatest && p.latest == key
			p.inFlight = nil
			if err == nil && current {
				p.delivered = key
				p.hasDelivered = true
				switch req.event.Transition {
				case "starting":
					if receipt.Disposition != "superseded" {
						backendExit = req.backendExit
						backendEpoch = req.event.BackendEpoch
					}
				case "idle", "failed":
					backendExit = nil
					backendEpoch = 0
				}
			}
			p.mu.Unlock()
			if err != nil && current && p.ctx.Err() == nil && req.event.Transition != "ready" {
				p.signalFatalIfCurrent(key, fmt.Errorf("transition %s epoch %d: %w", req.event.Transition, req.event.BackendEpoch, err))
			}
			req.complete(err)
			continue
		}

		select {
		case <-p.ctx.Done():
			return
		case <-p.notify:
		case <-backendExit:
			exitedEpoch := backendEpoch
			backendExit = nil
			backendEpoch = 0
			if p.onBackendExit != nil {
				p.onBackendExit(exitedEpoch)
			}
		}
	}
}

func (p *lazyProxyReadinessPublisher) send(req *lazyProxyReadinessRequest, key lazyProxyReadinessKey) (api.LazyProxyReadinessReceiptV1, error) {
	var (
		receipt api.LazyProxyReadinessReceiptV1
		err     error
	)
	for attempt := 0; attempt < 2; attempt++ {
		attemptCtx, cancel := context.WithTimeout(p.ctx, lazyProxyReadinessAttemptTimeout)
		receipt, err = p.cfg.Commit(attemptCtx, req.event)
		cancel()
		if err == nil {
			return receipt, nil
		}
		if p.ctx.Err() != nil || !p.isCurrent(key) {
			return receipt, err
		}
	}
	return receipt, err
}

func (p *lazyProxyReadinessPublisher) signalFatalIfCurrent(key lazyProxyReadinessKey, err error) {
	if p.cfg.Fatal == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !p.hasLatest || p.latest != key || p.ctx.Err() != nil {
		return
	}
	// Hold the mailbox lock through the send so the current-state decision and
	// fatal handoff are one linearized action. Production injects a buffered
	// one-element channel consumed by the composition select.
	select {
	case p.cfg.Fatal <- fmt.Errorf("lazy-readiness-publish-failed: %w", err):
	case <-p.ctx.Done():
	}
}

func (p *lazyProxyReadinessPublisher) stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.done
		return
	}
	p.closed = true
	pending := p.pending
	p.pending = nil
	p.mu.Unlock()
	if pending != nil {
		pending.complete(context.Canceled)
	}
	p.cancel()
	<-p.done
}

func (p *LazyProxy) publishReadinessStarting(epoch uint64, backendExit <-chan struct{}) error {
	if p.readiness == nil {
		return nil
	}
	return p.readiness.publish(epoch, "starting", backendExit, -1)
}

func (p *LazyProxy) publishReadinessReady(epoch uint64) {
	if p.readiness == nil {
		return
	}
	p.mu.Lock()
	if p.closed.Load() || p.endpoint == nil || p.endpointGeneration != epoch {
		p.mu.Unlock()
		return
	}
	first := p.readinessReadyEpoch != epoch
	if first {
		p.readinessReadyEpoch = epoch
	}
	p.mu.Unlock()
	waitLimit := time.Duration(0)
	if first {
		waitLimit = lazyProxyReadinessAttemptTimeout
	}
	// A publication failure never rewrites the already-received backend result.
	_ = p.readiness.publish(epoch, "ready", nil, waitLimit)
}

func (p *LazyProxy) publishReadinessInvalidation(epoch uint64, transition string) {
	if p.readiness != nil {
		_ = p.readiness.publish(epoch, transition, nil, 0)
	}
}

type backendExitNotifier interface {
	BackendExited() <-chan struct{}
}

func endpointBackendExited(ep MCPEndpoint) <-chan struct{} {
	if notifier, ok := ep.(backendExitNotifier); ok {
		return notifier.BackendExited()
	}
	return nil
}
