import { render, cleanup } from "@testing-library/preact";
import { describe, it, expect, afterEach, vi, beforeEach } from "vitest";
import { CapabilitiesScreen } from "./Capabilities";
import * as api from "../api";
import type { HealthSnapshot } from "../types";

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
