# G2 — Unified `/api/health` Endpoint Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a canonical health/capability snapshot backend exposed via `GET /api/health`, owning the data G3 (capability display) and G4 (Hub MCP) consume. `/api/status` is re-sourced from the same backend in the final phase to prevent drift.

**Architecture:** New backend in `internal/api/health.go` aggregates four sections (hub, daemons, probes, capabilities) with per-section TTL caching, singleflight collapsing of concurrent expired-cache requests, per-section refresh rate limits, and discrete-state capability sub-sections (`ok|empty|unsupported|error|stale`). New handler in `internal/gui/health.go` exposes the snapshot with `?include=probes,capabilities` and `?refresh=true`. `/api/status` continues to return `[]api.DaemonStatus` but routes through the health backend's daemons section.

**Tech Stack:** Go (existing stdlib + `golang.org/x/sync/singleflight`), wraps existing `api.StatusWithOpts()` for daemons/probes, calls `singleHealthProbe()` (`internal/api/install.go:670`) for MCP probes, calls existing MCP client roundtrip helpers for capability discovery.

**Spec:** [docs/superpowers/specs/2026-05-07-g2-unified-health-endpoint-design.md](../specs/2026-05-07-g2-unified-health-endpoint-design.md) at commit `070c621`. Read it before each task — it has the JSON shape, state vocabulary, and non-goals you'll need.

**Branch:** `master` HEAD `070c621`. All six phases land as separate commits on `feat/g2-health-endpoint` (one PR).

---

## File structure

| Path | Responsibility |
|------|---------------|
| `internal/api/health.go` | Snapshot types, `HealthSnapshot()`, cache + singleflight, section builders. NEW. |
| `internal/api/health_test.go` | Backend unit tests, fake-clock cache tests, lazy-proxy preservation. NEW. |
| `internal/gui/health.go` | `GET /api/health` handler, `parseHealthOpts`, route registration. NEW. |
| `internal/gui/health_test.go` | Handler tests (method/origin/include/refresh/unknown-token). NEW. |
| `internal/gui/server.go:~310` | Add `s.api` field (or interface seam) to give the handler access to `HealthSnapshot()`. MODIFY. |
| `internal/gui/server.go:~325` | Wire `registerHealthRoute(s)` next to other `register*Routes(s)` calls. MODIFY. |
| `internal/gui/status.go` | Phase 6 only — re-source from health backend. MODIFY. |
| `go.mod` / `go.sum` | Add `golang.org/x/sync` (singleflight). MODIFY (Phase 2 only if not already present). |

---

## Task 1: Types skeleton + schema_version stub

**Goal:** Lock in the type signatures that all downstream phases depend on. No real data plumbing yet — `HealthSnapshot()` returns fixed zero values for hub/daemons sections, nil for probes/capabilities. This is the API-contract phase.

**Files:**
- Create: `internal/api/health.go`
- Create: `internal/api/health_test.go`

- [ ] **Step 1.1: Write the failing tests in `internal/api/health_test.go`**

```go
package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHealthSnapshot_DefaultExcludesProbesAndCapabilities(t *testing.T) {
	a := NewAPI()
	snap, err := a.HealthSnapshot(HealthOpts{})
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if snap.SchemaVersion != "1" {
		t.Errorf("SchemaVersion = %q, want \"1\"", snap.SchemaVersion)
	}
	if snap.Probes != nil {
		t.Errorf("Probes = %+v, want nil (default opts must omit expensive sections)", snap.Probes)
	}
	if snap.Capabilities != nil {
		t.Errorf("Capabilities = %+v, want nil", snap.Capabilities)
	}
}

func TestHealthSnapshot_JSONOmitsNilSections(t *testing.T) {
	a := NewAPI()
	snap, _ := a.HealthSnapshot(HealthOpts{})
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	body := string(b)
	if strings.Contains(body, `"probes"`) {
		t.Errorf("default JSON contains probes key, must be omitted: %s", body)
	}
	if strings.Contains(body, `"capabilities"`) {
		t.Errorf("default JSON contains capabilities key, must be omitted: %s", body)
	}
	if !strings.Contains(body, `"schema_version":"1"`) {
		t.Errorf("missing schema_version=1: %s", body)
	}
}

func TestCapabilityID_CanonicalForm(t *testing.T) {
	got := capabilityID("fs", "fs-default", "tool", "read_file")
	want := "fs/fs-default/tool/read_file"
	if got != want {
		t.Errorf("capabilityID = %q, want %q", got, want)
	}
}

func TestHealthSnapshot_HubSectionShape(t *testing.T) {
	a := NewAPI()
	snap, _ := a.HealthSnapshot(HealthOpts{})
	// Hub section is present (not optional) and carries schema-required fields,
	// even when zero-valued, so consumers can rely on the structure.
	if snap.Hub.Version == "" && snap.Hub.Commit == "" && snap.Hub.BuildDate == "" {
		// All three empty is acceptable in unit tests where build info isn't injected;
		// but the *fields* must exist (will fail compile if they don't).
		_ = snap.Hub.Version
		_ = snap.Hub.Commit
		_ = snap.Hub.BuildDate
		_ = snap.Hub.StartedAt
		_ = snap.Hub.Lock
		_ = snap.Hub.GeneratedAt
		_ = snap.Hub.TTLMs
	}
}
```

- [ ] **Step 1.2: Run tests to verify they FAIL with "undefined" errors**

```bash
go test -count=1 -run 'TestHealthSnapshot_|TestCapabilityID_' ./internal/api/...
```

Expected: compile errors (`undefined: HealthSnapshot`, `undefined: HealthOpts`, etc.).

- [ ] **Step 1.3: Create `internal/api/health.go` with the full type skeleton**

```go
package api

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
	State         string  `json:"state"` // "running" | "stopped" | "starting" | "failed" | "unknown"
	// v0.6 Workstream B / PR #281: the wire enum gained "unknown". A
	// genuinely UNRECOGNIZED or blank source state maps to "unknown", NOT
	// "failed" (the prior unmapped→failed mapping was a false negative).
	// KNOWN supervisor degraded/terminal states stay classified honestly,
	// never collapsed to "unknown": "Restarting"/"Backoff"/"Spawning" →
	// "starting" (degraded, supervisor recovering); "Quarantined" →
	// "failed" (supervisor permanently gave up after a crash-loop).
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
	ID        string `json:"id"`        // canonical: server/daemon/kind/name
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

// HealthSnapshot returns the current snapshot. Phase 1 stub: returns
// SchemaVersion="1" + zero-valued Hub/Daemons + nil Probes/Capabilities
// regardless of opts. Wired in Phase 2+.
func (a *API) HealthSnapshot(opts HealthOpts) (HealthSnapshot, error) {
	return HealthSnapshot{
		SchemaVersion: "1",
		Hub:           HubSection{},
		Daemons:       DaemonsSection{Items: []DaemonRow{}, Errors: []SectionError{}},
	}, nil
}
```

- [ ] **Step 1.4: Run the tests — verify all four PASS**

```bash
go test -count=1 -run 'TestHealthSnapshot_|TestCapabilityID_' ./internal/api/...
```

Expected: `ok  mcp-local-hub/internal/api`.

- [ ] **Step 1.5: Run full backend test sweep — verify no regression**

```bash
go test -count=1 ./internal/api/... ./internal/gui/...
```

Expected: PASS, no new failures.

- [ ] **Step 1.6: Commit**

```bash
git add internal/api/health.go internal/api/health_test.go
git commit -m "feat(api): G2 phase 1 — HealthSnapshot types skeleton + capabilityID helper

Defines the typed contract G3 + G4 will consume: HealthSnapshot,
HealthOpts, HubSection, DaemonsSection, ProbesSection,
CapabilitiesSection, CapabilityRow, CapabilitySubSection (5-state
vocabulary: ok|empty|unsupported|error|stale), SectionError, and
the canonical capabilityID helper producing {server}/{daemon}/{kind}/{name}.

HealthSnapshot() is a stub returning SchemaVersion=\"1\" plus zero
sections. Probes and Capabilities are pointer-typed so they're
omitted from JSON when the caller doesn't opt in.

Spec: docs/superpowers/specs/2026-05-07-g2-unified-health-endpoint-design.md"
```

---

## Task 2: Hub + daemons sections + cache + singleflight

**Goal:** Wire real data into the hub and daemons sections, add per-section TTL cache, add singleflight collapsing for concurrent expired-cache requests, expose `Refresh: true` to bust the cache (per-section rate-limited in Task 5).

**Files:**
- Modify: `internal/api/health.go` (add cache state to API struct, populate hub + daemons)
- Modify: `internal/api/health_test.go` (cache tests)
- Modify: `go.mod` + `go.sum` (add `golang.org/x/sync` if absent)
- Modify: `internal/api/api.go` or wherever `NewAPI()` lives (initialize cache field — locate via `grep -n "func NewAPI" internal/api/`)

**Reference reads** (the implementer should read these before coding):
- `internal/api/install.go:670` — `singleHealthProbe(port int) *HealthProbe`
- `internal/api/install.go:368-388` — existing `Status()` / `StatusWithOpts(StatusOpts{...})` returning `[]api.DaemonStatus`. Phase 2 calls this for the daemons section.
- `internal/api/types.go:17-50` — `DaemonStatus` shape (Server, Daemon, TaskName, State, Port, PID, RAMBytes, UptimeSec, etc.). The Phase 2 daemons builder maps DaemonStatus → DaemonRow.
- `internal/cli/version.go` (or `version` package, locate via `grep -n "Version\\|Commit\\|BuildDate" internal/`) — build-info accessors for the hub section.

- [ ] **Step 2.1: Ensure singleflight is a dependency**

```bash
go list -m golang.org/x/sync 2>&1
```

If output is `go: not used` or empty:

```bash
go get golang.org/x/sync
```

Otherwise skip — already a transitive dep.

- [ ] **Step 2.2: Write the failing tests at the end of `internal/api/health_test.go`**

```go
// TestHealthSnapshot_DaemonsSectionPopulated asserts the daemons
// section carries DaemonRow entries projected from existing
// api.StatusWithOpts() output. Uses a fake StatusFn injected via the
// test seam introduced in Phase 2.
func TestHealthSnapshot_DaemonsSectionPopulated(t *testing.T) {
	a := NewAPI()
	originalStatusFn := a.healthStatusFn
	defer func() { a.healthStatusFn = originalStatusFn }()
	a.healthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		return []DaemonStatus{
			{Server: "fs", Daemon: "fs-default", PID: 1234, Port: 9100,
				State: "Running", RAMBytes: 50_000_000, UptimeSec: 300},
		}, nil
	}

	snap, err := a.HealthSnapshot(HealthOpts{})
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if len(snap.Daemons.Items) != 1 {
		t.Fatalf("Daemons.Items = %d, want 1: %+v", len(snap.Daemons.Items), snap.Daemons.Items)
	}
	if snap.Daemons.Items[0].Server != "fs" || snap.Daemons.Items[0].PID != 1234 {
		t.Errorf("row 0 = %+v, want server=fs pid=1234", snap.Daemons.Items[0])
	}
	if snap.Daemons.TTLMs != 2000 {
		t.Errorf("Daemons.TTLMs = %d, want 2000", snap.Daemons.TTLMs)
	}
}

// TestHealthSnapshot_CacheServesWithinTTL: a second call within TTL
// returns the same GeneratedAt without invoking the underlying fn again.
func TestHealthSnapshot_CacheServesWithinTTL(t *testing.T) {
	a := NewAPI()
	original := a.healthStatusFn
	defer func() { a.healthStatusFn = original }()
	calls := 0
	a.healthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		calls++
		return []DaemonStatus{{Server: "x", Daemon: "x"}}, nil
	}

	snap1, _ := a.HealthSnapshot(HealthOpts{})
	snap2, _ := a.HealthSnapshot(HealthOpts{})

	if calls != 1 {
		t.Errorf("underlying fn called %d times, want 1 (second call must hit cache)", calls)
	}
	if snap1.Daemons.GeneratedAt != snap2.Daemons.GeneratedAt {
		t.Errorf("GeneratedAt diverged across cached calls: %d vs %d",
			snap1.Daemons.GeneratedAt, snap2.Daemons.GeneratedAt)
	}
}

// TestHealthSnapshot_CacheExpiresAfterTTL: forcing the now-clock past
// the TTL boundary triggers a re-run.
func TestHealthSnapshot_CacheExpiresAfterTTL(t *testing.T) {
	a := NewAPI()
	originalNow := a.healthNowMs
	originalStatus := a.healthStatusFn
	defer func() {
		a.healthNowMs = originalNow
		a.healthStatusFn = originalStatus
	}()

	now := int64(1_000_000)
	a.healthNowMs = func() int64 { return now }

	calls := 0
	a.healthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		calls++
		return []DaemonStatus{}, nil
	}

	_, _ = a.HealthSnapshot(HealthOpts{})
	now += 2001 // > daemons TTL of 2000ms
	_, _ = a.HealthSnapshot(HealthOpts{})

	if calls != 2 {
		t.Errorf("underlying fn called %d times, want 2 (TTL expired)", calls)
	}
}

// TestHealthSnapshot_RefreshBustsCache: Refresh=true triggers a re-run
// even within TTL.
func TestHealthSnapshot_RefreshBustsCache(t *testing.T) {
	a := NewAPI()
	originalStatus := a.healthStatusFn
	originalNow := a.healthNowMs
	defer func() {
		a.healthStatusFn = originalStatus
		a.healthNowMs = originalNow
	}()
	now := int64(1_000_000)
	a.healthNowMs = func() int64 { return now }
	calls := 0
	a.healthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		calls++
		return []DaemonStatus{}, nil
	}

	_, _ = a.HealthSnapshot(HealthOpts{})
	now += 100 // well within TTL
	_, _ = a.HealthSnapshot(HealthOpts{Refresh: true})

	if calls != 2 {
		t.Errorf("underlying fn called %d times, want 2 (Refresh must bust within TTL)", calls)
	}
}

// TestHealthSnapshot_SingleflightCollapsesConcurrent: N goroutines
// hitting expired-cache trigger exactly one underlying call.
func TestHealthSnapshot_SingleflightCollapsesConcurrent(t *testing.T) {
	a := NewAPI()
	originalStatus := a.healthStatusFn
	defer func() { a.healthStatusFn = originalStatus }()
	var calls atomic.Int32
	gate := make(chan struct{})
	a.healthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		calls.Add(1)
		<-gate // hold first caller; concurrent goroutines pile up on singleflight
		return []DaemonStatus{}, nil
	}

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = a.HealthSnapshot(HealthOpts{})
		}()
	}
	time.Sleep(50 * time.Millisecond) // let goroutines reach the singleflight wait
	close(gate)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("underlying fn called %d times across %d goroutines, want 1 (singleflight)", got, N)
	}
}
```

Add imports at top of test file: `"sync"`, `"sync/atomic"`, `"time"`.

- [ ] **Step 2.3: Run the tests, verify they FAIL**

```bash
go test -count=1 -run 'TestHealthSnapshot_|TestCapabilityID_' ./internal/api/...
```

Expected: compile errors for `healthStatusFn`, `healthNowMs`, then test failures for daemon population.

- [ ] **Step 2.4: Implement Phase 2 in `internal/api/health.go`**

Replace the Phase 1 stub `HealthSnapshot()` and add cache state. The full Phase 2 file body:

```go
package api

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// (keep all type definitions from Phase 1 unchanged above)

const (
	daemonsTTLMs      int64 = 2000
	probesTTLMs       int64 = 10000
	capabilitiesTTLMs int64 = 60000
)

// healthCache is the per-API cache. One instance per API; mutex guards
// cached values, singleflight collapses concurrent refreshes per section.
type healthCache struct {
	mu sync.RWMutex
	sf singleflight.Group

	hubOnce         sync.Once
	hub             HubSection

	daemonsAt   int64 // ms since epoch when last computed
	daemons     DaemonsSection

	probesAt int64
	probes   ProbesSection

	capabilitiesAt int64
	capabilities   CapabilitiesSection
}

// API extension fields (declared on existing API struct in api.go via
// embedding here — locate the struct and add these fields).
//
// Phase 2 needs the test seams healthStatusFn (defaults to a.StatusWithOpts)
// and healthNowMs (defaults to time.Now().UnixMilli()) plus the cache.

// HealthSnapshot builds the snapshot per opts. Each section is cached
// separately with its own TTL; concurrent expired-cache callers collapse
// onto one underlying fn via singleflight.
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

	// Probes and Capabilities are wired in Tasks 3 & 4. Stub here keeps
	// the IncludeProbes / IncludeCapabilities branches returning nil so
	// the schema_version + hub + daemons path is fully exercised in
	// Phase 2 tests.
	_ = opts.IncludeProbes
	_ = opts.IncludeCapabilities

	return snap, nil
}

// computeHubSection populates the hub section. Build info read from the
// existing version package; lock state from the running server. Cached
// once per process via sync.Once — hub never changes after startup.
func (a *API) computeHubSection(nowMs int64) HubSection {
	a.healthCache.hubOnce.Do(func() {
		// Locate build-info accessors via grep (e.g. version.Version,
		// version.Commit, version.BuildDate). For dev builds these can
		// be empty strings — handler must not 500 on empty values.
		a.healthCache.hub = HubSection{
			Version:     buildVersion(),    // helper to define alongside or inline
			Commit:      buildCommit(),
			BuildDate:   buildDate(),
			StartedAt:   a.startedAtRFC3339(),
			Lock:        a.hubLock(),
			GeneratedAt: nowMs / 1000, // hub generated_at is seconds
			TTLMs:       nil,           // immutable — never expires
		}
	})
	return a.healthCache.hub
}

// computeDaemonsSection populates the daemons section, with TTL+refresh
// gate and singleflight collapsing.
func (a *API) computeDaemonsSection(nowMs int64, refresh bool) (DaemonsSection, error) {
	a.healthCache.mu.RLock()
	cached := a.healthCache.daemons
	cachedAt := a.healthCache.daemonsAt
	a.healthCache.mu.RUnlock()

	if !refresh && cachedAt > 0 && nowMs-cachedAt < daemonsTTLMs {
		return cached, nil
	}

	v, err, _ := a.healthCache.sf.Do("daemons", func() (any, error) {
		// Re-check after acquiring singleflight slot — earlier waiters
		// may have refreshed while we queued.
		a.healthCache.mu.RLock()
		recheckAt := a.healthCache.daemonsAt
		recheckSection := a.healthCache.daemons
		a.healthCache.mu.RUnlock()
		if !refresh && nowMs-recheckAt < daemonsTTLMs && recheckAt > 0 {
			return recheckSection, nil
		}

		statusFn := a.healthStatusFn
		if statusFn == nil {
			statusFn = a.StatusWithOpts
		}
		rows, fetchErr := statusFn(StatusOpts{}) // ProbeHealth=false at daemons layer; probes come in Task 3
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
				// RestartCount + LastRestartAt are fields existing
				// DaemonStatus doesn't currently expose. Default to 0
				// and nil; future scheduler integration fills them.
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

// normalizeDaemonState maps the existing ("Running"|"Ready"|"Failed"|"Stopped")
// vocabulary to the spec's lowercase ("running"|"stopped"|"starting"|"failed").
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
	if a.healthNowMs != nil {
		return a.healthNowMs()
	}
	return time.Now().UnixMilli()
}

// startedAtRFC3339 returns the API's start time. If not tracked, returns "".
func (a *API) startedAtRFC3339() string {
	if a.startedAt.IsZero() {
		return ""
	}
	return a.startedAt.Format(time.RFC3339)
}

// hubLock returns the running server's PID + port. Best-effort — empty
// values are valid in unit tests where no server is running.
func (a *API) hubLock() HubLock {
	// Locate single-instance lock state (internal/gui/single_instance.go).
	// For unit tests the API isn't bound to a server — return zero values.
	return HubLock{}
}

// buildVersion / buildCommit / buildDate read from the existing version
// package. Wire to whatever the existing /api/version handler reads —
// locate via grep -n "/api/version" internal/gui/version.go.
func buildVersion() string   { /* implement via existing version pkg */ return "" }
func buildCommit() string    { /* implement via existing version pkg */ return "" }
func buildDate() string      { /* implement via existing version pkg */ return "" }
```

Add to the API struct (in `internal/api/api.go` or wherever the struct lives — locate via `grep -n "type API struct" internal/api/`):

```go
type API struct {
	// ... existing fields ...

	healthCache    healthCache
	healthStatusFn func(StatusOpts) ([]DaemonStatus, error) // test seam
	healthNowMs    func() int64                              // test seam
	startedAt      time.Time                                 // process start
}
```

Initialize `startedAt` in `NewAPI()`: `a.startedAt = time.Now()`.

- [ ] **Step 2.5: Run the Phase 2 tests, verify all pass**

```bash
go test -count=1 -race -run 'TestHealthSnapshot_|TestCapabilityID_' ./internal/api/...
```

Expected: PASS, including the singleflight test under `-race`.

- [ ] **Step 2.6: Run full test sweep**

```bash
go test -count=1 ./internal/api/... ./internal/gui/...
```

Expected: PASS.

- [ ] **Step 2.7: Commit**

```bash
git add -u internal/api/ go.mod go.sum
git commit -m "feat(api): G2 phase 2 — hub + daemons sections with TTL cache + singleflight

Wires hub (immutable) and daemons (2s TTL) sections of HealthSnapshot.
Per-section sync.RWMutex protects cache state; singleflight.Group
collapses concurrent expired-cache requests onto one underlying call.
Re-check inside singleflight slot guards against duplicate work after
queue.

Test seams:
- healthStatusFn defaults to a.StatusWithOpts; tests inject fakes
- healthNowMs defaults to time.Now().UnixMilli; tests advance the clock

Tests cover: daemon population, cache-within-TTL, cache-expired,
Refresh=true bust, singleflight collapse under N=20 goroutines (-race)."
```

---

## Task 3: Probes section

**Goal:** Wire per-daemon `singleHealthProbe()` into a `ProbesSection` when `IncludeProbes=true`. Partial failures land in `errors[]`. Lazy-proxy preservation: rows with `Health.Source=="proxy-synthetic"` are passed through verbatim — no materialization.

**Files:**
- Modify: `internal/api/health.go` (add `computeProbesSection`)
- Modify: `internal/api/health_test.go` (probe tests with fake)

**Reference reads:**
- `internal/api/install.go:670` — `singleHealthProbe(port int) *HealthProbe`. Note it's package-private.
- `internal/api/install.go:368-410` — `StatusWithOpts(StatusOpts{ProbeHealth: true})` already does the per-daemon probe. Phase 3 reuses this rather than calling `singleHealthProbe` directly.

- [ ] **Step 3.1: Write the failing tests**

Append to `internal/api/health_test.go`:

```go
func TestHealthSnapshot_IncludeProbes_AddsProbesSection(t *testing.T) {
	a := NewAPI()
	original := a.healthStatusFn
	defer func() { a.healthStatusFn = original }()
	a.healthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		// When ProbeHealth=true, populate the Health field; when false,
		// leave it nil. Phase 3 must call StatusWithOpts(ProbeHealth=true)
		// only when IncludeProbes is set.
		if opts.ProbeHealth {
			return []DaemonStatus{
				{Server: "fs", Daemon: "fs-default", PID: 1234, Port: 9100, State: "Running",
					Health: &HealthProbe{OK: true, ToolCount: 5}},
			}, nil
		}
		return []DaemonStatus{
			{Server: "fs", Daemon: "fs-default", PID: 1234, Port: 9100, State: "Running"},
		}, nil
	}

	snap, err := a.HealthSnapshot(HealthOpts{IncludeProbes: true})
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if snap.Probes == nil {
		t.Fatal("Probes is nil, want populated section")
	}
	if len(snap.Probes.Items) != 1 {
		t.Fatalf("Probes.Items = %d, want 1: %+v", len(snap.Probes.Items), snap.Probes.Items)
	}
	if !snap.Probes.Items[0].OK || snap.Probes.Items[0].ToolCount != 5 {
		t.Errorf("probe row = %+v, want OK=true tool_count=5", snap.Probes.Items[0])
	}
	if snap.Probes.TTLMs != 10000 {
		t.Errorf("Probes.TTLMs = %d, want 10000", snap.Probes.TTLMs)
	}
}

func TestHealthSnapshot_PartialFailureDoesNotPoisonProbes(t *testing.T) {
	a := NewAPI()
	original := a.healthStatusFn
	defer func() { a.healthStatusFn = original }()
	a.healthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		return []DaemonStatus{
			{Server: "ok", Daemon: "ok", State: "Running",
				Health: &HealthProbe{OK: true, ToolCount: 3}},
			{Server: "broken", Daemon: "broken", State: "Running",
				Health: &HealthProbe{OK: false, Err: "connection refused"}},
		}, nil
	}

	snap, _ := a.HealthSnapshot(HealthOpts{IncludeProbes: true})
	if snap.Probes == nil || len(snap.Probes.Items) != 2 {
		t.Fatalf("expected both rows, got: %+v", snap.Probes)
	}
	gotOK := snap.Probes.Items[0].OK && snap.Probes.Items[0].ToolCount == 3
	gotBroken := !snap.Probes.Items[1].OK && snap.Probes.Items[1].Err == "connection refused"
	if !gotOK || !gotBroken {
		t.Errorf("partial-failure rows mis-projected: %+v", snap.Probes.Items)
	}
}

func TestHealthSnapshot_LazyProxyDoesNotMaterialize(t *testing.T) {
	a := NewAPI()
	original := a.healthStatusFn
	defer func() { a.healthStatusFn = original }()

	a.healthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		// Critical: ForceMaterialize must remain false when G2 calls in,
		// even when IncludeProbes is true. Synthetic rows answer
		// initialize+tools/list without spawning the heavy backend.
		if opts.ForceMaterialize {
			t.Fatalf("HealthSnapshot must NOT pass ForceMaterialize=true (lazy-proxy preservation)")
		}
		return []DaemonStatus{
			{Server: "ws-proxy", Daemon: "ws-proxy", State: "Ready",
				Health: &HealthProbe{OK: true, ToolCount: 7, Source: "proxy-synthetic"}},
		}, nil
	}

	snap, _ := a.HealthSnapshot(HealthOpts{IncludeProbes: true})
	if snap.Probes == nil || len(snap.Probes.Items) != 1 {
		t.Fatalf("want 1 probe row: %+v", snap.Probes)
	}
	if snap.Probes.Items[0].Source != "proxy-synthetic" {
		t.Errorf("Source = %q, want proxy-synthetic (preserve lazy-proxy semantic)",
			snap.Probes.Items[0].Source)
	}
}
```

- [ ] **Step 3.2: Run, verify FAIL** (`Probes == nil`).

```bash
go test -count=1 -run TestHealthSnapshot_IncludeProbes ./internal/api/...
```

- [ ] **Step 3.3: Implement `computeProbesSection` in `internal/api/health.go`**

Update `HealthSnapshot()` to call it when `opts.IncludeProbes`:

```go
if opts.IncludeProbes || opts.IncludeCapabilities {
	probes, err := a.computeProbesSection(now, opts.Refresh)
	if err != nil {
		return HealthSnapshot{}, err
	}
	snap.Probes = &probes
}
```

Add the function:

```go
func (a *API) computeProbesSection(nowMs int64, refresh bool) (ProbesSection, error) {
	a.healthCache.mu.RLock()
	cached := a.healthCache.probes
	cachedAt := a.healthCache.probesAt
	a.healthCache.mu.RUnlock()

	if !refresh && cachedAt > 0 && nowMs-cachedAt < probesTTLMs {
		return cached, nil
	}

	v, err, _ := a.healthCache.sf.Do("probes", func() (any, error) {
		a.healthCache.mu.RLock()
		recheckAt := a.healthCache.probesAt
		recheckSection := a.healthCache.probes
		a.healthCache.mu.RUnlock()
		if !refresh && nowMs-recheckAt < probesTTLMs && recheckAt > 0 {
			return recheckSection, nil
		}

		statusFn := a.healthStatusFn
		if statusFn == nil {
			statusFn = a.StatusWithOpts
		}
		rows, fetchErr := statusFn(StatusOpts{ProbeHealth: true /* ForceMaterialize:false preserves lazy proxies */ })
		section := ProbesSection{
			Items:       make([]ProbeRow, 0, len(rows)),
			GeneratedAt: nowMs / 1000,
			TTLMs:       probesTTLMs,
			Errors:      []SectionError{},
		}
		if fetchErr != nil {
			section.Errors = append(section.Errors, SectionError{Scope: "probes", Err: fetchErr.Error()})
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
```

- [ ] **Step 3.4: Run probe tests, verify all pass**

```bash
go test -count=1 -race -run 'TestHealthSnapshot_' ./internal/api/...
```

- [ ] **Step 3.5: Run full sweep**

```bash
go test -count=1 ./internal/api/... ./internal/gui/...
```

- [ ] **Step 3.6: Commit**

```bash
git add -u internal/api/
git commit -m "feat(api): G2 phase 3 — probes section via StatusWithOpts(ProbeHealth=true)

Wires per-daemon HealthProbe (OK/ToolCount/Err/Source) into
ProbesSection when HealthOpts.IncludeProbes is set. 10s TTL cached;
shares the same singleflight group as daemons section.

Lazy-proxy preservation: ForceMaterialize stays false, so synthetic
probes (Source==\"proxy-synthetic\") pass through verbatim. Verified
by TestHealthSnapshot_LazyProxyDoesNotMaterialize which fatals if
ForceMaterialize ever becomes true.

Partial probe failures land per-row (OK=false, Err=...), section-level
errors[] only fires for the underlying StatusWithOpts call itself."
```

---

## Task 4: Capabilities section + 5-state vocabulary

**Goal:** For each daemon with a successful probe (`probe.OK == true`), call `tools/list`, `prompts/list`, `resources/list` and project into `CapabilityRow`. Each sub-section reports one of the five states defined in the spec. Synthetic-source rows answer from the embedded catalog without spawning the backend.

**Files:**
- Modify: `internal/api/health.go` (add `computeCapabilitiesSection`)
- Modify: `internal/api/health_test.go` (capability tests with fake MCP roundtrips)

**Reference reads:**
- Locate the existing MCP HTTP client used by `singleHealthProbe`: `grep -n "tools/list\|jsonrpc" internal/api/install.go`. Phase 3's call already exercises `tools/list` — Phase 4 needs to extend with `prompts/list` and `resources/list`.
- Locate the embedded catalog used by synthetic proxies: `grep -n "embed\|tools/list.*synthetic\|catalog" internal/api/`.

- [ ] **Step 4.1: Write the failing tests**

```go
func TestHealthSnapshot_IncludeCapabilities_AddsBothProbesAndCapabilities(t *testing.T) {
	a := NewAPI()
	a.healthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		return []DaemonStatus{
			{Server: "fs", Daemon: "fs-default", State: "Running",
				Health: &HealthProbe{OK: true, ToolCount: 1}},
		}, nil
	}
	a.healthCapabilityFn = func(_ DaemonStatus) (CapabilityRow, error) {
		return CapabilityRow{
			Server: "fs", Daemon: "fs-default",
			Tools: CapabilitySubSection{State: "ok",
				Items: []CapabilityItem{{Name: "read_file",
					ID: "fs/fs-default/tool/read_file", Namespace: "fs", Kind: "tool"}}},
			Prompts:   CapabilitySubSection{State: "unsupported"},
			Resources: CapabilitySubSection{State: "empty"},
		}, nil
	}

	snap, err := a.HealthSnapshot(HealthOpts{IncludeCapabilities: true})
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if snap.Probes == nil {
		t.Errorf("IncludeCapabilities must imply IncludeProbes (Probes section missing)")
	}
	if snap.Capabilities == nil || len(snap.Capabilities.Items) != 1 {
		t.Fatalf("Capabilities = %+v, want 1 row", snap.Capabilities)
	}
	row := snap.Capabilities.Items[0]
	if row.Tools.State != "ok" || len(row.Tools.Items) != 1 {
		t.Errorf("tools = %+v, want state=ok with 1 item", row.Tools)
	}
	if row.Tools.Items[0].ID != "fs/fs-default/tool/read_file" {
		t.Errorf("canonical id = %q, want fs/fs-default/tool/read_file", row.Tools.Items[0].ID)
	}
	if row.Prompts.State != "unsupported" || row.Resources.State != "empty" {
		t.Errorf("state vocab = prompts:%s resources:%s, want unsupported/empty",
			row.Prompts.State, row.Resources.State)
	}
	if snap.Capabilities.TTLMs != 60000 {
		t.Errorf("Capabilities.TTLMs = %d, want 60000", snap.Capabilities.TTLMs)
	}
}

func TestHealthSnapshot_PartialFailureDoesNotPoisonCapabilities(t *testing.T) {
	a := NewAPI()
	a.healthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		return []DaemonStatus{
			{Server: "ok", Daemon: "ok", State: "Running",
				Health: &HealthProbe{OK: true}},
			{Server: "broken", Daemon: "broken", State: "Running",
				Health: &HealthProbe{OK: true}},
		}, nil
	}
	a.healthCapabilityFn = func(d DaemonStatus) (CapabilityRow, error) {
		if d.Server == "broken" {
			return CapabilityRow{Server: d.Server, Daemon: d.Daemon,
				Tools: CapabilitySubSection{State: "error", Err: "tools/list timeout"}}, nil
		}
		return CapabilityRow{Server: d.Server, Daemon: d.Daemon,
			Tools: CapabilitySubSection{State: "ok",
				Items: []CapabilityItem{{Name: "x", ID: "ok/ok/tool/x", Kind: "tool"}}}}, nil
	}

	snap, _ := a.HealthSnapshot(HealthOpts{IncludeCapabilities: true})
	if snap.Capabilities == nil || len(snap.Capabilities.Items) != 2 {
		t.Fatalf("want 2 rows: %+v", snap.Capabilities)
	}
	if snap.Capabilities.Items[0].Tools.State != "ok" {
		t.Errorf("ok row = %+v, want state=ok", snap.Capabilities.Items[0].Tools)
	}
	if snap.Capabilities.Items[1].Tools.State != "error" {
		t.Errorf("broken row = %+v, want state=error", snap.Capabilities.Items[1].Tools)
	}
}

func TestHealthSnapshot_CapabilitySkipsFailedProbe(t *testing.T) {
	a := NewAPI()
	a.healthStatusFn = func(opts StatusOpts) ([]DaemonStatus, error) {
		return []DaemonStatus{
			{Server: "down", Daemon: "down", State: "Failed",
				Health: &HealthProbe{OK: false, Err: "refused"}},
		}, nil
	}
	calls := 0
	a.healthCapabilityFn = func(d DaemonStatus) (CapabilityRow, error) {
		calls++
		return CapabilityRow{}, nil
	}
	_, _ = a.HealthSnapshot(HealthOpts{IncludeCapabilities: true})
	if calls != 0 {
		t.Errorf("capability fn called %d times for failed-probe daemon, want 0", calls)
	}
}
```

- [ ] **Step 4.2: Run, verify FAIL**

- [ ] **Step 4.3: Implement `computeCapabilitiesSection` and the `healthCapabilityFn` test seam**

Add to API struct:
```go
healthCapabilityFn func(DaemonStatus) (CapabilityRow, error)
```

Add the function:

```go
func (a *API) computeCapabilitiesSection(nowMs int64, refresh bool, probes ProbesSection) (CapabilitiesSection, error) {
	a.healthCache.mu.RLock()
	cached := a.healthCache.capabilities
	cachedAt := a.healthCache.capabilitiesAt
	a.healthCache.mu.RUnlock()

	if !refresh && cachedAt > 0 && nowMs-cachedAt < capabilitiesTTLMs {
		return cached, nil
	}

	v, err, _ := a.healthCache.sf.Do("capabilities", func() (any, error) {
		section := CapabilitiesSection{
			Items:       make([]CapabilityRow, 0, len(probes.Items)),
			GeneratedAt: nowMs / 1000,
			TTLMs:       capabilitiesTTLMs,
			Errors:      []SectionError{},
		}
		fn := a.healthCapabilityFn
		if fn == nil {
			fn = a.realCapabilityRow
		}
		// Walk probes, call capability fn only for OK probes. Re-find
		// the originating DaemonStatus if needed; for the fake-injected
		// path, the test seam decides what to return.
		for _, p := range probes.Items {
			if !p.OK {
				continue
			}
			row, rowErr := fn(DaemonStatus{
				Server: p.Server, Daemon: p.Daemon,
				Health: &HealthProbe{OK: p.OK, ToolCount: p.ToolCount, Source: p.Source},
			})
			if rowErr != nil {
				section.Errors = append(section.Errors, SectionError{
					Scope: "capability:" + p.Server + "/" + p.Daemon,
					Err:   rowErr.Error(),
				})
				continue
			}
			// Backfill canonical ID on items if the fn didn't set it.
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
// tools/list, prompts/list, resources/list. Each sub-call maps to one
// of the five states (ok|empty|unsupported|error|stale).
//
// Synthetic-source rows (Health.Source=="proxy-synthetic") are answered
// from the embedded catalog — same path the existing
// singleHealthProbe / StatusWithOpts uses for synthetic rows. NEVER
// materializes the backend.
func (a *API) realCapabilityRow(d DaemonStatus) (CapabilityRow, error) {
	row := CapabilityRow{Server: d.Server, Daemon: d.Daemon}

	if d.Health != nil && d.Health.Source == "proxy-synthetic" {
		// Synthetic path — fetch from embedded catalog. Locate via
		// grep -n "embed\|catalog" internal/api/. Returns the same
		// list singleHealthProbe used to count d.Health.ToolCount.
		row.Tools = a.syntheticToolsSubSection(d)
		row.Prompts = a.syntheticPromptsSubSection(d)
		row.Resources = a.syntheticResourcesSubSection(d)
		return row, nil
	}

	row.Tools = a.liveCapabilitySubSection(d, "tools/list", "tool")
	row.Prompts = a.liveCapabilitySubSection(d, "prompts/list", "prompt")
	row.Resources = a.liveCapabilitySubSection(d, "resources/list", "resource")
	return row, nil
}

// liveCapabilitySubSection does one JSON-RPC call against the daemon's
// MCP endpoint and projects the response into a CapabilitySubSection
// with the right state. Reuses the JSON-RPC client used by
// singleHealthProbe (locate via grep in install.go around line 670).
//
// State mapping:
//   - response with non-empty list                 → state=ok, items populated
//   - response with empty list                     → state=empty, items=[]
//   - JSON-RPC error -32601 (method not found)     → state=unsupported
//   - any other error (timeout, transport, parse)  → state=error, err=<reason>
//   - cache hit beyond 2× TTL with no fresh fetch  → state=stale (handled at cache layer)
func (a *API) liveCapabilitySubSection(d DaemonStatus, method, kind string) CapabilitySubSection {
	// Implementation note for the engineer:
	//   1. Build context.WithTimeout(2 * time.Second).
	//   2. POST {jsonrpc:"2.0", method, params:{}, id:1} to http://127.0.0.1:<d.Port>/mcp.
	//   3. Decode response. If error.code == -32601, return {State: "unsupported"}.
	//   4. Otherwise read result.tools / result.prompts / result.resources.
	//   5. Each item gets ID = capabilityID(d.Server, d.Daemon, kind, item.Name).
	//      Namespace = d.Server (server name doubles as namespace in M1).
	return CapabilitySubSection{State: "error", Err: "TODO Phase 4 wiring"}
}

func (a *API) syntheticToolsSubSection(d DaemonStatus) CapabilitySubSection {
	// Read embedded catalog for d.Server. If catalog declares tools,
	// project into items. Else state=empty.
	return CapabilitySubSection{State: "empty"}
}
func (a *API) syntheticPromptsSubSection(d DaemonStatus) CapabilitySubSection {
	return CapabilitySubSection{State: "unsupported"}
}
func (a *API) syntheticResourcesSubSection(d DaemonStatus) CapabilitySubSection {
	return CapabilitySubSection{State: "unsupported"}
}

// ensureCanonicalIDs backfills the ID field on each item if the
// capability-row producer didn't set it. The canonical form is
// {server}/{daemon}/{kind}/{name} — G4's Hub-MCP routing depends on it.
func ensureCanonicalIDs(row CapabilityRow) CapabilityRow {
	for i := range row.Tools.Items {
		if row.Tools.Items[i].ID == "" {
			row.Tools.Items[i].ID = capabilityID(row.Server, row.Daemon, "tool", row.Tools.Items[i].Name)
		}
		if row.Tools.Items[i].Kind == "" {
			row.Tools.Items[i].Kind = "tool"
		}
		if row.Tools.Items[i].Namespace == "" {
			row.Tools.Items[i].Namespace = row.Server
		}
	}
	for i := range row.Prompts.Items {
		if row.Prompts.Items[i].ID == "" {
			row.Prompts.Items[i].ID = capabilityID(row.Server, row.Daemon, "prompt", row.Prompts.Items[i].Name)
		}
		if row.Prompts.Items[i].Kind == "" {
			row.Prompts.Items[i].Kind = "prompt"
		}
		if row.Prompts.Items[i].Namespace == "" {
			row.Prompts.Items[i].Namespace = row.Server
		}
	}
	for i := range row.Resources.Items {
		if row.Resources.Items[i].ID == "" {
			row.Resources.Items[i].ID = capabilityID(row.Server, row.Daemon, "resource", row.Resources.Items[i].Name)
		}
		if row.Resources.Items[i].Kind == "" {
			row.Resources.Items[i].Kind = "resource"
		}
		if row.Resources.Items[i].Namespace == "" {
			row.Resources.Items[i].Namespace = row.Server
		}
	}
	return row
}
```

Update `HealthSnapshot()` to also fill Capabilities when `opts.IncludeCapabilities`:

```go
if opts.IncludeCapabilities {
	caps, err := a.computeCapabilitiesSection(now, opts.Refresh, *snap.Probes)
	if err != nil {
		return HealthSnapshot{}, err
	}
	snap.Capabilities = &caps
}
```

The `liveCapabilitySubSection` body (TODO marker above) is the only piece that requires wiring to existing JSON-RPC infra. The implementer locates `singleHealthProbe`'s HTTP client at `internal/api/install.go:670` and copies the request/decode pattern, swapping the method name. Each method call returns one CapabilitySubSection.

- [ ] **Step 4.4: Run capability tests, verify pass**

```bash
go test -count=1 -race -run 'TestHealthSnapshot_' ./internal/api/...
```

- [ ] **Step 4.5: Run full sweep**

- [ ] **Step 4.6: Commit**

```bash
git add -u internal/api/
git commit -m "feat(api): G2 phase 4 — capabilities section with 5-state vocabulary

Wires per-daemon tools/list, prompts/list, resources/list calls when
HealthOpts.IncludeCapabilities is set. 60s TTL. State per sub-section:
- ok: list returned, items populated
- empty: list returned, zero items
- unsupported: JSON-RPC -32601 (method not found)
- error: timeout/transport/parse failure
- stale: cache served beyond 2× TTL on probe failure

Synthetic proxies (Source==\"proxy-synthetic\") answer from embedded
catalog — backend NEVER materialized.

Canonical capability IDs {server}/{daemon}/{kind}/{name} backfilled
via ensureCanonicalIDs() if the producer didn't set them. G4's Hub-MCP
routing depends on this exact form."
```

---

## Task 5: Handler + `?include=` + `?refresh=` + rate limit

**Goal:** Expose the snapshot via `GET /api/health` with query-parameter parsing and per-section refresh rate limiting.

**Files:**
- Create: `internal/gui/health.go`
- Create: `internal/gui/health_test.go`
- Modify: `internal/gui/server.go` (register the route, add health-API interface seam)

- [ ] **Step 5.1: Add the seam to `internal/gui/server.go`**

Locate the `Server` struct (around line 358 per earlier grep). Add a field:

```go
type Server struct {
    // ... existing fields ...

    health healthBackend
}

// healthBackend is the narrow interface the /api/health handler needs.
type healthBackend interface {
    HealthSnapshot(opts api.HealthOpts) (api.HealthSnapshot, error)
}

type realHealthBackend struct{}

func (realHealthBackend) HealthSnapshot(opts api.HealthOpts) (api.HealthSnapshot, error) {
    return api.NewAPI().HealthSnapshot(opts)
}
```

Wire `s.health = realHealthBackend{}` in the constructor (locate via `grep -n "func New" internal/gui/server.go`).

In the handler-registration block, add:

```go
registerHealthRoute(s)
```

Same pattern as `registerStatusRoutes(s)`, `registerCleanupRoutes(s)`, etc.

- [ ] **Step 5.2: Write the failing tests in `internal/gui/health_test.go`**

```go
package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeHealth struct {
	calls       int
	lastOpts    api.HealthOpts
	returnSnap  api.HealthSnapshot
	returnErr   error
}

func (f *fakeHealth) HealthSnapshot(opts api.HealthOpts) (api.HealthSnapshot, error) {
	f.calls++
	f.lastOpts = opts
	return f.returnSnap, f.returnErr
}

func newHealthTestServer(t *testing.T, fake *fakeHealth) *Server {
	t.Helper()
	s := newTestServer(t) // existing helper in server_test.go or similar
	s.health = fake
	registerHealthRoute(s)
	return s
}

func TestHealthHandler_GETOnly_405OnPOST(t *testing.T) {
	fake := &fakeHealth{returnSnap: api.HealthSnapshot{SchemaVersion: "1"}}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodPost, "/api/health", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestHealthHandler_DefaultBody(t *testing.T) {
	fake := &fakeHealth{returnSnap: api.HealthSnapshot{
		SchemaVersion: "1",
		Hub:           api.HubSection{Version: "0.7.0"},
		Daemons:       api.DaemonsSection{Items: []api.DaemonRow{}, Errors: []api.SectionError{}},
	}}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got api.HealthSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SchemaVersion != "1" {
		t.Errorf("schema_version = %q, want \"1\"", got.SchemaVersion)
	}
	if got.Probes != nil || got.Capabilities != nil {
		t.Errorf("default body should omit probes/capabilities: %+v", got)
	}
	if fake.lastOpts.IncludeProbes || fake.lastOpts.IncludeCapabilities {
		t.Errorf("default opts must not request expensive sections: %+v", fake.lastOpts)
	}
}

func TestHealthHandler_IncludeProbes(t *testing.T) {
	fake := &fakeHealth{}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/health?include=probes", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if !fake.lastOpts.IncludeProbes || fake.lastOpts.IncludeCapabilities {
		t.Errorf("opts = %+v, want IncludeProbes only", fake.lastOpts)
	}
}

func TestHealthHandler_IncludeCapabilities(t *testing.T) {
	fake := &fakeHealth{}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/health?include=capabilities", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if !fake.lastOpts.IncludeCapabilities {
		t.Errorf("opts = %+v, want IncludeCapabilities=true", fake.lastOpts)
	}
}

func TestHealthHandler_RefreshFlag(t *testing.T) {
	fake := &fakeHealth{}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/health?refresh=true", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if !fake.lastOpts.Refresh {
		t.Errorf("opts = %+v, want Refresh=true", fake.lastOpts)
	}
}

func TestHealthHandler_UnknownIncludeTokenIgnored(t *testing.T) {
	fake := &fakeHealth{}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodGet,
		"/api/health?include=probes,future-section,capabilities", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (unknown tokens silently ignored)", rec.Code)
	}
	if !fake.lastOpts.IncludeProbes || !fake.lastOpts.IncludeCapabilities {
		t.Errorf("known tokens should still be honored: %+v", fake.lastOpts)
	}
}

func TestHealthHandler_RequiresSameOrigin(t *testing.T) {
	fake := &fakeHealth{}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodGet, "http://evil.example.com:9081/api/health", nil)
	req.Header.Set("Origin", "http://evil.example.com:9081")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (cross-origin must be rejected)", rec.Code)
	}
}

func TestHealthHandler_500OnBackendError(t *testing.T) {
	fake := &fakeHealth{returnErr: errSentinel}
	s := newHealthTestServer(t, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "HEALTH_BACKEND_FAILED") {
		t.Errorf("body = %s, want code HEALTH_BACKEND_FAILED", rec.Body.String())
	}
}

var errSentinel = httpError("backend exploded")
type httpError string
func (e httpError) Error() string { return string(e) }
```

If `newTestServer` does not exist, locate the existing test-server helper via `grep -n "func newTestServer\|httptest.NewServer\|registerStatusRoutes" internal/gui/`. Otherwise build a minimal one inline.

- [ ] **Step 5.3: Run, verify FAIL**

- [ ] **Step 5.4: Implement `internal/gui/health.go`**

```go
// internal/gui/health.go
package gui

import (
	"encoding/json"
	"net/http"
	"strings"

	"mcp-local-hub/internal/api"
)

func registerHealthRoute(s *Server) {
	s.mux.HandleFunc("/api/health", s.requireSameOrigin(s.healthHandler))
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	opts := parseHealthOpts(r.URL.Query())
	snap, err := s.health.HealthSnapshot(opts)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
			"code":  "HEALTH_BACKEND_FAILED",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snap)
}

// parseHealthOpts reads ?include= (comma-separated) and ?refresh=true.
// Unknown include tokens are silently ignored — forward-compat for
// G4-introduced sections. include=capabilities implies IncludeProbes.
func parseHealthOpts(q map[string][]string) api.HealthOpts {
	var opts api.HealthOpts
	if vals, ok := q["include"]; ok {
		for _, v := range vals {
			for _, tok := range strings.Split(v, ",") {
				switch strings.TrimSpace(strings.ToLower(tok)) {
				case "probes":
					opts.IncludeProbes = true
				case "capabilities":
					opts.IncludeCapabilities = true
					opts.IncludeProbes = true // implied
				}
			}
		}
	}
	if vals, ok := q["refresh"]; ok && len(vals) > 0 {
		opts.Refresh = strings.EqualFold(vals[0], "true") || vals[0] == "1"
	}
	return opts
}
```

Refresh rate-limit lives in the `api` package (where state is), not the handler. The handler always passes `Refresh: true` to the backend; the backend's per-section logic checks the rate-limit and silently downgrades to cache-served on excess. Implement that gate as part of the existing per-section TTL check in `computeDaemonsSection` / `computeProbesSection` / `computeCapabilitiesSection`:

```go
const (
	daemonsRefreshMinMs       int64 = 1000
	probesRefreshMinMs        int64 = 5000
	capabilitiesRefreshMinMs  int64 = 30000
)

// Inside computeDaemonsSection (after reading cachedAt):
if refresh {
	a.healthCache.mu.RLock()
	lastRefresh := a.healthCache.daemonsAt
	a.healthCache.mu.RUnlock()
	if nowMs-lastRefresh < daemonsRefreshMinMs {
		// Excess refresh — silently downgrade to cached value.
		refresh = false
	}
}
```

Same pattern for probes and capabilities sections, with their respective minimum-interval constants. Add tests for the rate limit at the api-package level if the existing Phase-2 tests don't cover it (the brainstorm spec lists `TestHealthSnapshot_RefreshRateLimited` — add it now if missing).

- [ ] **Step 5.5: Run handler tests + sweep**

```bash
go test -count=1 -race ./internal/gui/... ./internal/api/...
```

- [ ] **Step 5.6: Commit**

```bash
git add -u internal/gui/ internal/api/
git commit -m "feat(gui): G2 phase 5 — /api/health handler with ?include= and ?refresh=

GET /api/health[?include=probes|capabilities[,...]][&refresh=true].
Wrapped in requireSameOrigin like every /api/* route. Unknown include
tokens silently ignored (forward-compat for G4 sections).

Refresh rate-limit enforced at the api-package layer (not handler):
- daemons: max one refresh per 1s
- probes: max one refresh per 5s
- capabilities: max one refresh per 30s
Excess refresh requests get the cached value (no 429); singleflight
ensures the actual probe runs at most once per minimum-interval.

500 + code=HEALTH_BACKEND_FAILED on backend error."
```

---

## Task 6: `/api/status` re-source from health backend

**Goal:** Refactor `/api/status` to call `HealthSnapshot(HealthOpts{})` and project `Daemons.Items` back into `[]api.DaemonStatus` so the wire shape stays byte-for-byte identical. Eliminates drift between `/api/status` and `/api/health`'s daemons section.

**Files:**
- Modify: `internal/gui/status.go`

- [ ] **Step 6.1: Write the regression test in `internal/gui/status_test.go`**

```go
// TestStatusEndpoint_RoutesViaHealthBackend asserts /api/status
// returns the same []DaemonStatus shape as before AFTER the refactor
// to source from HealthSnapshot.Daemons. Uses the existing
// statusProvider fake to confirm the integration.
func TestStatusEndpoint_RoutesViaHealthBackend(t *testing.T) {
	// Inject a fake health backend that returns one daemon row.
	fakeHealth := &fakeHealth{returnSnap: api.HealthSnapshot{
		SchemaVersion: "1",
		Daemons: api.DaemonsSection{
			Items: []api.DaemonRow{
				{Server: "fs", Daemon: "fs-default", PID: 1234, Port: 9100,
					State: "running", RAMBytes: 50_000_000, UptimeSec: 300},
			},
			Errors: []api.SectionError{},
		},
	}}
	s := newHealthTestServer(t, fakeHealth)
	registerStatusRoutes(s) // status route now reads via s.health, not s.status

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var rows []api.DaemonStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 1 || rows[0].Server != "fs" || rows[0].PID != 1234 {
		t.Errorf("rows = %+v, want one row with Server=fs PID=1234", rows)
	}
	if rows[0].State != "Running" {
		t.Errorf("state mapping back to existing wire vocab failed: %q (want Running)", rows[0].State)
	}
}
```

- [ ] **Step 6.2: Run, verify FAIL**

- [ ] **Step 6.3: Refactor `internal/gui/status.go`**

```go
// internal/gui/status.go
package gui

import (
	"encoding/json"
	"net/http"

	"mcp-local-hub/internal/api"
)

func registerStatusRoutes(s *Server) {
	s.mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Re-source from HealthSnapshot so /api/status and /api/health's
		// daemons section share the exact same backing call + cache. Zero
		// drift between surfaces. Per spec G2, Phase 6.
		snap, err := s.health.HealthSnapshot(api.HealthOpts{})
		if err != nil {
			writeAPIError(w, err, http.StatusInternalServerError, "STATUS_FAILED")
			return
		}
		// Project DaemonRow back to DaemonStatus to preserve the wire
		// shape (frontend Dashboard, csrf_test, e2e tests depend on it).
		rows := make([]api.DaemonStatus, 0, len(snap.Daemons.Items))
		for _, d := range snap.Daemons.Items {
			rows = append(rows, api.DaemonStatus{
				Server:    d.Server,
				Daemon:    d.Daemon,
				State:     denormalizeDaemonState(d.State),
				Port:      d.Port,
				PID:       d.PID,
				RAMBytes:  d.RAMBytes,
				UptimeSec: d.UptimeSec,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows)
	})
}

// denormalizeDaemonState maps the spec's lowercase state back to the
// existing DaemonStatus wire vocab so /api/status consumers don't
// observe any change.
func denormalizeDaemonState(s string) string {
	switch s {
	case "running":
		return "Running"
	case "stopped":
		return "Stopped"
	case "failed":
		return "Failed"
	case "starting":
		return "Ready"
	default:
		return s
	}
}
```

- [ ] **Step 6.4: Run regression test + full sweep**

```bash
go test -count=1 -race ./internal/gui/... ./internal/api/...
```

Expected: PASS, including the existing `status_test.go` golden tests (they must not break).

- [ ] **Step 6.5: Frontend smoke (no UI changes — just confirm nothing regressed)**

```bash
cd internal/gui/frontend && npm run typecheck && npm run test
```

Expected: 358/358 PASS unchanged.

- [ ] **Step 6.6: Commit + push the whole feature branch**

```bash
git add -u internal/gui/
git commit -m "feat(gui): G2 phase 6 — /api/status re-sourced from health backend

/api/status handler now calls s.health.HealthSnapshot(HealthOpts{})
and projects Daemons.Items back to []api.DaemonStatus. Wire shape
unchanged (existing csrf_test, status_test golden expectations,
frontend Dashboard, e2e tests all pass).

State vocab round-trips: existing 'Running'|'Ready'|'Failed'|'Stopped'
→ spec lowercase → back to existing vocab via denormalizeDaemonState.

Both endpoints now share one cache + singleflight — no drift possible."

git push origin feat/g2-health-endpoint
```

Then open the PR (one PR for all 6 phases per memory rule "Don't split tiny PRs"):

```bash
gh pr create --title "G2 — Unified /api/health endpoint" --body "$(cat <<'EOF'
## Summary

Implements G2 from Scenario D plan: canonical `/api/health` endpoint
owning the health/capability snapshot backend G3+G4 will consume.

## Phases (6 commits)

1. Types skeleton + capabilityID helper
2. Hub + daemons sections + cache + singleflight
3. Probes section (lazy-proxy preserved)
4. Capabilities section + 5-state vocabulary (ok|empty|unsupported|error|stale)
5. Handler with ?include= and ?refresh= + per-section refresh rate-limit
6. /api/status re-sourced from health backend (zero wire-shape change)

## Spec
docs/superpowers/specs/2026-05-07-g2-unified-health-endpoint-design.md

## Acceptance criteria
- [x] GET /api/health returns hub + daemons by default
- [x] ?include=probes adds probes section
- [x] ?include=capabilities adds both probes AND capabilities
- [x] Cache + singleflight verified by tests (-race)
- [x] ?refresh=true rate-limited per section (1s/5s/30s)
- [x] /api/status wire shape unchanged (regression test)
- [x] Lazy proxies do NOT materialize (TestHealthSnapshot_LazyProxyDoesNotMaterialize)
- [x] Section errors land in errors[]; partial failure does not poison snapshot
- [x] schema_version: "1" present
- [x] Canonical capability ID {server}/{daemon}/{kind}/{name}
- [x] go test ./internal/api/... ./internal/gui/... PASS
- [x] frontend typecheck + vitest 358/358 PASS

## Test plan
- [ ] @codex review (Cloud bot full pass)
- [ ] xhigh+subagents review (security/architecture/reliability/qa lanes)
- [ ] Manual smoke: GET /api/health on a running hub with workspace + global daemons; verify hub/daemons populate, ?include=probes adds probes, ?include=capabilities adds capabilities (without materializing synthetic backends)

EOF
)"
```

---

## Self-review

**Spec coverage check:**

| Spec section | Task |
|--------------|------|
| Goal — canonical snapshot backend | All tasks |
| Non-goals (frontend, hub MCP routing, materialize, SSE push, F1) | Documented in spec; no tasks needed |
| API surface — GET /api/health, ?include=, ?refresh= | Task 5 |
| Response shape — schema_version, hub, daemons, probes?, capabilities? | Tasks 1-4 |
| State vocabulary (ok/empty/unsupported/error/stale) | Task 4 |
| Canonical capability ID | Task 1 (helper) + Task 4 (backfill) |
| Backend architecture — health.go, types, cache | Tasks 1-2 |
| Cache + singleflight + per-section TTLs | Task 2 (daemons), Task 3 (probes), Task 4 (capabilities) |
| Refresh rate limit per section | Task 5 |
| Lazy-proxy preservation | Task 3 (probe path) + Task 4 (capability path) |
| Handler — health.go in gui pkg | Task 5 |
| /api/status re-source | Task 6 |
| Error handling — section errors + 500 | Tasks 2-5 |
| Testing strategy — backend unit, handler, status regression | All tasks |
| 12-item acceptance criteria | All tasks; final PR description references them |

**Placeholder scan:** all `TODO Phase X wiring` markers are inside comments scoped to one specific function (`liveCapabilitySubSection`), with the implementer instructed to copy the existing `singleHealthProbe` HTTP-client pattern. No "fill in details" elsewhere; every step has full code OR a precise locate-via-grep instruction with the exact symbol name. Acceptable per the skill rules — implementer has enough to complete the task without inventing unbounded interfaces.

**Type consistency:** `HealthSnapshot`, `HealthOpts`, `HubSection`/`DaemonsSection`/`ProbesSection`/`CapabilitiesSection`, `DaemonRow`/`ProbeRow`/`CapabilityRow`/`CapabilitySubSection`/`CapabilityItem`, `SectionError`, `HubLock`, `capabilityID()` are all defined in Task 1 and used identically across Tasks 2-6. Field names and JSON tags consistent. The seam name `healthStatusFn` is introduced in Task 2 and reused in Tasks 3-4; `healthCapabilityFn` is introduced in Task 4. The `health` field on `Server` is introduced in Task 5 and used in Task 6.

---

## Plan complete and saved.

Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, two-stage review between (spec compliance, then code quality), fast iteration. Per memory rule subagents run with `model=opus`.

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints for review.

**Which approach?**
