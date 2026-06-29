import { render, cleanup, fireEvent } from "@testing-library/preact";
import { describe, it, expect, afterEach, vi, beforeEach } from "vitest";
import { CapabilitiesScreen } from "./Capabilities";
import * as api from "../api";
import type { HealthSnapshot, CapabilityRow, ProbeRow } from "../types";

afterEach(cleanup);

describe("CapabilitiesScreen — Phase 1 placeholder", () => {
  // Codex bot PR #144 r10 qa MINOR: mock the on-mount fetch in these
  // placeholder tests. Without it, vitest runs unmocked fetch against
  // jsdom localhost:3000 and emits ECONNREFUSED stderr noise after
  // each render. Tests pass either way (heading renders before fetch
  // resolves), but the noise pollutes CI output.
  beforeEach(() => {
    vi.spyOn(api, "fetchOrThrow").mockReturnValue(new Promise(() => { /* never resolves */ }));
  });
  afterEach(() => vi.restoreAllMocks());

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

// Phase 2 fixture mirrors the HealthSnapshot wire types corrected in
// commit a258f27: hub.started_at is RFC3339 string (not unix seconds),
// hub.lock is required (no omitempty on the Go side), hub.ttl_ms is
// nullable, daemon.last_restart_at is *string, probe.err and probe.source
// are required (always emitted, "" when no err / no synthetic source).
const emptySnapshot: HealthSnapshot = {
  schema_version: "1",
  hub: {
    version: "0.3.0",
    commit: "abc",
    build_date: "2026-05-08",
    started_at: "2026-05-08T00:00:00Z",
    lock: { pid: 0, port: 0 },
    generated_at: 0,
    ttl_ms: null,
  },
  daemons: { items: [], generated_at: 0, ttl_ms: 2000, errors: [] },
  probes: { items: [], generated_at: 0, ttl_ms: 10000, errors: [] },
  capabilities: { items: [], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
};

describe("CapabilitiesScreen — Phase 2 LoadState", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("renders the loading state while fetch is in flight", () => {
    vi.spyOn(api, "fetchOrThrow").mockReturnValue(new Promise(() => { /* never resolves */ }));
    const { getByText, container } = render(<CapabilitiesScreen />);
    expect(getByText("Loading capabilities")).toBeTruthy();
    expect(container.querySelector(".loading-state")).toBeTruthy();
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

  it("mid-REFRESH-fetch unmount does NOT call setState (AC #18, codex stage-1 BLOCKER fix)", async () => {
    // Codex stage-1 review finding #1: AC #18 wants the REFRESH path
    // tested, not the initial mount fetch. The mountedRef guard added
    // to onRefresh below must prevent setRefreshing(false) /
    // setRefreshError() / setState({status:"ok"}) from firing on a
    // stale instance. Pattern: render → wait for OK state → click
    // Refresh (deferred fetch) → unmount BEFORE refresh resolves →
    // resolve the refresh → assert no console.error.
    let resolveRefresh!: (value: HealthSnapshot) => void;
    const refreshDeferred = new Promise<HealthSnapshot>((resolve) => {
      resolveRefresh = resolve;
    });
    vi.spyOn(api, "fetchOrThrow")
      .mockResolvedValueOnce(emptySnapshot)   // initial mount fetch resolves
      .mockReturnValueOnce(refreshDeferred);  // refresh stays pending

    const consoleSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const { findByTestId, unmount } = render(<CapabilitiesScreen />);
    const button = await findByTestId("capabilities-refresh-btn");
    fireEvent.click(button);
    await Promise.resolve();          // refresh fetch fires
    unmount();                        // unmount BEFORE refresh resolves
    resolveRefresh(emptySnapshot);
    await refreshDeferred;
    await Promise.resolve();
    expect(consoleSpy).not.toHaveBeenCalled();
    consoleSpy.mockRestore();
  });

  it("mid-MOUNT-fetch unmount does NOT call setState (companion guard)", async () => {
    // Companion test for the initial-mount cancelled-flag pattern.
    // The original AC #18 plan tested only this path; codex stage-1
    // requires both this AND the refresh-path test above.
    let resolveFn!: (value: HealthSnapshot) => void;
    const deferred = new Promise<HealthSnapshot>((resolve) => { resolveFn = resolve; });
    vi.spyOn(api, "fetchOrThrow").mockReturnValue(deferred);

    const consoleSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const { unmount } = render(<CapabilitiesScreen />);
    unmount();
    resolveFn(emptySnapshot);
    await deferred;
    await Promise.resolve();
    expect(consoleSpy).not.toHaveBeenCalled();
    consoleSpy.mockRestore();
  });

  it("distinguishes probe/capability failures from true empty state (codex bot PR #144 round 4 P2)", async () => {
    // Codex bot finding: when daemons exist but probes/capabilities
    // failed, the backend legitimately returns capabilities.items=[]
    // (per health.go::computeCapabilitiesSection skip-on-failure).
    // The UI MUST distinguish "no servers installed" (truly empty)
    // from "servers installed but failed to probe" (real failure).
    const failureSnapshot: HealthSnapshot = {
      ...emptySnapshot,
      daemons: {
        items: [
          { server: "fs", daemon: "default", pid: 0, port: 0, ram_bytes: 0,
            uptime_sec: 0, state: "running", restart_count: 0, last_restart_at: null },
        ],
        generated_at: 0, ttl_ms: 2000, errors: [],
      },
      probes: {
        items: [],
        generated_at: 0, ttl_ms: 10000,
        errors: [{ scope: "probe:fs/default", err: "initialize: HTTP 500" }],
      },
      capabilities: {
        items: [],
        generated_at: 1715164800, ttl_ms: 60000,
        errors: [{ scope: "capability:fs/default", err: "tools/list: parse: unexpected EOF" }],
      },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(failureSnapshot);

    const { findByTestId, queryByTestId } = render(<CapabilitiesScreen />);
    // Failure-empty branch should render with section errors listed.
    const failureEmpty = await findByTestId("capabilities-empty-failures");
    expect(failureEmpty.textContent).toContain("Capabilities not yet available");
    expect(failureEmpty.textContent).toContain("1 daemon");
    expect(failureEmpty.textContent).toContain("probe:fs/default");
    expect(failureEmpty.textContent).toContain("initialize: HTTP 500");
    expect(failureEmpty.textContent).toContain("capability:fs/default");
    expect(failureEmpty.textContent).toContain("tools/list: parse: unexpected EOF");
    // True empty-state copy MUST NOT be present (would misdirect operator).
    expect(queryByTestId("capabilities-empty")).toBeNull();
  });

  it("failed probe.items[*].ok=false with empty probes.errors triggers failure-empty (codex bot PR #144 round 7 P2 #1)", async () => {
    // Codex bot finding (round 7 #1): probe failures often arrive as
    // `probes.items[*].ok === false` with non-empty err, NOT in
    // `probes.errors[]`. computeCapabilitiesSection skips !p.OK,
    // leaving capabilities.items=[]. Without checking failedProbes,
    // the screen would misdiagnose this as "No capabilities found".
    const failedProbeSnapshot: HealthSnapshot = {
      ...emptySnapshot,
      daemons: {
        items: [
          { server: "fs", daemon: "default", pid: 0, port: 0, ram_bytes: 0,
            uptime_sec: 0, state: "running", restart_count: 0, last_restart_at: null },
        ],
        generated_at: 0, ttl_ms: 2000, errors: [],
      },
      probes: {
        items: [
          { server: "fs", daemon: "default", ok: false, tool_count: 0,
            err: "initialize: HTTP 500", source: "" },
        ],
        generated_at: 0, ttl_ms: 10000, errors: [],
      },
      capabilities: { items: [], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(failedProbeSnapshot);

    const { findByTestId, queryByTestId } = render(<CapabilitiesScreen />);
    const failureEmpty = await findByTestId("capabilities-empty-failures");
    expect(failureEmpty.textContent).toContain("Capabilities not yet available");
    expect(failureEmpty.textContent).toContain("probe:fs/default");
    expect(failureEmpty.textContent).toContain("initialize: HTTP 500");
    // Negative: must NOT show the canonical "install servers" copy.
    expect(queryByTestId("capabilities-empty")).toBeNull();
  });

  it("partial failures show section-errors banner alongside successful cards (codex bot PR #144 round 7 P2 #2)", async () => {
    // Codex bot finding (round 7 #2): when one daemon succeeds and
    // another fails, section.errors WERE only rendered inside the
    // empty-state branch. Operators with partial failures lost
    // visibility into the failed daemons. The partial-failures banner
    // must render alongside successful cards.
    const partialSnapshot: HealthSnapshot = {
      ...emptySnapshot,
      daemons: {
        items: [
          { server: "memory", daemon: "default", pid: 0, port: 0, ram_bytes: 0,
            uptime_sec: 0, state: "running", restart_count: 0, last_restart_at: null },
          { server: "fs", daemon: "default", pid: 0, port: 0, ram_bytes: 0,
            uptime_sec: 0, state: "running", restart_count: 0, last_restart_at: null },
        ],
        generated_at: 0, ttl_ms: 2000, errors: [],
      },
      probes: {
        items: [
          { server: "memory", daemon: "default", ok: true, tool_count: 1, err: "", source: "" },
        ],
        generated_at: 0, ttl_ms: 10000, errors: [],
      },
      capabilities: {
        items: [
          { server: "memory", daemon: "default",
            tools:     { state: "ok", items: [{ name: "alpha", id: "memory/default/tool/alpha", namespace: "memory", kind: "tool" }] },
            prompts:   { state: "empty", items: [] },
            resources: { state: "empty", items: [] } },
        ],
        generated_at: 1715164800, ttl_ms: 60000,
        errors: [{ scope: "capability:fs/default", err: "tools/list: timeout" }],
      },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(partialSnapshot);

    const { findByTestId, queryByTestId } = render(<CapabilitiesScreen />);
    // Successful card renders.
    const card = await findByTestId("capability-card-memory-default");
    expect(card).toBeTruthy();
    // Partial-failures banner ALSO renders alongside.
    const banner = await findByTestId("capabilities-partial-failures");
    expect(banner.textContent).toContain("Some daemons reported probe or capability failures");
    expect(banner.textContent).toContain("capability:fs/default");
    expect(banner.textContent).toContain("tools/list: timeout");
    // Empty-state branch must NOT render — there are cards.
    expect(queryByTestId("capabilities-empty")).toBeNull();
    expect(queryByTestId("capabilities-empty-failures")).toBeNull();
  });

  it("probe.ok=false with sentinel err (daemon not running) is NOT a failure (codex bot PR #144 round 8 P2)", async () => {
    // Codex bot finding (round 8): health.go emits ok=false +
    // err="no probe (daemon not running or probe disabled)" for
    // daemons that are stopped or have probing disabled. This is a
    // normal operator state. Treating it as a hard failure made the
    // failure-empty banner fire for healthy stopped systems.
    const sentinelSnapshot: HealthSnapshot = {
      ...emptySnapshot,
      daemons: {
        items: [
          { server: "fs", daemon: "default", pid: 0, port: 0, ram_bytes: 0,
            uptime_sec: 0, state: "stopped", restart_count: 0, last_restart_at: null },
        ],
        generated_at: 0, ttl_ms: 2000, errors: [],
      },
      probes: {
        items: [
          { server: "fs", daemon: "default", ok: false, tool_count: 0,
            err: "no probe (daemon not running or probe disabled)", source: "" },
        ],
        generated_at: 0, ttl_ms: 10000, errors: [],
      },
      capabilities: { items: [], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(sentinelSnapshot);

    const { findByTestId, queryByTestId } = render(<CapabilitiesScreen />);
    // Should show canonical empty-state, NOT failure-empty.
    const empty = await findByTestId("capabilities-empty");
    expect(empty.textContent).toContain("No capabilities found");
    expect(queryByTestId("capabilities-empty-failures")).toBeNull();
    expect(queryByTestId("capabilities-partial-failures")).toBeNull();
  });

  it("failure-empty count reflects ONLY failed daemons, not total (codex bot PR #144 r10 P2)", async () => {
    // Codex bot finding (round 10): the banner used to interpolate
    // daemons.items.length (total known daemons), which overstated
    // impact when only some failed. With 1 stopped daemon (not a
    // failure, sentinel err) + 1 daemon with real probe error, the
    // banner should say "failed for 1 daemon", NOT "failed for 2".
    const mixedSnapshot: HealthSnapshot = {
      ...emptySnapshot,
      daemons: {
        items: [
          { server: "stopped-svc", daemon: "default", pid: 0, port: 0, ram_bytes: 0,
            uptime_sec: 0, state: "stopped", restart_count: 0, last_restart_at: null },
          { server: "broken-svc", daemon: "default", pid: 0, port: 0, ram_bytes: 0,
            uptime_sec: 0, state: "running", restart_count: 0, last_restart_at: null },
        ],
        generated_at: 0, ttl_ms: 2000, errors: [],
      },
      probes: {
        items: [
          // Stopped daemon with sentinel — NOT counted as a failure.
          { server: "stopped-svc", daemon: "default", ok: false, tool_count: 0,
            err: "no probe (daemon not running or probe disabled)", source: "" },
          // Real probe error — counted.
          { server: "broken-svc", daemon: "default", ok: false, tool_count: 0,
            err: "initialize: HTTP 500", source: "" },
        ],
        generated_at: 0, ttl_ms: 10000, errors: [],
      },
      capabilities: { items: [], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(mixedSnapshot);

    const { findByTestId } = render(<CapabilitiesScreen />);
    const failureEmpty = await findByTestId("capabilities-empty-failures");
    // CRITICAL: must say "1 daemon" (singular), NOT "2 daemons" (total).
    expect(failureEmpty.textContent).toContain("failed for 1 daemon");
    expect(failureEmpty.textContent).not.toContain("failed for 2 daemons");
    // The broken-svc probe error is visible.
    expect(failureEmpty.textContent).toContain("broken-svc/default");
    expect(failureEmpty.textContent).toContain("initialize: HTTP 500");
    // The stopped-svc sentinel is NOT shown (filtered out by isActionableProbeFailure).
    expect(failureEmpty.textContent).not.toContain("stopped-svc");
  });

  it("daemons present but NO errors → canonical empty (not failure-empty) (codex bot PR #144 round 6 P2)", async () => {
    // Codex bot finding (round 6): daemons with probe.ok=false are
    // SKIPPED from capabilities.items by computeCapabilitiesSection
    // (legitimately yielding rows=[] without errors). Showing the
    // red "failed for N daemons" alert misreports a healthy-but-
    // stopped setup as a failure. The failure-empty branch must
    // trigger ONLY on real backend errors, NOT on daemonCount alone.
    const stoppedSnapshot: HealthSnapshot = {
      ...emptySnapshot,
      daemons: {
        items: [
          { server: "fs", daemon: "default", pid: 0, port: 0, ram_bytes: 0,
            uptime_sec: 0, state: "stopped", restart_count: 0, last_restart_at: null },
        ],
        generated_at: 0, ttl_ms: 2000, errors: [],
      },
      probes: { items: [], generated_at: 0, ttl_ms: 10000, errors: [] },
      capabilities: { items: [], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(stoppedSnapshot);

    const { findByTestId, queryByTestId } = render(<CapabilitiesScreen />);
    // Should show the canonical empty-state, NOT the failure-empty.
    const empty = await findByTestId("capabilities-empty");
    expect(empty.textContent).toContain("No capabilities found");
    expect(queryByTestId("capabilities-empty-failures")).toBeNull();
  });

  it("true empty state renders the install-servers copy when no daemons + no errors (codex bot PR #144 round 4 P2 negative case)", async () => {
    // Companion: when daemons.items=[] AND no errors, show the
    // canonical "No capabilities found — install servers" copy.
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(emptySnapshot);
    const { findByTestId, queryByTestId } = render(<CapabilitiesScreen />);
    const empty = await findByTestId("capabilities-empty");
    expect(empty.textContent).toContain("No capabilities found");
    expect(queryByTestId("capabilities-empty-failures")).toBeNull();
  });

  it("two same-tick Refresh clicks fire only ONE fetch (codex deep-sec PR #144 r10 reliability MINOR)", async () => {
    // Codex deep-sec finding: refresh single-flight guard. The
    // `refreshing` state alone has a one-tick gap before Preact
    // commits the disabled prop. Two same-tick click handlers both
    // observe `refreshing===false` and would both fire fetches with
    // last-writer-wins races. The synchronous refreshInFlightRef
    // closes that gap.
    let resolveRefresh!: (value: HealthSnapshot) => void;
    const refreshDeferred = new Promise<HealthSnapshot>((resolve) => {
      resolveRefresh = resolve;
    });
    const spy = vi.spyOn(api, "fetchOrThrow")
      .mockResolvedValueOnce(emptySnapshot)   // initial mount fetch
      .mockReturnValueOnce(refreshDeferred);  // refresh stays pending

    const { findByTestId } = render(<CapabilitiesScreen />);
    const button = await findByTestId("capabilities-refresh-btn");
    // Two clicks in the same synchronous tick.
    fireEvent.click(button);
    fireEvent.click(button);
    await Promise.resolve();
    // Only ONE refresh fetch should have fired (initial + 1 refresh = 2 total).
    expect(spy).toHaveBeenCalledTimes(2);
    expect(spy).toHaveBeenLastCalledWith("/api/health?include=capabilities&refresh=true", "object");
    resolveRefresh(emptySnapshot);
    await refreshDeferred;
  });

  it("error-state Refresh failure surfaces LATEST retry error, not stale initial-load error (codex bot PR #144 round 3 P2)", async () => {
    // Codex bot finding: when initial load fails, state.error holds
    // the first error. If user clicks Refresh in error state and the
    // retry ALSO fails (with a different error), only refreshError was
    // updated — state.error stayed stale, and the error branch only
    // displays state.error. Result: user kept seeing the original
    // diagnostic and lost context on what just failed.
    vi.spyOn(api, "fetchOrThrow")
      .mockRejectedValueOnce(new Error("first error: daemon starting"))
      .mockRejectedValueOnce(new Error("second error: rate limited"));

    const { findByRole, findByTestId } = render(<CapabilitiesScreen />);
    const initialAlert = await findByRole("alert");
    expect(initialAlert.textContent).toContain("first error: daemon starting");
    const button = await findByTestId("capabilities-refresh-btn");
    fireEvent.click(button);
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
    // After retry failure, the alert MUST reflect the LATEST error.
    const updatedAlert = await findByRole("alert");
    expect(updatedAlert.textContent).toContain("second error: rate limited");
    expect(updatedAlert.textContent).not.toContain("first error: daemon starting");
  });

  it("error state still renders Refresh button so initial-load failure isn't a dead end (codex bot PR #144 round 2 P2)", async () => {
    // Codex bot finding: when /api/health initial fetch fails, the
    // error branch must not strip the Refresh affordance. Operators
    // hitting a transient failure (daemon still starting, network
    // hiccup) need to retry from the screen itself.
    const okSnapshot: HealthSnapshot = { ...emptySnapshot,
      capabilities: { items: [], generated_at: 1715164800, ttl_ms: 60000, errors: [] } };
    vi.spyOn(api, "fetchOrThrow")
      .mockRejectedValueOnce(new Error("daemon still starting"))
      .mockResolvedValueOnce(okSnapshot);

    const { findByRole, findByTestId } = render(<CapabilitiesScreen />);
    // Initial render should land on the error branch.
    const alert = await findByRole("alert");
    expect(alert.textContent).toContain("daemon still starting");
    // Refresh button MUST be present in error state (regression guard).
    const button = await findByTestId("capabilities-refresh-btn");
    expect(button).toBeTruthy();
    // Click Refresh: fetch resolves, state flips to ok-empty.
    fireEvent.click(button);
    await Promise.resolve();
    await Promise.resolve();
    const empty = await findByTestId("capabilities-empty");
    expect(empty).toBeTruthy();
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
    // Third microtask hop: pre-rejected promise → .then → .catch → setStates
    // → Preact's debounceRendering (Promise.resolve().then) → DOM commit. The
    // catch path adds one extra microtask vs. the resolve path because the
    // rejection has to traverse .then before reaching .catch.
    await Promise.resolve();

    const alert = queryByRole("alert");
    expect(alert).not.toBeNull();
    expect(alert!.textContent).toContain("rate limited");
    // Prior empty-state still visible (we did not blank the screen).
    const stillEmpty = await findByTestId("capabilities-empty");
    expect(stillEmpty).toBeTruthy();
  });
});

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
    const probe: ProbeRow = { server: "memory", daemon: "default", ok: true, tool_count: 2, err: "", source: "" };
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
    const probe: ProbeRow = { server: "filesystem", daemon: "default", ok: false, tool_count: 0, err: "initialize: HTTP 500", source: "" };
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
    expect(errPill?.textContent).toContain("✗ probe err");
    expect(card.textContent).toContain("initialize: HTTP 500");
  });

  it("renders 'not probed' unknown state when no probe row exists for the server (codex bot PR #144 round 5 P2)", async () => {
    // Codex bot finding: cache drift / daemon churn can produce a
    // capabilities row WITHOUT a matching probe row. Defaulting
    // probeOk to true would mis-render green ✓ probed when nothing
    // was probed. UI must show explicit unknown/not-probed state.
    const row: CapabilityRow = {
      server: "memory",
      daemon: "default",
      tools:     { state: "ok", items: [{ name: "a", id: "memory/default/tool/a", namespace: "memory", kind: "tool" }] },
      prompts:   { state: "empty", items: [] },
      resources: { state: "empty", items: [] },
    };
    // Capabilities row exists; NO matching probe row in probes.items.
    const snap: HealthSnapshot = {
      ...emptySnapshot,
      probes:       { ...emptySnapshot.probes!, items: [] },
      capabilities: { items: [row], generated_at: 1715164800, ttl_ms: 60000, errors: [] },
    };
    vi.spyOn(api, "fetchOrThrow").mockResolvedValue(snap);

    const { findByTestId } = render(<CapabilitiesScreen />);
    const card = await findByTestId("capability-card-memory-default");
    const unknownPill = card.querySelector(".capability-card-probe-status.unknown");
    expect(unknownPill).not.toBeNull();
    expect(unknownPill?.textContent).toContain("? not probed");
    // Critical negative: must NOT render the green "probed" pill when
    // probe is null.
    expect(card.querySelector(".capability-card-probe-status.ok")).toBeNull();
    expect(card.querySelector(".capability-card-probe-status.err")).toBeNull();
  });
});

describe("CapabilitiesScreen — Phase 5 collapsible + StateBadge", () => {
  beforeEach(() => vi.restoreAllMocks());

  function renderWithRow(row: CapabilityRow) {
    const probe: ProbeRow = { server: row.server, daemon: row.daemon, ok: true, tool_count: 0, err: "", source: "" };
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

  it("section header exposes aria-expanded for assistive tech (codex bot PR #144 round 6 P3)", async () => {
    // Codex bot a11y finding: role="button" + custom toggle handler
    // must publish aria-expanded so screen readers announce
    // collapsed/expanded state after activation.
    const row: CapabilityRow = {
      server: "s", daemon: "d",
      tools:     { state: "ok", items: [{ name: "x", id: "s/d/tool/x", namespace: "s", kind: "tool" }] },
      prompts:   { state: "empty", items: [] },
      resources: { state: "empty", items: [] },
    };
    const { findByTestId } = renderWithRow(row);
    const section = await findByTestId("capability-section-tools");
    const header = section.querySelector(".capability-section-header") as HTMLElement;

    expect(header.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(header);
    expect(header.getAttribute("aria-expanded")).toBe("true");
    fireEvent.click(header);
    expect(header.getAttribute("aria-expanded")).toBe("false");
  });
});

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
    const probe: ProbeRow = { server: "memory", daemon: "default", ok: true, tool_count: 2, err: "", source: "" };
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

    // Critical: NO actionable Run-tool affordances on items (AC #7).
    // Codex stage-1 review finding #3: broaden the assertion beyond
    // <button>. Reject buttons, anchors-with-href, role="button"
    // descendants, and onclick handlers on every .capability-item node.
    // Section toggles, Refresh, and legend controls live OUTSIDE
    // .capability-item-list so they are not affected by this scoped
    // assertion.
    const itemNodes = list!.querySelectorAll(".capability-item");
    for (const node of Array.from(itemNodes)) {
      expect(node.querySelectorAll("button").length).toBe(0);
      expect(node.querySelectorAll("a[href]").length).toBe(0);
      expect(node.querySelectorAll('[role="button"]').length).toBe(0);
      // The <li> itself must not be actionable either.
      expect((node as HTMLElement).getAttribute("role")).not.toBe("button");
      expect((node as HTMLElement).onclick).toBeNull();
    }
  });

  it("items: null normalizes to empty list without crashing (AC #19)", async () => {
    const row: CapabilityRow = {
      server: "fs", daemon: "default",
      tools:     { state: "unsupported", items: null },
      prompts:   { state: "error", items: null, err: "tools/list: parse: unexpected EOF" },
      resources: { state: "empty", items: [] },
    };
    const probe: ProbeRow = { server: "fs", daemon: "default", ok: true, tool_count: 0, err: "", source: "" };
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

describe("CapabilitiesScreen — Phase 7 synthetic pill + legend", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("renders synthetic-source pill when probe.source === 'proxy-synthetic' (AC #8)", async () => {
    const row: CapabilityRow = {
      server: "lazy", daemon: "default",
      tools:     { state: "unsupported", items: null },
      prompts:   { state: "unsupported", items: null },
      resources: { state: "unsupported", items: null },
    };
    const probe: ProbeRow = { server: "lazy", daemon: "default", ok: true, tool_count: 0, err: "", source: "proxy-synthetic" };
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
    const probe: ProbeRow = { server: "real", daemon: "default", ok: true, tool_count: 0, err: "", source: "" };
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
    const probe: ProbeRow = { server: "old", daemon: "default", ok: true, tool_count: 0, err: "", source: "" };
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
