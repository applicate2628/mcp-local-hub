package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ErrMaterializeInFlight is returned by DoBounded to a caller whose bounded
// wait budget elapsed while the singleflight materialization is still running
// in the background. It is NOT a failure: the shared materialize continues
// under the detached hard ceiling and its result lands in the singleflight
// cache-line for the next caller. Handlers map this sentinel to a
// bounded-wait "cold start in progress; retry" response instead of a generic
// error, and must NOT stamp the backend as failed on it.
var ErrMaterializeInFlight = errors.New("materialization in flight")

// materializeHardCeiling is the fixed, detached upper bound on how long a
// shared background materialization may run once started. It intentionally
// REPLACES the earlier winner-deadline propagation: with a bounded join
// (DoBounded), propagating the winning caller's per-request deadline into the
// shared materialize would abort the background work at that caller's (short)
// budget, defeating the whole design — every subsequent joiner would then
// re-trigger a fresh materialize. A generous fixed ceiling lets a slow LSP
// cold start (spawn + MCP handshake + first-index) finish detached while
// still bounding a genuinely wedged materialize goroutine.
const materializeHardCeiling = 120 * time.Second

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
	// activeFlights counts, per key, how many singleflight fn executions are
	// currently in flight (0 or 1 under singleflight, but tracked as a count for
	// robustness against overlapping forget/re-enter). HasActiveFlight exposes
	// this so a caller that already has a materialize running for its key can
	// distinguish "join my own in-flight work (spawns nothing)" from "start a
	// fresh cold start" — used by the cold-start-slot gate to avoid refusing a
	// retry that merely joins the flight it started (F3).
	activeFlights map[string]int
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
		minRetryGap:   minRetryGap,
		lastFailure:   map[string]failureEntry{},
		activeFlights: map[string]int{},
	}
}

// Do runs fn exactly once per in-flight key and blocks the caller until fn
// returns (or the caller's own context is canceled). It is DoBounded with an
// unbounded wait budget: no ErrMaterializeInFlight can be returned. After a
// failure, further Do calls within minRetryGap return the cached error
// without invoking fn. A successful Do clears the failure state for key.
func (g *InflightGate) Do(ctx context.Context, key string, fn func(context.Context) (any, error)) (any, error) {
	return g.DoBounded(ctx, key, 0, fn)
}

// DoBounded runs fn exactly once per in-flight key (singleflight) and returns
// its result to all concurrent callers, but bounds how long THIS caller is
// willing to wait for the shared materialization. When budget > 0 and it
// elapses before fn returns (and before the caller's ctx is canceled),
// DoBounded returns ErrMaterializeInFlight to this caller while the
// materialization keeps running in the background; the result lands in the
// singleflight cache-line for the next caller. A budget <= 0 means "wait as
// long as fn runs" (the classic Do semantics).
//
// fn receives a DETACHED context: it preserves the winner's values (tracing,
// etc.) but drops the caller's cancellation and deadline, then applies the
// FIXED materializeHardCeiling. Detaching cancellation means a hung or
// short-budget caller cannot abort a materialization other callers still
// need; the fixed ceiling (NOT the winner's per-request deadline) bounds a
// wedged materialize without aborting a legitimately-slow cold start early.
//
// After a failure, further calls within minRetryGap return the cached error
// without invoking fn. A successful call clears the failure state for key.
//
// Panic semantics (accepted tradeoff): singleflight re-raises a panic from fn
// in the goroutine that reads the result channel. The lazy-proxy materialize fn
// does not recover panics, so a panic during a cold start crashes the daemon
// process. That is intentional — the supervisor's Job-Object reaper respawns the
// proxy from persisted intent, and a crash is a louder, more debuggable signal
// than a silently-poisoned singleflight key. The activeFlights bookkeeping below
// is unwound by a deferred decrement even on the panic path.
func (g *InflightGate) DoBounded(ctx context.Context, key string, budget time.Duration, fn func(context.Context) (any, error)) (any, error) {
	// Fast-path throttle check.
	g.mu.Lock()
	if fe, ok := g.lastFailure[key]; ok {
		if time.Since(fe.at) < g.minRetryGap {
			g.mu.Unlock()
			return nil, fe.err
		}
	}
	g.mu.Unlock()

	ch := g.sf.DoChan(key, func() (any, error) {
		// Mark this key as having an in-flight materialize for its whole
		// singleflight execution so HasActiveFlight can report it. The deferred
		// decrement runs even on the fn-panic unwind path.
		g.mu.Lock()
		g.activeFlights[key]++
		g.mu.Unlock()
		defer func() {
			g.mu.Lock()
			if g.activeFlights[key] <= 1 {
				delete(g.activeFlights, key)
			} else {
				g.activeFlights[key]--
			}
			g.mu.Unlock()
		}()

		// Re-check throttle inside the singleflight winner path so callers
		// that raced past the outer fast-path cannot bypass minRetryGap.
		g.mu.Lock()
		if fe, ok := g.lastFailure[key]; ok && time.Since(fe.at) < g.minRetryGap {
			g.mu.Unlock()
			return nil, fe.err
		}
		g.mu.Unlock()

		// Detach from the winner's request cancellation/deadline and apply a
		// FIXED ceiling. A bounded joiner's budget expiry must NOT abort this
		// shared background materialize.
		materializeCtx := context.WithoutCancel(ctx)
		materializeCtx, cancel := context.WithTimeout(materializeCtx, materializeHardCeiling)
		defer cancel()

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

	if budget <= 0 {
		select {
		case res := <-ch:
			return res.Val, res.Err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.Val, res.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, ErrMaterializeInFlight
	}
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

// HasActiveFlight reports whether a materialize fn for key is currently running
// under the gate. The cold-start-slot gate uses it to let a retry that would
// merely JOIN this proxy's own in-flight materialize through, instead of
// refusing it as if it were a fresh cold start that spawns another backend (F3).
func (g *InflightGate) HasActiveFlight(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.activeFlights[key] > 0
}
