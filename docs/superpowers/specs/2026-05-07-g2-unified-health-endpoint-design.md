# G2 — Unified `/api/health` endpoint design

**Status:** approved 2026-05-07 (post-brainstorm). Owns canonical health/capability snapshot backend that G3 (capability status display, read-only) and G4 (opt-in unified Hub MCP endpoint) consume.

**Branch base:** `master` HEAD `2cea4cf` (PR #131 merged).

**Codex consult:** advisory at `.scratch/g2-codex-consult-prompt.md`. All five design choices below align with Codex's xhigh recommendations.

---

## Goal

Single canonical backend producing a typed snapshot of hub-self info, per-daemon process state, per-server MCP probe results, and per-server capability lists. Exposed via one HTTP endpoint with opt-in expensive sections. G3 reuses the same backend to render capability status; G4 reuses the same `{server, daemon, kind, name, namespace}` capability ID for Hub-MCP routing.

## Non-goals (deferred to G3 / G4 / Cleanup-6)

- Frontend UI for capability display — G3.
- Hub MCP routing implementation — G4.
- Workspace-scoped lazy proxy materialization on probe — preserve `source="proxy-synthetic"` semantic, do NOT materialize.
- WebSocket / SSE push for capability changes — G2 ships polling + cache; reactive pushes (if needed) handled in G3+.
- Linux-server scheduler probe paths — `F1`-tracked separately, not pulled in.

---

## API surface

### `GET /api/health`

Default returns cheap sections only:

```jsonc
{
  "schema_version": "1",
  "hub": { /* see below */ },
  "daemons": { /* see below */ }
}
```

Optional `?include=` query parameter adds expensive sections:

- `?include=probes` adds `probes` section (per-server `HealthProbe` results).
- `?include=capabilities` adds both `probes` AND `capabilities` (capability discovery depends on a successful probe).
- `?include=probes,capabilities` is equivalent to `?include=capabilities`.

Optional `?refresh=true` busts the cache for the requested sections (rate-limited — see "Refresh rate limit" below).

Method-restricted to `GET`. Wrapped in `requireSameOrigin` like every other `/api/*` route.

### `GET /api/status` — preserved URL, new source

Existing route at `internal/gui/status.go:10`. JSON shape stays exactly as it is today (frontend Dashboard, `csrf_test.go`, `status_test.go`, e2e tests depend on it). Implementation re-sources from the new health backend internally — `/api/status` becomes `health.Snapshot().Daemons.Items` projected into the existing `StatusResponse` shape. Zero drift between the two surfaces.

### `GET /api/version` — unchanged

Stays as-is. The `hub` section of `/api/health` includes the same fields plus `started_at` and `lock.{pid,port}` for completeness, but `/api/version` itself is not refactored — too small to be worth the touch.

---

## Response shape

```jsonc
{
  "schema_version": "1",

  "hub": {
    "version": "0.7.0",
    "commit": "2cea4cf",
    "build_date": "2026-05-07T03:00:00Z",
    "started_at": "2026-05-07T03:30:00Z",
    "lock": { "pid": 12345, "port": 9125 },
    "generated_at": 1714752000,
    "ttl_ms": null      // immutable per process — never expires
  },

  "daemons": {
    "items": [
      {
        "server": "fs",
        "daemon": "fs-default",
        "pid": 1234, "port": 9100,
        "ram_bytes": 50000000,
        "uptime_sec": 300,
        "state": "running",        // "running" | "stopped" | "starting" | "failed" | "unknown"
                                   //   (v0.6 Workstream B / PR #281): the enum gained "unknown".
                                   //   Polarity: a genuinely UNRECOGNIZED or blank source state maps
                                   //   to "unknown", NOT "failed" (the prior unmapped→failed mapping was
                                   //   a false negative). KNOWN supervisor degraded/terminal states are
                                   //   still classified honestly, NOT collapsed to "unknown":
                                   //   "Restarting"/"Backoff"/"Spawning" → "starting" (degraded, recovering);
                                   //   "Quarantined" → "failed" (supervisor permanently gave up).
        "restart_count": 0,
        "last_restart_at": null
      }
    ],
    "generated_at": 1714752000,
    "ttl_ms": 2000,
    "errors": []                   // [{ "scope": "wmic" | "daemon:<name>", "err": "..." }]
  },

  "probes": {                       // only present when ?include=probes or ?include=capabilities
    "items": [
      {
        "server": "fs", "daemon": "fs-default",
        "ok": true,
        "tool_count": 5,
        "err": "",
        "source": ""               // "" | "proxy-synthetic"
      }
    ],
    "generated_at": 1714752000,
    "ttl_ms": 10000,
    "errors": []
  },

  "capabilities": {                 // only present when ?include=capabilities
    "items": [
      {
        "server": "fs", "daemon": "fs-default",
        "tools":     { "state": "ok",          "items": [
                       { "name": "read_file",
                         "id":   "fs/fs-default/tool/read_file",
                         "namespace": "fs",
                         "kind": "tool" }
                     ] },
        "prompts":   { "state": "unsupported", "items": [] },
        "resources": { "state": "empty",       "items": [] }
      }
    ],
    "generated_at": 1714752000,
    "ttl_ms": 60000,
    "errors": []
  }
}
```

### Section state vocabulary

Each `tools` / `prompts` / `resources` block carries one of four discrete states:

- `"ok"` — list returned with one or more items (`items` populated).
- `"empty"` — list returned successfully but had zero items.
- `"unsupported"` — server responded with method-not-found or capability-not-declared (legitimate per MCP spec for servers that opt out of a capability).
- `"error"` — request failed (timeout, parse error, transport error). Reason in adjacent `err` string field.
- `"stale"` — last successful fetch is older than 2× TTL but still served as best-effort (probe failed temporarily). The `err` field carries the freshness gap reason.

Per-server-or-per-section errors land in `errors[]` arrays at the section level, not at the top level. Partial failure does not poison the whole snapshot.

### Canonical capability ID

`{server}/{daemon}/{kind}/{name}` where `kind ∈ {tool, prompt, resource}`. The full `id` string is what G4's Hub-MCP routing layer uses to dispatch. Defined in G2 — G4 must NOT introduce a parallel ID scheme.

---

## Backend architecture

### Package layout

New file `internal/api/health.go` owns:

```go
type HealthSnapshot struct {
    SchemaVersion string                  `json:"schema_version"`
    Hub           HubSection              `json:"hub"`
    Daemons       DaemonsSection          `json:"daemons"`
    Probes        *ProbesSection          `json:"probes,omitempty"`
    Capabilities  *CapabilitiesSection    `json:"capabilities,omitempty"`
}

type HealthOpts struct {
    IncludeProbes       bool
    IncludeCapabilities bool
    Refresh             bool   // bust cache for included sections
}

func (a *API) HealthSnapshot(opts HealthOpts) (HealthSnapshot, error)
```

Section types live in same file with explicit `json:` tags.

### Cache + singleflight

Each section has its own cache slot guarded by `sync.RWMutex` and a `golang.org/x/sync/singleflight.Group`. TTL per section:

| Section       | TTL     | Reason |
|---------------|---------|--------|
| hub           | ∞       | immutable per process lifetime |
| daemons       | 2s      | wmic ~500ms is the cost ceiling; daemon state changes during fast restart cycles |
| probes        | 10s     | ~100ms × N servers; tool count rarely changes mid-session |
| capabilities  | 60s     | ~300ms × N servers (tools+prompts+resources/list × N); change on server restart only |

Singleflight collapses N concurrent expired-cache requests into one underlying probe — second caller waits on first.

### Refresh rate limit

`?refresh=true` requests are gated by per-section minimum interval to prevent local-DoS via repeated refresh:

- `daemons`: max one refresh every 1s
- `probes`: max one refresh every 5s
- `capabilities`: max one refresh every 30s

Excess refresh requests get the cached value (silently — no 429). Singleflight ensures the actual probe runs at most once per minimum interval.

### Lazy-proxy preservation

Workspace-scoped servers carry `source="proxy-synthetic"` from `singleHealthProbe()`. The capability discovery path MUST honor this: do NOT spawn the heavy backend just to enumerate capabilities. For synthetic probes, capability section comes from the embedded catalog (already used by `singleHealthProbe` to answer `tools/list` synthetically). Backend lifecycle status (LifecycleActive / Missing / Failed) is a separate field on each daemon row, not conflated with `probe.ok`.

---

## Handler

New file `internal/gui/health.go`:

```go
func (s *Server) registerHealthRoute() {
    s.mux.HandleFunc("/api/health", s.requireSameOrigin(s.healthHandler))
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet { /* 405 */ }
    opts := parseHealthOpts(r.URL.Query())
    snap, err := s.api.HealthSnapshot(opts)
    if err != nil { /* 500 + code */ }
    writeJSON(w, http.StatusOK, snap)
}
```

`parseHealthOpts` parses `include=` (comma-separated) and `refresh=true`. Unknown include tokens silently ignored (forward-compat for G4-introduced sections).

`/api/status` handler refactored to call `s.api.HealthSnapshot(HealthOpts{})` and project `Daemons.Items` into the existing `StatusResponse` shape.

---

## Error handling

- Section-level errors → `errors[]` in that section, `state: "error"` for capability sub-sections, snapshot still returned 200.
- Backend-level errors (e.g. `runProcessSnapshot()` total failure, can't read manifests) → 500 with `{ "error": "...", "code": "HEALTH_BACKEND_FAILED" }`.
- Cache-section read errors (e.g. mutex poisoning — should not happen) → 500.
- Forward-compat: unknown `?include=` tokens are silently ignored. Unknown response fields in older clients are tolerated by JSON parsing convention.

---

## Testing strategy

### Backend unit tests (`internal/api/health_test.go`)

- `TestHealthSnapshot_DefaultExcludesProbesAndCapabilities` — opts zero-value, response has `hub` + `daemons` only.
- `TestHealthSnapshot_IncludeProbes_AddsProbesSection`.
- `TestHealthSnapshot_IncludeCapabilities_AddsBothProbesAndCapabilities`.
- `TestHealthSnapshot_CacheServesWithinTTL` — second call within TTL returns same `generated_at`, no probe re-run (use a counting fake probe).
- `TestHealthSnapshot_CacheExpiresAfterTTL` — second call after TTL re-runs the probe.
- `TestHealthSnapshot_RefreshBustsCache` — `Refresh: true` triggers re-run even within TTL.
- `TestHealthSnapshot_RefreshRateLimited` — second `Refresh: true` within minimum-interval gets cached value, no re-run.
- `TestHealthSnapshot_SingleflightCollapsesConcurrent` — N goroutines hitting expired cache trigger exactly one probe.
- `TestHealthSnapshot_PartialFailureDoesNotPoison` — one server's tools/list fails, capability section returns with that server's `tools.state = "error"` and the others succeed.
- `TestHealthSnapshot_LazyProxyDoesNotMaterialize` — synthetic probe path; capability comes from embedded catalog, no MCP roundtrip to spawn the backend.
- `TestCapabilityID_CanonicalForm` — `{server, daemon, kind, name}` → `"fs/fs-default/tool/read_file"`.

### Handler tests (`internal/gui/health_test.go`)

- `TestHealthHandler_GETOnly_405OnPOST`.
- `TestHealthHandler_RequiresSameOrigin` — cross-origin Origin header → 403.
- `TestHealthHandler_DefaultBody` — JSON has `schema_version: "1"`, `hub`, `daemons`, no `probes`, no `capabilities`.
- `TestHealthHandler_IncludeProbes`.
- `TestHealthHandler_IncludeCapabilities`.
- `TestHealthHandler_Refresh`.
- `TestHealthHandler_UnknownIncludeTokenIgnored`.

### `/api/status` regression tests

- `TestStatusEndpoint_StillReturnsExistingShape` — verify the refactor preserves the wire shape byte-for-byte. Use the existing `status_test.go` golden expectations.

### Frontend smoke (no UI changes in G2)

- `cd internal/gui/frontend && npm run typecheck && npm run test` — must remain green; G2 doesn't touch the frontend.

### E2E

- Defer to G3 — the new endpoint has no UI consumer in G2.

---

## Implementation phases

To be split by writing-plans skill into commit-sized phases. Rough sketch:

1. **Backend types + skeleton** — `HealthSnapshot`, sections, `HealthOpts`. No probe wiring yet. Tests for shape only.
2. **Hub + daemons sections** — wire to existing scheduler/process snapshot. TTL cache + singleflight for daemons. Tests for cache behavior.
3. **Probes section** — wire `singleHealthProbe()` per daemon. Tests for partial failure.
4. **Capabilities section** — `tools/list`, `prompts/list`, `resources/list` per probe. State vocabulary. Lazy-proxy preservation. Tests.
5. **Handler** — `/api/health` route, `?include=`, `?refresh=`. Tests.
6. **`/api/status` re-source** — call into health backend. Regression tests.

Estimate: **~2 days** wall time. Original backlog said ~1d but capability discovery + tiered cache + canonical ID extends scope. Trade-off: dropping capabilities to tools-only (defer prompts/resources to G3) saves ~½ day; leaving cache as flat 5s saves ~½ day. Recommendation: keep full scope — G3+G4 reuse depends on it.

---

## Out of scope (explicit)

- Frontend changes — G3.
- Hub MCP request routing — G4.
- Linux scheduler probe paths — F1.
- Backup of capability state across hub restarts — never. Probe is always live-derived from servers.
- WebSocket / SSE push notifications for capability changes — G3 considers; G2 polls.

---

## Acceptance criteria (G2 done)

- [ ] `GET /api/health` returns the `hub` + `daemons` shape from a real running hub.
- [ ] `?include=probes` returns probes for all running daemons.
- [ ] `?include=capabilities` returns capability lists for all running daemons.
- [ ] Cache + singleflight verified by tests.
- [ ] `?refresh=true` rate-limited; verified by tests.
- [ ] `/api/status` shape unchanged (regression tests).
- [ ] Lazy proxies do NOT materialize.
- [ ] All section errors land in section-level `errors[]`; no top-level error on partial failure.
- [ ] `schema_version: "1"` present.
- [ ] Canonical capability ID format `{server}/{daemon}/{kind}/{name}` documented in code.
- [ ] `go test ./internal/api/... ./internal/gui/...` PASS.
- [ ] Frontend typecheck + vitest unchanged (no UI touched).
- [ ] No new lint warnings beyond pre-existing.

---

## Terms and Abbreviations

- `MCP` — Model Context Protocol; the spec the hub speaks upstream.
- `HealthProbe` — existing per-server probe type at `internal/api/types.go:91`.
- `singleflight` — `golang.org/x/sync/singleflight` package; collapses concurrent identical requests into one execution.
- `lazy proxy` / `proxy-synthetic` — workspace-scoped server proxy that answers `initialize`+`tools/list` from the embedded catalog without spawning the heavy backend; identified by `source="proxy-synthetic"` on the probe result.
- `wmic` — Windows Management Instrumentation Command-line; the snapshot tool used for daemon process info on Windows.
- `kosyak` — Russian for "fuckup"; per dima_'s standing rule, every shipped mistake gets a confession file in `d:/dev/kosyaks/`.
