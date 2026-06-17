package api

import (
	"context"
	"sync/atomic"
)

// LoopEvent is the supervisor's FIFO event envelope. Per spec Q4 v13.
type LoopEvent struct {
	Kind     SMEvent
	TaskName string
	Body     map[string]any
}

// EventLoop is a single-threaded FIFO event consumer. Capacity >= 16
// per spec to absorb quiesce-complete posts from the side-goroutine
// drain handler without rendezvous deadlock.
//
// Two-channel design (closes bot finding on PR #236 1c0ea09 P2):
//
//   - ch is the main channel for EXTERNAL producers (reconcile loop,
//     IPC commands, crashCh translator, daemon-intent watcher, timer
//     goroutines). External producers call Post (blocking) or TryPost
//     (best-effort).
//
//   - selfCh is a PRIORITY channel for HANDLER-self-posts (e.g. the
//     supervisor controller posting EvHealthOK or synthetic EvChildExit
//     from inside executeSideEffect). Handler callers use PostSelf
//     (non-blocking). Run priority-drains selfCh before reading from
//     ch on each iteration, so a handler-issued follow-up event
//     processes ahead of pre-queued external events. This closes the
//     FIFO race the bot flagged: previously a pre-queued
//     EvIntentUpdate(stopped) could be processed against StSpawning
//     (no transition -> drop) before the handler's inline-Post
//     EvHealthOK / synthetic EvChildExit transitioned the daemon
//     out of StSpawning.
//
// The selfCh design also eliminates the inline-Post deadlock: the
// handler is the only producer of selfCh, so PostSelf cannot collide
// with external producers filling the buffer. PostSelf is
// non-blocking (TryPost semantics), and the consumer is the SAME
// handler that just returned - so the priority-drain on next iteration
// guarantees the self-event lands before any external work.
type EventLoop struct {
	ch     chan LoopEvent
	selfCh chan LoopEvent
	// handlers is copy-on-write via an atomic pointer so RegisterHandler —
	// which the supervisor calls AFTER `go loop.Run` has already started (the
	// ctrl.handleLoopEvent registration at supervise.go can only happen once
	// ctrl is constructed, which is after the loop goroutine launches) —
	// cannot data-race the dispatch goroutine's read of the slice. Conc-F5
	// (PR #268 deep-sec P3): the old plain-slice `append` was verified safe
	// only because no producer Posts in the register→Run window, an invariant
	// one early producer away from a genuine -race hit. COW removes the
	// dependency without restructuring supervise.go's ctrl-construction order.
	handlers atomic.Pointer[[]func(LoopEvent)]

	// onPanic, when set via SetPanicHandler, is invoked if a handler
	// panics during dispatch. It exists ONLY to make an otherwise-silent
	// handler-panic death attributable (the supervisor process crashes on
	// the loop goroutine with NO supervisor-exit event otherwise) — see
	// dispatch. It must NOT swallow the panic; dispatch re-raises after
	// calling it.
	//
	// Stored behind an atomic pointer for the SAME reason `handlers` is
	// (Conc-F5): production wires SetPanicHandler before `go loop.Run`, so
	// the write happens-before the loop goroutine's dispatch read — but a
	// plain field is "safe only because no concurrent setter exists", an
	// invariant one early caller away from a genuine -race hit on
	// dispatch's read vs SetPanicHandler's write. The atomic pointer makes
	// the read lock-free (a single Load in dispatch) and the field
	// genuinely synchronized regardless of call ordering. nil pointer ==
	// no observer installed.
	onPanic atomic.Pointer[func(recovered any, e LoopEvent)]
}

func NewEventLoop(capacity int) *EventLoop {
	if capacity < 16 {
		capacity = 16
	}
	// selfCh matches the main channel capacity for symmetry. In practice
	// selfCh only fills if many handler-self-posts queue up faster than
	// the loop drains them, which requires a deeply-stacked SM cycle.
	// At cap == main cap the headroom matches the burst absorption the
	// main channel was sized for.
	return &EventLoop{
		ch:     make(chan LoopEvent, capacity),
		selfCh: make(chan LoopEvent, capacity),
	}
}

func (l *EventLoop) RegisterHandler(h func(LoopEvent)) {
	// Copy-on-write: build a fresh slice (old + h) and atomically swap it in.
	// Registration is rare (startup) and dispatch is hot, so COW keeps the hot
	// path lock-free (a single atomic load in dispatch) while making concurrent
	// registration safe. The CAS retry loop covers the (practically impossible
	// but cheap to handle) case of two registrations racing.
	for {
		old := l.handlers.Load()
		var next []func(LoopEvent)
		if old != nil {
			next = make([]func(LoopEvent), len(*old), len(*old)+1)
			copy(next, *old)
		} else {
			next = make([]func(LoopEvent), 0, 1)
		}
		next = append(next, h)
		if l.handlers.CompareAndSwap(old, &next) {
			return
		}
	}
}

// SetPanicHandler installs an observer invoked when a handler panics
// during dispatch. The supervisor wires this to emit a durable
// `supervisor-handler-panic` event BEFORE the process dies, so the
// otherwise-silent loop-goroutine crash (no supervisor-exit event,
// the exact gap behind the "supervisor died with no event" mystery)
// becomes attributable. dispatch RE-RAISES the panic after calling the
// observer — the death stays loud so the recovery layer respawns, and a
// half-applied state-machine transition is never silently continued.
//
// Typically set before Run; the atomic store makes a concurrent set
// data-race-free regardless (mirrors RegisterHandler's COW posture).
func (l *EventLoop) SetPanicHandler(f func(recovered any, e LoopEvent)) {
	if f == nil {
		l.onPanic.Store(nil)
		return
	}
	l.onPanic.Store(&f)
}

// dispatch fans one event to every handler. When a panic handler is
// installed, a handler panic is observed via onPanic (best-effort) and
// then RE-RAISED — never swallowed: swallowing mid-transition would
// leave a daemon in a half-applied restart-policy state. With no panic
// handler set the behavior is identical to a bare handler loop (the
// panic propagates unchanged).
func (l *EventLoop) dispatch(e LoopEvent) {
	if onPanic := l.onPanic.Load(); onPanic != nil {
		defer func() {
			if r := recover(); r != nil {
				(*onPanic)(r, e)
				panic(r)
			}
		}()
	}
	hs := l.handlers.Load()
	if hs == nil {
		return
	}
	for _, h := range *hs {
		h(e)
	}
}

// Post sends an event to the loop's main (external) channel. Buffered
// up to the loop's capacity (default 1024 per supervise.go after PR
// #236 r4); when the buffer is full, Post BLOCKS until a slot opens.
// Closes sonnet impl-r1 BLOCKER on misleading "non-blocking"
// comment; codex impl-r1 LOW flagged the same. Blocking is the safer
// choice for state-machine events — silently dropping an EvChildExit
// or EvIntentUpdate would leave a daemon stuck in the wrong SM state
// with no recovery short of supervisor restart.
//
// Producers that cannot tolerate blocking (e.g. interrupt handlers)
// should use TryPost instead.
//
// Handler callers MUST use PostSelf, NOT Post — see PostSelf doc for
// the deadlock + FIFO-race reasoning.
//
// Capacity is sized at construction (supervise.go uses 1024 to absorb
// quiesce-complete + crash bursts + Phase 9 maintenance-timer
// publishers per consultant memo on PR #236 r4). If a workload starts
// blocking on Post in production, the right response is increasing
// capacity at construction, not silently dropping events.
func (l *EventLoop) Post(e LoopEvent) {
	l.ch <- e
}

// TryPost is non-blocking. Returns true if the event was enqueued,
// false if the buffer was full. Callers that use this MUST handle the
// drop locally — emit an audit-warn or escalate, do not silently lose
// state-machine events.
func (l *EventLoop) TryPost(e LoopEvent) bool {
	select {
	case l.ch <- e:
		return true
	default:
		return false
	}
}

// PostCtx is a CONTEXT-AWARE blocking enqueue: it waits for a free slot on
// the main channel like Post, but ABANDONS the wait when ctx is canceled
// (returns context.Canceled / context.DeadlineExceeded). It is the bounded
// variant external producers use when a wedged or stopped loop with a FULL
// buffer would otherwise block Post FOREVER.
//
// The motivating case (Codex pr302 r6 finding 3): the reconcile-apply sync
// barrier (supervisor_controller.refreshSupervisorIntentSync) posts an
// evReapScan then waits on a done-channel under a ctx/timeout select. With
// the plain blocking Post the timeout could never fire when the buffer was
// already full — Post blocked on `l.ch <- e` BEFORE the caller reached its
// select, so a full/stopped loop froze the IPC handler indefinitely. Routing
// the enqueue through PostCtx makes the SAME ctx (a deadline-bounded context)
// bound the enqueue too, so the documented timeout actually caps the path.
//
// On the SUCCESS path the enqueue still completes normally (the event lands
// on the channel and the caller proceeds to wait for the done-barrier), so
// the cache-swap-observable-before-return guarantee the r6 barrier
// established is preserved — only the blocked path is bounded. Returns nil
// when the event was enqueued, ctx.Err() when ctx fired first.
func (l *EventLoop) PostCtx(ctx context.Context, e LoopEvent) error {
	if ctx == nil {
		l.ch <- e
		return nil
	}
	select {
	case l.ch <- e:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PostSelf enqueues a handler-self-post on the priority channel.
// Non-blocking by design - returns false if selfCh is full so the
// caller can audit-log the drop. The loop priority-drains selfCh
// before reading from the main ch, so a successful PostSelf is
// guaranteed to land BEFORE any pre-queued external event.
//
// PostSelf is the ONLY safe way to post from inside a handler. An
// inline Post (blocking) from a handler can deadlock when the buffer
// is full because the handler IS the only consumer; an inline TryPost
// from a handler races with pre-queued external events because it
// goes to the main channel tail.
//
// Closes bot finding on PR #236 1c0ea09 (P2 blocking-Post deadlock +
// FIFO race with queued external EvIntentUpdate).
func (l *EventLoop) PostSelf(e LoopEvent) bool {
	select {
	case l.selfCh <- e:
		return true
	default:
		return false
	}
}

// Run consumes events until ctx is canceled. selfCh is priority-drained
// before reading from ch on each iteration so handler-self-posts
// process FIRST and cannot be overtaken by pre-queued external events.
//
// Priority-drain loop: inner for-select with default reads ALL pending
// selfCh entries before blocking on the outer select. This guarantees
// that if a handler enqueued N self-posts (e.g. a complex spawn cycle
// that fires multiple state-machine transitions), all N drain before
// the next external event is read.
func (l *EventLoop) Run(ctx context.Context) {
	drainSelf := func() {
		for {
			select {
			case e := <-l.selfCh:
				l.dispatch(e)
			default:
				return
			}
		}
	}
	for {
		drainSelf()
		select {
		case <-ctx.Done():
			return
		case e := <-l.selfCh:
			// selfCh fired between drainSelf and outer select; process
			// it directly without entering ch.
			l.dispatch(e)
		case e := <-l.ch:
			l.dispatch(e)
		}
	}
}
