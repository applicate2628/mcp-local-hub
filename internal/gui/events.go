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

	"mcp-local-hub/internal/api"
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

// persistRequest carries one envelope-ready event onto the async
// persist channel. The shape is intentionally small so the channel
// buffer stays cheap.
type persistRequest struct {
	ev Event
}

// Capacity of the persist channel. When full, Publish drops the
// envelope (matches the existing SSE drop-on-full policy). 256 is
// chosen so a brief burst of bulk-action / daemon-state events
// doesn't lose any in practice while keeping memory bounded.
const persistChannelCap = 256

// Broadcaster is a fan-out channel for Events. Each Subscribe call
// returns a dedicated buffered channel; Publish writes to every
// subscriber without blocking (dropped if the buffer is full).
//
// G9: each Publish also persists a structured envelope to
// gui-events.log via api.AppendGUIEventLog. To keep Publish non-
// blocking (Codex P2 on PR #150 line 162), persistence goes through a
// buffered channel drained by a single background goroutine. The
// single drain goroutine also guarantees publish ORDER survives to
// disk (Codex P2 on PR #150 line 156) — channel sends happen under
// b.mu in the same critical section as the SSE fan-out, and the
// goroutine consumes in FIFO order.
//
// Lifecycle: NewBroadcaster spawns the drain goroutine. Close()
// stops it (idempotent + blocks until all pending entries flushed,
// so tests can verify persisted state deterministically).
//
// Tests that don't care about disk persistence (or run before
// DaemonStateDir is initialized) set DisableGUIEventLog=true. The
// api handle is injected via SetAPI so tests use a state-rooted
// instance; nil falls back to api.NewAPI() for production GUI.
type Broadcaster struct {
	mu                 sync.Mutex
	subs               map[chan Event]struct{}
	api                *api.API
	DisableGUIEventLog bool

	persistCh     chan persistRequest
	persistDoneCh chan struct{}
	closeOnce     sync.Once
	// closed is set under b.mu inside Close() before close(persistCh).
	// Publish checks it under the same mutex so a concurrent Close()
	// cannot cause a send-on-closed-channel panic (Codex P1 on PR #150
	// line 227).
	closed bool
}

// NewBroadcaster constructs an empty Broadcaster with no subscribers.
// Spawns a single background goroutine that drains the persist queue;
// callers that want clean shutdown should invoke Close() during
// teardown.
func NewBroadcaster() *Broadcaster {
	b := &Broadcaster{
		subs:          map[chan Event]struct{}{},
		persistCh:     make(chan persistRequest, persistChannelCap),
		persistDoneCh: make(chan struct{}),
	}
	go b.drainPersist()
	return b
}

// SetAPI threads an api.API handle through the broadcaster so the
// drain goroutine can call AppendGUIEventLog without constructing a
// fresh API per event. Optional — nil/unset falls back to
// api.NewAPI() inside persistOne. Production server bootstrap calls
// this once with the process-wide api handle.
func (b *Broadcaster) SetAPI(a *api.API) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.api = a
}

// Close stops the drain goroutine and blocks until any in-flight or
// queued persist calls finish. Idempotent.
//
// Codex P1 on PR #150 line 227: a concurrent Publish (poller / HTTP
// handler still emitting during teardown) racing with close(persistCh)
// would panic on send-after-close. Fix: set b.closed=true and
// close(persistCh) atomically under b.mu, so Publish's check + send
// happens entirely before OR entirely after the close — never
// interleaved.
func (b *Broadcaster) Close() {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		close(b.persistCh)
		b.mu.Unlock()
	})
	<-b.persistDoneCh
}

func (b *Broadcaster) drainPersist() {
	defer close(b.persistDoneCh)
	for req := range b.persistCh {
		b.persistOne(req.ev)
	}
}

func (b *Broadcaster) persistOne(ev Event) {
	source, severity := classifyEvent(ev.Type)
	b.mu.Lock()
	a := b.api
	b.mu.Unlock()
	if a == nil {
		a = api.NewAPI()
	}
	_ = a.AppendGUIEventLog(api.GUIEventEntry{
		Type:     ev.Type,
		Source:   source,
		Severity: severity,
		Body:     ev.Body,
	})
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
// reached. The capacity check + insertion happen under one lock-acquire
// of b.mu so two concurrent callers cannot both observe room and then
// both insert, overflowing the cap. The unsubscribe goroutine must
// take b.mu separately, so this insertion CANNOT call back into any
// path that also locks b.mu.
//
// /api/events uses this so a 503 can be returned before HTTP headers
// are committed; replacing Subscribe with this for that path lets the
// existing browser client see the rejection cleanly.
func (b *Broadcaster) TrySubscribe(ctx context.Context) (<-chan Event, bool) {
	ch := make(chan Event, 16)
	b.mu.Lock()
	if len(b.subs) >= maxSSESubscribers {
		b.mu.Unlock()
		return nil, false
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
		close(ch)
	}()
	return ch, true
}

// Publish fans out to all subscribers AND enqueues a persist request.
// Both operations are non-blocking and complete in O(subscribers) wall
// time — the persist channel is sized so a brief burst is queued
// without blocking the caller.
//
// Codex P2 on PR #150 lines 156 + 162: the persist enqueue happens
// UNDER b.mu so the FIFO order of channel sends matches the SSE fan-
// out order. The single drainPersist goroutine consumes from the
// channel sequentially and calls AppendGUIEventLog — order survives
// to disk. The select/default on the channel makes the send non-
// blocking, so a stalled filesystem cannot block Publish (the entry
// is dropped instead; future work can add an atomic drop counter).
//
// Set DisableGUIEventLog=true to opt out of persistence entirely
// (tests + ephemeral subscribers).
func (b *Broadcaster) Publish(ev Event) {
	b.mu.Lock()
	for c := range b.subs {
		select {
		case c <- ev:
		default: // drop
		}
	}
	// Skip the persist enqueue once Close() has flipped the shutdown
	// flag (Codex P1 on PR #150 line 227 — guards against send-on-
	// closed-channel panic). Both the flag set and channel close
	// happen under b.mu inside Close(), so checking + sending here is
	// atomic with respect to teardown.
	if !b.DisableGUIEventLog && !b.closed {
		select {
		case b.persistCh <- persistRequest{ev: ev}:
		default: // persist channel full → drop (matches SSE drop-on-full policy)
		}
	}
	b.mu.Unlock()
}

// classifyEvent maps the wire event type to (source, severity). Keeps
// the envelope concise so log consumers can filter by source without
// inspecting the body. Type-to-source mapping reflects the current
// emit sites; new event types should add a row here.
func classifyEvent(eventType string) (source string, severity string) {
	switch eventType {
	case "daemon-state":
		return "poller", api.GUIEventSeverityInfo
	case "poller-error":
		return "poller", api.GUIEventSeverityError
	case "bulk-action":
		return "servers", api.GUIEventSeverityInfo
	default:
		return "gui", api.GUIEventSeverityInfo
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
