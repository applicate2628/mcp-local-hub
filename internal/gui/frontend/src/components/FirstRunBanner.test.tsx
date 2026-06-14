import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, waitFor, cleanup, fireEvent } from "@testing-library/preact";
import { FirstRunBanner } from "./FirstRunBanner";
import type { DaemonStatus } from "../types";

// happy-dom does not ship EventSource. FirstRunBanner subscribes to
// /api/events for the live daemon-state delta, so a bare render hits
// `new EventSource(...)`. Stub matches the Dashboard test's pattern.
class StubEventSource {
  url: string;
  constructor(url: string) {
    this.url = url;
  }
  addEventListener(_t: string, _l: (ev: MessageEvent) => void) {}
  removeEventListener(_t: string, _l: (ev: MessageEvent) => void) {}
  close() {}
}
(globalThis as unknown as { EventSource: typeof StubEventSource }).EventSource =
  StubEventSource;

const runningRow: DaemonStatus = {
  server: "memory",
  daemon: "default",
  port: 9123,
  pid: 12345,
  state: "Running",
};

const maintenanceRow: DaemonStatus = {
  server: "mcp-local-hub-weekly-refresh",
  daemon: "default",
  state: "Running",
  is_maintenance: true,
};

function statusResponse(rows: DaemonStatus[]): Response {
  return new Response(JSON.stringify(rows), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

describe("FirstRunBanner — §14 first-run onboarding", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });
  afterEach(() => {
    cleanup();
  });

  it("renders on empty-servers state with a CTA to the Add-server flow", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([]));
    const { findByTestId } = render(<FirstRunBanner />);

    const banner = await findByTestId("first-run-banner");
    expect(banner).toBeTruthy();
    const cta = await findByTestId("first-run-banner-cta");
    expect(cta.getAttribute("href")).toBe("#/add-server");
  });

  it("treats a status list of only maintenance rows as empty (banner shows)", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      statusResponse([maintenanceRow]),
    );
    const { findByTestId } = render(<FirstRunBanner />);
    expect(await findByTestId("first-run-banner")).toBeTruthy();
  });

  it("is hidden when a server is present", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      statusResponse([runningRow]),
    );
    const { queryByTestId } = render(<FirstRunBanner />);

    // Give the status fetch a chance to resolve, then assert the banner
    // never rendered.
    await waitFor(() => {
      expect(globalThis.fetch).toHaveBeenCalled();
    });
    await Promise.resolve();
    expect(queryByTestId("first-run-banner")).toBeNull();
  });

  it("is hidden when /api/status errors (onboarding sugar must not block the shell)", async () => {
    vi.spyOn(globalThis, "fetch").mockRejectedValue(new Error("network down"));
    const { queryByTestId } = render(<FirstRunBanner />);
    await waitFor(() => {
      expect(globalThis.fetch).toHaveBeenCalled();
    });
    await Promise.resolve();
    expect(queryByTestId("first-run-banner")).toBeNull();
  });

  it("dismiss hides the banner", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([]));
    const { findByTestId, queryByTestId } = render(<FirstRunBanner />);

    const dismiss = await findByTestId("first-run-banner-dismiss");
    fireEvent.click(dismiss);

    await waitFor(() => {
      expect(queryByTestId("first-run-banner")).toBeNull();
    });
  });
});
