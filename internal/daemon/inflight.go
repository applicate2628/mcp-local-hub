package daemon

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// InflightGate is the lazy-proxy's per-(workspace,language) concurrency
// control. It has two responsibilities:
//
//  1. Singleflight: concurrent first-callers for the same key collapse
//     into one invocation of fn. All callers observe the same result.
//  2. Retry throttle: after a failed invocation for key K, further Do(K, _)
//     calls within minRetryGap of the last failure return the cached error
//     immediately — fn is NOT invoked. This prevents a pathological client
//     loop from re-spawning a wedged backend every millisecond.
//
// A successful invocation clears the throttle state for that key so the
// next Do runs normally. Forget drops both inflight and throttle state
// explicitly (used when the caller knows the backend was deliberately
// shut down and any cached error is stale).
type InflightGate struct {
	sf          singleflight.Group
	minRetryGap time.Duration

	mu          sync.Mutex
	lastFailure map[string]failureEntry
}

type failureEntry struct {
	at  time.Time
	err error
}

// NewInflightGate returns a gate with minRetryGap as the minimum gap
// between failed attempts per key. Must be >= 0; negative values clamp
// to 0 (no throttling).
func NewInflightGate(minRetryGap time.Duration) *InflightGate {
	if minRetryGap < 0 {
		minRetryGap = 0
	}
	return &InflightGate{
		minRetryGap: minRetryGap,
		lastFailure: map[string]failureEntry{},
	}
}

// Do runs fn exactly once per in-flight key and returns its result to all
// concurrent callers. After a failure, further Do calls within minRetryGap
// return the cached error without invoking fn. A successful Do clears the
// failure state for key.
//
// fn receives a DETACHED context that preserves the winner's values
// (deadline, tracing, etc. via context.WithoutCancel-equivalent) but
// whose cancellation is independent of the caller's request context.
// Without this, a short-lived or disconnected winner request (e.g. a
// canceled HTTP request from one client) would abort the shared
// materialization AND cache the canceled-error for the retry-gap
// window — making healthy concurrent callers fail for no reason.
func (g *InflightGate) Do(ctx context.Context, key string, fn func(context.Context) (any, error)) (any, error) {
	// Fast-path throttle check.
	g.mu.Lock()
	if fe, ok := g.lastFailure[key]; ok {
		if time.Since(fe.at) < g.minRetryGap {
			g.mu.Unlock()
			return nil, fe.err
		}
	}
	g.mu.Unlock()

	// Detach the materialization context from the winner's request
	// cancellation so a hung client cannot abort a materialization
	// that other concurrent callers still need. BUT preserve the
	// winner's deadline so materialization still honors the per-
	// request timeout — WithoutCancel drops both cancellation AND
	// deadline, which would let backend startup drift up to the
	// backend's default handshake timeout (≥10s) even when the
	// caller asked for a much shorter bound.
	materializeCtx := context.WithoutCancel(ctx)
	if dl, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		materializeCtx, cancel = context.WithDeadline(materializeCtx, dl)
		defer cancel()
	}

	v, err, _ := g.sf.Do(key, func() (any, error) {
		res, err := fn(materializeCtx)
		g.mu.Lock()
		defer g.mu.Unlock()
		if err != nil {
			g.lastFailure[key] = failureEntry{at: time.Now(), err: err}
		} else {
			delete(g.lastFailure, key)
		}
		return res, err
	})
	return v, err
}

// Forget drops all inflight + throttle state for key. The lazy proxy calls
// this when the materialized endpoint is explicitly closed (e.g. shutdown
// or backend swap) so a subsequent restart is not accidentally throttled
// by a stale failure record.
func (g *InflightGate) Forget(key string) {
	g.sf.Forget(key)
	g.mu.Lock()
	delete(g.lastFailure, key)
	g.mu.Unlock()
}
