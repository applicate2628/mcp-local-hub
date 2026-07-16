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
	// Transition with an operator action → publishes + carries the action.
	s.hubHealth.set(HubHealthNeedsReconcile, hubReconcileOperatorAction)
	ev := recvHubHealth(t, ch)
	if ev.Body["state"] != "needs-reconcile" || ev.Body["operator_action"] != hubReconcileOperatorAction {
		t.Fatalf("ev.Body=%+v, want needs-reconcile + reconcile action", ev.Body)
	}
}

// The emitFn wrapper maps the restart-driver event names to hub-health states and
// still delegates to the underlying log emit.
func TestHubHealthEmitWrapperMapsRestartEvents(t *testing.T) {
	cases := []struct {
		event string
		want  HubHealthState
	}{
		{"hub-listener-restart-failed", HubHealthRecovering},
		{"hub-listener-restart-exhausted", HubHealthNeedsReconcile},
		{"hub-listener-restart-instance-id-changed", HubHealthNeedsReconcile},
		{"hub-listener-restart-abandoned", HubHealthDown},
		{"hub-listener-restarted", HubHealthHealthy},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			s := NewServer(Config{})
			s.hubHealth.set(HubHealthDown, "seed") // seed a distinct state so the map is a change
			delegated := false
			wrap := s.hubHealthEmitWrapper(func(level, event string, fields map[string]any) error {
				delegated = true
				return nil
			})
			_ = wrap("warn", tc.event, nil)
			if got, _ := s.hubHealth.snapshot(); got != tc.want {
				t.Errorf("event %q → state %q, want %q", tc.event, got, tc.want)
			}
			if !delegated {
				t.Errorf("wrapper did not delegate to the base emit for %q", tc.event)
			}
		})
	}
}

// GET /api/hub/health returns the current state + degraded flag for initial load.
func TestHubHealthGetReturnsCurrentState(t *testing.T) {
	s := NewServer(Config{})
	s.hubHealth.set(HubHealthNeedsReconcile, hubReconcileOperatorAction)

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

	// Healthy state → not degraded.
	s.hubHealth.set(HubHealthHealthy, "")
	rec2 := httptest.NewRecorder()
	s.mux.ServeHTTP(rec2, req)
	var dto2 hubHealthDTO
	_ = json.NewDecoder(rec2.Body).Decode(&dto2)
	if dto2.State != "healthy" || dto2.Degraded {
		t.Fatalf("dto2=%+v, want healthy + not degraded", dto2)
	}
}
