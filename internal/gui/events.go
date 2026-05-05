// Package gui — SSE event bus.
//
// Broadcaster is a fan-out channel used by the GUI HTTP server to push
// real-time updates (daemon-state, scan progress, etc.) to any connected
// browser client via GET /api/events. The design is intentionally
// minimal: subscribers own their lifetime via context, slow consumers
// drop events rather than backpressure the publisher, and there is no
// replay — late subscribers start from "now".
//
// Spec: §3.6 (real-time event bus).
// Task 11 lays the plumbing. Task 12 adds a poller that publishes
// daemon-state events into this broadcaster; Task 18 consumes them in
// the Dashboard screen.
package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"sync"
)

// maxSSESubscribers is the per-Broadcaster admission cap applied via
// TrySubscribe at /api/events. Internal callers that use Subscribe are
// not bounded — the cap exists to prevent an attacker holding open
// long-lived browser SSE streams from exhausting GUI memory.
const maxSSESubscribers = 64

// Event is the shape pushed onto /api/events. Type matches spec §3.6;
// Body is an arbitrary JSON-serializable payload.
type Event struct {
	Type string         `json:"type"`
	Body map[string]any `json:"body,omitempty"`
}

// Broadcaster is a fan-out channel for Events. Each Subscribe call
// returns a dedicated buffered channel; Publish writes to every
// subscriber without blocking (dropped if the buffer is full).
type Broadcaster struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewBroadcaster constructs an empty Broadcaster with no subscribers.
// It starts no goroutines — the poller that feeds it is owned by
// Task 12 and started separately.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: map[chan Event]struct{}{}}
}

// Subscribe returns a channel that will receive every Event published
// while ctx is alive. The channel is closed when ctx is canceled.
// Buffered at 16 — a slow consumer drops events rather than backpressures.
//
// Subscribe is unbounded by design: internal callers (poller, tests) rely
// on always getting a channel. Untrusted external callers (e.g. the
// /api/events HTTP handler) should use TrySubscribe to participate in
// the global subscriber cap.
func (b *Broadcaster) Subscribe(ctx context.Context) <-chan Event {
	return b.subscribeUnchecked(ctx)
}

func (b *Broadcaster) subscribeUnchecked(ctx context.Context) chan Event {
	ch := make(chan Event, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
		close(ch)
	}()
	return ch
}

// TrySubscribe attempts to add a subscriber, returning the channel and
// true on success or (nil, false) if the global subscriber cap is
// reached. The capacity check + insertion is atomic under b.mu so two
// concurrent callers cannot both observe room and overflow the cap.
//
// /api/events uses this so a 503 can be returned before HTTP headers
// are committed; replacing Subscribe with this for that path lets the
// existing browser client see the rejection cleanly.
func (b *Broadcaster) TrySubscribe(ctx context.Context) (<-chan Event, bool) {
	b.mu.Lock()
	if len(b.subs) >= maxSSESubscribers {
		b.mu.Unlock()
		return nil, false
	}
	b.mu.Unlock()
	return b.subscribeUnchecked(ctx), true
}

// Publish fans out to all subscribers. Non-blocking: a subscriber with
// a full buffer simply misses the event.
//
// The send happens while holding b.mu so that a concurrent ctx-cancel
// unsubscribe (which also holds b.mu before close) cannot close the
// channel mid-send. Because the send is non-blocking (select/default),
// holding the mutex through the fan-out cannot deadlock.
func (b *Broadcaster) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.subs {
		select {
		case c <- ev:
		default: // drop
		}
	}
}

// acceptsEventStream returns true if the Accept header contains a
// structured `text/event-stream` media-type token. Empty Accept is
// treated as accepting anything (browsers do this on EventSource
// requests). Wildcards (`*/*`, `text/*`) also accept.
//
// The handler must NOT use a substring match like
// `strings.Contains(accept, "text/event-stream")` because that admits
// bogus tokens such as `text/event-streamx` or content with the
// substring buried in a parameter value.
func acceptsEventStream(accept string) bool {
	if accept == "" {
		return true
	}
	for _, part := range strings.Split(accept, ",") {
		mt, _, err := mime.ParseMediaType(part)
		if err != nil {
			continue
		}
		switch mt {
		case "text/event-stream", "text/*", "*/*":
			return true
		}
	}
	return false
}

// registerEventsRoutes wires GET /api/events as a text/event-stream
// handler. Each connected client gets its own TrySubscribe channel; the
// handler exits when either the client disconnects (request context
// canceled) or the subscription channel is closed.
//
// Admission is checked BEFORE setting/flushing stream headers so a 503
// (subscriber cap) or 406 (bad Accept) can return as a real error code
// to the browser. Once headers are flushed, the connection is committed
// to a 200/text-event-stream response and the only signal we have left
// is closing the stream.
func registerEventsRoutes(s *Server) {
	s.mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !acceptsEventStream(r.Header.Get("Accept")) {
			http.Error(w, "Accept must include text/event-stream", http.StatusNotAcceptable)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		ctx := r.Context()
		ch, ok := s.events.TrySubscribe(ctx)
		if !ok {
			http.Error(w, "too many SSE subscribers", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()

		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				body, _ := json.Marshal(ev.Body)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, body)
				flusher.Flush()
			}
		}
	})
}
