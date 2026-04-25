// Package api is the single source of truth for operations exposed through
// the mcp-local-hub CLI and GUI frontends. Every command the user runs (via
// cobra) or every HTTP endpoint the GUI calls dispatches into one function
// here; they never reach directly into internal/clients, internal/scheduler,
// internal/config, or internal/secrets.
//
// This keeps CLI and GUI from skipping layers: capabilities live in api so
// both frontends can reach them without bypassing validation, backup, or
// audit logic. NOTE: not every api function has a CLI command today —
// api.Demigrate is wired into the GUI (/api/demigrate) but has no mcphub
// subcommand; adding one is a separate follow-up.
package api

// API is the orchestration handle held by cli and gui. Methods are safe for
// concurrent use unless noted otherwise.
//
// Lifecycle: the CLI layer creates one API per process via NewAPI. The GUI
// layer currently constructs a fresh API inside every real adapter (see
// internal/gui/server.go's realManifestCreator / realManifestGetter /
// realManifestEditor / realManifestValidator / realDemigrater). This is
// safe today because newEventBus (internal/api/events.go) returns an empty
// struct — no goroutine, no background resource. When EventBus is populated
// (Task 22 per events.go's source comment), the GUI adapters should be
// refactored to share one API handle via the Server struct.
type API struct {
	state *State
	bus   *EventBus
}

// NewAPI constructs a fresh API with an initialized state and event bus.
// Cheap: allocates the State struct + daemon map + EventBus struct header,
// no background resources. See the API type doc for the per-process vs
// per-request lifecycle caveat.
func NewAPI() *API {
	return &API{
		state: &State{Daemons: make(map[string]DaemonStatus)},
		bus:   newEventBus(),
	}
}
