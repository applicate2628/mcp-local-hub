// hub_listener_health_watcher_test.go — B1 footgun: hub-listener-hang
// observability tests. Pure: dial + emit seams are injected, so no real
// socket is bound and no event hits hub-mcp.log.

package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

type capturedEvent struct {
	level  string
	event  string
	fields map[string]any
}

// newTestHealthWatcher builds a watcher with injected dial + emit seams.
func newTestHealthWatcher(port int) (*HubListenerHealthWatcher, *[]capturedEvent, *func(error)) {
	var events []capturedEvent
	// dialErr is swapped by the test to steer the dial outcome.
	var dialErr error
	setDial := func(e error) { dialErr = e }

	w := NewHubListenerHealthWatcher(port, time.Hour) // interval irrelevant; tests drive probeOnce directly
	w.dialFn = func(ctx context.Context, network, addr string, timeout time.Duration) error {
		return dialErr
	}
	w.emit = func(level, event string, fields map[string]any) error {
		events = append(events, capturedEvent{level: level, event: event, fields: fields})
		return nil
	}
	return w, &events, &setDial
}

// TestHealthWatcherWarnsAfterConsecutiveFailures pins B1: the warn fires
// exactly once, only after hubHealthUnresponsiveThreshold consecutive
// failed dials (a single transient failure must NOT warn).
func TestHealthWatcherWarnsAfterConsecutiveFailures(t *testing.T) {
	w, events, setDial := newTestHealthWatcher(3439)
	ctx := context.Background()
	fail := errors.New("connection refused")

	// One failure: below threshold, no event.
	(*setDial)(fail)
	w.probeOnce(ctx)
	if len(*events) != 0 {
		t.Fatalf("after 1 failure: got %d events, want 0 (below threshold)", len(*events))
	}

	// Reach the threshold (default 3): warn fires once.
	w.probeOnce(ctx)
	w.probeOnce(ctx)
	if len(*events) != 1 {
		t.Fatalf("at threshold: got %d events, want 1", len(*events))
	}
	ev := (*events)[0]
	if ev.level != "warn" || ev.event != "hub-listener-unresponsive" {
		t.Errorf("event = %s/%s, want warn/hub-listener-unresponsive", ev.level, ev.event)
	}
	if ev.fields["port"] != 3439 {
		t.Errorf("event port = %v, want 3439", ev.fields["port"])
	}

	// More failures while already unresponsive: no duplicate warn.
	w.probeOnce(ctx)
	w.probeOnce(ctx)
	if len(*events) != 1 {
		t.Errorf("warn duplicated while already unresponsive: got %d events, want 1", len(*events))
	}
}

// TestHealthWatcherRecoveryEvent pins the recovery transition: after a
// warn, a successful dial emits exactly one info recovery event, and the
// failure counter resets so a subsequent outage warns again.
func TestHealthWatcherRecoveryEvent(t *testing.T) {
	w, events, setDial := newTestHealthWatcher(3439)
	ctx := context.Background()
	fail := errors.New("connection refused")

	// Drive into the unresponsive state.
	(*setDial)(fail)
	for i := 0; i < hubHealthUnresponsiveThreshold; i++ {
		w.probeOnce(ctx)
	}
	if len(*events) != 1 {
		t.Fatalf("setup: want 1 warn, got %d", len(*events))
	}

	// Recover: one successful dial → recovery info.
	(*setDial)(nil)
	w.probeOnce(ctx)
	if len(*events) != 2 {
		t.Fatalf("after recovery: got %d events, want 2", len(*events))
	}
	rec := (*events)[1]
	if rec.level != "info" || rec.event != "hub-listener-probe-recovered" {
		t.Errorf("recovery event = %s/%s, want info/hub-listener-probe-recovered", rec.level, rec.event)
	}

	// A fresh outage warns again (state was reset on recovery).
	(*setDial)(fail)
	for i := 0; i < hubHealthUnresponsiveThreshold; i++ {
		w.probeOnce(ctx)
	}
	if len(*events) != 3 {
		t.Errorf("second outage did not re-warn: got %d events, want 3", len(*events))
	}
}

// TestHealthWatcherHealthyNeverEmits pins the steady-state: a reachable
// listener emits nothing across many probes.
func TestHealthWatcherHealthyNeverEmits(t *testing.T) {
	w, events, setDial := newTestHealthWatcher(3439)
	ctx := context.Background()
	(*setDial)(nil)
	for i := 0; i < 10; i++ {
		w.probeOnce(ctx)
	}
	if len(*events) != 0 {
		t.Errorf("healthy listener emitted %d events, want 0", len(*events))
	}
}

// TestHealthWatcherCancelledCtxNotCountedAsOutage pins the teardown
// guard: a dial failure observed while ctx is already cancelled (the
// shutdown path) must NOT count as an outage or emit a warn.
func TestHealthWatcherCancelledCtxNotCountedAsOutage(t *testing.T) {
	w, events, setDial := newTestHealthWatcher(3439)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before probe

	(*setDial)(errors.New("use of closed network connection"))
	for i := 0; i < hubHealthUnresponsiveThreshold+2; i++ {
		w.probeOnce(ctx)
	}
	if len(*events) != 0 {
		t.Errorf("cancelled-ctx dial failures emitted %d events, want 0", len(*events))
	}
	if w.consecutiveFailures != 0 {
		t.Errorf("cancelled-ctx dial mutated consecutiveFailures = %d, want 0", w.consecutiveFailures)
	}
}
