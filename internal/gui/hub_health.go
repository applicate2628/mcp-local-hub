package gui

import (
	"encoding/json"
	"net/http"
	"sync"
)

// HubHealthState is the honest state of the gate-ON hub-aggregate listener, as
// surfaced to the GUI. Productization Phase-0 item 1: the health watcher +
// restart driver already compute these transitions but only log them to
// hub-mcp.log, so a hung/dead/exhausted hub silently killed all aggregated MCP
// traffic while the Dashboard painted every daemon card green. This state is
// published to the SSE bus so the Dashboard + Groups can render honest status.
type HubHealthState string

const (
	// HubHealthHealthy — the aggregate listener is responsive (or was restarted
	// successfully with its instance_id preserved).
	HubHealthHealthy HubHealthState = "healthy"
	// HubHealthRecovering — the watcher declared the listener unresponsive and an
	// auto-restart is in flight (or a restart attempt failed and is retrying).
	HubHealthRecovering HubHealthState = "recovering"
	// HubHealthNeedsReconcile — the listener restarted successfully on a NEW
	// instance_id, so the hub is serving but gated client configs need
	// `mcphub install --reconcile-hub-mode`. Operator action required.
	HubHealthNeedsReconcile HubHealthState = "needs-reconcile"
	// HubHealthDown — auto-recovery gave up; the aggregate is not serving and will
	// not self-heal.
	HubHealthDown HubHealthState = "down"
)

// hubReconcileOperatorAction is the command that repoints gated client configs
// at the live hub port/instance after an instance-id change (mirrors the
// operator_action the restart driver already writes into its log fields).
const hubReconcileOperatorAction = "mcphub install --reconcile-hub-mode"

// hubHealthDegraded reports whether the Dashboard should show a health banner.
func hubHealthDegraded(state HubHealthState) bool {
	return state == HubHealthRecovering || state == HubHealthNeedsReconcile || state == HubHealthDown
}

// hubHealthServing reports whether the aggregate is currently serving clients.
func hubHealthServing(state HubHealthState) bool {
	return state == HubHealthHealthy || state == HubHealthNeedsReconcile
}

// hubHealthTracker is the single owner of the GUI's hub-aggregate health surface.
// The health watcher (unresponsive/recovered) and the restart driver
// (recovering/needs-reconcile/healthy/down) feed it; it publishes a `hub-health`
// SSE event on every state CHANGE so the frontend renders honest status.
type hubHealthTracker struct {
	mu     sync.Mutex
	state  HubHealthState
	action string // operator-action hint for needs-reconcile (empty otherwise)
	pub    func(Event)
}

func newHubHealthTracker(pub func(Event)) *hubHealthTracker {
	return &hubHealthTracker{state: HubHealthHealthy, pub: pub}
}

// set transitions the state and publishes a `hub-health` SSE event ONLY on a
// change (dedup), so a steady state never spams the bus.
func (h *hubHealthTracker) set(state HubHealthState, action string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := h.state != state || h.action != action
	h.state = state
	h.action = action
	if changed && h.pub != nil {
		h.pub(Event{Type: "hub-health", Body: map[string]any{
			"state":           string(state),
			"operator_action": action,
			"degraded":        hubHealthDegraded(state),
		}})
	}
}

// snapshot returns the current state + action (for the GET initial-load route).
func (h *hubHealthTracker) snapshot() (HubHealthState, string) {
	if h == nil {
		return HubHealthHealthy, ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state, h.action
}

// onHubListenerRecovered is the watcher recovery hook → healthy.
func (s *Server) onHubListenerRecovered() { s.hubHealth.set(HubHealthHealthy, "") }

// hubHealthDTO is the GET /api/hub/health body (initial load; SSE `hub-health`
// events push subsequent transitions).
type hubHealthDTO struct {
	State          string `json:"state"`
	OperatorAction string `json:"operator_action,omitempty"`
	Degraded       bool   `json:"degraded"`
}

func registerHubHealthRoutes(s *Server) {
	s.mux.HandleFunc("/api/hub/health", s.requireSameOrigin(s.hubHealthHandler))
}

func (s *Server) hubHealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	state, action := s.hubHealth.snapshot()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(hubHealthDTO{
		State:          string(state),
		OperatorAction: action,
		Degraded:       hubHealthDegraded(state),
	})
}

// hubHealthEmitWrapper wraps the restart driver's emitFn so hub-health-relevant
// event names drive the tracker state, then delegates to the underlying log emit
// (so the hub-mcp.log audit trail is unchanged).
func (s *Server) hubHealthEmitWrapper(base func(level, event string, fields map[string]any) error) func(string, string, map[string]any) error {
	return func(level, event string, fields map[string]any) error {
		switch event {
		case "hub-listener-restarted":
			s.hubHealth.set(HubHealthHealthy, "")
		case "hub-listener-restart-failed":
			s.hubHealth.set(HubHealthRecovering, "")
		case "hub-listener-restart-exhausted":
			if retryScheduled, ok := fields["no_signal_retry_scheduled"].(bool); ok && retryScheduled {
				s.hubHealth.set(HubHealthRecovering, "")
			} else {
				s.hubHealth.set(HubHealthDown, "")
			}
		case "hub-listener-restart-instance-id-changed":
			s.hubHealth.set(HubHealthNeedsReconcile, hubReconcileOperatorAction)
		case "hub-listener-restart-abandoned":
			s.hubHealth.set(HubHealthDown, "")
		}
		if base == nil {
			return nil
		}
		return base(level, event, fields)
	}
}
