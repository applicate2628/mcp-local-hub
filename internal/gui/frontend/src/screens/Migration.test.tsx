// internal/gui/frontend/src/screens/Migration.test.tsx
//
// Discovery (DiscoveryScreen, route #/migration) auto-refresh + Rescan.
// The screen is scan-driven and has NO edit/dirty state, so auto-refresh
// is never paused: useAutoScan(loadScan, false) polls /api/scan every
// SCAN_POLL_MS while mounted and visible, and the "Rescan now" button
// fires an immediate refetch.
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, cleanup, fireEvent, screen, act } from "@testing-library/preact";
import { DiscoveryScreen } from "./Migration";
import type { ScanResult } from "../types";

// happy-dom doesn't ship EventSource. DiscoveryScreen subscribes to
// /api/events for daemon-state / clients-rescan; minimal stub.
class StubEventSource {
  url: string;
  constructor(url: string) {
    this.url = url;
  }
  addEventListener(): void {}
  removeEventListener(): void {}
  close(): void {}
}
(globalThis as unknown as { EventSource: typeof StubEventSource }).EventSource =
  StubEventSource;

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function fetchRouter(routes: Record<string, (init?: RequestInit) => Response | Promise<Response>>) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString();
    for (const prefix of Object.keys(routes)) {
      if (url.startsWith(prefix)) return routes[prefix](init);
    }
    throw new Error(`unexpected fetch: ${url}`);
  });
}

function scan(name = "memory"): ScanResult {
  return {
    at: "2026-06-15T00:00:00Z",
    entries: [
      { name, status: "can-migrate", manifest_exists: true, can_migrate: true, client_presence: {} },
    ],
    client_config_presence: {},
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

// SCAN_POLL_MS is 10_000 (see useAutoScan.ts).
const POLL_MS = 10_000;

describe("DiscoveryScreen — auto-refresh + Rescan", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useFakeTimers();
    Object.defineProperty(document, "hidden", { configurable: true, get: () => false });
    window.location.hash = "#/migration";
  });
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  it("renders the Rescan control (never paused — no edit state)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scan()),
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
      }) as unknown as typeof fetch,
    );
    render(<DiscoveryScreen />);
    await vi.waitFor(() => {
      expect(screen.queryByTestId("scan-rescan-btn")).toBeTruthy();
    });
    const btn = screen.getByTestId("scan-rescan-btn") as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
    // Discovery has no dirty state → no paused note ever.
    expect(screen.queryByTestId("scan-paused-note")).toBeNull();
  });

  it("re-fetches /api/scan after SCAN_POLL_MS while mounted", async () => {
    let scanCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => {
          scanCalls += 1;
          return jsonResponse(200, scan());
        },
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
      }) as unknown as typeof fetch,
    );
    render(<DiscoveryScreen />);
    await vi.waitFor(() => expect(scanCalls).toBeGreaterThanOrEqual(1));
    const initial = scanCalls;

    await vi.advanceTimersByTimeAsync(POLL_MS);
    await vi.waitFor(() => expect(scanCalls).toBe(initial + 1));
  });

  it("Rescan now triggers an immediate /api/scan refetch", async () => {
    let scanCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => {
          scanCalls += 1;
          return jsonResponse(200, scan());
        },
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
      }) as unknown as typeof fetch,
    );
    render(<DiscoveryScreen />);
    await vi.waitFor(() => expect(screen.queryByTestId("scan-rescan-btn")).toBeTruthy());
    const before = scanCalls;
    fireEvent.click(screen.getByTestId("scan-rescan-btn"));
    await vi.waitFor(() => expect(scanCalls).toBe(before + 1));
  });


  it("ignores an older overlapping refresh that resolves after a newer one", async () => {
    const older = deferred<Response>();
    const newer = deferred<Response>();
    let scanCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => {
          scanCalls += 1;
          return scanCalls === 1 ? older.promise : newer.promise;
        },
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
      }) as unknown as typeof fetch,
    );
    render(<DiscoveryScreen />);
    await vi.waitFor(() => expect(scanCalls).toBe(1));

    await vi.advanceTimersByTimeAsync(POLL_MS);
    await vi.waitFor(() => expect(scanCalls).toBe(2));

    await act(async () => {
      newer.resolve(jsonResponse(200, scan("fresh-memory")));
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });
    await vi.waitFor(() => expect(screen.queryByText("fresh-memory")).toBeTruthy());

    await act(async () => {
      older.resolve(jsonResponse(200, scan("stale-memory")));
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });
    await vi.waitFor(() => expect(screen.queryByText("stale-memory")).toBeNull());
    expect(screen.queryByText("fresh-memory")).toBeTruthy();
  });

  it("skips the poll tick while the tab is hidden", async () => {
    let scanCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => {
          scanCalls += 1;
          return jsonResponse(200, scan());
        },
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
      }) as unknown as typeof fetch,
    );
    render(<DiscoveryScreen />);
    await vi.waitFor(() => expect(scanCalls).toBeGreaterThanOrEqual(1));
    const initial = scanCalls;

    // Hide the tab and dispatch the event the hook listens for.
    Object.defineProperty(document, "hidden", { configurable: true, get: () => true });
    document.dispatchEvent(new Event("visibilitychange"));

    await vi.advanceTimersByTimeAsync(POLL_MS * 3);
    expect(scanCalls).toBe(initial); // hidden → no poll fetches
  });
});
