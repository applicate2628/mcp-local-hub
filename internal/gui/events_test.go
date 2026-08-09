package gui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func TestBroadcaster_SubscribeReceivesPublishedEvent(t *testing.T) {
	b := newEphemeralBroadcaster(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx)
	go b.Publish(Event{Type: "test", Body: map[string]any{"k": "v"}})

	select {
	case ev := <-ch:
		if ev.Type != "test" {
			t.Errorf("type = %s, want test", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("subscriber timed out")
	}
}

func TestBroadcaster_UnsubscribeOnContextCancel(t *testing.T) {
	b := newEphemeralBroadcaster(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch := b.Subscribe(ctx)
	cancel()
	// Give the unsubscribe goroutine a moment to run.
	time.Sleep(50 * time.Millisecond)
	// Publish after cancel; the subscriber's channel should have been closed.
	b.Publish(Event{Type: "after-cancel"})
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("subscriber channel should be closed after context cancel")
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("cancelled subscriber should have returned from receive immediately")
	}
}

// TestBroadcaster_Publish_PersistsToGUIEventLog covers G9: every
// Publish writes a structured envelope to gui-events.log via
// AppendGUIEventLog. The classifyEvent helper assigns source +
// severity per type. SSE fan-out is unaffected.
//
// The persist channel is drained by a background goroutine; Close()
// blocks until the drain finishes, so the post-Publish read is
// deterministic even though Publish is non-blocking.
func TestBroadcaster_Publish_PersistsToGUIEventLog(t *testing.T) {
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)
	a := api.NewAPI()
	b := NewBroadcaster()
	b.SetAPI(a)
	b.Publish(Event{Type: "daemon-state", Body: map[string]any{"server": "memory"}})
	b.Publish(Event{Type: "daemon-backend-lost", Body: map[string]any{"server": "serena"}})
	b.Publish(Event{Type: "poller-error", Body: map[string]any{"err": "boom"}})
	b.Publish(Event{Type: "bulk-action", Body: map[string]any{"action": "restart"}})
	b.Close() // flush drain goroutine before reading

	tail := a.ReadGUIEventLogTail(10)
	if len(tail) != 4 {
		t.Fatalf("tail len = %d, want 4", len(tail))
	}
	cases := []struct {
		etype, wantSource, wantSeverity string
	}{
		{"daemon-state", "poller", api.GUIEventSeverityInfo},
		{"daemon-backend-lost", "poller", api.GUIEventSeverityInfo},
		{"poller-error", "poller", api.GUIEventSeverityError},
		{"bulk-action", "servers", api.GUIEventSeverityInfo},
	}
	for i, c := range cases {
		if tail[i].Type != c.etype {
			t.Errorf("[%d] type = %q, want %q", i, tail[i].Type, c.etype)
		}
		if tail[i].Source != c.wantSource {
			t.Errorf("[%d] source = %q, want %q", i, tail[i].Source, c.wantSource)
		}
		if tail[i].Severity != c.wantSeverity {
			t.Errorf("[%d] severity = %q, want %q", i, tail[i].Severity, c.wantSeverity)
		}
		if tail[i].SchemaVersion != api.GUIEventLogSchemaVersion {
			t.Errorf("[%d] schema_version = %q, want %q", i, tail[i].SchemaVersion, api.GUIEventLogSchemaVersion)
		}
	}
}

func TestClassifyEvent_RestartV3ProgressAndDiscriminators(t *testing.T) {
	t.Parallel()

	cases := []struct {
		eventType, wantSeverity string
	}{
		{"gui-restart-progress", api.GUIEventSeverityInfo},
		{"gui-restart-lock-acquired", api.GUIEventSeverityInfo},
		{"gui-restart-reservation-held", api.GUIEventSeverityInfo},
		{"gui-restart-spawn-failed", api.GUIEventSeverityWarn},
		{"gui-restart-proof-timeout", api.GUIEventSeverityWarn},
		{"gui-restart-proof-mismatch", api.GUIEventSeverityWarn},
		{"gui-restart-child-bind-failed", api.GUIEventSeverityWarn},
		{"gui-restart-pre-release-rollback", api.GUIEventSeverityWarn},
		{"gui-restart-quiesce-timeout", api.GUIEventSeverityWarn},
		{"gui-restart-reservation-write-failed", api.GUIEventSeverityWarn},
		{"gui-restart-interrupted-free-flock", api.GUIEventSeverityWarn},
		{"gui-restart-live-holder-wedged", api.GUIEventSeverityWarn},
		{"gui-restart-owner-unknown", api.GUIEventSeverityWarn},
		{"gui-restart-interrupted-owner-recovering", api.GUIEventSeverityWarn},
		{"gui-restart-pre-release-rollback-failed", api.GUIEventSeverityError},
		{"gui-restart-interrupted-marker-write-failed", api.GUIEventSeverityError},
		{"gui-restart-commit-write-failed", api.GUIEventSeverityError},
	}
	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			source, severity := classifyEvent(tc.eventType)
			if source != "gui" || severity != tc.wantSeverity {
				t.Fatalf("classifyEvent(%q) = %q/%q, want gui/%q", tc.eventType, source, severity, tc.wantSeverity)
			}
		})
	}

	source, severity := classifyEvent("future-gui-event")
	if source != "gui" || severity != api.GUIEventSeverityInfo {
		t.Fatalf("unknown fallback = %q/%q, want gui/info", source, severity)
	}
}

// TestBroadcaster_DisableGUIEventLog_SkipsPersist guards the opt-out
// path used by tests and ephemeral surfaces.
func TestBroadcaster_DisableGUIEventLog_SkipsPersist(t *testing.T) {
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)
	a := api.NewAPI()
	b := NewBroadcaster()
	b.SetAPI(a)
	b.DisableGUIEventLog = true
	b.Publish(Event{Type: "daemon-state", Body: map[string]any{"server": "memory"}})
	b.Close() // drain so goroutine doesn't leak

	tail := a.ReadGUIEventLogTail(10)
	if len(tail) != 0 {
		t.Errorf("tail len = %d, want 0 (DisableGUIEventLog should skip persist)", len(tail))
	}
}

// TestBroadcaster_Close_HonorsDrainTimeout guards Codex P2 on PR
// #150 round 5 line 557: AppendGUIEventLog uses blocking flock with
// no timeout, so a stalled persist could block shutdown indefinitely.
// Close() must return within ~closeDrainTimeout + slop even when the
// drain goroutine is stuck. The happy-path Publish + Close case
// returns in milliseconds — anything close to closeDrainTimeout
// would indicate a regression in the bounded-wait branch.
func TestBroadcaster_Close_HonorsDrainTimeout(t *testing.T) {
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)
	a := api.NewAPI()
	b := NewBroadcaster()
	b.SetAPI(a)
	// Trigger the lazy spawn by publishing once.
	b.Publish(Event{Type: "daemon-state", Body: map[string]any{"i": 0}})
	start := time.Now()
	b.Close()
	elapsed := time.Since(start)
	// closeDrainTimeout (3s) + slop. Normal happy path returns in ms.
	if elapsed > 4*time.Second {
		t.Errorf("Close() took %v, want <4s (closeDrainTimeout + slop)", elapsed)
	}
}

// TestBroadcaster_Close_NeverPublished_DoesNotHang guards Codex P2 on
// PR #150 round 4 line 101: with lazy drain spawn, Close() must
// terminate cleanly when no drain goroutine ever ran. Without the
// "close persistDoneCh manually if !persistStarted" branch in
// Close(), this test would hang on <-persistDoneCh.
func TestBroadcaster_Close_NeverPublished_DoesNotHang(t *testing.T) {
	b := NewBroadcaster()
	done := make(chan struct{})
	go func() {
		b.Close()
		close(done)
	}()
	select {
	case <-done:
		// pass — Close returned promptly without a spawned drain.
	case <-time.After(2 * time.Second):
		t.Fatal("Close hangs when no drain goroutine was spawned")
	}
}

// TestBroadcaster_NoSpawn_WhenUnused guards the lazy-spawn contract:
// a Broadcaster constructed via NewBroadcaster() that never Publishes
// a persistable event MUST NOT spawn a goroutine. Codex P2 on PR #150
// round 4 line 101 — verifies persistStarted stays false in the
// no-publish path.
func TestBroadcaster_NoSpawn_WhenUnused(t *testing.T) {
	b := NewBroadcaster()
	b.mu.Lock()
	started := b.persistStarted
	b.mu.Unlock()
	if started {
		t.Errorf("persistStarted = true after NewBroadcaster — drain spawned eagerly (regression)")
	}
	b.Close()
	b.mu.Lock()
	started = b.persistStarted
	b.mu.Unlock()
	if started {
		t.Errorf("persistStarted = true after Close — drain spawned by Close (regression)")
	}
}

// TestBroadcaster_NoSpawn_WhenDisabledPublish covers the
// DisableGUIEventLog=true path: even with Publish calls, the drain
// goroutine must not spawn because no persistence is requested.
func TestBroadcaster_NoSpawn_WhenDisabledPublish(t *testing.T) {
	b := NewBroadcaster()
	b.DisableGUIEventLog = true
	for i := 0; i < 5; i++ {
		b.Publish(Event{Type: "daemon-state", Body: map[string]any{"i": i}})
	}
	b.mu.Lock()
	started := b.persistStarted
	b.mu.Unlock()
	if started {
		t.Errorf("persistStarted = true with DisableGUIEventLog=true — drain spawned despite opt-out")
	}
	b.Close()
}

// TestBroadcaster_Close_RaceWithConcurrentPublish guards Codex P1 on
// PR #150 line 227: send-after-close panic when Publish runs
// concurrently with Close(). Under -race this exercises the b.closed
// flag + mutex coordination. Without the fix, sending on a closed
// persistCh would panic the test binary.
func TestBroadcaster_Close_RaceWithConcurrentPublish(t *testing.T) {
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)
	a := api.NewAPI()
	b := NewBroadcaster()
	b.SetAPI(a)

	const publishers = 20
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(publishers)
	for i := 0; i < publishers; i++ {
		i := i
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.Publish(Event{Type: "daemon-state", Body: map[string]any{"i": i}})
				}
			}
		}()
	}
	// Let publishers spin briefly before Close() races them.
	time.Sleep(10 * time.Millisecond)
	b.Close()
	close(stop)
	wg.Wait()
	// If we got here without panicking, the close-race guard worked.
}

// TestBroadcaster_Publish_OrderUnderConcurrency covers Codex P2 on PR
// #150 line 156: with multiple concurrent publishers, the on-disk log
// order must match the SSE fan-out order. Channel sends happen UNDER
// b.mu (same critical section as the SSE fan-out), and the single
// drain goroutine consumes FIFO — so the persisted order matches the
// order in which Publish acquired b.mu.
func TestBroadcaster_Publish_OrderUnderConcurrency(t *testing.T) {
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)
	a := api.NewAPI()
	b := NewBroadcaster()
	b.SetAPI(a)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	// Each publisher takes a unique sequence number. After all goroutines
	// finish + drain, the persisted Body["seq"] sequence must be strictly
	// monotonic — proves no out-of-order interleaving across goroutines.
	for i := 0; i < n; i++ {
		seq := i
		go func() {
			defer wg.Done()
			b.Publish(Event{Type: "daemon-state", Body: map[string]any{"seq": seq}})
		}()
	}
	wg.Wait()
	b.Close()

	tail := a.ReadGUIEventLogTail(n + 5)
	if len(tail) != n {
		t.Fatalf("tail len = %d, want %d", len(tail), n)
	}
	seen := map[int]bool{}
	for i, e := range tail {
		v, _ := e.Body["seq"].(float64)
		k := int(v)
		if seen[k] {
			t.Errorf("[%d] duplicate seq %d in persisted log", i, k)
		}
		seen[k] = true
	}
	if len(seen) != n {
		t.Errorf("persisted log has %d unique seq values, want %d", len(seen), n)
	}
}

// sseRecorder is a goroutine-safe http.ResponseWriter + http.Flusher
// used only by TestEventsSSE_StreamsPublishedEvents. httptest.ResponseRecorder
// is not safe for concurrent access between the handler goroutine (which
// writes) and the test goroutine (which reads) — this type serializes both
// sides under a mutex so the -race detector stays happy.
type sseRecorder struct {
	mu     sync.Mutex
	header http.Header
	buf    strings.Builder
	status int
}

func newSSERecorder() *sseRecorder {
	return &sseRecorder{header: http.Header{}, status: http.StatusOK}
}

func (r *sseRecorder) Header() http.Header { return r.header }

func (r *sseRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(b)
}

func (r *sseRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = code
}

func (r *sseRecorder) Flush() {}

func (r *sseRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// TestBroadcaster_Publish_CountsDroppedSSEEvent covers the deep-review P3
// finding: a full subscriber channel used to drop an event completely
// silently (no counter, no log line). With the subscriber channel
// (buffered at 16) saturated, the next Publish must increment the sse
// drop counter exposed via DroppedCounts.
func TestBroadcaster_Publish_CountsDroppedSSEEvent(t *testing.T) {
	b := NewBroadcaster()
	b.DisableGUIEventLog = true
	// A dropped Publish lazily spawns the out-of-band drop reporter;
	// Close() in cleanup stops + joins it so the test leaks no goroutine.
	t.Cleanup(b.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := b.Subscribe(ctx)

	// Saturate the subscriber's buffered channel (cap 16) without ever
	// draining it, so the next Publish hits the full-channel default
	// branch and counts as dropped.
	for i := 0; i < 32; i++ {
		b.Publish(Event{Type: "daemon-state", Body: map[string]any{"i": i}})
	}

	sse, _ := b.DroppedCounts()
	if sse == 0 {
		t.Fatalf("DroppedCounts().sse = 0, want > 0 after saturating a 16-buffer subscriber with 32 publishes")
	}
	// Drain so the test doesn't leak a goroutine blocked on send (it
	// isn't — Publish never blocks — but drain anyway for hygiene).
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// TestBroadcaster_Publish_CountsDroppedPersistEvent covers the persist-
// channel counterpart of the same finding: when the persist channel is
// saturated, Publish must count the drop via DroppedCounts instead of
// silently losing the gui-events.log row.
//
// The drain goroutine must be DETERMINISTICALLY bypassed here so the
// test's manually-pre-filled buffer actually stays full when Publish
// tries to enqueue (PR #476 bot P3: setting persistStarted=true alone
// did NOT bypass ensurePersistDrain — the sync.Once tracks its own
// internal done flag, not the struct field, so the first Publish could
// still spawn a real drain that drains the buffer and the drop would
// not fire). We consume the sync.Once ourselves with an empty func, so
// ensurePersistDrain's persistStart.Do(...) becomes a true no-op and no
// real drainPersist goroutine ever runs. persistStarted is kept in sync
// for Close()'s wait logic.
func TestBroadcaster_Publish_CountsDroppedPersistEvent(t *testing.T) {
	b := NewBroadcaster()
	b.DisableGUIEventLog = false
	// Replace persistCh with a cap-1 buffer and FIRE the sync.Once now
	// with a no-op so ensurePersistDrain() never spawns a consumer.
	// persistStarted stays false, so Close() (in cleanup) closes
	// persistDoneCh itself without a 3s wait on a drain that never ran.
	b.persistCh = make(chan persistRequest, 1)
	b.persistStart.Do(func() {})
	// The first dropped Publish lazily spawns the out-of-band drop
	// reporter; Close() in cleanup stops + joins it (no goroutine leak).
	t.Cleanup(b.Close)

	b.Publish(Event{Type: "daemon-state", Body: map[string]any{"i": 0}}) // fills the cap-1 buffer
	b.Publish(Event{Type: "daemon-state", Body: map[string]any{"i": 1}}) // must drop: buffer full, nobody draining

	_, persist := b.DroppedCounts()
	if persist == 0 {
		t.Fatalf("DroppedCounts().persist = 0, want > 0 after publishing past a saturated, undrained persistCh")
	}
}

// TestBroadcaster_DropReporter_EmitsWarnOutOfBand proves the PR #476 bot
// P2 fix: the drop warn is emitted by the out-of-band reporter
// goroutine reading the atomic counters, NOT synchronously on the
// Publish hot path. Drives runDropReporter with a tiny interval against
// a per-test-isolated hub-mcp.log (so a sibling test's row in the
// binary-shared log can't be mistaken for this one's) and asserts a
// `gui-events-dropped` warn lands carrying the counter totals.
func TestBroadcaster_DropReporter_EmitsWarnOutOfBand(t *testing.T) {
	root := t.TempDir()
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	b := NewBroadcaster()
	// Simulate observed drops WITHOUT going through Publish, so the test
	// targets exactly the reporter's read-counters-and-warn path.
	b.sseDropped.Store(3)
	b.persistDropped.Store(2)

	done := make(chan struct{})
	go func() {
		b.runDropReporter(10 * time.Millisecond)
		close(done)
	}()

	logPath := filepath.Join(root, "hub-mcp.log")
	deadline := time.Now().Add(3 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(logPath); err == nil &&
			strings.Contains(string(data), "gui-events-dropped") &&
			strings.Contains(string(data), `"sse_dropped_total":3`) &&
			strings.Contains(string(data), `"persist_dropped_total":2`) {
			found = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	// Stop the reporter (closeOnce-protected close of dropReporterStop)
	// and join it so the goroutine doesn't outlive the test.
	close(b.dropReporterStop)
	<-done
	if !found {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("out-of-band reporter never emitted a gui-events-dropped warn with the expected totals; hub-mcp.log=%q", data)
	}
}

func TestEventsSSE_StreamsPublishedEvents(t *testing.T) {
	s := newEphemeralServer(t, Config{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		s.Broadcaster().Publish(Event{Type: "daemon-state", Body: map[string]any{"server": "memory"}})
	}()

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	// Bound the test: give the handler a short-lived context via httptest request.
	ctx, cancel := context.WithTimeout(req.Context(), 800*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	rec := newSSERecorder()
	done := make(chan struct{})
	go func() {
		s.mux.ServeHTTP(rec, req)
		close(done)
	}()
	// Read the SSE output until we see an event or the handler returns.
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(rec.body(), "event: daemon-state") {
			cancel()
			<-done
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatalf("never saw event in stream; body: %q", rec.body())
}
