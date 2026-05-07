package api

import (
	"time"

	"mcp-local-hub/internal/buildinfo"
)

// HealthSnapshot is the canonical snapshot returned by GET /api/health.
// Owns the contract G3 (capability display) and G4 (Hub MCP routing)
// consume. Per spec docs/superpowers/specs/2026-05-07-g2-unified-health-endpoint-design.md.
//
// Probes and Capabilities are pointers so they're omitted from JSON when
// the caller didn't opt in via HealthOpts.IncludeProbes /
// IncludeCapabilities — keeps the cheap default response cheap.
type HealthSnapshot struct {
	SchemaVersion string               `json:"schema_version"`
	Hub           HubSection           `json:"hub"`
	Daemons       DaemonsSection       `json:"daemons"`
	Probes        *ProbesSection       `json:"probes,omitempty"`
	Capabilities  *CapabilitiesSection `json:"capabilities,omitempty"`
}

// HealthOpts toggles which expensive sections appear in the snapshot.
// IncludeCapabilities implies IncludeProbes (capability discovery
// requires a successful probe first).
type HealthOpts struct {
	IncludeProbes       bool
	IncludeCapabilities bool
	Refresh             bool // bust cache for included sections (rate-limited)
}

// HubSection describes hub-self info. Immutable per process lifetime;
// TTLMs is *int64 with nil meaning "never expires".
type HubSection struct {
	Version     string  `json:"version"`
	Commit      string  `json:"commit"`
	BuildDate   string  `json:"build_date"`
	StartedAt   string  `json:"started_at"`
	Lock        HubLock `json:"lock"`
	GeneratedAt int64   `json:"generated_at"`
	TTLMs       *int64  `json:"ttl_ms"`
}

type HubLock struct {
	PID  int `json:"pid"`
	Port int `json:"port"`
}

// DaemonsSection wraps the per-daemon process state.
type DaemonsSection struct {
	Items       []DaemonRow    `json:"items"`
	GeneratedAt int64          `json:"generated_at"`
	TTLMs       int64          `json:"ttl_ms"`
	Errors      []SectionError `json:"errors"`
}

type DaemonRow struct {
	Server        string  `json:"server"`
	Daemon        string  `json:"daemon"`
	PID           int     `json:"pid"`
	Port          int     `json:"port"`
	RAMBytes      uint64  `json:"ram_bytes"`
	UptimeSec     int64   `json:"uptime_sec"`
	State         string  `json:"state"` // "running" | "stopped" | "starting" | "failed"
	RestartCount  int     `json:"restart_count"`
	LastRestartAt *string `json:"last_restart_at"`
}

type ProbesSection struct {
	Items       []ProbeRow     `json:"items"`
	GeneratedAt int64          `json:"generated_at"`
	TTLMs       int64          `json:"ttl_ms"`
	Errors      []SectionError `json:"errors"`
}

type ProbeRow struct {
	Server    string `json:"server"`
	Daemon    string `json:"daemon"`
	OK        bool   `json:"ok"`
	ToolCount int    `json:"tool_count"`
	Err       string `json:"err"`
	Source    string `json:"source"` // "" | "proxy-synthetic"
}

type CapabilitiesSection struct {
	Items       []CapabilityRow `json:"items"`
	GeneratedAt int64           `json:"generated_at"`
	TTLMs       int64           `json:"ttl_ms"`
	Errors      []SectionError  `json:"errors"`
}

type CapabilityRow struct {
	Server    string               `json:"server"`
	Daemon    string               `json:"daemon"`
	Tools     CapabilitySubSection `json:"tools"`
	Prompts   CapabilitySubSection `json:"prompts"`
	Resources CapabilitySubSection `json:"resources"`
}

// CapabilitySubSection state vocabulary per spec:
//   - "ok"          — list returned, items populated
//   - "empty"       — list returned successfully, zero items
//   - "unsupported" — server responded method-not-found / capability not declared
//   - "error"       — request failed (timeout, parse, transport); Err populated
//   - "stale"       — last successful fetch older than 2× TTL, served best-effort
type CapabilitySubSection struct {
	State string           `json:"state"`
	Items []CapabilityItem `json:"items"`
	Err   string           `json:"err,omitempty"`
}

type CapabilityItem struct {
	Name      string `json:"name"`
	ID        string `json:"id"` // canonical: server/daemon/kind/name
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"` // tool|prompt|resource
}

// SectionError is one partial-failure entry. Scope is "wmic", "daemon:<name>",
// "probe:<server>/<daemon>", "tools:<server>/<daemon>", etc. — granular enough
// for the operator to locate the failure surface.
type SectionError struct {
	Scope string `json:"scope"`
	Err   string `json:"err"`
}

// capabilityID builds the canonical capability identifier per spec:
// {server}/{daemon}/{kind}/{name}. G4's Hub-MCP routing layer uses this
// exact form — do not introduce a parallel scheme.
func capabilityID(server, daemon, kind, name string) string {
	return server + "/" + daemon + "/" + kind + "/" + name
}

// Per-section TTLs in ms. Daemons refresh quickly because operators
// expect near-real-time process state; probes and capabilities are
// expensive and tolerate longer staleness.
const (
	daemonsTTLMs      int64 = 2000
	probesTTLMs       int64 = 10000
	capabilitiesTTLMs int64 = 60000
)

// HealthSnapshot builds the snapshot per opts. Each section is cached
// separately with its own TTL; concurrent expired-cache callers collapse
// onto one underlying fn via singleflight. Phase 2 wires hub + daemons.
// Phases 3 + 4 add probes + capabilities.
func (a *API) HealthSnapshot(opts HealthOpts) (HealthSnapshot, error) {
	now := a.healthNow()

	hub := a.computeHubSection(now)
	daemons, err := a.computeDaemonsSection(now, opts.Refresh)
	if err != nil {
		return HealthSnapshot{}, err
	}

	snap := HealthSnapshot{
		SchemaVersion: "1",
		Hub:           hub,
		Daemons:       daemons,
	}

	if opts.IncludeProbes || opts.IncludeCapabilities {
		probes, err := a.computeProbesSection(now, opts.Refresh)
		if err != nil {
			return HealthSnapshot{}, err
		}
		snap.Probes = &probes
	}
	// IncludeCapabilities is wired in Phase 4 (consumes the probes section above).
	_ = opts.IncludeCapabilities

	return snap, nil
}

// computeHubSection populates the hub section. Build info comes from
// internal/buildinfo (the same source GET /api/version uses). Lock state
// is best-effort — Phase 2 returns HubLock{} since the API struct doesn't
// have a back-reference to the running server. Future phase (or G3
// integration) wires the lock if needed.
//
// Cached once per process via sync.Once — hub never changes after startup.
func (a *API) computeHubSection(nowMs int64) HubSection {
	a.healthCache.hubOnce.Do(func() {
		v, c, d := buildinfo.Get()
		a.healthCache.hub = HubSection{
			Version:     v,
			Commit:      c,
			BuildDate:   d,
			StartedAt:   a.startedAtRFC3339(),
			Lock:        HubLock{}, // wired later if needed
			GeneratedAt: nowMs / 1000,
			TTLMs:       nil, // immutable — never expires
		}
	})
	return a.healthCache.hub
}

// computeDaemonsSection populates the daemons section, with TTL+refresh
// gate and singleflight collapsing on concurrent expired-cache callers.
func (a *API) computeDaemonsSection(nowMs int64, refresh bool) (DaemonsSection, error) {
	a.healthCache.mu.RLock()
	cached := a.healthCache.daemons
	cachedAt := a.healthCache.daemonsAt
	a.healthCache.mu.RUnlock()

	if !refresh && cachedAt > 0 && nowMs-cachedAt < daemonsTTLMs {
		return cached, nil
	}

	v, err, _ := a.healthCache.sf.Do("daemons", func() (any, error) {
		// Re-check after acquiring the singleflight slot — earlier waiters
		// may have refreshed while we queued.
		a.healthCache.mu.RLock()
		recheckAt := a.healthCache.daemonsAt
		recheckSection := a.healthCache.daemons
		a.healthCache.mu.RUnlock()
		if !refresh && nowMs-recheckAt < daemonsTTLMs && recheckAt > 0 {
			return recheckSection, nil
		}

		statusFn := a.HealthStatusFn
		if statusFn == nil {
			statusFn = a.StatusWithOpts
		}
		rows, fetchErr := statusFn(StatusOpts{}) // ProbeHealth=false; probes come in Phase 3
		section := DaemonsSection{
			Items:       make([]DaemonRow, 0, len(rows)),
			GeneratedAt: nowMs / 1000,
			TTLMs:       daemonsTTLMs,
			Errors:      []SectionError{},
		}
		if fetchErr != nil {
			section.Errors = append(section.Errors, SectionError{
				Scope: "daemons",
				Err:   fetchErr.Error(),
			})
			a.healthCache.mu.Lock()
			a.healthCache.daemons = section
			a.healthCache.daemonsAt = nowMs
			a.healthCache.mu.Unlock()
			return section, nil
		}
		for _, r := range rows {
			section.Items = append(section.Items, DaemonRow{
				Server:    r.Server,
				Daemon:    r.Daemon,
				PID:       r.PID,
				Port:      r.Port,
				RAMBytes:  r.RAMBytes,
				UptimeSec: r.UptimeSec,
				State:     normalizeDaemonState(r.State),
				// RestartCount + LastRestartAt: existing DaemonStatus
				// doesn't currently expose them; default 0/nil. Future
				// scheduler integration fills them.
			})
		}
		a.healthCache.mu.Lock()
		a.healthCache.daemons = section
		a.healthCache.daemonsAt = nowMs
		a.healthCache.mu.Unlock()
		return section, nil
	})
	if err != nil {
		return DaemonsSection{}, err
	}
	return v.(DaemonsSection), nil
}

// computeProbesSection populates the per-daemon probe results.
// Calls StatusWithOpts(ProbeHealth=true, ForceMaterialize=false) so the
// existing probe orchestration (synthetic answers for proxy-synthetic
// rows; live MCP roundtrip for real backends) is reused as-is. Per spec,
// ForceMaterialize MUST stay false — it would spawn heavy backends just
// to enumerate tools, defeating the lazy-proxy contract.
//
// 10s TTL. Same per-section RWMutex + singleflight pattern as daemons.
func (a *API) computeProbesSection(nowMs int64, refresh bool) (ProbesSection, error) {
	a.healthCache.mu.RLock()
	cached := a.healthCache.probes
	cachedAt := a.healthCache.probesAt
	a.healthCache.mu.RUnlock()

	if !refresh && cachedAt > 0 && nowMs-cachedAt < probesTTLMs {
		return cached, nil
	}

	v, err, _ := a.healthCache.sf.Do("probes", func() (any, error) {
		// Re-check after acquiring the singleflight slot — earlier waiters
		// may have refreshed while we queued.
		a.healthCache.mu.RLock()
		recheckAt := a.healthCache.probesAt
		recheckSection := a.healthCache.probes
		a.healthCache.mu.RUnlock()
		if !refresh && nowMs-recheckAt < probesTTLMs && recheckAt > 0 {
			return recheckSection, nil
		}

		statusFn := a.HealthStatusFn
		if statusFn == nil {
			statusFn = a.StatusWithOpts
		}
		// ProbeHealth=true triggers per-daemon initialize+tools/list;
		// ForceMaterialize=false (zero value) preserves lazy-proxy semantic
		// — synthetic rows answer from the embedded catalog without
		// spawning the heavy backend.
		rows, fetchErr := statusFn(StatusOpts{ProbeHealth: true})
		section := ProbesSection{
			Items:       make([]ProbeRow, 0, len(rows)),
			GeneratedAt: nowMs / 1000,
			TTLMs:       probesTTLMs,
			Errors:      []SectionError{},
		}
		if fetchErr != nil {
			section.Errors = append(section.Errors, SectionError{
				Scope: "probes",
				Err:   fetchErr.Error(),
			})
			a.healthCache.mu.Lock()
			a.healthCache.probes = section
			a.healthCache.probesAt = nowMs
			a.healthCache.mu.Unlock()
			return section, nil
		}
		for _, r := range rows {
			pr := ProbeRow{Server: r.Server, Daemon: r.Daemon}
			if r.Health != nil {
				pr.OK = r.Health.OK
				pr.ToolCount = r.Health.ToolCount
				pr.Err = r.Health.Err
				pr.Source = r.Health.Source
			} else {
				pr.Err = "no probe (daemon not running or probe disabled)"
			}
			section.Items = append(section.Items, pr)
		}
		a.healthCache.mu.Lock()
		a.healthCache.probes = section
		a.healthCache.probesAt = nowMs
		a.healthCache.mu.Unlock()
		return section, nil
	})
	if err != nil {
		return ProbesSection{}, err
	}
	return v.(ProbesSection), nil
}

// normalizeDaemonState maps the existing ("Running"|"Ready"|"Failed"|"Stopped")
// vocabulary to the spec's lowercase ("running"|"stopped"|"starting"|"failed").
//
// FIXME(phase-6): project back to DaemonStatus.State Title Case during
// /api/status re-source so the existing wire shape stays unchanged.
func normalizeDaemonState(s string) string {
	switch s {
	case "Running", "Ready":
		return "running"
	case "Failed":
		return "failed"
	case "Stopped":
		return "stopped"
	default:
		return "starting"
	}
}

// healthNow returns current time in ms. Test seam.
func (a *API) healthNow() int64 {
	if a.HealthNowMs != nil {
		return a.HealthNowMs()
	}
	return time.Now().UnixMilli()
}

// startedAtRFC3339 returns the API's start time. If not tracked, returns "".
func (a *API) startedAtRFC3339() string {
	if a.StartedAt.IsZero() {
		return ""
	}
	return a.StartedAt.Format(time.RFC3339)
}
