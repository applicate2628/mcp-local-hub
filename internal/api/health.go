package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	Server string `json:"server"`
	Daemon string `json:"daemon"`
	// Backend is the workspace-scoped lazy-proxy backend kind
	// ("mcp-language-server", "gopls-mcp", etc.); empty for global daemons.
	// Required so computeCapabilitiesSection can rebuild a synthetic
	// DaemonStatus with the kind ToolCatalogForBackend keys on — without
	// it, every workspace-scoped lazy proxy reports an empty tools list.
	Backend       string  `json:"backend,omitempty"`
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

// Per-section refresh-rate-limit minimums in ms. When a caller sets
// HealthOpts.Refresh=true, the rate-limit gate (in compute*Section)
// silently downgrades the request to a cache-read if the previous
// refresh happened within this window. Singleflight already collapses
// CONCURRENT refreshes; this guards against ABSENT-singleflight
// back-to-back refreshes (e.g. a malicious or bug-driven client
// repeatedly hitting /api/health?refresh=true).
//
// Numbers chosen to bound the per-section refresh rate at 1/window:
//   - daemons:      1/s   — wmic / ps scans are cheap but not free
//   - probes:       1/5s  — initialize+tools/list per daemon
//   - capabilities: 1/30s — three list calls per daemon
const (
	daemonsRefreshMinMs      int64 = 1000
	probesRefreshMinMs       int64 = 5000
	capabilitiesRefreshMinMs int64 = 30000
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

	// IncludeCapabilities implies IncludeProbes — the capabilities section
	// only walks daemons whose probe succeeded (capability discovery
	// requires a reachable backend), so the probes section must be
	// computed first regardless of whether the caller asked for it.
	if opts.IncludeProbes || opts.IncludeCapabilities {
		probes, err := a.computeProbesSection(now, opts.Refresh)
		if err != nil {
			return HealthSnapshot{}, err
		}
		snap.Probes = &probes

		if opts.IncludeCapabilities {
			caps, err := a.computeCapabilitiesSection(now, opts.Refresh, probes)
			if err != nil {
				return HealthSnapshot{}, err
			}
			snap.Capabilities = &caps
		}
	}

	return snap, nil
}

// DaemonStatusSnapshot returns the cached []DaemonStatus rows that
// /api/status consumes. Shares the daemons-section cache with
// HealthSnapshot — one StatusWithOpts call serves both endpoints
// within the daemons TTL, preventing drift and amortizing the wmic
// (Windows) / ps (POSIX) cost.
//
// Returns the rich canonical form (TaskName, NextRun, Health,
// workspace-scoped fields) so /api/status preserves its existing
// wire shape; the thinner DaemonRow projection lives only inside
// HealthSnapshot.Daemons.Items.
//
// The returned slice is a defensive copy so callers can mutate it
// freely (e.g. sorting) without poisoning the cache. On total-backend
// failure (StatusWithOpts returns an error), the error is propagated
// out as the second return value AND recorded in DaemonsSection.Errors
// for /api/health partial-failure introspection — /api/status's
// handler maps the propagated error to 500 + STATUS_FAILED, restoring
// the pre-G2 fail-loud contract that operations dashboards rely on.
//
// Phase 6 of G2 per spec
// docs/superpowers/specs/2026-05-07-g2-unified-health-endpoint-design.md.
// Error-propagation contract restored on PR #132 follow-up after the
// Cloud bot P1 escalation: prior implementation swallowed fetchErr
// into section.Errors and returned nil, so operators saw 200 [] on
// total backend failure instead of 500 STATUS_FAILED.
func (a *API) DaemonStatusSnapshot() ([]DaemonStatus, error) {
	nowMs := a.healthNow()
	// Reuse computeDaemonsSection's TTL+singleflight logic. We discard
	// the returned DaemonsSection — what we want is the side-effect of
	// having `daemonStatuses` populated in the same critical section.
	if _, err := a.computeDaemonsSection(nowMs, false); err != nil {
		return nil, err
	}
	a.healthCache.mu.RLock()
	rows := a.healthCache.daemonStatuses
	a.healthCache.mu.RUnlock()
	if len(rows) == 0 {
		// Distinguish nil-from-uninitialized vs. empty-by-design: both
		// resolve to the same zero-length response, but the consumers
		// (frontend, csrf_test, e2e) decode `[]` not `null`. Return a
		// fresh empty slice so the JSON encoder always emits `[]`.
		return []DaemonStatus{}, nil
	}
	out := make([]DaemonStatus, len(rows))
	copy(out, rows)
	return out, nil
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

	// Per-section refresh rate-limit gate. Excess refresh requests
	// within the minimum-interval window get the cached value rather
	// than triggering a fresh probe. This bounds the cost of repeated
	// ?refresh=true on the handler. Singleflight (below) collapses
	// concurrent refreshes; this gates ABSENT-singleflight back-to-back
	// refreshes that arrive in series.
	if refresh && cachedAt > 0 && nowMs-cachedAt < daemonsRefreshMinMs {
		refresh = false
	}

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
			// On fetch error, store an empty canonical slice (not nil)
			// so DaemonStatusSnapshot returns []DaemonStatus{} instead
			// of nil — preserves the "always returns a non-nil slice"
			// contract the existing /api/status wire-shape consumers expect.
			a.healthCache.daemonStatuses = []DaemonStatus{}
			a.healthCache.daemonsAt = nowMs
			a.healthCache.mu.Unlock()
			// Propagate the error so /api/status returns 500 STATUS_FAILED
			// and /api/health returns 500 HEALTH_BACKEND_FAILED on total
			// backend failure. section.Errors[] is also populated for any
			// future client that introspects partial-failure scopes.
			// Cloud bot P1 fix on PR #132 commit a8a54c1: prior swallow
			// behavior masked real backend failures as "no daemons".
			return section, fetchErr
		}
		for _, r := range rows {
			section.Items = append(section.Items, DaemonRow{
				Server:    r.Server,
				Daemon:    r.Daemon,
				Backend:   r.Backend, // empty for global daemons; carries lazy-proxy kind for workspace-scoped rows
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
		// Cache the canonical []DaemonStatus alongside the projected
		// DaemonsSection so /api/status (DaemonStatusSnapshot) can serve
		// reads from the same slot WITHOUT calling StatusWithOpts again.
		// Phase 6 of G2: one fetch, two surfaces — zero drift between
		// /api/status and /api/health's daemons section.
		//
		// We store the rows as-is from statusFn. Snapshot reads make a
		// defensive copy before returning so a mutation by one reader
		// can't poison the next.
		a.healthCache.daemonStatuses = rows
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

	// Per-section refresh rate-limit gate (see computeDaemonsSection).
	if refresh && cachedAt > 0 && nowMs-cachedAt < probesRefreshMinMs {
		refresh = false
	}

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
			// Propagate the error so /api/health returns 500
			// HEALTH_BACKEND_FAILED on total probe-backend failure.
			// section.Errors[] is also populated for partial-failure
			// introspection. Symmetric with the daemons-section fix —
			// Cloud bot P1 on PR #132 commit a8a54c1.
			return section, fetchErr
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

// computeCapabilitiesSection populates the per-daemon capability section.
// Walks the (already-computed) probes section, skips rows whose probe
// failed (capability discovery requires a reachable backend), and for the
// rest invokes a.HealthCapabilityFn (or its production fallback,
// a.realCapabilityRow) to project tools/list, prompts/list, resources/list
// into a CapabilityRow with the spec's 5-state vocabulary
// (ok|empty|unsupported|error|stale).
//
// 60s TTL — capability discovery is expensive (one initialize+list
// roundtrip per sub-section per daemon) and the result rarely changes
// outside of an upgrade. Same per-section RWMutex + singleflight pattern
// as daemons/probes; the cache key is "capabilities" (NOT "daemons" or
// "probes") so concurrent callers across different sections don't collide
// on the singleflight slot.
func (a *API) computeCapabilitiesSection(nowMs int64, refresh bool, probes ProbesSection) (CapabilitiesSection, error) {
	a.healthCache.mu.RLock()
	cached := a.healthCache.capabilities
	cachedAt := a.healthCache.capabilitiesAt
	a.healthCache.mu.RUnlock()

	// Per-section refresh rate-limit gate (see computeDaemonsSection).
	if refresh && cachedAt > 0 && nowMs-cachedAt < capabilitiesRefreshMinMs {
		refresh = false
	}

	if !refresh && cachedAt > 0 && nowMs-cachedAt < capabilitiesTTLMs {
		return cached, nil
	}

	v, err, _ := a.healthCache.sf.Do("capabilities", func() (any, error) {
		// Re-check after acquiring the singleflight slot — earlier waiters
		// may have refreshed while we queued.
		a.healthCache.mu.RLock()
		recheckAt := a.healthCache.capabilitiesAt
		recheckSection := a.healthCache.capabilities
		a.healthCache.mu.RUnlock()
		if !refresh && nowMs-recheckAt < capabilitiesTTLMs && recheckAt > 0 {
			return recheckSection, nil
		}

		// Build server/daemon → {port, backend} lookups so the capability
		// fn can reach the daemon's MCP endpoint and (for workspace-scoped
		// lazy proxies) keep the Backend kind for ToolCatalogForBackend.
		// The probes section doesn't carry either field (intentional —
		// ProbeRow is a thin projection); pull from the daemons cache.
		// Backend is critical for the synthetic-source branch in
		// realCapabilityRow → syntheticToolsSubSection: dropping it makes
		// ToolCatalogForBackend("") return (zero, false) and reports
		// state="empty" for every lazy-proxy daemon.
		daemons, _ := a.computeDaemonsSection(nowMs, false)
		portByServerDaemon := make(map[string]int, len(daemons.Items))
		backendByServerDaemon := make(map[string]string, len(daemons.Items))
		for _, d := range daemons.Items {
			portByServerDaemon[d.Server+"/"+d.Daemon] = d.Port
			backendByServerDaemon[d.Server+"/"+d.Daemon] = d.Backend
		}

		section := CapabilitiesSection{
			Items:       make([]CapabilityRow, 0, len(probes.Items)),
			GeneratedAt: nowMs / 1000,
			TTLMs:       capabilitiesTTLMs,
			Errors:      []SectionError{},
		}
		fn := a.HealthCapabilityFn
		if fn == nil {
			fn = a.realCapabilityRow
		}
		for _, p := range probes.Items {
			if !p.OK {
				continue // Skip failed-probe daemons; capability discovery requires reachable backend.
			}
			row, rowErr := fn(DaemonStatus{
				Server:  p.Server,
				Daemon:  p.Daemon,
				Backend: backendByServerDaemon[p.Server+"/"+p.Daemon],
				Port:    portByServerDaemon[p.Server+"/"+p.Daemon],
				Health:  &HealthProbe{OK: p.OK, ToolCount: p.ToolCount, Source: p.Source},
			})
			if rowErr != nil {
				section.Errors = append(section.Errors, SectionError{
					Scope: "capability:" + p.Server + "/" + p.Daemon,
					Err:   rowErr.Error(),
				})
				continue
			}
			row = ensureCanonicalIDs(row)
			section.Items = append(section.Items, row)
		}
		a.healthCache.mu.Lock()
		a.healthCache.capabilities = section
		a.healthCache.capabilitiesAt = nowMs
		a.healthCache.mu.Unlock()
		return section, nil
	})
	if err != nil {
		return CapabilitiesSection{}, err
	}
	return v.(CapabilitiesSection), nil
}

// realCapabilityRow does the live MCP roundtrip for one daemon. Calls
// tools/list, prompts/list, resources/list and projects each into one
// CapabilitySubSection per spec's 5-state vocabulary
// (ok|empty|unsupported|error|stale).
//
// Synthetic-source rows (Health.Source=="proxy-synthetic") answer from
// the embedded ToolCatalogForBackend(d.Backend) catalog — same path the
// lazy proxy uses for its synthetic tools/list response. NEVER
// materializes the heavy backend (matches Phase 3's lazy-proxy contract:
// the operator can enumerate capabilities without spawning subprocesses).
//
// Per spec the embedded catalog ONLY models tools (no prompts or
// resources for any current backend kind), so the synthetic prompts and
// resources sub-sections are reported as "unsupported" — that is honest
// about the data we have.
func (a *API) realCapabilityRow(d DaemonStatus) (CapabilityRow, error) {
	row := CapabilityRow{Server: d.Server, Daemon: d.Daemon}

	if d.Health != nil && d.Health.Source == "proxy-synthetic" {
		row.Tools = a.syntheticToolsSubSection(d)
		row.Prompts = a.syntheticPromptsSubSection(d)
		row.Resources = a.syntheticResourcesSubSection(d)
		return row, nil
	}

	if d.Port == 0 {
		// No port → cannot reach the backend. All three sub-sections
		// report "error" with a concrete reason so the operator sees
		// the gap rather than a phantom "empty" / "unsupported".
		msg := "no port for daemon"
		row.Tools = CapabilitySubSection{State: "error", Err: msg}
		row.Prompts = CapabilitySubSection{State: "error", Err: msg}
		row.Resources = CapabilitySubSection{State: "error", Err: msg}
		return row, nil
	}

	row.Tools = a.liveCapabilitySubSection(d, "tools/list", "tool")
	row.Prompts = a.liveCapabilitySubSection(d, "prompts/list", "prompt")
	row.Resources = a.liveCapabilitySubSection(d, "resources/list", "resource")
	return row, nil
}

// liveCapabilitySubSection does one JSON-RPC call against the daemon's
// MCP endpoint and projects the response. Mirrors singleHealthProbe's
// shape (initialize → method call → SSE-or-JSON parse).
//
// State mapping per spec:
//   - response with non-empty list  → "ok"
//   - response with empty list      → "empty"
//   - JSON-RPC error code -32601    → "unsupported" (method not found)
//   - any other failure             → "error" + Err populated
func (a *API) liveCapabilitySubSection(d DaemonStatus, method, kind string) CapabilitySubSection {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", d.Port)
	client := &http.Client{Timeout: 3 * time.Second}

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"mcphub-health","version":"1"}}}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(initBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return CapabilitySubSection{State: "error", Err: "initialize: " + err.Error()}
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		return CapabilitySubSection{State: "error", Err: fmt.Sprintf("initialize: HTTP %d", resp.StatusCode)}
	}

	listBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"%s"}`, method)
	req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(listBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req2.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp2, err := client.Do(req2)
	if err != nil {
		return CapabilitySubSection{State: "error", Err: method + ": " + err.Error()}
	}
	defer resp2.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp2.Body, maxHealthProbeResponseBytes+1))
	if err != nil {
		return CapabilitySubSection{State: "error", Err: method + ": read: " + err.Error()}
	}
	if len(raw) > maxHealthProbeResponseBytes {
		return CapabilitySubSection{State: "error", Err: fmt.Sprintf("%s: response too large (> %d bytes)", method, maxHealthProbeResponseBytes)}
	}
	payload := raw
	// SSE-wrapped response: pull JSON out of the first data: line. Same
	// shape as singleHealthProbe — keep them in sync.
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "data: ") {
			payload = []byte(strings.TrimPrefix(line, "data: "))
			break
		}
	}

	// Parse: error first (preserve method-not-found code so the spec's
	// "unsupported" state is distinguishable from generic "error").
	var errEnv struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &errEnv); err == nil && errEnv.Error != nil {
		if errEnv.Error.Code == -32601 {
			return CapabilitySubSection{State: "unsupported"}
		}
		return CapabilitySubSection{State: "error", Err: fmt.Sprintf("%s: %s", method, errEnv.Error.Message)}
	}

	// Decode the kind-specific result list.
	var raws []json.RawMessage
	switch kind {
	case "tool":
		var p struct {
			Result struct {
				Tools []json.RawMessage `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return CapabilitySubSection{State: "error", Err: method + ": parse: " + err.Error()}
		}
		raws = p.Result.Tools
	case "prompt":
		var p struct {
			Result struct {
				Prompts []json.RawMessage `json:"prompts"`
			} `json:"result"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return CapabilitySubSection{State: "error", Err: method + ": parse: " + err.Error()}
		}
		raws = p.Result.Prompts
	case "resource":
		var p struct {
			Result struct {
				Resources []json.RawMessage `json:"resources"`
			} `json:"result"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return CapabilitySubSection{State: "error", Err: method + ": parse: " + err.Error()}
		}
		raws = p.Result.Resources
	}

	items := make([]CapabilityItem, 0, len(raws))
	for _, rm := range raws {
		var named struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(rm, &named); err != nil || named.Name == "" {
			continue
		}
		items = append(items, CapabilityItem{
			Name:      named.Name,
			ID:        capabilityID(d.Server, d.Daemon, kind, named.Name),
			Namespace: d.Server,
			Kind:      kind,
		})
	}
	if len(items) == 0 {
		return CapabilitySubSection{State: "empty", Items: items}
	}
	return CapabilitySubSection{State: "ok", Items: items}
}

// syntheticToolsSubSection answers from the embedded ToolCatalogForBackend
// catalog — same path the lazy proxy uses for its synthetic tools/list
// response (see internal/api/tool_catalog.go::SyntheticToolsListResponse).
// Backend kind is sourced from DaemonStatus.Backend
// ("mcp-language-server" | "gopls-mcp"). When the backend kind is unknown
// the catalog is empty → state "empty".
func (a *API) syntheticToolsSubSection(d DaemonStatus) CapabilitySubSection {
	cat, ok := ToolCatalogForBackend(d.Backend)
	if !ok || len(cat.Tools) == 0 {
		return CapabilitySubSection{State: "empty", Items: []CapabilityItem{}}
	}
	items := make([]CapabilityItem, 0, len(cat.Tools))
	for _, t := range cat.Tools {
		items = append(items, CapabilityItem{
			Name:      t.Name,
			ID:        capabilityID(d.Server, d.Daemon, "tool", t.Name),
			Namespace: d.Server,
			Kind:      "tool",
		})
	}
	return CapabilitySubSection{State: "ok", Items: items}
}

// syntheticPromptsSubSection: the embedded catalog only models tools.
// Reporting "unsupported" here is more honest than "empty" because we
// know up front that no synthetic-prompt path exists for any current
// backend kind — the lazy proxy never declares "prompts" capability in
// its synthetic initialize response unless and until someone wires one.
func (a *API) syntheticPromptsSubSection(d DaemonStatus) CapabilitySubSection {
	return CapabilitySubSection{State: "unsupported"}
}

// syntheticResourcesSubSection: same rationale as syntheticPromptsSubSection.
// The synthetic catalog has no resource entries; "unsupported" tells the
// operator that this is a deliberate gap rather than a transient failure.
func (a *API) syntheticResourcesSubSection(d DaemonStatus) CapabilitySubSection {
	return CapabilitySubSection{State: "unsupported"}
}

// ensureCanonicalIDs backfills ID/Kind/Namespace on every CapabilityItem
// the producer (test seam or live MCP roundtrip) didn't fully populate.
// Canonical form is {server}/{daemon}/{kind}/{name} per spec — G4's
// Hub-MCP routing layer relies on this exact shape, so any producer that
// forgets the field gets the right value here. Idempotent: a fully
// populated row is unchanged.
func ensureCanonicalIDs(row CapabilityRow) CapabilityRow {
	backfill := func(sub CapabilitySubSection, kind string) CapabilitySubSection {
		for i := range sub.Items {
			if sub.Items[i].ID == "" {
				sub.Items[i].ID = capabilityID(row.Server, row.Daemon, kind, sub.Items[i].Name)
			}
			if sub.Items[i].Kind == "" {
				sub.Items[i].Kind = kind
			}
			if sub.Items[i].Namespace == "" {
				sub.Items[i].Namespace = row.Server
			}
		}
		return sub
	}
	row.Tools = backfill(row.Tools, "tool")
	row.Prompts = backfill(row.Prompts, "prompt")
	row.Resources = backfill(row.Resources, "resource")
	return row
}

// normalizeDaemonState maps the existing ("Running"|"Ready"|"Failed"|"Stopped")
// vocabulary to the spec's lowercase ("running"|"stopped"|"starting"|"failed").
//
// Used only by computeDaemonsSection when projecting into DaemonRow
// (the lowercase wire form for HealthSnapshot.Daemons.Items). The
// /api/status surface bypasses this projection entirely after Phase 6 —
// DaemonStatusSnapshot returns the canonical []DaemonStatus with the
// original Title-Case state vocabulary intact, no round-trip needed.
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
