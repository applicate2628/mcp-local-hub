import { render, cleanup, fireEvent } from "@testing-library/preact";
import { describe, it, expect, afterEach, vi, beforeEach } from "vitest";
import { CapabilitiesScreen } from "./Capabilities";
import * as api from "../api";
import type { HealthSnapshot, CapabilityRow, ProbeRow } from "../types";

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
    expect(card.textContent).toContain("initialize: HTTP 500");
  });
});
