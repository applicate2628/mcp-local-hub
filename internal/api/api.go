// Package api is the single source of truth for operations exposed through
// the mcp-local-hub CLI and GUI frontends. Every command the user runs (via
// cobra) or every HTTP endpoint the GUI calls dispatches into one function
// here; they never reach directly into internal/clients, internal/scheduler,
// internal/config, or internal/secrets.
//
// This enforces CLI ≡ UI parity structurally: if a capability is in api, it
// is reachable from both frontends by construction; if it is not, neither
// frontend can expose it.
package api

// API is the orchestration handle held by cli and gui. A single instance is
// created per process via NewAPI. Methods are safe for concurrent use unless
// noted otherwise.
type API struct {
	state *State
	bus   *EventBus
}

// NewAPI constructs a fresh API with an initialized state and event bus.
func NewAPI() *API {
	return &API{
		state: &State{Daemons: make(map[string]DaemonStatus)},
		bus:   newEventBus(),
	}
}
