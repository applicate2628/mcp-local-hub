package gui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBroadcaster_SubscribeReceivesPublishedEvent(t *testing.T) {
	b := NewBroadcaster()
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
	b := NewBroadcaster()
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

func TestEventsSSE_StreamsPublishedEvents(t *testing.T) {
	s := NewServer(Config{})
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
