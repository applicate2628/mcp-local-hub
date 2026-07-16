package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
