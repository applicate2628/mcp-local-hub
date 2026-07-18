package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func TestServerHubHealthRestoresDurableReconcilePendingOnFreshStartup(t *testing.T) {
	setupInitialHubPortDependencyTest(t)
	ep, err := api.EnsureHubEndpoint(3439, 111)
	if err != nil {
		t.Fatalf("EnsureHubEndpoint: %v", err)
	}
	rotated, err := api.RotateHubInstanceID()
	if err != nil {
		t.Fatalf("RotateHubInstanceID: %v", err)
	}
	if rotated.InstanceID == ep.InstanceID {
		t.Fatalf("InstanceID did not rotate: %q", ep.InstanceID)
	}

	startFromEndpoint := func(context.Context, bool, *api.API, startHubMcpListenerOptions) (*HubListenerComponents, error) {
		current, err := api.LoadHubEndpoint()
		if err != nil {
			return nil, err
		}
		comp := liveRestartTestComp(current.Port)
		comp.reconcilePending = current.ReconcilePending
		return comp, nil
	}

	s := NewServer(Config{Port: 0})
	s.hubEndpointGateFn = func(*api.API) bool { return true }
	s.startHubMcpListenerFn = startFromEndpoint

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	startDone := make(chan error, 1)
	go func() { startDone <- s.Start(ctx, ready) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		cancel()
		select {
		case <-startDone:
		case <-time.After(2 * time.Second):
			t.Error("Server.Start did not stop after cancellation")
		}
		stopped = true
	}
	defer stop()

	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("Server.Start did not signal GUI readiness")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		state, action := s.hubHealth.snapshot()
		if state == HubHealthNeedsReconcile && action == hubReconcileOperatorAction {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fresh-start health = state %q action %q, want needs-reconcile + action", state, action)
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop() // simulate shutdown after the persisted rotation, before reconciliation

	lk, err := api.AcquireHubMcpLock()
	if err != nil {
		t.Fatalf("AcquireHubMcpLock: %v", err)
	}
	if err := api.ClearHubReconcilePendingLocked(); err != nil {
		_ = lk.Unlock()
		t.Fatalf("ClearHubReconcilePendingLocked: %v", err)
	}
	if err := lk.Unlock(); err != nil {
		t.Fatalf("unlock hub-mcp.lock: %v", err)
	}

	next := NewServer(Config{Port: 0})
	next.hubEndpointGateFn = func(*api.API) bool { return true }
	next.startHubMcpListenerFn = startFromEndpoint
	nextCtx, nextCancel := context.WithCancel(context.Background())
	nextReady := make(chan struct{})
	nextDone := make(chan error, 1)
	go func() { nextDone <- next.Start(nextCtx, nextReady) }()
	defer func() {
		nextCancel()
		select {
		case <-nextDone:
		case <-time.After(2 * time.Second):
			t.Error("subsequent Server.Start did not stop after cancellation")
		}
	}()
	select {
	case <-nextReady:
	case <-time.After(2 * time.Second):
		t.Fatal("subsequent Server.Start did not signal GUI readiness")
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		state, action := next.hubHealth.snapshot()
		if state == HubHealthHealthy && action == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("post-clear startup health = state %q action %q, want healthy without reconcile", state, action)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func recvHubHealth(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	for {
		select {
		case ev := <-ch:
			if ev.Type == "hub-health" {
				return ev
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no hub-health event published")
			return Event{} // unreachable after Fatal, but Go's flow analysis needs it
		}
	}
}

// The tracker publishes a hub-health SSE event on every state CHANGE and dedups a
// repeat of the same state (so a steady state never spams the bus).
func TestHubHealthTrackerPublishesOnChangeAndDedups(t *testing.T) {
	s := NewServer(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.Broadcaster().Subscribe(ctx)

	s.hubHealth.set(HubHealthRecovering, "")
	if ev := recvHubHealth(t, ch); ev.Body["state"] != "recovering" {
		t.Fatalf("state=%v, want recovering", ev.Body["state"])
	}
	// Same state again → NO publish.
	s.hubHealth.set(HubHealthRecovering, "")
	select {
	case ev := <-ch:
		t.Fatalf("duplicate set published an event: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
	// Return to healthy, then record pending reconciliation. The latter
	// transition publishes + carries the operator action.
	s.hubHealth.markHealthy()
	if ev := recvHubHealth(t, ch); ev.Body["state"] != "healthy" {
		t.Fatalf("state=%v, want healthy", ev.Body["state"])
	}
	s.hubHealth.markReconcilePending()
	ev := recvHubHealth(t, ch)
	if ev.Body["state"] != "needs-reconcile" || ev.Body["operator_action"] != hubReconcileOperatorAction {
		t.Fatalf("ev.Body=%+v, want needs-reconcile + reconcile action", ev.Body)
	}
}

func TestHubHealthTrackerClearReconcilePendingIfResolved(t *testing.T) {
	t.Run("disk clear downgrades needs-reconcile and publishes", func(t *testing.T) {
		var events []Event
		h := newHubHealthTracker(func(ev Event) { events = append(events, ev) })
		h.markReconcilePending()
		events = nil

		h.clearReconcilePendingIfResolved(false)

		if got, action := h.snapshot(); got != HubHealthHealthy || action != "" {
			t.Fatalf("health = state %q action %q, want healthy without action", got, action)
		}
		if len(events) != 1 || events[0].Body["state"] != string(HubHealthHealthy) {
			t.Fatalf("events = %+v, want one healthy hub-health event", events)
		}
	})

	t.Run("pending disk marker is a no-op", func(t *testing.T) {
		var events []Event
		h := newHubHealthTracker(func(ev Event) { events = append(events, ev) })
		h.markReconcilePending()
		events = nil

		h.clearReconcilePendingIfResolved(true)

		if got, action := h.snapshot(); got != HubHealthNeedsReconcile || action != hubReconcileOperatorAction {
			t.Fatalf("health = state %q action %q, want needs-reconcile + action", got, action)
		}
		if len(events) != 0 {
			t.Fatalf("events = %+v, want no publish", events)
		}
	})

	t.Run("disk clear does not override down", func(t *testing.T) {
		var events []Event
		h := newHubHealthTracker(func(ev Event) { events = append(events, ev) })
		h.markReconcilePending()
		h.set(HubHealthDown, "")
		events = nil

		h.clearReconcilePendingIfResolved(false)

		if got, action := h.snapshot(); got != HubHealthDown || action != "" {
			t.Fatalf("health = state %q action %q, want down without action", got, action)
		}
		if len(events) != 0 {
			t.Fatalf("events = %+v, want no publish", events)
		}
		h.mu.Lock()
		pending := h.reconcilePending
		h.mu.Unlock()
		if pending {
			t.Fatal("reconcilePending = true after disk clear, want false")
		}
	})
}

func TestHubHealthRestartedEventPreservesNeedsReconcile(t *testing.T) {
	t.Run("pending reconcile survives an outage", func(t *testing.T) {
		s := NewServer(Config{})
		wrap := s.hubHealthEmitWrapper(nil)
		s.hubHealth.markReconcilePending()
		_ = wrap("error", "hub-listener-restart-failed", nil)
		if got, action := s.hubHealth.snapshot(); got != HubHealthRecovering || action != "" {
			t.Fatalf("restart-failed = state %q action %q, want recovering", got, action)
		}
		_ = wrap("info", "hub-listener-restarted", nil)
		if got, action := s.hubHealth.snapshot(); got != HubHealthNeedsReconcile || action != hubReconcileOperatorAction {
			t.Fatalf("restarted = state %q action %q, want needs-reconcile + action", got, action)
		}
	})

	t.Run("plain recovering without pending reconcile becomes healthy", func(t *testing.T) {
		s := NewServer(Config{})
		s.hubHealth.set(HubHealthRecovering, "")
		_ = s.hubHealthEmitWrapper(nil)("info", "hub-listener-restarted", nil)
		if got, action := s.hubHealth.snapshot(); got != HubHealthHealthy || action != "" {
			t.Fatalf("restarted = state %q action %q, want healthy", got, action)
		}
	})
}

func TestHubHealthInstanceIDChangeFromRecoveringPublishesNeedsReconcile(t *testing.T) {
	s := NewServer(Config{})
	s.hubHealth.set(HubHealthRecovering, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.Broadcaster().Subscribe(ctx)

	_ = s.hubHealthEmitWrapper(nil)("error", "hub-listener-restart-instance-id-changed", nil)
	if got, action := s.hubHealth.snapshot(); got != HubHealthNeedsReconcile || action != hubReconcileOperatorAction {
		t.Fatalf("instance-id-changed = state %q action %q, want needs-reconcile + action", got, action)
	}
	ev := recvHubHealth(t, ch)
	if ev.Body["state"] != "needs-reconcile" || ev.Body["operator_action"] != hubReconcileOperatorAction {
		t.Fatalf("ev.Body=%+v, want published needs-reconcile + reconcile action", ev.Body)
	}
}

func TestHubHealthInstanceIDChangeWhileDownDefersReconcileUntilRecovery(t *testing.T) {
	s := NewServer(Config{})
	wrap := s.hubHealthEmitWrapper(nil)
	s.hubHealth.set(HubHealthDown, "")

	_ = wrap("error", "hub-listener-restart-instance-id-changed", nil)
	if got, action := s.hubHealth.snapshot(); got != HubHealthDown || action != "" {
		t.Fatalf("instance-id-changed while down = state %q action %q, want down with no action", got, action)
	}

	_ = wrap("info", "hub-listener-restarted", nil)
	if got, action := s.hubHealth.snapshot(); got != HubHealthNeedsReconcile || action != hubReconcileOperatorAction {
		t.Fatalf("restarted = state %q action %q, want needs-reconcile + action", got, action)
	}
}

// The emitFn wrapper maps the restart-driver event names to hub-health states and
// still delegates to the underlying log emit.
func TestHubHealthEmitWrapperMapsRestartEvents(t *testing.T) {
	cases := []struct {
		name       string
		event      string
		fields     map[string]any
		want       HubHealthState
		wantAction string
	}{
		{name: "restart failed", event: "hub-listener-restart-failed", want: HubHealthRecovering},
		{name: "restart exhausted", event: "hub-listener-restart-exhausted", want: HubHealthDown},
		{
			name:   "restart exhausted with retry scheduled",
			event:  "hub-listener-restart-exhausted",
			fields: map[string]any{"no_signal_retry_scheduled": true},
			want:   HubHealthRecovering,
		},
		{
			name:   "restart exhausted with wrong typed retry field",
			event:  "hub-listener-restart-exhausted",
			fields: map[string]any{"no_signal_retry_scheduled": "true"},
			want:   HubHealthDown,
		},
		{
			name:       "instance id changed",
			event:      "hub-listener-restart-instance-id-changed",
			want:       HubHealthNeedsReconcile,
			wantAction: hubReconcileOperatorAction,
		},
		{name: "restart abandoned", event: "hub-listener-restart-abandoned", want: HubHealthDown},
		{name: "restarted", event: "hub-listener-restarted", want: HubHealthHealthy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Config{})
			if tc.event == "hub-listener-restart-instance-id-changed" {
				s.hubHealth.set(HubHealthRecovering, "")
			} else {
				s.hubHealth.set(HubHealthDown, "seed") // seed a distinct state so the map is a change
			}
			delegated := false
			wrap := s.hubHealthEmitWrapper(func(level, event string, fields map[string]any) error {
				delegated = true
				return nil
			})
			_ = wrap("warn", tc.event, tc.fields)
			if got, action := s.hubHealth.snapshot(); got != tc.want || action != tc.wantAction {
				t.Errorf("event %q → state %q action %q, want %q action %q", tc.event, got, action, tc.want, tc.wantAction)
			}
			if !delegated {
				t.Errorf("wrapper did not delegate to the base emit for %q", tc.event)
			}
		})
	}
}

func TestHubHealthServing(t *testing.T) {
	cases := []struct {
		state HubHealthState
		want  bool
	}{
		{HubHealthHealthy, true},
		{HubHealthNeedsReconcile, true},
		{HubHealthRecovering, false},
		{HubHealthDown, false},
	}
	for _, tc := range cases {
		if got := hubHealthServing(tc.state); got != tc.want {
			t.Errorf("hubHealthServing(%q)=%v, want %v", tc.state, got, tc.want)
		}
	}
}

// GET /api/hub/health returns the current state + degraded flag for initial load.
func TestHubHealthGetReturnsCurrentState(t *testing.T) {
	s := NewServer(Config{})
	s.hubHealth.markReconcilePending()

	req := httptest.NewRequest(http.MethodGet, "/api/hub/health", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var dto hubHealthDTO
	if err := json.NewDecoder(rec.Body).Decode(&dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.State != "needs-reconcile" || !dto.Degraded || dto.OperatorAction != hubReconcileOperatorAction {
		t.Fatalf("dto=%+v, want needs-reconcile + degraded + reconcile action", dto)
	}

	// A fresh process starts healthy and not degraded.
	s = NewServer(Config{})
	rec2 := httptest.NewRecorder()
	s.mux.ServeHTTP(rec2, req)
	var dto2 hubHealthDTO
	_ = json.NewDecoder(rec2.Body).Decode(&dto2)
	if dto2.State != "healthy" || dto2.Degraded {
		t.Fatalf("dto2=%+v, want healthy + not degraded", dto2)
	}
}

func TestServerHubHealthTracksInitialStartup(t *testing.T) {
	for _, tc := range []struct {
		name          string
		enabled       bool
		startErr      error
		deadOnArrival bool
		pendingWant   HubHealthState
		finalWant     HubHealthState
	}{
		{
			name:        "gate on success",
			enabled:     true,
			pendingWant: HubHealthRecovering,
			finalWant:   HubHealthHealthy,
		},
		{
			name:          "gate on dead-on-arrival component",
			enabled:       true,
			deadOnArrival: true,
			pendingWant:   HubHealthRecovering,
			finalWant:     HubHealthDown,
		},
		{
			name:        "gate off stays inert",
			enabled:     false,
			pendingWant: HubHealthHealthy,
			finalWant:   HubHealthHealthy,
		},
		{
			name:        "gate on first failure recovers through driver",
			enabled:     true,
			startErr:    errors.New("injected hub startup failure"),
			pendingWant: HubHealthRecovering,
			finalWant:   HubHealthHealthy,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Config{Port: 0})
			s.hubEndpointGateFn = func(*api.API) bool { return tc.enabled }
			started := make(chan struct{})
			release := make(chan struct{})
			attempts := 0
			s.startHubMcpListenerFn = func(ctx context.Context, enabled bool, _ *api.API, _ startHubMcpListenerOptions) (*HubListenerComponents, error) {
				attempts++
				if enabled != tc.enabled {
					t.Errorf("startup enabled = %v, want %v", enabled, tc.enabled)
				}
				if attempts == 1 {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				if tc.startErr != nil && attempts == 1 {
					return nil, tc.startErr
				}
				if !enabled {
					return nil, nil
				}
				comp := liveRestartTestComp(9201)
				if tc.deadOnArrival {
					comp.alive.Store(false)
					s.hubHealth.set(HubHealthDown, "")
				}
				return comp, nil
			}

			ctx, cancel := context.WithCancel(context.Background())
			ready := make(chan struct{})
			startDone := make(chan error, 1)
			go func() { startDone <- s.Start(ctx, ready) }()
			defer func() {
				cancel()
				select {
				case <-startDone:
				case <-time.After(2 * time.Second):
					t.Error("Server.Start did not stop after cancellation")
				}
			}()

			select {
			case <-ready:
			case <-time.After(2 * time.Second):
				t.Fatal("Server.Start did not signal GUI readiness")
			}
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("hub startup seam was not entered")
			}
			if got, action := s.hubHealth.snapshot(); got != tc.pendingWant || action != "" {
				t.Fatalf("pending health = state %q action %q, want %q", got, action, tc.pendingWant)
			}

			close(release)
			deadline := time.Now().Add(2 * time.Second)
			for {
				got, action := s.hubHealth.snapshot()
				if got == tc.finalWant && action == "" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("final health = state %q action %q, want %q", got, action, tc.finalWant)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}
