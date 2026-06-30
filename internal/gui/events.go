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
	"sync/atomic"
	"time"

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
	persistStart  sync.Once
	// closed is set under b.mu inside Close() before close(persistCh).
	// Publish checks it under the same mutex so a concurrent Close()
	// cannot cause a send-on-closed-channel panic (Codex P1 on PR #150
	// line 227).
	closed bool
	// persistStarted records whether the drain goroutine was spawned.
	// Close() consults this to know whether to wait on persistDoneCh
	// (drain closes it) or close it manually (no drain ever ran).
	// Lazy spawn closes Codex P2 on PR #150 line 101: NewBroadcaster
	// no longer leaks one goroutine per construction, because tests +
	// handler-only call sites that never Publish never spawn anything.
	persistStarted bool

	// sseDropped / persistDropped count events dropped because a
	// subscriber's buffered channel (sseDropped) or the persist channel
	// (persistDropped) was full when Publish tried to send. Both are
	// best-effort observability counters (deep-review P3 finding: a
	// drop used to be completely silent — no counter, no log line). Read
	// via DroppedCounts(); both are atomic so Publish never needs to take
	// b.mu just to bump them on the hot path. Plain uint64 (not capped) —
	// these are diagnostic counters, not security-sensitive state, and a
	// theoretical wraparound after ~1.8e19 drops is not a practical
	// concern for a desktop GUI process.
	sseDropped     atomic.Uint64
	persistDropped atomic.Uint64

	// lastDropWarnAt throttles the stderr/hub-mcp.log warn emitted on a
	// drop so a sustained burst of drops (e.g. a wedged subscriber) does
	// not itself spam the log. Guarded by dropWarnMu, not b.mu, so the
	// warn path never contends with the SSE fan-out / persist enqueue
	// critical section. time.Time zero value means "never warned yet".
	dropWarnMu     sync.Mutex
	lastDropWarnAt time.Time
}

// dropWarnInterval bounds how often Publish emits the throttled
// "events-dropped" warn line, mirroring the transition-throttled
// pattern api.HubListenerHealthWatcher already uses for
// hub-listener-unresponsive (one warn per sustained condition, not one
// per occurrence).
const dropWarnInterval = 30 * time.Second

// NewBroadcaster constructs an empty Broadcaster with no subscribers.
// Does NOT spawn any goroutine — the persist drain is started lazily
// on the first Publish() with persistence enabled. This eliminates
// goroutine leaks from short-lived test broadcasters and handler-only
// call sites that never publish.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		subs:          map[chan Event]struct{}{},
		persistCh:     make(chan persistRequest, persistChannelCap),
		persistDoneCh: make(chan struct{}),
	}
}

// ensurePersistDrain spawns the persist drain goroutine once (sync.Once).
// Called from Publish under b.mu so the persistStarted flag is set
// atomically with the spawn — Close() reads the same field under the
// same lock to decide whether to wait on persistDoneCh.
func (b *Broadcaster) ensurePersistDrain() {
	b.persistStart.Do(func() {
		b.persistStarted = true
		go b.drainPersist()
	})
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

// closeDrainTimeout caps how long Close() will wait for the persist
// drain goroutine to finish. Codex P2 on PR #150 round 5 line 557:
// AppendGUIEventLog uses a blocking flock.Lock() with no timeout, so
// a stalled filesystem or contended lock could block shutdown
// indefinitely. The 3s cap lets shutdown honor the gui server's 5s
// graceful-shutdown budget — losing the in-flight persist write is
// preferable to hanging the whole process.
const closeDrainTimeout = 3 * time.Second

// Close stops the drain goroutine and blocks until any in-flight or
// queued persist calls finish (or until closeDrainTimeout elapses).
// Idempotent.
//
// Codex P1 on PR #150 line 227: a concurrent Publish racing with
// close(persistCh) would panic on send-after-close. Fix: set
// b.closed=true and close(persistCh) atomically under b.mu, so
// Publish's check + send happens entirely before OR entirely after
// the close — never interleaved.
//
// Codex P2 on PR #150 round 4 line 101: with lazy drain spawn, Close()
// must also close persistDoneCh manually when no drain ever ran
// (otherwise the wait below would hang forever).
//
// Codex P2 on PR #150 round 5 line 557: cap the wait so a stalled
// flock acquire in AppendGUIEventLog cannot block shutdown past the
// gui server's 5s graceful-shutdown budget.
func (b *Broadcaster) Close() {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		started := b.persistStarted
		close(b.persistCh)
		if !started {
			// No drain goroutine ever spawned — close the done
			// channel ourselves so callers waiting on it return.
			close(b.persistDoneCh)
		}
		b.mu.Unlock()
	})
	select {
	case <-b.persistDoneCh:
		// drain exited cleanly within the budget
	case <-time.After(closeDrainTimeout):
		// drain stalled (likely flock contention or slow disk).
		// Abandon the wait — drain is a daemon-style goroutine and
		// will eventually exit when the OS releases its lock; in
		// the worst case it leaks past process exit, but shutdown
		// won't hang. Any unflushed entries are lost — best-effort
		// persist semantics already documented in Publish.
	}
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
// is dropped instead).
//
// deep-review P3 finding: a drop on either channel used to be
// completely silent. Both drop paths now increment an atomic counter
// (sseDropped / persistDropped, readable via DroppedCounts) and a
// throttled warn is emitted (at most once per dropWarnInterval) so a
// sustained drop condition (wedged SSE subscriber, stalled disk) is
// observable instead of silently losing audit/SSE rows. The select/
// default itself stays non-blocking — only the COUNTING is new; the
// publisher is never made to wait on the warn emission (that happens
// after b.mu is released, see maybeWarnDropped).
//
// Set DisableGUIEventLog=true to opt out of persistence entirely
// (tests + ephemeral subscribers).
func (b *Broadcaster) Publish(ev Event) {
	b.mu.Lock()
	droppedSSE := false
	for c := range b.subs {
		select {
		case c <- ev:
		default: // drop
			droppedSSE = true
		}
	}
	droppedPersist := false
	// Skip the persist enqueue once Close() has flipped the shutdown
	// flag (Codex P1 on PR #150 line 227 — guards against send-on-
	// closed-channel panic). Both the flag set and channel close
	// happen under b.mu inside Close(), so checking + sending here is
	// atomic with respect to teardown.
	if !b.DisableGUIEventLog && !b.closed {
		// Lazy-spawn the drain goroutine on first persistable Publish
		// (Codex P2 on PR #150 round 4 line 101). Tests + handler-
		// only call sites that never Publish never spawn anything.
		b.ensurePersistDrain()
		select {
		case b.persistCh <- persistRequest{ev: ev}:
		default: // persist channel full → drop (matches SSE drop-on-full policy)
			droppedPersist = true
		}
	}
	b.mu.Unlock()

	if droppedSSE {
		b.sseDropped.Add(1)
	}
	if droppedPersist {
		b.persistDropped.Add(1)
	}
	if droppedSSE || droppedPersist {
		b.maybeWarnDropped(ev.Type, droppedSSE, droppedPersist)
	}
}

// DroppedCounts returns the cumulative number of events dropped because
// a subscriber's SSE channel (sse) or the persist channel (persist) was
// full at Publish time. Exposed for /api/status and tests; the GUI
// status surface can render these as an operator-visible signal that
// audit/SSE rows were lost.
func (b *Broadcaster) DroppedCounts() (sse, persist uint64) {
	return b.sseDropped.Load(), b.persistDropped.Load()
}

// PublishOperatorAction is the single owner for emitting a gui-events.log
// row on an operator-initiated, security-relevant GUI mutation (deep-
// review P3 finding: supervisor restart, migrate, demigrate, install,
// secret mutation, backup delete/clean previously left no audit row at
// all). Call ONLY after the mutation has actually committed — a failed
// op must not emit a misleading success record.
//
// action names the operation (e.g. "supervisor-restart", "migrate",
// "secret-rotate"); detail carries non-sensitive identifiers only
// (server name, client name, secret KEY NAME — never a secret value or
// other credential material; callers are responsible for that
// redaction, mirroring the discipline emitSecretAuditEvent already
// applies to hub-mcp.log). actor is the OS user performing the
// mutation (api.CurrentOSUser()).
//
// Nil-safe: a *Broadcaster obtained from a bare &Server{} test fixture
// (no NewServer/NewBroadcaster call) is nil, and every call site below
// is reached from production handlers registered on a real *Server
// where s.events is always set — but unit tests construct handler-only
// servers directly, so this guard keeps every call site simple.
func (b *Broadcaster) PublishOperatorAction(action, actor string, detail map[string]any) {
	if b == nil {
		return
	}
	body := make(map[string]any, len(detail)+2)
	for k, v := range detail {
		body[k] = v
	}
	body["action"] = action
	body["actor"] = actor
	b.Publish(Event{Type: "operator-action", Body: body})
}

// maybeWarnDropped emits a throttled warn to hub-mcp.log when Publish
// drops an event on either channel. Throttled to at most once per
// dropWarnInterval (mirrors the transition-throttled warn pattern
// api.HubListenerHealthWatcher uses for hub-listener-unresponsive) so a
// sustained burst of drops cannot itself flood the log. Called OUTSIDE
// b.mu so a slow flock append on hub-mcp.log can never contend with the
// SSE fan-out / persist enqueue critical section.
func (b *Broadcaster) maybeWarnDropped(eventType string, droppedSSE, droppedPersist bool) {
	b.dropWarnMu.Lock()
	now := time.Now()
	if !b.lastDropWarnAt.IsZero() && now.Sub(b.lastDropWarnAt) < dropWarnInterval {
		b.dropWarnMu.Unlock()
		return
	}
	b.lastDropWarnAt = now
	b.dropWarnMu.Unlock()

	sseTotal, persistTotal := b.DroppedCounts()
	_ = api.LogHubMcpEvent("warn", "gui-events-dropped", map[string]any{
		"event_type":            eventType,
		"dropped_sse":           droppedSSE,
		"dropped_persist":       droppedPersist,
		"sse_dropped_total":     sseTotal,
		"persist_dropped_total": persistTotal,
		"note":                  "SSE subscriber or gui-events.log persist channel was full; event(s) dropped (best-effort delivery by design — see events.go)",
	})
}

// classifyEvent maps the wire event type to (source, severity). Keeps
// the envelope concise so log consumers can filter by source without
// inspecting the body. Type-to-source mapping reflects the current
// emit sites; new event types should add a row here.
func classifyEvent(eventType string) (source string, severity string) {
	switch eventType {
	case "daemon-state":
		return "poller", api.GUIEventSeverityInfo
	case "daemon-failed":
		// Rising-edge daemon failure observed by the poller (same predicate
		// as the tray icon + toast). Error severity so the gui-events.log
		// reader can filter the failure onset out of the steady-state
		// daemon-state churn.
		return "poller", api.GUIEventSeverityError
	case "daemon-backend-lost":
		return "poller", api.GUIEventSeverityInfo
	case "daemon-recovered":
		// Falling edge — a previously-failed daemon is healthy again (the
		// supervisor auto-restart, or a manual restart, succeeded). Info
		// severity: it is the all-clear paired with daemon-failed.
		return "poller", api.GUIEventSeverityInfo
	case "poller-error":
		return "poller", api.GUIEventSeverityError
	case "bulk-action":
		return "servers", api.GUIEventSeverityInfo
	case "operator-action":
		// Operator-initiated, security-relevant GUI mutations (supervisor
		// restart, migrate, demigrate, install, secret mutation, backup
		// delete/clean — deep-review P3 finding). Body.action names the
		// specific operation; warn severity matches the level
		// emitSecretAuditEvent already uses for credential mutations in
		// hub-mcp.log, so an operator scanning gui-events.log by severity
		// sees these alongside other operator-significant events.
		return "operator", api.GUIEventSeverityWarn
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
