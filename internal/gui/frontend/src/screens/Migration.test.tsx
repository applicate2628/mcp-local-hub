// internal/gui/frontend/src/screens/Migration.test.tsx
//
// Discovery (DiscoveryScreen, route #/migration) auto-refresh + Rescan.
// The screen is scan-driven and has NO edit/dirty state, so auto-refresh
// is never paused: useAutoScan(loadScan, false) polls /api/scan every
// SCAN_POLL_MS while mounted and visible, and the "Rescan now" button
// fires an immediate refetch.
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, cleanup, fireEvent, screen, act, within } from "@testing-library/preact";
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

function unmanagedDiscoveryScan(): ScanResult {
  return {
    at: "2026-06-15T00:00:00Z",
    entries: [
      {
        name: "local-stdio",
        status: "unknown",
        manifest_exists: false,
        can_migrate: false,
        client_presence: {
          "codex-cli": { transport: "stdio" },
        },
      },
      {
        name: "context7",
        status: "external",
        manifest_exists: false,
        can_migrate: false,
        client_presence: {
          "codex-cli": { transport: "http", endpoint: "https://mcp.context7.com/mcp" },
        },
      },
    ],
    client_config_presence: {},
    client_capabilities: {
      "codex-cli": {
        scannable: true,
        direct_installable: true,
        remote_http_capable: true,
        adopt_supported: true,
      },
    },
  };
}

function unsupportedAdoptDiscoveryScan(): ScanResult {
  return {
    at: "2026-06-15T00:00:00Z",
    entries: [
      {
        name: "zed-local-stdio",
        status: "unknown",
        manifest_exists: false,
        can_migrate: false,
        client_presence: {
          zed: { transport: "stdio" },
        },
      },
      {
        name: "codex-local-stdio",
        status: "unknown",
        manifest_exists: false,
        can_migrate: false,
        client_presence: {
          "codex-cli": { transport: "stdio" },
        },
      },
    ],
    client_config_presence: {},
    client_capabilities: {
      zed: {
        scannable: true,
        direct_installable: false,
        remote_http_capable: false,
        adopt_supported: false,
      },
      "codex-cli": {
        scannable: true,
        direct_installable: true,
        remote_http_capable: true,
        adopt_supported: true,
      },
    },
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
    HTMLDialogElement.prototype.showModal = function () {
      this.open = true;
      this.setAttribute("open", "");
    };
    HTMLDialogElement.prototype.close = function () {
      this.open = false;
      this.removeAttribute("open");
    };
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

  it("shows Adopt into hub only on unknown stdio rows", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, unmanagedDiscoveryScan()),
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
      }) as unknown as typeof fetch,
    );
    render(<DiscoveryScreen />);
    const unknownRow = (await screen.findByText("local-stdio")).closest("li");
    const externalRow = screen.getByText("context7").closest("li");
    expect(unknownRow).not.toBeNull();
    expect(externalRow).not.toBeNull();
    expect(
      within(unknownRow as HTMLElement).getByRole("button", { name: "Adopt into hub" }),
    ).toBeTruthy();
    expect(
      within(externalRow as HTMLElement).queryByRole("button", { name: "Adopt into hub" }),
    ).toBeNull();
  });

  it("omits Adopt into hub for unknown stdio rows from unsupported adopt clients", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, unsupportedAdoptDiscoveryScan()),
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
      }) as unknown as typeof fetch,
    );
    render(<DiscoveryScreen />);
    const unsupportedRow = (await screen.findByText("zed-local-stdio")).closest("li");
    const supportedRow = screen.getByText("codex-local-stdio").closest("li");
    expect(unsupportedRow).not.toBeNull();
    expect(supportedRow).not.toBeNull();
    expect(
      within(unsupportedRow as HTMLElement).queryByRole("button", { name: "Adopt into hub" }),
    ).toBeNull();
    expect(
      within(supportedRow as HTMLElement).getByRole("button", { name: "Adopt into hub" }),
    ).toBeTruthy();
  });

  it("plans, requires symlink consent, confirms adopt, and refreshes scan", async () => {
    const planRequests: unknown[] = [];
    const adoptRequests: unknown[] = [];
    let scanCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/adopt/plan": (init) => {
          planRequests.push(JSON.parse(String(init?.body ?? "{}")));
          return jsonResponse(200, {
            EntryName: "local-stdio",
            SourceClient: "codex-cli",
            ManifestName: "local-stdio",
            Port: 9325,
            AdoptClients: ["codex-cli", "claude-code"],
            AlsoPresent: ["cursor"],
            SignatureMismatches: [{ Client: "vscode", Reason: "args differ" }],
            DisabledSameName: [{ Client: "gemini-cli" }],
            SecretRoutedKeys: ["LOCAL_STDIO_API_KEY"],
            ManifestYAML: "name: local-stdio\n",
            symlink_targets: [
              { client: "codex-cli", resolved_path: "C:\\Users\\d\\.codex\\config.toml" },
            ],
          });
        },
        "/api/adopt": (init) => {
          adoptRequests.push(JSON.parse(String(init?.body ?? "{}")));
          return jsonResponse(201, { name: "local-stdio", port: 9325 });
        },
        "/api/scan": () => {
          scanCalls += 1;
          return jsonResponse(200, unmanagedDiscoveryScan());
        },
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
      }) as unknown as typeof fetch,
    );
    render(<DiscoveryScreen />);
    fireEvent.click(await screen.findByRole("button", { name: "Adopt into hub" }));

    const modal = await screen.findByTestId("adopt-confirm-modal");
    await vi.waitFor(() => expect((modal as HTMLDialogElement).open).toBe(true));
    expect(planRequests).toEqual([{ entry: "local-stdio", client: "codex-cli" }]);
    expect(within(modal).getByText("Manifest: local-stdio")).toBeTruthy();
    expect(within(modal).getByText("Port: 9325")).toBeTruthy();
    expect(within(modal).getByText("codex-cli")).toBeTruthy();
    expect(within(modal).getByText("claude-code")).toBeTruthy();
    expect(within(modal).getByText("cursor: also present, not selected")).toBeTruthy();
    expect(within(modal).getByText("vscode: args differ")).toBeTruthy();
    expect(within(modal).getByText("gemini-cli: disabled")).toBeTruthy();
    expect(within(modal).getByText("LOCAL_STDIO_API_KEY")).toBeTruthy();
    expect(within(modal).getByText(/codex-cli config is a symlink/)).toBeTruthy();

    const confirm = within(modal).getByRole("button", { name: "Adopt into hub" }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    fireEvent.click(within(modal).getByLabelText(/I understand/));
    expect(confirm.disabled).toBe(false);
    const beforeConfirmScanCalls = scanCalls;
    fireEvent.click(confirm);

    await vi.waitFor(() => expect(adoptRequests).toHaveLength(1));
    expect(adoptRequests[0]).toEqual({
      entry: "local-stdio",
      client: "codex-cli",
      clients: ["codex-cli", "claude-code"],
      name: "local-stdio",
      port: 9325,
      symlink_consent: [
        { client: "codex-cli", resolved_path: "C:\\Users\\d\\.codex\\config.toml" },
      ],
    });
    await vi.waitFor(() => expect(scanCalls).toBeGreaterThan(beforeConfirmScanCalls));
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
