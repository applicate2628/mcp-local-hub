# G3 — Capability Status Display Design

**Status:** approved 2026-05-08 (post-brainstorm). Read-only frontend screen consuming the G2 unified `/api/health` backend (shipped in PR #131/#132/#133). No backend changes.

## Goal

Surface probed MCP capabilities (tools / prompts / resources) per server in the GUI so an operator can see at a glance which servers expose what, with probe error / state visibility. Tool EXECUTION is explicitly out of scope — items are read-only labels per the Phase 3B-II backlog gate.

## Architecture

Single new Preact screen consuming `GET /api/health?include=capabilities` (which transitively includes `?include=probes`). Renders per-server cards with collapsible Tools / Prompts / Resources sections. Manual Refresh button bypasses the 60s server-side cache via `?refresh=true`. No SSE, no polling, no run-tool affordance.

**Tech stack:** Preact + TypeScript (existing `internal/gui/frontend` pipeline). Plain CSS. Embedded into Go via `internal/gui/assets/` after `npm run build` + `go generate ./internal/gui/...`.

## Sidebar placement

New nav entry **"Capabilities"** with hash route `#/capabilities`. Inserted in sidebar at position 7, between Logs (#6) and Settings (#7 → moves to #8). Final sidebar order:

| # | Label | Hash |
|---|---|---|
| 1 | Servers | `#/servers` |
| 2 | Migration | `#/migration` |
| 3 | Add server | `#/add-server` |
| 4 | Secrets | `#/secrets` |
| 5 | Dashboard | `#/dashboard` |
| 6 | Logs | `#/logs` |
| **7** | **Capabilities** | **`#/capabilities`** |
| 8 | Settings | `#/settings` |
| 9 | About | `#/about` |

Total nav-link count goes from 8 → 9. The shell E2E test at `internal/gui/e2e/tests/shell.spec.ts` (currently asserts `toHaveCount(8)`) updates to `toHaveCount(9)` and gains the new label assertion.

The `allowedScreens` enum in `app.tsx:45-48` gains `"capabilities"`. Default-screen logic stays intact.

## Data model

Frontend types mirror the G2 backend (`internal/api/health.go:22-138`). New types in `internal/gui/frontend/src/types.ts`:

```ts
export interface HealthSnapshot {
  schema_version: string;
  hub: HubSection;
  daemons: DaemonsSection;
  probes?: ProbesSection;
  capabilities?: CapabilitiesSection;
}

export interface CapabilitiesSection {
  items: CapabilityRow[];
  generated_at: number;  // unix seconds
  ttl_ms: number;
  errors: SectionError[];
}

export interface CapabilityRow {
  server: string;
  daemon: string;
  tools: CapabilitySubSection;
  prompts: CapabilitySubSection;
  resources: CapabilitySubSection;
}

export interface CapabilitySubSection {
  state: "ok" | "empty" | "unsupported" | "error" | "stale";
  items: CapabilityItem[];
  err?: string;
}

export interface CapabilityItem {
  name: string;
  id: string;        // canonical "server/daemon/kind/name"
  namespace: string; // == server name
  kind: "tool" | "prompt" | "resource";
}

export interface ProbesSection {
  items: ProbeRow[];
  generated_at: number;
  ttl_ms: number;
  errors: SectionError[];
}

export interface ProbeRow {
  server: string;
  daemon: string;
  ok: boolean;
  tool_count: number;
  err?: string;
  source?: "" | "proxy-synthetic";
}

export interface SectionError {
  scope: string;
  err: string;
}
```

Existing types in `types.ts` are unchanged.

## Screen — Capabilities (`screens/Capabilities.tsx`)

### Loading state

LoadState discriminated union (mirrors `About.tsx:20-23`):

```ts
type LoadState =
  | { status: "loading" }
  | { status: "ok"; data: HealthSnapshot }
  | { status: "error"; error: string };
```

On-mount fetch via `fetchOrThrow<HealthSnapshot>("/api/health?include=capabilities", "object")`. The `fetchOrThrow` helper at `internal/gui/frontend/src/api.ts:8-36` already handles non-200 + shape validation; no new helper needed.

### Layout

```
┌─ h1: "Capabilities" ─────────────────────────────────────┐
│                                                          │
│  Generated 2026-05-08T11:30:00Z (cached 32s ago)         │
│                                            [ Refresh ]   │
│                                                          │
│  ┌─ memory ─────────────────────────────────────────┐    │
│  │ Server: memory   Daemon: default   ✓ probed      │    │
│  │ ▸ Tools (12)                                     │    │
│  │ ▸ Prompts (0)                                    │    │
│  │ ▸ Resources (3)                                  │    │
│  └──────────────────────────────────────────────────┘    │
│                                                          │
│  ┌─ filesystem ─────────────────────────────────────┐    │
│  │ Server: filesystem   Daemon: default  ✗ probe err│    │
│  │   tools: error — initialize: HTTP 500            │    │
│  │ ▾ Tools (0)                                      │    │
│  │     (no items — see error above)                 │    │
│  │ ▸ Prompts (0)                                    │    │
│  │ ▸ Resources (0)                                  │    │
│  └──────────────────────────────────────────────────┘    │
└──────────────────────────────────────────────────────────┘
```

### Components

- **`CapabilitiesScreen`** (top-level): manages LoadState, fetches on mount, renders header + Refresh button + list of `CapabilityCard`s. Empty state: "No capabilities found — install servers via the Add server screen." Error state: inline `<p class="error" role="alert">` matching existing pattern.
- **`CapabilityCard`** props `{ row: CapabilityRow, probe: ProbeRow | null }`: server header + 3 collapsible sections + probe-error pill if `probe.ok === false`.
- **`CapabilitySection`** props `{ kind, sub: CapabilitySubSection, expanded, onToggle }`: header showing kind + count + state badge; body shows item list when expanded.
- **`StateBadge`** props `{ state }`: colored chip — green for `ok`, gray for `empty`, yellow for `unsupported`, red for `error`, orange for `stale`.

### CSS class naming

BEM-ish, consistent with existing screens:

```
.capabilities-screen
.capabilities-header (h1 + meta + refresh button)
.capabilities-meta   (generated_at + cached-Ns-ago)
.capabilities-refresh-btn
.capabilities-empty
.capability-card
.capability-card-header
.capability-card-server
.capability-card-daemon
.capability-card-probe-status (.ok / .err)
.capability-card-probe-err
.capability-section
.capability-section-header (button-role for keyboard expand)
.capability-section-count
.capability-section-state (badge)
.capability-section-err
.capability-item-list
.capability-item
.capability-item-id (small monospace)
.state-badge .state-badge-ok / -empty / -unsupported / -error / -stale
```

### Refresh button semantics

Click → `fetchOrThrow<HealthSnapshot>("/api/health?include=capabilities&refresh=true", "object")`. While inflight: button text "Refreshing…", button disabled. Success: state replaced. Error: existing data retained, inline error shows `<p class="error">Refresh failed: <msg></p>` next to the button (clears on next successful fetch).

The button acknowledges the backend's 30s refresh-rate-limit by showing a friendly error text "Backend rate-limited — try again in <N>s" if the response payload includes `errors` array entries with `scope: "capabilities"` indicating the rate-limit hit (TBD: confirm backend response shape on rate-limit during plan stage; if backend returns 200 with stale data + section error, we surface that inline; if backend returns 429, fetchOrThrow's existing error path handles it).

### Synthetic-source disclosure

When `probe.source === "proxy-synthetic"`, render a small "synthetic" pill next to the probe status. Tooltip: "Capabilities reported by the lazy-proxy stub; not a live MCP roundtrip."

### Tool execution: explicitly absent

`CapabilityItem` renders as `<li class="capability-item"><span class="capability-item-name">{name}</span><span class="capability-item-id">{id}</span></li>` — no buttons, no click handlers, no hover affordance suggesting actionability. Per the v0.3.0 backlog "tool execution stays disabled or explicitly gated" — read-only display, future Run-feature is a separate threat-model gate.

### Workspace-scoped daemons

Included. Lazy-proxy daemons (where `is_workspace_scoped === true`) are exposed by the backend with synthetic capabilities (`internal/api/health.go:653-659`); G3 surfaces them like any other server. Synthetic-source pill (above) makes the distinction visible.

### State badge legend

Render at the top of the screen (collapsed by default; clickable "?" icon expands a small legend panel):

| State | Meaning |
|---|---|
| ok | Server reported items successfully |
| empty | Server reported the category supports no items |
| unsupported | Server explicitly declared no support for this category |
| error | Probe failed (see err) |
| stale | Last probe is older than the section TTL but no fresh data available |

## E2E tests

New file `internal/gui/e2e/tests/capabilities.spec.ts`:

1. Sidebar: navigate to `#/capabilities`, assert h1 = "Capabilities", active sidebar link.
2. Empty state: with no servers (default fixture), screen shows "No capabilities found — install servers..." copy, no `<article class="capability-card">` elements.
3. Refresh button visible + click triggers a `/api/health` request with `refresh=true` (use `page.waitForRequest`).
4. Update `internal/gui/e2e/tests/shell.spec.ts`: nav-link `toHaveCount(9)` (was 8); add Capabilities label assertion at index 6.

Populated-fixture tests (cards rendering, collapsible toggles, badge states) deferred to a follow-up unless trivially mockable via `page.route()` — flag this in the implementation plan.

## Frontend unit tests

New file `internal/gui/frontend/src/screens/Capabilities.test.tsx`:

1. Loading state renders `<p>Loading…</p>` + no card.
2. Error state renders `.error` paragraph with the error message.
3. OK state with empty `capabilities.items` → renders empty-state copy.
4. OK state with one server + tools.state=ok + 2 items → card renders, click "Tools (2)" expands, both items visible.
5. OK state with `tools.state="error" + err="..."` → red badge + err message visible BEFORE expansion.
6. Refresh button click triggers a second fetch with `refresh=true`.
7. Synthetic-source pill renders when `probe.source === "proxy-synthetic"`.

## Out of scope

- Tool / prompt / resource EXECUTION from the GUI (separate threat-model gate; tracked under G4 + future Phase 3D).
- Per-item timestamps (`last_seen_at`, `last_changed_at`) — would require backend additions; G2 only carries section-level `generated_at`.
- Tool description / input-schema / version metadata — backend live roundtrip discards these (`health.go:786-800`); G3 stays at name + kind.
- SSE push for capability changes — G2 spec explicitly defers to "G3+" but G3 chooses manual Refresh.
- Configuration of which probes run — operator manages servers; G3 only displays.

## Implementation order (handed to writing-plans)

1. Frontend types + `fetchOrThrow` call wiring (no UI yet).
2. Skeleton screen with LoadState + on-mount fetch + Refresh button. Empty state. Inline error. NO cards yet.
3. CapabilityCard component (per-server header + 3 collapsed sections, no item list yet).
4. CapabilitySection collapsible + StateBadge.
5. Item list rendering inside expanded sections.
6. Synthetic-source pill + state legend panel.
7. CSS + theming polish (light / dark / density variants).
8. E2E test additions + shell.spec.ts count update.
9. Frontend unit tests (Capabilities.test.tsx).
10. Build + embed bundle (`go generate ./internal/gui/...`).

## Acceptance criteria

1. New "Capabilities" sidebar link navigates to `#/capabilities`.
2. On-mount fetch hits `/api/health?include=capabilities` exactly once.
3. Refresh button triggers a single `?include=capabilities&refresh=true` fetch and updates the screen on success.
4. Per-server cards render with the correct probe state + 3 collapsible sections.
5. Section state badges colored per the vocabulary (ok=green, empty=gray, unsupported=yellow, error=red, stale=orange).
6. Probe error text rendered inline (red `.error` styling) when probe.ok === false.
7. NO actionable Run-tool buttons or click handlers anywhere on the screen.
8. Synthetic-source pill renders when probe.source === "proxy-synthetic".
9. Workspace-scoped daemons included in the list (with the synthetic pill where applicable).
10. Empty state copy renders with no servers in the snapshot.
11. Inline error renders on fetch failure (not a global toast).
12. shell.spec.ts updated to assert 9 nav links.
13. New `capabilities.spec.ts` covers navigation + empty state + refresh request.
14. New `Capabilities.test.tsx` covers loading / error / ok / refresh / state-badge / synthetic-pill paths.
15. `go build ./...`, `go vet ./...`, `cd internal/gui/frontend && npm run build`, `npm test` all pass.
16. `go generate ./internal/gui/...` regenerates the embedded bundle; no stale assets in the diff.

## Effort estimate

~1d implementation + ~½d test coverage + ½d for bot-review iteration. Within the original "1-2d" backlog estimate.

## Terms and Abbreviations

- `capability`: a tool, prompt, or resource declared by an MCP server.
- `tool`: an executable MCP capability that performs an action (out of G3 scope to invoke).
- `prompt`: an MCP capability returning a prompt template.
- `resource`: an MCP capability returning a resource snapshot.
- `probe`: live MCP roundtrip (`initialize` + `tools/list` etc.) used to populate the capability snapshot.
- `synthetic-source`: capability data reported by the lazy-proxy stub without a live MCP roundtrip; flagged on the wire as `source: "proxy-synthetic"`.
- `state`: per-section vocabulary `ok|empty|unsupported|error|stale`.
- `LoadState`: frontend three-state discriminated union (`loading|ok|error`).
- `BEM`: a CSS class-naming convention (Block-Element-Modifier); the repo uses a relaxed BEM-ish style.
- `SSE`: Server-Sent Events; one-way push channel from server to browser.
- `TTL`: Time To Live; cache-validity window in seconds or milliseconds.
- `G2`: prior phase that shipped the unified `/api/health` backend (PR #131/#132/#133).
- `G4`: future phase that ships the opt-in unified Hub MCP endpoint (post-G3).
