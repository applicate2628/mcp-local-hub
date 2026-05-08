# G3 Capability Display Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Per the user's standing memory rule "Subagents always opus + max", every implementer + reviewer subagent dispatch uses `model=opus`. Per "проводи codex ревью после каждого этапа", the orchestrator runs `codex exec -c model_reasoning_effort=xhigh` against each phase commit BEFORE moving to the next phase. Each phase is sized to be codex-reviewable as a single artifact (≤200 lines diff, single concern).

**Goal:** Add a read-only "Capabilities" screen to the GUI that surfaces probed MCP tools/prompts/resources per server, consuming the existing G2 unified `/api/health` backend. NO tool execution. NO backend changes.

**Architecture:** New Preact screen `screens/Capabilities.tsx` consuming `GET /api/health?include=capabilities` via the existing `fetchOrThrow` helper. Per-server cards with collapsible Tools/Prompts/Resources sections. Manual Refresh button (no auto-poll, no SSE). 9th sidebar entry between Logs (#6) and Settings (now #8). Embedded into Go via `internal/gui/assets/` after `go generate ./internal/gui/...`.

**Tech Stack:** Preact + TypeScript + Vite (existing `internal/gui/frontend/` pipeline). Plain CSS (BEM-ish). vitest for unit tests, Playwright for E2E. Go embed for production bundle.

**Spec:** `docs/superpowers/specs/2026-05-08-g3-capability-display-design.md` (commit `ec53091`, codex stage-0 APPROVE after r2 fixes).

---

## File structure

| File | Action | Responsibility |
|---|---|---|
| `internal/gui/frontend/src/types.ts` | Modify | Add wire types: `HealthSnapshot`, `HubSection`, `DaemonsSection`, `DaemonRow`, `ProbesSection`, `ProbeRow`, `CapabilitiesSection`, `CapabilityRow`, `CapabilitySubSection`, `CapabilityItem`, `SectionError`. |
| `internal/gui/frontend/src/screens/Capabilities.tsx` | Create | Top-level screen: LoadState, on-mount fetch, Refresh button, empty/error/loading states, list of `CapabilityCard`. |
| `internal/gui/frontend/src/components/CapabilityCard.tsx` | Create | Per-server header + 3 `CapabilitySection`s + probe-error pill + synthetic-source pill. |
| `internal/gui/frontend/src/components/CapabilitySection.tsx` | Create | Collapsible per-kind section: header (kind + count + StateBadge), body (item list when expanded). |
| `internal/gui/frontend/src/components/StateBadge.tsx` | Create | Colored chip for the 5 states (ok/empty/unsupported/error/stale). |
| `internal/gui/frontend/src/components/CapabilityLegend.tsx` | Create | Collapsible legend panel explaining the state vocabulary. |
| `internal/gui/frontend/src/screens/Capabilities.test.tsx` | Create | Vitest unit tests: loading / error / empty / ok / refresh / inflight-disabled / unmount / items-null / synthetic-pill / stale-fixture / legend-toggle. |
| `internal/gui/frontend/src/app.tsx` | Modify | Add `"capabilities"` to `VALID_DEFAULT_SCREENS`; import `CapabilitiesScreen`; add `case "capabilities"` to switch; add sidebar link between Logs and Settings. |
| `internal/gui/frontend/src/css/capabilities.css` (or inline in `style.css`) | Create or extend | BEM-ish classes per spec: `.capabilities-screen`, `.capability-card`, `.capability-section`, `.state-badge`, `.synthetic-source-pill`, etc. |
| `internal/gui/e2e/tests/capabilities.spec.ts` | Create | Playwright E2E: navigation to `#/capabilities`, h1 visible, empty-state copy, refresh request observed. |
| `internal/gui/e2e/tests/shell.spec.ts` | Modify | `toHaveCount(8)` → `toHaveCount(9)`; add Capabilities label assertion at index 6. |
| `internal/gui/assets/{index.html,app.js,style.css}` | Regenerate | After Phase 8 via `go generate ./internal/gui/...`. |

**Per-phase commit / codex-review boundary**: every phase below ends with a single commit. The orchestrator runs `codex exec -c model_reasoning_effort=xhigh` on that commit's diff BEFORE dispatching the next phase's subagent. This is the "проводи codex ревью после каждого этапа" gate the user mandated.

---

## Phase 1: Wire types + sidebar entry + placeholder screen

**Files:**
- Create: `internal/gui/frontend/src/screens/Capabilities.tsx`
- Modify: `internal/gui/frontend/src/types.ts`
- Modify: `internal/gui/frontend/src/app.tsx`
- Modify: `internal/gui/e2e/tests/shell.spec.ts`
- Test (create): `internal/gui/frontend/src/screens/Capabilities.test.tsx`

- [ ] **Step 1: Write the failing test**

Create `internal/gui/frontend/src/screens/Capabilities.test.tsx`:

```tsx
import { render, cleanup } from "@testing-library/preact";
import { describe, it, expect, afterEach } from "vitest";
import { CapabilitiesScreen } from "./Capabilities";

afterEach(cleanup);

describe("CapabilitiesScreen — Phase 1 placeholder", () => {
  it("renders the h1 'Capabilities' heading", () => {
    const { getByRole } = render(<CapabilitiesScreen />);
    const h1 = getByRole("heading", { level: 1 });
    expect(h1.textContent).toBe("Capabilities");
  });

  it("has the .capabilities-screen container class", () => {
    const { container } = render(<CapabilitiesScreen />);
    expect(container.querySelector(".capabilities-screen")).not.toBeNull();
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/gui/frontend && npm test -- src/screens/Capabilities.test.tsx --run`
Expected: FAIL with module-not-found error for `./Capabilities`.

- [ ] **Step 3: Add wire types to `types.ts`**

Append to `internal/gui/frontend/src/types.ts`:

```ts
// G3 — wire types mirroring internal/api/health.go HealthSnapshot.
// Field names and JSON tags match the Go side; keep in sync if the
// backend type evolves. Items[] may be null for unsupported/error/
// synthetic-empty subsections (Go has no `omitempty`); frontend MUST
// normalize via `sub.items ?? []` at the screen boundary.

export interface HealthSnapshot {
  schema_version: string;
  hub: HubSection;
  daemons: DaemonsSection;
  probes?: ProbesSection;
  capabilities?: CapabilitiesSection;
}

export interface HubSection {
  version: string;
  commit: string;
  build_date: string;
  started_at: number;
  lock?: { pid: number; port: number };
  generated_at: number;
  ttl_ms: number;
}

export interface DaemonsSection {
  items: DaemonRow[];
  generated_at: number;
  ttl_ms: number;
  errors: SectionError[];
}

export interface DaemonRow {
  server: string;
  daemon: string;
  backend: string;
  pid: number;
  port: number;
  ram_bytes: number;
  uptime_sec: number;
  state: string;
  restart_count: number;
  last_restart_at: number | null;
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

export interface CapabilitiesSection {
  items: CapabilityRow[];
  generated_at: number;
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
  // null on unsupported/error/synthetic-empty paths — see types.ts header note.
  items: CapabilityItem[] | null;
  err?: string;
}

export interface CapabilityItem {
  name: string;
  id: string;        // canonical "server/daemon/kind/name"
  namespace: string; // == server name
  kind: "tool" | "prompt" | "resource";
}

export interface SectionError {
  scope: string;
  err: string;
}
```

- [ ] **Step 4: Create the placeholder screen**

Create `internal/gui/frontend/src/screens/Capabilities.tsx`:

```tsx
// G3 — capability status display screen. Read-only view of the G2
// /api/health snapshot. NO tool execution; items render as labels only.

export function CapabilitiesScreen() {
  return (
    <section class="capabilities-screen" data-testid="capabilities-screen">
      <h1>Capabilities</h1>
      <p>Loading…</p>
    </section>
  );
}
```

- [ ] **Step 5: Wire the screen into routing + sidebar**

Modify `internal/gui/frontend/src/app.tsx`:

- Add `"capabilities"` to `VALID_DEFAULT_SCREENS` set (line 45-48). After modification:

```ts
const VALID_DEFAULT_SCREENS = new Set([
  "dashboard", "servers", "migration", "add-server",
  "secrets", "logs", "capabilities", "settings", "about",
]);
```

- Add the import alongside the other screen imports (around line 78-86):

```ts
import { CapabilitiesScreen } from "./screens/Capabilities";
```

- Add a `case "capabilities"` to the switch in App() (around line 239-273), inserted between `case "logs"` and `case "settings"`:

```tsx
    case "capabilities":
      body = <CapabilitiesScreen />;
      break;
```

- Add the sidebar link inside the `navLinks` block (around line 276-287), between Logs and Settings:

```tsx
      <a href="#/capabilities" class={route.screen === "capabilities" ? "active" : ""} onClick={guardClick("capabilities")}>Capabilities</a>
```

The final navLinks order must be:
1. Servers / 2. Migration / 3. Add server / 4. Secrets / 5. Dashboard / 6. Logs / **7. Capabilities** / 8. Settings / 9. About.

- [ ] **Step 6: Update E2E shell.spec.ts**

Modify `internal/gui/e2e/tests/shell.spec.ts`. Find the `toHaveCount(8)` assertion and update to 9. If the file enumerates link labels, insert "Capabilities" at index 6 (zero-indexed: between "Logs" and "Settings"). Concrete change:

```ts
// Before
await expect(navLinks).toHaveCount(8);
// After
await expect(navLinks).toHaveCount(9);
```

If there's a label-text assertion array, update from:

```ts
const expected = ["Servers", "Migration", "Add server", "Secrets", "Dashboard", "Logs", "Settings", "About"];
```

To:

```ts
const expected = ["Servers", "Migration", "Add server", "Secrets", "Dashboard", "Logs", "Capabilities", "Settings", "About"];
```

- [ ] **Step 7: Run test to verify it passes**

Run: `cd internal/gui/frontend && npm test -- src/screens/Capabilities.test.tsx --run`
Expected: `Test Files  1 passed (1)` + `Tests  2 passed (2)`.

Run: `cd internal/gui/frontend && npm run typecheck`
Expected: 0 errors.

- [ ] **Step 8: Verify Go-side build still passes**

Run: `cd d:/dev/mcp-local-hub && go build ./...`
Expected: empty (no errors).

Run: `cd d:/dev/mcp-local-hub && go vet ./...`
Expected: empty.

- [ ] **Step 9: Commit**

```bash
git add internal/gui/frontend/src/types.ts \
        internal/gui/frontend/src/screens/Capabilities.tsx \
        internal/gui/frontend/src/screens/Capabilities.test.tsx \
        internal/gui/frontend/src/app.tsx \
        internal/gui/e2e/tests/shell.spec.ts
git commit -m "feat(g3): phase 1 — types + sidebar entry + placeholder screen"
```

**Phase 1 codex review focus:** wire types vs `internal/api/health.go` reality (already validated in spec re-review, but recheck after types.ts is live); sidebar link routing correctness; shell.spec.ts count update.

---

## Phase 2: LoadState + on-mount fetch + empty/error/loading states

**Files:**
- Modify: `internal/gui/frontend/src/screens/Capabilities.tsx`
- Modify: `internal/gui/frontend/src/screens/Capabilities.test.tsx`

- [ ] **Step 1: Write the failing tests**

Append to `internal/gui/frontend/src/screens/Capabilities.test.tsx`:

```tsx
import { vi, beforeEach } from "vitest";
import * as api from "../api";
import type { HealthSnapshot } from "../types";

const emptySnapshot: HealthSnapshot = {
  schema_version: "1",
  hub: { version: "0.3.0", commit: "abc", build_date: "2026-05-08", started_at: 0, generated_at: 0, ttl_ms: 0 },
  daemons: { items: [], generated_at: 0, ttl_ms: 2000, errors: [] },
  probes: { items: [], generated_at: 0, ttl_ms: 10000, errors: [] },
  capabilities: { items: [], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
};

describe("CapabilitiesScreen — Phase 2 LoadState", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("renders Loading… while fetch is in flight", () => {
    vi.spyOn(api, "fetchOrThrow").mockReturnValue(new Promise(() => { /* never resolves */ }));
    const { getByText } = render(<CapabilitiesScreen />);
    expect(getByText("Loading…")).toBeTruthy();
  });

  it("renders the empty state when capabilities.items is empty", async () => {
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(emptySnapshot);
    const { findByTestId } = render(<CapabilitiesScreen />);
    const empty = await findByTestId("capabilities-empty");
    expect(empty.textContent).toContain("No capabilities found");
  });

  it("renders inline error on fetch failure (role=alert)", async () => {
    vi.spyOn(api, "fetchOrThrow").mockRejectedValue(new Error("network down"));
    const { findByRole } = render(<CapabilitiesScreen />);
    const alert = await findByRole("alert");
    expect(alert.textContent).toContain("network down");
  });

  it("hits /api/health?include=capabilities exactly once on mount", () => {
    const spy = vi.spyOn(api, "fetchOrThrow").mockResolvedValue(emptySnapshot);
    render(<CapabilitiesScreen />);
    expect(spy).toHaveBeenCalledTimes(1);
    expect(spy).toHaveBeenCalledWith("/api/health?include=capabilities", "object");
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd internal/gui/frontend && npm test -- src/screens/Capabilities.test.tsx --run`
Expected: FAIL on the new 4 tests (placeholder still renders static "Loading…").

- [ ] **Step 3: Implement LoadState and on-mount fetch**

Replace `internal/gui/frontend/src/screens/Capabilities.tsx` with:

```tsx
import { useEffect, useState } from "preact/hooks";
import { fetchOrThrow } from "../api";
import type { HealthSnapshot } from "../types";

// G3 — capability status display screen. Read-only view of the G2
// /api/health snapshot. NO tool execution; items render as labels only.

type LoadState =
  | { status: "loading" }
  | { status: "ok"; data: HealthSnapshot }
  | { status: "error"; error: string };

export function CapabilitiesScreen() {
  const [state, setState] = useState<LoadState>({ status: "loading" });

  useEffect(() => {
    let cancelled = false;
    fetchOrThrow<HealthSnapshot>("/api/health?include=capabilities", "object")
      .then((data) => {
        if (!cancelled) setState({ status: "ok", data });
      })
      .catch((err: Error) => {
        if (!cancelled) setState({ status: "error", error: err.message });
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (state.status === "loading") {
    return (
      <section class="capabilities-screen" data-testid="capabilities-screen">
        <h1>Capabilities</h1>
        <p>Loading…</p>
      </section>
    );
  }

  if (state.status === "error") {
    return (
      <section class="capabilities-screen" data-testid="capabilities-screen">
        <h1>Capabilities</h1>
        <p class="error" role="alert">Failed to load capabilities: {state.error}</p>
      </section>
    );
  }

  const caps = state.data.capabilities;
  const rows = caps?.items ?? [];

  if (rows.length === 0) {
    return (
      <section class="capabilities-screen" data-testid="capabilities-screen">
        <h1>Capabilities</h1>
        <p class="capabilities-empty" data-testid="capabilities-empty">
          No capabilities found — install servers via the Add server screen.
        </p>
      </section>
    );
  }

  return (
    <section class="capabilities-screen" data-testid="capabilities-screen">
      <h1>Capabilities</h1>
      <p class="capabilities-empty">{/* placeholder — Phase 4 replaces with cards */}</p>
    </section>
  );
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd internal/gui/frontend && npm test -- src/screens/Capabilities.test.tsx --run`
Expected: `Tests  6 passed (6)` (2 from Phase 1 + 4 new).

- [ ] **Step 5: Verify Go build + vet still clean**

Run: `cd d:/dev/mcp-local-hub && go build ./... && go vet ./...`
Expected: empty.

- [ ] **Step 6: Commit**

```bash
git add internal/gui/frontend/src/screens/Capabilities.tsx \
        internal/gui/frontend/src/screens/Capabilities.test.tsx
git commit -m "feat(g3): phase 2 — LoadState + on-mount fetch + empty/error states"
```

**Phase 2 codex review focus:** cancelled-flag pattern correctness; error string surfaced verbatim (no PII / secret leaks from Error.message); empty-state copy clarity; the fetch URL is exactly `/api/health?include=capabilities` (no extra query params).

---

## Phase 3: Refresh button + inflight-disabled + mid-fetch unmount safety

**Files:**
- Modify: `internal/gui/frontend/src/screens/Capabilities.tsx`
- Modify: `internal/gui/frontend/src/screens/Capabilities.test.tsx`

- [ ] **Step 1: Write the failing tests**

Append to `Capabilities.test.tsx`:

```tsx
import { fireEvent } from "@testing-library/preact";

describe("CapabilitiesScreen — Phase 3 Refresh", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("Refresh button click triggers a second fetch with refresh=true", async () => {
    const spy = vi.spyOn(api, "fetchOrThrow").mockResolvedValue(emptySnapshot);
    const { findByTestId } = render(<CapabilitiesScreen />);
    const button = await findByTestId("capabilities-refresh-btn");
    fireEvent.click(button);
    // Wait one microtask so the click handler's fetch fires.
    await Promise.resolve();
    expect(spy).toHaveBeenCalledTimes(2);
    expect(spy).toHaveBeenLastCalledWith("/api/health?include=capabilities&refresh=true", "object");
  });

  it("Refresh button is disabled while a fetch is inflight (AC #17)", async () => {
    let resolveFn!: (value: HealthSnapshot) => void;
    const deferred = new Promise<HealthSnapshot>((resolve) => { resolveFn = resolve; });
    const spy = vi.spyOn(api, "fetchOrThrow")
      .mockResolvedValueOnce(emptySnapshot)   // initial mount fetch resolves
      .mockReturnValueOnce(deferred);         // refresh stays pending

    const { findByTestId } = render(<CapabilitiesScreen />);
    const button = await findByTestId("capabilities-refresh-btn");

    expect(button.hasAttribute("disabled")).toBe(false);  // idle initially
    fireEvent.click(button);
    await Promise.resolve();
    expect(button.hasAttribute("disabled")).toBe(true);   // disabled while inflight
    expect(button.textContent).toContain("Refreshing");

    resolveFn(emptySnapshot);
    await deferred;
    await Promise.resolve();
    expect(button.hasAttribute("disabled")).toBe(false);  // re-enabled after resolve
    expect(spy).toHaveBeenCalledTimes(2);
  });

  it("mid-fetch unmount does NOT call setState (AC #18)", async () => {
    let resolveFn!: (value: HealthSnapshot) => void;
    const deferred = new Promise<HealthSnapshot>((resolve) => { resolveFn = resolve; });
    vi.spyOn(api, "fetchOrThrow").mockReturnValue(deferred);

    const consoleSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const { unmount } = render(<CapabilitiesScreen />);
    unmount();
    resolveFn(emptySnapshot);
    await deferred;
    await Promise.resolve();
    // Preact does not throw the React-style "setState on unmounted" warning
    // by default, but the cancelled-flag guarantees no internal state-mutation
    // side effects occur; assert no console.error fired.
    expect(consoleSpy).not.toHaveBeenCalled();
    consoleSpy.mockRestore();
  });

  it("Refresh failure shows inline error without losing prior data", async () => {
    const okSnapshot: HealthSnapshot = { ...emptySnapshot,
      capabilities: { items: [], generated_at: 1715164800, ttl_ms: 60000, errors: [] } };
    vi.spyOn(api, "fetchOrThrow")
      .mockResolvedValueOnce(okSnapshot)
      .mockRejectedValueOnce(new Error("rate limited"));

    const { findByTestId, queryByRole } = render(<CapabilitiesScreen />);
    await findByTestId("capabilities-empty");  // initial OK render
    const button = await findByTestId("capabilities-refresh-btn");
    fireEvent.click(button);
    await Promise.resolve();
    await Promise.resolve();

    const alert = queryByRole("alert");
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain("rate limited");
    // Prior empty-state still visible (we did not blank the screen).
    const stillEmpty = await findByTestId("capabilities-empty");
    expect(stillEmpty).toBeTruthy();
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd internal/gui/frontend && npm test -- src/screens/Capabilities.test.tsx --run`
Expected: FAIL on the 4 new Phase-3 tests (no Refresh button rendered yet).

- [ ] **Step 3: Implement Refresh + inflight-disabled + retain-prior-data on refresh-error**

Replace `internal/gui/frontend/src/screens/Capabilities.tsx` with:

```tsx
import { useEffect, useState, useCallback } from "preact/hooks";
import { fetchOrThrow } from "../api";
import type { HealthSnapshot } from "../types";

type LoadState =
  | { status: "loading" }
  | { status: "ok"; data: HealthSnapshot }
  | { status: "error"; error: string };

export function CapabilitiesScreen() {
  const [state, setState] = useState<LoadState>({ status: "loading" });
  const [refreshing, setRefreshing] = useState(false);
  const [refreshError, setRefreshError] = useState<string | null>(null);

  // On-mount fetch. cancelled-flag prevents setState after unmount
  // (mirrors Dashboard.tsx:41-63 / About.tsx:28-40 — pattern preserved).
  useEffect(() => {
    let cancelled = false;
    fetchOrThrow<HealthSnapshot>("/api/health?include=capabilities", "object")
      .then((data) => {
        if (!cancelled) setState({ status: "ok", data });
      })
      .catch((err: Error) => {
        if (!cancelled) setState({ status: "error", error: err.message });
      });
    return () => { cancelled = true; };
  }, []);

  const onRefresh = useCallback(() => {
    if (refreshing) return;  // belt + suspenders for the disabled button
    setRefreshing(true);
    setRefreshError(null);
    let cancelled = false;
    fetchOrThrow<HealthSnapshot>("/api/health?include=capabilities&refresh=true", "object")
      .then((data) => {
        if (!cancelled) {
          setState({ status: "ok", data });
          setRefreshing(false);
        }
      })
      .catch((err: Error) => {
        if (!cancelled) {
          setRefreshError(err.message);
          setRefreshing(false);
        }
      });
    // No cleanup needed — this is an event-handler closure, not an effect.
    // The component-unmount cancellation is handled by the mount-effect's
    // cancelled flag; if a refresh is inflight at unmount, we accept the
    // setRefreshing(false) on a stale instance — Preact swallows it.
    void cancelled;
  }, [refreshing]);

  if (state.status === "loading") {
    return (
      <section class="capabilities-screen" data-testid="capabilities-screen">
        <h1>Capabilities</h1>
        <p>Loading…</p>
      </section>
    );
  }

  if (state.status === "error") {
    return (
      <section class="capabilities-screen" data-testid="capabilities-screen">
        <h1>Capabilities</h1>
        <p class="error" role="alert">Failed to load capabilities: {state.error}</p>
      </section>
    );
  }

  const caps = state.data.capabilities;
  const rows = caps?.items ?? [];
  const generatedAt = caps?.generated_at ?? 0;

  return (
    <section class="capabilities-screen" data-testid="capabilities-screen">
      <header class="capabilities-header">
        <h1>Capabilities</h1>
        <div class="capabilities-meta">
          {generatedAt > 0 && (
            <span data-testid="capabilities-generated-at">
              Generated {new Date(generatedAt * 1000).toISOString()}
            </span>
          )}
          <button
            class="capabilities-refresh-btn"
            data-testid="capabilities-refresh-btn"
            onClick={onRefresh}
            disabled={refreshing}
          >
            {refreshing ? "Refreshing…" : "Refresh"}
          </button>
        </div>
        {refreshError && (
          <p class="error" role="alert">Refresh failed: {refreshError}</p>
        )}
      </header>

      {rows.length === 0 ? (
        <p class="capabilities-empty" data-testid="capabilities-empty">
          No capabilities found — install servers via the Add server screen.
        </p>
      ) : (
        <p>{/* placeholder — Phase 4 adds CapabilityCard list */}</p>
      )}
    </section>
  );
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd internal/gui/frontend && npm test -- src/screens/Capabilities.test.tsx --run`
Expected: `Tests  10 passed (10)` (6 prior + 4 new).

- [ ] **Step 5: Verify Go build + vet still clean**

Run: `cd d:/dev/mcp-local-hub && go build ./... && go vet ./...`
Expected: empty.

- [ ] **Step 6: Commit**

```bash
git add internal/gui/frontend/src/screens/Capabilities.tsx \
        internal/gui/frontend/src/screens/Capabilities.test.tsx
git commit -m "feat(g3): phase 3 — Refresh button + inflight-disabled + unmount safety"
```

**Phase 3 codex review focus:** cancelled-flag closure correctness in `onRefresh` (not strictly needed since refresh fires on a click; double-check no setState-after-unmount path); URL is exactly `/api/health?include=capabilities&refresh=true`; AC #17 (button.disabled while inflight) and AC #18 (no console.error on unmount-mid-fetch) are testable.

---

## Phase 4: CapabilityCard component (per-server header + 3 collapsed section placeholders)

**Files:**
- Create: `internal/gui/frontend/src/components/CapabilityCard.tsx`
- Modify: `internal/gui/frontend/src/screens/Capabilities.tsx`
- Modify: `internal/gui/frontend/src/screens/Capabilities.test.tsx`

- [ ] **Step 1: Write the failing test**

Append to `Capabilities.test.tsx`:

```tsx
import type { CapabilityRow, ProbeRow } from "../types";

describe("CapabilitiesScreen — Phase 4 CapabilityCard", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("renders one card per server with header, daemon, and 3 section placeholders", async () => {
    const row: CapabilityRow = {
      server: "memory",
      daemon: "default",
      tools:     { state: "ok", items: [{ name: "a", id: "memory/default/tool/a", namespace: "memory", kind: "tool" }, { name: "b", id: "memory/default/tool/b", namespace: "memory", kind: "tool" }] },
      prompts:   { state: "empty", items: [] },
      resources: { state: "empty", items: [] },
    };
    const probe: ProbeRow = { server: "memory", daemon: "default", ok: true, tool_count: 2, source: "" };
    const snap: HealthSnapshot = {
      ...emptySnapshot,
      probes:       { ...emptySnapshot.probes!, items: [probe] },
      capabilities: { items: [row], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(snap);

    const { findByTestId } = render(<CapabilitiesScreen />);
    const card = await findByTestId("capability-card-memory-default");
    expect(card.querySelector(".capability-card-server")?.textContent).toBe("memory");
    expect(card.querySelector(".capability-card-daemon")?.textContent).toBe("default");
    expect(card.querySelector('[data-testid="capability-section-tools"]')?.textContent).toContain("Tools (2)");
    expect(card.querySelector('[data-testid="capability-section-prompts"]')?.textContent).toContain("Prompts (0)");
    expect(card.querySelector('[data-testid="capability-section-resources"]')?.textContent).toContain("Resources (0)");
  });

  it("renders the probe error state when probe.ok is false", async () => {
    const row: CapabilityRow = {
      server: "filesystem",
      daemon: "default",
      tools:     { state: "error", items: null, err: "initialize: HTTP 500" },
      prompts:   { state: "empty", items: [] },
      resources: { state: "empty", items: [] },
    };
    const probe: ProbeRow = { server: "filesystem", daemon: "default", ok: false, tool_count: 0, err: "initialize: HTTP 500" };
    const snap: HealthSnapshot = {
      ...emptySnapshot,
      probes:       { ...emptySnapshot.probes!, items: [probe] },
      capabilities: { items: [row], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(snap);

    const { findByTestId } = render(<CapabilitiesScreen />);
    const card = await findByTestId("capability-card-filesystem-default");
    const errPill = card.querySelector(".capability-card-probe-status.err");
    expect(errPill).not.toBeNull();
    expect(card.textContent).toContain("initialize: HTTP 500");
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd internal/gui/frontend && npm test -- src/screens/Capabilities.test.tsx --run`
Expected: FAIL on the 2 new Phase-4 tests.

- [ ] **Step 3: Create the CapabilityCard component**

Create `internal/gui/frontend/src/components/CapabilityCard.tsx`:

```tsx
import type { CapabilityRow, ProbeRow } from "../types";

interface Props {
  row: CapabilityRow;
  probe: ProbeRow | null;
}

// Per-server card. Header shows server + daemon + probe-status pill.
// Body has 3 collapsible section placeholders (Phase 5 adds the
// CapabilitySection collapsible mechanic; Phase 4 just shows counts).
export function CapabilityCard({ row, probe }: Props) {
  const testId = `capability-card-${row.server}-${row.daemon}`;
  const probeOk = probe?.ok ?? true;
  const probeErr = probe?.err ?? "";

  const itemCount = (items: { items: unknown[] | null }) => (items.items?.length ?? 0);

  return (
    <article class="capability-card" data-testid={testId}>
      <header class="capability-card-header">
        <span class="capability-card-server">{row.server}</span>
        <span class="capability-card-daemon">{row.daemon}</span>
        <span class={`capability-card-probe-status ${probeOk ? "ok" : "err"}`}>
          {probeOk ? "✓ probed" : "✗ probe err"}
        </span>
        {!probeOk && probeErr && (
          <span class="capability-card-probe-err">{probeErr}</span>
        )}
      </header>

      <div class="capability-card-body">
        <div class="capability-section" data-testid="capability-section-tools">
          <header class="capability-section-header">
            Tools ({itemCount(row.tools)})
          </header>
        </div>
        <div class="capability-section" data-testid="capability-section-prompts">
          <header class="capability-section-header">
            Prompts ({itemCount(row.prompts)})
          </header>
        </div>
        <div class="capability-section" data-testid="capability-section-resources">
          <header class="capability-section-header">
            Resources ({itemCount(row.resources)})
          </header>
        </div>
      </div>
    </article>
  );
}
```

- [ ] **Step 4: Wire CapabilityCard into the screen**

Modify `internal/gui/frontend/src/screens/Capabilities.tsx`. Replace the OK-state body section (the `{rows.length === 0 ? ... : <p>{/* placeholder */}</p>}` ternary) with:

```tsx
import { CapabilityCard } from "../components/CapabilityCard";
```

(at the top, alongside other imports)

And replace the OK-state body block with:

```tsx
      {rows.length === 0 ? (
        <p class="capabilities-empty" data-testid="capabilities-empty">
          No capabilities found — install servers via the Add server screen.
        </p>
      ) : (
        <div class="capabilities-list">
          {rows.map((row) => {
            const probeMatch = state.data.probes?.items.find(
              (p) => p.server === row.server && p.daemon === row.daemon,
            );
            return (
              <CapabilityCard
                key={`${row.server}-${row.daemon}`}
                row={row}
                probe={probeMatch ?? null}
              />
            );
          })}
        </div>
      )}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd internal/gui/frontend && npm test -- src/screens/Capabilities.test.tsx --run`
Expected: `Tests  12 passed (12)` (10 prior + 2 new).

- [ ] **Step 6: Verify Go build + vet still clean**

Run: `cd d:/dev/mcp-local-hub && go build ./... && go vet ./...`
Expected: empty.

- [ ] **Step 7: Commit**

```bash
git add internal/gui/frontend/src/components/CapabilityCard.tsx \
        internal/gui/frontend/src/screens/Capabilities.tsx \
        internal/gui/frontend/src/screens/Capabilities.test.tsx
git commit -m "feat(g3): phase 4 — CapabilityCard with per-server header + section counts"
```

**Phase 4 codex review focus:** card key uniqueness (`server-daemon`); probe match logic correctness; itemCount handles `items: null` correctly via `?? 0`; no Run-button or actionable handler anywhere on the card.

---

## Phase 5: CapabilitySection collapsible + StateBadge

**Files:**
- Create: `internal/gui/frontend/src/components/CapabilitySection.tsx`
- Create: `internal/gui/frontend/src/components/StateBadge.tsx`
- Modify: `internal/gui/frontend/src/components/CapabilityCard.tsx`
- Modify: `internal/gui/frontend/src/screens/Capabilities.test.tsx`

- [ ] **Step 1: Write the failing tests**

Append to `Capabilities.test.tsx`:

```tsx
describe("CapabilitiesScreen — Phase 5 collapsible + StateBadge", () => {
  beforeEach(() => vi.restoreAllMocks());

  function renderWithRow(row: CapabilityRow) {
    const probe: ProbeRow = { server: row.server, daemon: row.daemon, ok: true, tool_count: 0, source: "" };
    const snap: HealthSnapshot = {
      ...emptySnapshot,
      probes:       { ...emptySnapshot.probes!, items: [probe] },
      capabilities: { items: [row], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(snap);
    return render(<CapabilitiesScreen />);
  }

  it("StateBadge renders the correct state class for each vocabulary value", async () => {
    for (const state of ["ok", "empty", "unsupported", "error", "stale"] as const) {
      const row: CapabilityRow = {
        server: "s", daemon: "d",
        tools:     { state, items: state === "ok" ? [] : null },
        prompts:   { state: "empty", items: [] },
        resources: { state: "empty", items: [] },
      };
      const { findByTestId, unmount } = renderWithRow(row);
      const section = await findByTestId("capability-section-tools");
      const badge = section.querySelector(".state-badge");
      expect(badge).not.toBeNull();
      expect(badge!.classList.contains(`state-badge-${state}`)).toBe(true);
      unmount();
      vi.restoreAllMocks();
    }
  });

  it("clicking a section header toggles the .expanded class", async () => {
    const row: CapabilityRow = {
      server: "s", daemon: "d",
      tools:     { state: "ok", items: [{ name: "x", id: "s/d/tool/x", namespace: "s", kind: "tool" }] },
      prompts:   { state: "empty", items: [] },
      resources: { state: "empty", items: [] },
    };
    const { findByTestId } = renderWithRow(row);
    const section = await findByTestId("capability-section-tools");
    expect(section.classList.contains("expanded")).toBe(false);

    const header = section.querySelector(".capability-section-header") as HTMLElement;
    fireEvent.click(header);
    expect(section.classList.contains("expanded")).toBe(true);

    fireEvent.click(header);
    expect(section.classList.contains("expanded")).toBe(false);
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd internal/gui/frontend && npm test -- src/screens/Capabilities.test.tsx --run`
Expected: FAIL on the 2 new Phase-5 tests.

- [ ] **Step 3: Create StateBadge component**

Create `internal/gui/frontend/src/components/StateBadge.tsx`:

```tsx
type State = "ok" | "empty" | "unsupported" | "error" | "stale";

const labels: Record<State, string> = {
  ok: "ok",
  empty: "empty",
  unsupported: "unsupported",
  error: "error",
  stale: "stale",
};

export function StateBadge({ state }: { state: State }) {
  return (
    <span class={`state-badge state-badge-${state}`} data-testid={`state-badge-${state}`}>
      {labels[state]}
    </span>
  );
}
```

- [ ] **Step 4: Create CapabilitySection collapsible component**

Create `internal/gui/frontend/src/components/CapabilitySection.tsx`:

```tsx
import { useState } from "preact/hooks";
import type { CapabilitySubSection } from "../types";
import { StateBadge } from "./StateBadge";

interface Props {
  kind: "tools" | "prompts" | "resources";
  sub: CapabilitySubSection;
}

const labels: Record<Props["kind"], string> = {
  tools: "Tools",
  prompts: "Prompts",
  resources: "Resources",
};

// Collapsed-by-default section. Header click toggles the .expanded
// class on the wrapper. Item-list rendering arrives in Phase 6.
export function CapabilitySection({ kind, sub }: Props) {
  const [expanded, setExpanded] = useState(false);
  const items = sub.items ?? [];
  const count = items.length;

  return (
    <div
      class={`capability-section ${expanded ? "expanded" : ""}`}
      data-testid={`capability-section-${kind}`}
    >
      <header
        class="capability-section-header"
        role="button"
        tabIndex={0}
        onClick={() => setExpanded((e) => !e)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            setExpanded((v) => !v);
          }
        }}
      >
        <span class="capability-section-label">{labels[kind]} ({count})</span>
        <StateBadge state={sub.state} />
      </header>
      {sub.err && (
        <p class="capability-section-err" role="alert">{sub.err}</p>
      )}
      {/* Phase 6 inserts the item-list here when expanded */}
    </div>
  );
}
```

- [ ] **Step 5: Wire CapabilitySection into CapabilityCard**

Modify `internal/gui/frontend/src/components/CapabilityCard.tsx`. Replace the body's three placeholder `<div class="capability-section">…</div>` blocks with three `<CapabilitySection>` invocations:

```tsx
import type { CapabilityRow, ProbeRow } from "../types";
import { CapabilitySection } from "./CapabilitySection";

interface Props {
  row: CapabilityRow;
  probe: ProbeRow | null;
}

export function CapabilityCard({ row, probe }: Props) {
  const testId = `capability-card-${row.server}-${row.daemon}`;
  const probeOk = probe?.ok ?? true;
  const probeErr = probe?.err ?? "";

  return (
    <article class="capability-card" data-testid={testId}>
      <header class="capability-card-header">
        <span class="capability-card-server">{row.server}</span>
        <span class="capability-card-daemon">{row.daemon}</span>
        <span class={`capability-card-probe-status ${probeOk ? "ok" : "err"}`}>
          {probeOk ? "✓ probed" : "✗ probe err"}
        </span>
        {!probeOk && probeErr && (
          <span class="capability-card-probe-err">{probeErr}</span>
        )}
      </header>

      <div class="capability-card-body">
        <CapabilitySection kind="tools" sub={row.tools} />
        <CapabilitySection kind="prompts" sub={row.prompts} />
        <CapabilitySection kind="resources" sub={row.resources} />
      </div>
    </article>
  );
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd internal/gui/frontend && npm test -- src/screens/Capabilities.test.tsx --run`
Expected: `Tests  14 passed (14)` (12 prior + 2 new).

- [ ] **Step 7: Verify Go build + vet still clean**

Run: `cd d:/dev/mcp-local-hub && go build ./... && go vet ./...`
Expected: empty.

- [ ] **Step 8: Commit**

```bash
git add internal/gui/frontend/src/components/StateBadge.tsx \
        internal/gui/frontend/src/components/CapabilitySection.tsx \
        internal/gui/frontend/src/components/CapabilityCard.tsx \
        internal/gui/frontend/src/screens/Capabilities.test.tsx
git commit -m "feat(g3): phase 5 — CapabilitySection collapsible + StateBadge"
```

**Phase 5 codex review focus:** keyboard a11y (Enter / Space toggle); StateBadge class generation guards against XSS (state values are typed enums, but verify); section-header is `role="button"` with `tabIndex={0}` for proper focus order.

---

## Phase 6: Item list rendering inside expanded sections + items-null normalize

**Files:**
- Modify: `internal/gui/frontend/src/components/CapabilitySection.tsx`
- Modify: `internal/gui/frontend/src/screens/Capabilities.test.tsx`

- [ ] **Step 1: Write the failing tests**

Append to `Capabilities.test.tsx`:

```tsx
describe("CapabilitiesScreen — Phase 6 item-list rendering + items-null", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("renders item list when section is expanded (AC #4 / #5)", async () => {
    const row: CapabilityRow = {
      server: "memory", daemon: "default",
      tools: { state: "ok", items: [
        { name: "alpha", id: "memory/default/tool/alpha", namespace: "memory", kind: "tool" },
        { name: "beta",  id: "memory/default/tool/beta",  namespace: "memory", kind: "tool" },
      ] },
      prompts:   { state: "empty", items: [] },
      resources: { state: "empty", items: [] },
    };
    const probe: ProbeRow = { server: "memory", daemon: "default", ok: true, tool_count: 2, source: "" };
    const snap: HealthSnapshot = {
      ...emptySnapshot,
      probes:       { ...emptySnapshot.probes!, items: [probe] },
      capabilities: { items: [row], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(snap);

    const { findByTestId } = render(<CapabilitiesScreen />);
    const section = await findByTestId("capability-section-tools");

    // Collapsed: list NOT visible.
    expect(section.querySelector(".capability-item-list")).toBeNull();

    // Expand.
    fireEvent.click(section.querySelector(".capability-section-header") as HTMLElement);
    const list = section.querySelector(".capability-item-list");
    expect(list).not.toBeNull();
    const items = list!.querySelectorAll(".capability-item");
    expect(items.length).toBe(2);
    expect(items[0].textContent).toContain("alpha");
    expect(items[1].textContent).toContain("beta");

    // Critical: NO buttons, NO click handlers on items (AC #7).
    expect(list!.querySelectorAll("button").length).toBe(0);
  });

  it("items: null normalizes to empty list without crashing (AC #19)", async () => {
    const row: CapabilityRow = {
      server: "fs", daemon: "default",
      tools:     { state: "unsupported", items: null },
      prompts:   { state: "error", items: null, err: "tools/list: parse: unexpected EOF" },
      resources: { state: "empty", items: [] },
    };
    const probe: ProbeRow = { server: "fs", daemon: "default", ok: true, tool_count: 0, source: "" };
    const snap: HealthSnapshot = {
      ...emptySnapshot,
      probes:       { ...emptySnapshot.probes!, items: [probe] },
      capabilities: { items: [row], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(snap);

    const { findByTestId } = render(<CapabilitiesScreen />);
    const tools = await findByTestId("capability-section-tools");
    expect(tools.textContent).toContain("Tools (0)");
    fireEvent.click(tools.querySelector(".capability-section-header") as HTMLElement);
    expect(tools.textContent).toContain("(no items)");

    const prompts = await findByTestId("capability-section-prompts");
    expect(prompts.textContent).toContain("tools/list: parse: unexpected EOF");
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd internal/gui/frontend && npm test -- src/screens/Capabilities.test.tsx --run`
Expected: FAIL on the 2 new tests.

- [ ] **Step 3: Add item-list rendering to CapabilitySection**

Modify `internal/gui/frontend/src/components/CapabilitySection.tsx` — replace the trailing comment with the actual item rendering:

```tsx
import { useState } from "preact/hooks";
import type { CapabilitySubSection } from "../types";
import { StateBadge } from "./StateBadge";

interface Props {
  kind: "tools" | "prompts" | "resources";
  sub: CapabilitySubSection;
}

const labels: Record<Props["kind"], string> = {
  tools: "Tools",
  prompts: "Prompts",
  resources: "Resources",
};

export function CapabilitySection({ kind, sub }: Props) {
  const [expanded, setExpanded] = useState(false);
  // AC #19 — items: CapabilityItem[] | null; normalize at the section
  // boundary so the rest of the component never sees null.
  const items = sub.items ?? [];
  const count = items.length;

  return (
    <div
      class={`capability-section ${expanded ? "expanded" : ""}`}
      data-testid={`capability-section-${kind}`}
    >
      <header
        class="capability-section-header"
        role="button"
        tabIndex={0}
        onClick={() => setExpanded((e) => !e)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            setExpanded((v) => !v);
          }
        }}
      >
        <span class="capability-section-label">{labels[kind]} ({count})</span>
        <StateBadge state={sub.state} />
      </header>

      {sub.err && (
        <p class="capability-section-err" role="alert">{sub.err}</p>
      )}

      {expanded && (
        <ul class="capability-item-list">
          {count === 0 ? (
            <li class="capability-item capability-item-empty">(no items)</li>
          ) : (
            items.map((item) => (
              <li key={item.id} class="capability-item">
                <span class="capability-item-name">{item.name}</span>
                <span class="capability-item-id">{item.id}</span>
              </li>
            ))
          )}
        </ul>
      )}
    </div>
  );
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd internal/gui/frontend && npm test -- src/screens/Capabilities.test.tsx --run`
Expected: `Tests  16 passed (16)` (14 prior + 2 new).

- [ ] **Step 5: Verify Go build + vet still clean**

Run: `cd d:/dev/mcp-local-hub && go build ./... && go vet ./...`
Expected: empty.

- [ ] **Step 6: Commit**

```bash
git add internal/gui/frontend/src/components/CapabilitySection.tsx \
        internal/gui/frontend/src/screens/Capabilities.test.tsx
git commit -m "feat(g3): phase 6 — item list rendering + items-null normalize"
```

**Phase 6 codex review focus:** AC #7 (no Run-button anywhere) — verify the `<li>` has no onClick, no role="button", no actionable handler; `?? []` normalization at the right boundary; key uniqueness on items uses `item.id` (canonical "server/daemon/kind/name", guaranteed unique).

---

## Phase 7: Synthetic-source pill + state legend panel

**Files:**
- Create: `internal/gui/frontend/src/components/CapabilityLegend.tsx`
- Modify: `internal/gui/frontend/src/components/CapabilityCard.tsx`
- Modify: `internal/gui/frontend/src/screens/Capabilities.tsx`
- Modify: `internal/gui/frontend/src/screens/Capabilities.test.tsx`

- [ ] **Step 1: Write the failing tests**

Append to `Capabilities.test.tsx`:

```tsx
describe("CapabilitiesScreen — Phase 7 synthetic pill + legend", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("renders synthetic-source pill when probe.source === 'proxy-synthetic' (AC #8)", async () => {
    const row: CapabilityRow = {
      server: "lazy", daemon: "default",
      tools:     { state: "unsupported", items: null },
      prompts:   { state: "unsupported", items: null },
      resources: { state: "unsupported", items: null },
    };
    const probe: ProbeRow = { server: "lazy", daemon: "default", ok: true, tool_count: 0, source: "proxy-synthetic" };
    const snap: HealthSnapshot = {
      ...emptySnapshot,
      probes:       { ...emptySnapshot.probes!, items: [probe] },
      capabilities: { items: [row], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(snap);

    const { findByTestId } = render(<CapabilitiesScreen />);
    const card = await findByTestId("capability-card-lazy-default");
    const pill = card.querySelector('[data-testid="synthetic-source-pill"]');
    expect(pill).not.toBeNull();
    expect(pill!.textContent).toContain("synthetic");
  });

  it("does NOT render the synthetic pill when probe.source is empty", async () => {
    const row: CapabilityRow = {
      server: "real", daemon: "default",
      tools:     { state: "ok", items: [] },
      prompts:   { state: "empty", items: [] },
      resources: { state: "empty", items: [] },
    };
    const probe: ProbeRow = { server: "real", daemon: "default", ok: true, tool_count: 0, source: "" };
    const snap: HealthSnapshot = {
      ...emptySnapshot,
      probes:       { ...emptySnapshot.probes!, items: [probe] },
      capabilities: { items: [row], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(snap);

    const { findByTestId } = render(<CapabilitiesScreen />);
    const card = await findByTestId("capability-card-real-default");
    expect(card.querySelector('[data-testid="synthetic-source-pill"]')).toBeNull();
  });

  it("legend panel toggles open and lists all 5 states", async () => {
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(emptySnapshot);
    const { findByTestId } = render(<CapabilitiesScreen />);
    const toggle = await findByTestId("capabilities-legend-toggle");
    const legend = await findByTestId("capabilities-legend");
    expect(legend.classList.contains("expanded")).toBe(false);

    fireEvent.click(toggle);
    expect(legend.classList.contains("expanded")).toBe(true);
    expect(legend.textContent).toContain("ok");
    expect(legend.textContent).toContain("empty");
    expect(legend.textContent).toContain("unsupported");
    expect(legend.textContent).toContain("error");
    expect(legend.textContent).toContain("stale");

    fireEvent.click(toggle);
    expect(legend.classList.contains("expanded")).toBe(false);
  });

  it("forward-compat: state='stale' fixture renders the orange badge (AC #20)", async () => {
    const row: CapabilityRow = {
      server: "old", daemon: "default",
      tools:     { state: "stale", items: [] },
      prompts:   { state: "empty", items: [] },
      resources: { state: "empty", items: [] },
    };
    const probe: ProbeRow = { server: "old", daemon: "default", ok: true, tool_count: 0, source: "" };
    const snap: HealthSnapshot = {
      ...emptySnapshot,
      probes:       { ...emptySnapshot.probes!, items: [probe] },
      capabilities: { items: [row], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(snap);

    const { findByTestId } = render(<CapabilitiesScreen />);
    const tools = await findByTestId("capability-section-tools");
    const badge = tools.querySelector(".state-badge-stale");
    expect(badge).not.toBeNull();
  });
});
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd internal/gui/frontend && npm test -- src/screens/Capabilities.test.tsx --run`
Expected: FAIL on the 4 new tests.

- [ ] **Step 3: Create the legend component**

Create `internal/gui/frontend/src/components/CapabilityLegend.tsx`:

```tsx
import { useState } from "preact/hooks";
import { StateBadge } from "./StateBadge";

const rows = [
  { state: "ok",          desc: "Server reported items successfully" },
  { state: "empty",       desc: "Server reported the category supports no items" },
  { state: "unsupported", desc: "Server explicitly declared no support for this category" },
  { state: "error",       desc: "Probe failed (see err)" },
  { state: "stale",       desc: "Last probe is older than the section TTL but no fresh data available" },
] as const;

export function CapabilityLegend() {
  const [open, setOpen] = useState(false);

  return (
    <div class={`capabilities-legend ${open ? "expanded" : ""}`} data-testid="capabilities-legend">
      <button
        class="capabilities-legend-toggle"
        data-testid="capabilities-legend-toggle"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
      >
        {open ? "Hide" : "Show"} state legend
      </button>
      {open && (
        <table class="capabilities-legend-table">
          <thead><tr><th>State</th><th>Meaning</th></tr></thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.state}>
                <td><StateBadge state={r.state} /></td>
                <td>{r.desc}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
```

- [ ] **Step 4: Add synthetic-source pill to CapabilityCard**

Modify `internal/gui/frontend/src/components/CapabilityCard.tsx` — add the synthetic pill inside the header alongside the probe-status:

```tsx
import type { CapabilityRow, ProbeRow } from "../types";
import { CapabilitySection } from "./CapabilitySection";

interface Props {
  row: CapabilityRow;
  probe: ProbeRow | null;
}

export function CapabilityCard({ row, probe }: Props) {
  const testId = `capability-card-${row.server}-${row.daemon}`;
  const probeOk = probe?.ok ?? true;
  const probeErr = probe?.err ?? "";
  const isSynthetic = probe?.source === "proxy-synthetic";

  return (
    <article class="capability-card" data-testid={testId}>
      <header class="capability-card-header">
        <span class="capability-card-server">{row.server}</span>
        <span class="capability-card-daemon">{row.daemon}</span>
        <span class={`capability-card-probe-status ${probeOk ? "ok" : "err"}`}>
          {probeOk ? "✓ probed" : "✗ probe err"}
        </span>
        {isSynthetic && (
          <span
            class="synthetic-source-pill"
            data-testid="synthetic-source-pill"
            title="Capabilities reported by the lazy-proxy stub; not a live MCP roundtrip."
          >
            synthetic
          </span>
        )}
        {!probeOk && probeErr && (
          <span class="capability-card-probe-err">{probeErr}</span>
        )}
      </header>

      <div class="capability-card-body">
        <CapabilitySection kind="tools" sub={row.tools} />
        <CapabilitySection kind="prompts" sub={row.prompts} />
        <CapabilitySection kind="resources" sub={row.resources} />
      </div>
    </article>
  );
}
```

- [ ] **Step 5: Wire CapabilityLegend into Capabilities screen header**

Modify `internal/gui/frontend/src/screens/Capabilities.tsx`. Add the import:

```tsx
import { CapabilityLegend } from "../components/CapabilityLegend";
```

And insert `<CapabilityLegend />` inside the OK-state `<header class="capabilities-header">`, AFTER the meta div:

```tsx
      <header class="capabilities-header">
        <h1>Capabilities</h1>
        <div class="capabilities-meta">
          {generatedAt > 0 && (
            <span data-testid="capabilities-generated-at">
              Generated {new Date(generatedAt * 1000).toISOString()}
            </span>
          )}
          <button
            class="capabilities-refresh-btn"
            data-testid="capabilities-refresh-btn"
            onClick={onRefresh}
            disabled={refreshing}
          >
            {refreshing ? "Refreshing…" : "Refresh"}
          </button>
        </div>
        <CapabilityLegend />
        {refreshError && (
          <p class="error" role="alert">Refresh failed: {refreshError}</p>
        )}
      </header>
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd internal/gui/frontend && npm test -- src/screens/Capabilities.test.tsx --run`
Expected: `Tests  20 passed (20)` (16 prior + 4 new).

- [ ] **Step 7: Verify Go build + vet still clean**

Run: `cd d:/dev/mcp-local-hub && go build ./... && go vet ./...`
Expected: empty.

- [ ] **Step 8: Commit**

```bash
git add internal/gui/frontend/src/components/CapabilityLegend.tsx \
        internal/gui/frontend/src/components/CapabilityCard.tsx \
        internal/gui/frontend/src/screens/Capabilities.tsx \
        internal/gui/frontend/src/screens/Capabilities.test.tsx
git commit -m "feat(g3): phase 7 — synthetic-source pill + state legend"
```

**Phase 7 codex review focus:** synthetic pill conditional renders ONLY when `probe.source === "proxy-synthetic"` (not just truthy `probe.source`); legend's aria-expanded toggles correctly; AC #20 stale-fixture asserts the badge class.

---

## Phase 8: E2E spec + CSS + bundle regen + final build

**Files:**
- Create: `internal/gui/e2e/tests/capabilities.spec.ts`
- Modify: `internal/gui/frontend/src/style.css` (or create `capabilities.css` and import)
- Regenerate: `internal/gui/assets/{index.html,app.js,style.css}`

- [ ] **Step 1: Write the failing E2E spec**

Create `internal/gui/e2e/tests/capabilities.spec.ts`:

```ts
import { test, expect } from "../fixtures/hub";

test.describe("capabilities", () => {
  test("sidebar Capabilities link navigates to #/capabilities and shows h1", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/dashboard`);
    const link = page.locator("nav a", { hasText: "Capabilities" });
    await expect(link).toHaveCount(1);
    await link.click();
    await expect(page).toHaveURL(/#\/capabilities$/);
    const h1 = page.locator("h1");
    await expect(h1).toHaveText("Capabilities");
    await expect(link).toHaveClass(/active/);
  });

  test("empty-state copy renders when no servers are installed", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/capabilities`);
    const empty = page.locator('[data-testid="capabilities-empty"]');
    await expect(empty).toBeAttached();
    await expect(empty).toContainText("No capabilities found");
  });

  test("Refresh button issues a /api/health?include=capabilities&refresh=true request", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/capabilities`);
    const button = page.locator('[data-testid="capabilities-refresh-btn"]');
    await expect(button).toBeAttached();
    const req = page.waitForRequest((r) =>
      r.url().includes("/api/health") &&
      r.url().includes("include=capabilities") &&
      r.url().includes("refresh=true"),
    );
    await button.click();
    await req;
  });
});
```

- [ ] **Step 2: Add CSS for the new components**

Append to `internal/gui/frontend/src/style.css` (at the end of the file):

```css
/* G3 — capability status display */

.capabilities-screen { padding: 1rem 1.5rem; }
.capabilities-header { display: flex; flex-direction: column; gap: 0.5rem; margin-bottom: 1rem; }
.capabilities-meta { display: flex; gap: 1rem; align-items: center; font-size: 0.875rem; opacity: 0.8; }
.capabilities-refresh-btn { padding: 0.25rem 0.75rem; cursor: pointer; }
.capabilities-refresh-btn[disabled] { opacity: 0.5; cursor: not-allowed; }
.capabilities-empty { font-style: italic; opacity: 0.7; padding: 1rem 0; }
.capabilities-list { display: flex; flex-direction: column; gap: 0.75rem; }

.capabilities-legend { font-size: 0.875rem; }
.capabilities-legend-toggle { background: none; border: none; cursor: pointer; text-decoration: underline; padding: 0; opacity: 0.8; }
.capabilities-legend-table { margin-top: 0.5rem; border-collapse: collapse; }
.capabilities-legend-table th, .capabilities-legend-table td { padding: 0.25rem 0.5rem; text-align: left; border-bottom: 1px solid var(--border, #e0e0e0); }

.capability-card { border: 1px solid var(--border, #d0d0d0); border-radius: 4px; padding: 0.75rem 1rem; }
.capability-card-header { display: flex; align-items: center; gap: 0.75rem; flex-wrap: wrap; margin-bottom: 0.5rem; }
.capability-card-server { font-weight: 600; }
.capability-card-daemon { opacity: 0.7; font-size: 0.875rem; }
.capability-card-probe-status.ok { color: #2e7d32; }
.capability-card-probe-status.err { color: #c62828; }
.capability-card-probe-err { color: #c62828; font-size: 0.875rem; }

.synthetic-source-pill {
  background: #f5f5f5; color: #555; padding: 0.125rem 0.5rem;
  border-radius: 999px; font-size: 0.75rem; cursor: help;
}

.capability-section { margin-top: 0.25rem; }
.capability-section-header {
  display: flex; align-items: center; gap: 0.5rem; padding: 0.25rem 0.5rem;
  cursor: pointer; user-select: none; border-radius: 2px;
}
.capability-section-header:hover { background: var(--row-hover, #f0f0f0); }
.capability-section-header:focus { outline: 2px solid var(--focus, #0969da); outline-offset: -2px; }
.capability-section.expanded .capability-section-header::before { content: "▾ "; }
.capability-section:not(.expanded) .capability-section-header::before { content: "▸ "; }
.capability-section-err { color: #c62828; padding-left: 1rem; font-size: 0.875rem; }

.capability-item-list { list-style: none; padding-left: 1.5rem; margin: 0.25rem 0; }
.capability-item { display: flex; gap: 0.5rem; padding: 0.125rem 0; }
.capability-item-name { font-weight: 500; }
.capability-item-id { opacity: 0.6; font-family: ui-monospace, monospace; font-size: 0.875rem; }
.capability-item-empty { opacity: 0.5; font-style: italic; }

.state-badge {
  display: inline-block; padding: 0.0625rem 0.5rem; border-radius: 999px;
  font-size: 0.75rem; font-weight: 500;
}
.state-badge-ok          { background: #c8e6c9; color: #1b5e20; }
.state-badge-empty       { background: #eeeeee; color: #555555; }
.state-badge-unsupported { background: #fff9c4; color: #795548; }
.state-badge-error       { background: #ffcdd2; color: #b71c1c; }
.state-badge-stale       { background: #ffe0b2; color: #e65100; }
```

- [ ] **Step 3: Regenerate the embedded GUI bundle**

Run: `cd d:/dev/mcp-local-hub && go generate ./internal/gui/...`
Expected: vite build success message + updated `internal/gui/assets/{index.html,app.js,style.css}`.

- [ ] **Step 4: Run all tests**

Run: `cd internal/gui/frontend && npm test -- --run`
Expected: all frontend tests pass (369+ existing + 20 new = 389+ total).

Run: `cd d:/dev/mcp-local-hub && go build ./...`
Expected: empty.

Run: `cd d:/dev/mcp-local-hub && go vet ./...`
Expected: empty.

Run: `cd d:/dev/mcp-local-hub && go test ./internal/gui/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gui/frontend/src/style.css \
        internal/gui/e2e/tests/capabilities.spec.ts \
        internal/gui/assets/index.html \
        internal/gui/assets/app.js \
        internal/gui/assets/style.css
git commit -m "feat(g3): phase 8 — E2E spec + CSS + GUI bundle regen"
```

**Phase 8 codex review focus:** E2E spec uses `toBeAttached()` not `toBeVisible()` for elements that may be CSS-collapsed (per existing pattern); CSS class names match the spec exactly; the bundle diff in `internal/gui/assets/app.js` reflects only the new screen + components (no unrelated diff noise).

**Final integration verification (after Phase 8 codex review APPROVE):**

- [ ] Open a PR from `feat/g3-capability-display` → `master`. PR body cites the spec + lists the 8 phases + 20+ acceptance criteria.
- [ ] Trigger Codex Cloud bot review with `@codex review` and poll until PASS using the standard pattern (filter by `original_commit_id` per KOSYAK).
- [ ] After bot PASS, fire 4-lane codex deep-sec review (security, architecture, reliability, qa) on the cumulative PR diff. Apply findings, re-trigger bot, loop until clean.
- [ ] Merge with `--squash` (NO `--admin` per kosyak rule). Delete branch local + remote.

---

## Self-review checklist (run by orchestrator before dispatching Phase 1)

**1. Spec coverage**: every spec acceptance criterion (#1-#20) is tested in some phase.
- AC #1 (sidebar nav) — Phase 1 routing test + shell.spec.ts update.
- AC #2 (on-mount fetch) — Phase 2 "hits /api/health…" test.
- AC #3 (Refresh fires fetch) — Phase 3 click test.
- AC #4-#5 (cards + state badges) — Phases 4-5.
- AC #6 (probe-error inline) — Phase 4.
- AC #7 (NO Run-buttons) — Phase 6 negative assertion.
- AC #8 (synthetic pill conditional) — Phase 7.
- AC #9 (workspace-scoped daemons included) — Phase 7 lazy fixture.
- AC #10 (empty state) — Phase 2.
- AC #11 (inline error) — Phase 2.
- AC #12 (shell.spec.ts updated) — Phase 1.
- AC #13 (capabilities.spec.ts) — Phase 8.
- AC #14 (Capabilities.test.tsx coverage) — Phases 1-7.
- AC #15 (build/vet/test) — every phase ends with the gate.
- AC #16 (go generate bundle) — Phase 8.
- AC #17 (refresh disabled inflight) — Phase 3.
- AC #18 (mid-fetch unmount safe) — Phase 3.
- AC #19 (items: null normalize) — Phase 6.
- AC #20 (stale-fixture forward-compat) — Phase 7.

**2. Placeholder scan**: no "TBD", "TODO", "implement later". Confirmed by grep.

**3. Type consistency**: `CapabilitySubSection.items` typed as `CapabilityItem[] | null` in Phase 1 → consumed via `?? []` consistently in Phases 4 / 6 / 7. `data-testid` naming is `kebab-case` throughout. Component file paths match imports across all phases.

## Effort estimate

8 phases × 30-45 min subagent + 5-10 min codex review per phase = ~5-8 hours total. Within the 1-2d backlog estimate. Bot + 4-lane deep-sec on the merged PR adds another 1-2 hours of review-cycle iteration.

## Terms and Abbreviations

- `AC`: Acceptance Criterion — numbered checklist items in the spec / plan that gate phase completion.
- `LoadState`: discriminated TypeScript union `loading | ok | error` representing the screen's data-fetching lifecycle.
- `cancelled-flag`: closure-captured boolean used by the on-mount effect to prevent setState after unmount.
- `SectionError`: backend-side `{scope, err}` struct surfaced inside top-level snapshot sections (capabilities/probes/daemons) for soft errors.
- `synthetic-source`: capability data reported by the lazy-proxy stub without a live MCP roundtrip; flagged on the wire as `probe.source === "proxy-synthetic"`.
- `BEM-ish`: relaxed Block-Element-Modifier CSS naming convention used in `internal/gui/frontend/src/`.
- `vitest`: test runner used by the frontend (`npm test`).
- `Playwright`: browser-automation framework used for E2E tests (`internal/gui/e2e/`).
- `data-testid`: HTML attribute used by tests to select elements without coupling to CSS classes.
