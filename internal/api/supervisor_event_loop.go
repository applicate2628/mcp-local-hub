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

// Post is non-blocking when the loop's channel has capacity.
// Side-goroutine handlers (quiesce-timers, exit{graceful}) post here.
func (l *EventLoop) Post(e LoopEvent) {
	l.ch <- e
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
