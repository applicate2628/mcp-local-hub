package api

import "context"

// LoopEvent is the supervisor's FIFO event envelope. Per spec Q4 v13.
type LoopEvent struct {
	Kind     SMEvent
	TaskName string
	Body     map[string]any
}

// EventLoop is a single-threaded FIFO event consumer. Capacity >= 16
// per spec to absorb quiesce-complete posts from the side-goroutine
// drain handler without rendezvous deadlock.
type EventLoop struct {
	ch       chan LoopEvent
	handlers []func(LoopEvent)
}

func NewEventLoop(capacity int) *EventLoop {
	if capacity < 16 {
		capacity = 16
	}
	return &EventLoop{ch: make(chan LoopEvent, capacity)}
}

func (l *EventLoop) RegisterHandler(h func(LoopEvent)) {
	l.handlers = append(l.handlers, h)
}

// Post sends an event to the loop. Buffered up to the loop's
// capacity (default 64); when the buffer is full, Post BLOCKS until a
// slot opens. Closes sonnet impl-r1 BLOCKER on misleading "non-blocking"
// comment; codex impl-r1 LOW flagged the same. Blocking is the safer
// choice for state-machine events — silently dropping an EvChildExit
// or EvIntentUpdate would leave a daemon stuck in the wrong SM state
// with no recovery short of supervisor restart.
//
// Producers that cannot tolerate blocking (e.g. interrupt handlers)
// should use TryPost instead.
//
// Capacity is sized at construction (supervise.go uses 64 to absorb
// quiesce-complete + crash bursts). If a workload starts blocking on
// Post in production, the right response is increasing capacity at
// construction, not silently dropping events.
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

// Run consumes events until ctx is canceled.
func (l *EventLoop) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-l.ch:
			for _, h := range l.handlers {
				h(e)
			}
		}
	}
}
