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

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

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

	// G2 health snapshot cache + test seams. See internal/api/health.go for
	// the cache lifecycle, TTL constants, and singleflight collapsing logic.
	healthCache healthCache

	// HealthStatusFn is the test seam used by HealthSnapshot to read the
	// daemons section. Production: nil → falls back to a.StatusWithOpts.
	// Tests overwrite this to inject deterministic []DaemonStatus.
	HealthStatusFn func(StatusOpts) ([]DaemonStatus, error)

	// HealthCapabilityFn is the test seam used by HealthSnapshot to project
	// one daemon row into a CapabilityRow. Production: nil → falls back to
	// a.realCapabilityRow (live MCP roundtrip / synthetic-catalog answer).
	// Tests overwrite this to inject deterministic CapabilityRow values
	// without spinning up a real MCP backend.
	HealthCapabilityFn func(DaemonStatus) (CapabilityRow, error)

	// HealthNowMs is the test seam used by HealthSnapshot to read the
	// current time in milliseconds. Production: nil → time.Now().UnixMilli().
	// Tests advance this manually to drive TTL boundaries deterministically.
	HealthNowMs func() int64

	// StartedAt is the API's process-start timestamp. Surfaced as
	// HubSection.StartedAt in HealthSnapshot's hub section.
	StartedAt time.Time
}

// healthCache holds per-section cache state for HealthSnapshot. Embedded
// in API. Mutex guards cached values; singleflight collapses concurrent
// refreshes per section so N callers waiting on an expired cache trigger
// exactly one underlying call. The hub section uses sync.Once instead of
// the TTL machinery because it's immutable across process lifetime.
type healthCache struct {
	mu sync.RWMutex
	sf singleflight.Group

	hubOnce sync.Once
	hub     HubSection

	daemonsAt int64 // ms since epoch when last computed
	daemons   DaemonsSection
	// daemonsErr caches the last fetch error (nil on success). Read on
	// the cache-hit fast path AND inner re-check so /api/status and
	// /api/health stay fail-loud while the backend is down — the cached
	// section alone (with section.Errors[] populated) is not enough,
	// because the function's second return value drives the HTTP-status
	// gate in cmd/mcphub. Cloud bot P1×2 fix on PR #132 commit 2062818.
	daemonsErr error
	// daemonStatuses caches the canonical []DaemonStatus rows produced
	// by the same underlying StatusWithOpts call that fed `daemons`.
	// /api/status (DaemonStatusSnapshot) reads from this slot; /api/health
	// reads the projected `daemons` (DaemonRow) form. One fetch serves
	// both surfaces — Phase 6 of G2 per
	// docs/superpowers/specs/2026-05-07-g2-unified-health-endpoint-design.md.
	//
	// Field order: keep canonical (DaemonStatus) above projected (DaemonRow)
	// so future readers see "rich source -> thin view" rather than the
	// reverse.
	daemonStatuses []DaemonStatus

	probesAt int64
	probes   ProbesSection
	// probesErr caches the last probe-fetch error (nil on success).
	// Symmetric with daemonsErr — required so /api/health?include=probes
	// stays 500 HEALTH_BACKEND_FAILED on cache-hit retries while probe
	// backend is still down. Cloud bot P1×2 fix on PR #132 commit 2062818.
	probesErr error

	capabilitiesAt int64
	capabilities   CapabilitiesSection
}

// NewAPI constructs a fresh API with an initialized state and event bus.
// Cheap: allocates the State struct + daemon map + EventBus struct header,
// no background resources. See the API type doc for the per-process vs
// per-request lifecycle caveat.
func NewAPI() *API {
	return &API{
		state:     &State{Daemons: make(map[string]DaemonStatus)},
		bus:       newEventBus(),
		StartedAt: time.Now(),
	}
}
