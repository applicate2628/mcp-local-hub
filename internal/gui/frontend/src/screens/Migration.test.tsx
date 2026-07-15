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
import { ToastContainer } from "../components/Toast";
import { clearAllToasts } from "../lib/toast-store";
import type { DeAdoptPlan, DeAdoptRouting, ScanResult } from "../types";

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
    if (url.startsWith("/api/deadopt/eligible")) {
      return jsonResponse(200, {
        eligible: false,
        adopt_owned: false,
        gate_on: false,
        gate_on_clients: [],
        blocked_reason: "",
      });
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

function managedDiscoveryScan(name = "adopted-server"): ScanResult {
  return {
    at: "2026-07-15T00:00:00Z",
    entries: [
      {
        name,
        status: "via-hub",
        managed: true,
        manifest_exists: true,
        can_migrate: false,
        client_presence: {},
      },
    ],
    client_config_presence: {},
  };
}

function recoverableDeAdoptingScan(): ScanResult {
  return {
    at: "2026-07-15T00:00:00Z",
    entries: [
      {
        name: "resume-server",
        status: "can-migrate",
        managed: false,
        manifest_exists: true,
        can_migrate: true,
        client_presence: {},
      },
    ],
    client_config_presence: {},
  };
}

function basicDeAdoptPlan(
  routing: DeAdoptRouting = "FRESH",
  refusalReason = "",
): DeAdoptPlan {
  return {
    ManifestName: "adopted-server",
    SourceEntryName: "legacy-server",
    AdoptClients: [],
    Routing: routing,
    RefusalReason: refusalReason,
    Manifest: {
      Present: true,
      AlreadyAbsent: false,
      HashReady: true,
      ExpectedHash: "expected",
      ActualHash: "actual",
      Reason: "",
    },
    Eligibility: {
      AdoptOwned: true,
      GateOn: false,
      Eligible: routing !== "REFUSE",
      GateOnClients: [],
      BlockedReason: refusalReason,
    },
    Clients: [],
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
    clearAllToasts();
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
    clearAllToasts();
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

  it("hides de-adopt when backend eligibility says the server is not adopt-owned", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, managedDiscoveryScan()),
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
        "/api/deadopt/eligible": () =>
          jsonResponse(200, {
            eligible: false,
            adopt_owned: false,
            gate_on: false,
            gate_on_clients: [],
            blocked_reason: 'manifest "adopted-server" is not adopt-owned',
          }),
      }) as unknown as typeof fetch,
    );

    render(<DiscoveryScreen />);
    const row = (await screen.findByText("adopted-server")).closest("li");
    expect(row).not.toBeNull();
    expect(
      within(row as HTMLElement).queryByRole("button", { name: "De-adopt to native" }),
    ).toBeNull();
  });

  it("keeps de-adopt fail-closed and shows a visible warning when eligibility cannot be read", async () => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, managedDiscoveryScan()),
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
        "/api/deadopt/eligible": () =>
          jsonResponse(503, { error: "eligibility service unavailable" }),
      }) as unknown as typeof fetch,
    );

    render(<DiscoveryScreen />);
    const row = (await screen.findByText("adopted-server")).closest("li");
    expect(row).not.toBeNull();
    expect(
      within(row as HTMLElement).queryByRole("button", { name: "De-adopt to native" }),
    ).toBeNull();
    expect(
      await within(row as HTMLElement).findByText("Couldn't check de-adopt eligibility."),
    ).toBeTruthy();
  });

  it("fetches eligibility and renders de-adopt for a recoverable non-via-hub row", async () => {
    const fetchMock = fetchRouter({
      "/api/scan": () => jsonResponse(200, recoverableDeAdoptingScan()),
      "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
      "/api/deadopt/eligible": () =>
        jsonResponse(200, {
          eligible: true,
          adopt_owned: true,
          gate_on: false,
          gate_on_clients: [],
          blocked_reason: "",
        }),
    });
    vi.spyOn(globalThis, "fetch").mockImplementation(fetchMock as unknown as typeof fetch);

    render(<DiscoveryScreen />);
    await screen.findByText("De-adopt recovery");
    const row = screen.getByText("resume-server").closest("li");
    expect(row).not.toBeNull();
    expect(
      await within(row as HTMLElement).findByRole("button", { name: "De-adopt to native" }),
    ).toBeTruthy();

    expect(
      fetchMock.mock.calls.some(([input]) =>
        String(input).includes("/api/deadopt/eligible?server=resume-server"),
      ),
    ).toBe(true);
  });

  it("surfaces a manifest-only adopt-owned row in de-adopt recovery", async () => {
    const manifestOnlyScan: ScanResult = {
      at: "2026-07-15T00:00:00Z",
      entries: [
        {
          name: "manifest-only",
          status: "not-installed",
          managed: false,
          manifest_exists: true,
          can_migrate: false,
          client_presence: {},
        },
      ],
      client_config_presence: {},
    };
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, manifestOnlyScan),
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
        "/api/deadopt/eligible": () =>
          jsonResponse(200, {
            eligible: true,
            adopt_owned: true,
            gate_on: false,
            gate_on_clients: [],
            blocked_reason: "",
          }),
      }) as unknown as typeof fetch,
    );

    render(<DiscoveryScreen />);
    expect(await screen.findByText("De-adopt recovery")).toBeTruthy();
    const row = screen.getByText("manifest-only").closest("li");
    expect(row).not.toBeNull();
    expect(
      within(row as HTMLElement).getByRole("button", { name: "De-adopt to native" }),
    ).toBeTruthy();
  });

  it("excludes adopt-owned can-migrate rows from selection and the migrate request", async () => {
    const migrateRequests: unknown[] = [];
    const mixedScan: ScanResult = {
      at: "2026-07-15T00:00:00Z",
      entries: [
        {
          name: "resume-server",
          status: "can-migrate",
          manifest_exists: true,
          can_migrate: true,
          client_presence: {},
        },
        {
          name: "normal-server",
          status: "can-migrate",
          manifest_exists: true,
          can_migrate: true,
          client_presence: {},
        },
      ],
      client_config_presence: {},
    };
    vi.spyOn(globalThis, "fetch").mockImplementation(
      (async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        if (url === "/api/scan") return jsonResponse(200, mixedScan);
        if (url === "/api/dismissed") return jsonResponse(200, { unknown: [] });
        if (url.startsWith("/api/deadopt/eligible")) {
          const adoptOwned = url.includes("server=resume-server");
          return jsonResponse(200, {
            eligible: adoptOwned,
            adopt_owned: adoptOwned,
            gate_on: false,
            gate_on_clients: [],
            blocked_reason: "",
          });
        }
        if (url === "/api/migrate") {
          migrateRequests.push(JSON.parse(String(init?.body ?? "{}")));
          return new Response(null, { status: 204 });
        }
        throw new Error(`unexpected fetch: ${url}`);
      }) as typeof fetch,
    );

    render(<DiscoveryScreen />);
    expect(await screen.findByText("De-adopt recovery")).toBeTruthy();
    const recoveryRow = screen.getByText("resume-server").closest("li");
    expect(recoveryRow).not.toBeNull();
    expect(within(recoveryRow as HTMLElement).queryByRole("checkbox")).toBeNull();

    const migrate = screen.getByRole("button", { name: "Migrate selected (1)" });
    fireEvent.click(migrate);
    await vi.waitFor(() => expect(migrateRequests).toHaveLength(1));
    expect(migrateRequests).toEqual([{ servers: ["normal-server"] }]);
  });

  it("renders scan rows without waiting for a hanging eligibility read", async () => {
    let eligibilitySignal: AbortSignal | undefined;
    let eligibilityAborted = false;
    vi.spyOn(console, "warn").mockImplementation(() => {});
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scan("slow-eligibility")),
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
        "/api/deadopt/eligible": (init) => {
          eligibilitySignal = init?.signal ?? undefined;
          return new Promise<Response>((_resolve, reject) => {
            eligibilitySignal?.addEventListener("abort", () => {
              eligibilityAborted = true;
              reject(new DOMException("Aborted", "AbortError"));
            });
          });
        },
      }) as unknown as typeof fetch,
    );

    render(<DiscoveryScreen />);
    const row = (await screen.findByText("slow-eligibility")).closest("li");
    expect(row).not.toBeNull();
    expect(
      within(row as HTMLElement).queryByText("Couldn't check de-adopt eligibility."),
    ).toBeNull();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_000);
    });
    expect(
      within(row as HTMLElement).getByText("Couldn't check de-adopt eligibility."),
    ).toBeTruthy();
    expect(eligibilitySignal).toBeDefined();
    expect(eligibilitySignal?.aborted).toBe(true);
    expect(eligibilityAborted).toBe(true);
  });

  it("does not probe invalid manifest names or show an eligibility warning", async () => {
    let eligibilityCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scan("Invalid Name")),
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
        "/api/deadopt/eligible": () => {
          eligibilityCalls += 1;
          return jsonResponse(400, { error: "invalid manifest name" });
        },
      }) as unknown as typeof fetch,
    );

    render(<DiscoveryScreen />);
    const row = (await screen.findByText("Invalid Name")).closest("li");
    expect(row).not.toBeNull();
    expect(eligibilityCalls).toBe(0);
    expect(
      within(row as HTMLElement).queryByText("Couldn't check de-adopt eligibility."),
    ).toBeNull();
  });

  it("shows a disabled de-adopt affordance and backend gate-OFF reason while gate is ON", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, managedDiscoveryScan()),
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
        "/api/deadopt/eligible": () =>
          jsonResponse(200, {
            eligible: false,
            adopt_owned: true,
            gate_on: true,
            gate_on_clients: ["codex-cli"],
            blocked_reason: "gate is ON for 1 client(s) (codex-cli); gate OFF first, then de-adopt",
          }),
      }) as unknown as typeof fetch,
    );

    render(<DiscoveryScreen />);
    const row = (await screen.findByText("adopted-server")).closest("li");
    expect(row).not.toBeNull();
    const button = await within(row as HTMLElement).findByRole("button", {
      name: "De-adopt to native",
    }) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    expect(within(row as HTMLElement).getByText(/gate OFF first/)).toBeTruthy();
  });

  it("shows a REFUSE reason, disables confirmation, and never executes de-adopt", async () => {
    let executeCalls = 0;
    const refusalReason = "de-adopt refused because the hub gate is ON";
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/deadopt/eligible": () =>
          jsonResponse(200, {
            eligible: true,
            adopt_owned: true,
            gate_on: false,
            gate_on_clients: [],
            blocked_reason: "",
          }),
        "/api/deadopt/plan": () =>
          jsonResponse(200, basicDeAdoptPlan("REFUSE", refusalReason)),
        "/api/deadopt": () => {
          executeCalls += 1;
          return jsonResponse(200, { restored: [], accepted: [], failed: [] });
        },
        "/api/scan": () => jsonResponse(200, managedDiscoveryScan()),
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
      }) as unknown as typeof fetch,
    );

    render(<DiscoveryScreen />);
    fireEvent.click(await screen.findByRole("button", { name: "De-adopt to native" }));

    const modal = await screen.findByTestId("deadopt-confirm-modal");
    await vi.waitFor(() => expect((modal as HTMLDialogElement).open).toBe(true));
    expect(within(modal).getByText(refusalReason)).toBeTruthy();
    const confirm = within(modal).getByRole("button", {
      name: "De-adopt to native",
    }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    fireEvent.click(confirm);
    expect(executeCalls).toBe(0);
  });

  it("shows irreversible snapshot-destruction consent for accept-conflict", async () => {
    const plan = basicDeAdoptPlan();
    plan.Clients = [
      {
        Client: "claude-code",
        OriginalState: "present",
        Disposition: "failed",
        AcceptEligible: true,
        Reason: "current native entry conflicts with the pinned pre-adopt snapshot",
      },
    ];
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, managedDiscoveryScan()),
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
        "/api/deadopt/eligible": () =>
          jsonResponse(200, {
            eligible: true,
            adopt_owned: true,
            gate_on: false,
            gate_on_clients: [],
            blocked_reason: "",
          }),
        "/api/deadopt/plan": () => jsonResponse(200, plan),
      }) as unknown as typeof fetch,
    );

    render(<DiscoveryScreen />);
    fireEvent.click(await screen.findByRole("button", { name: "De-adopt to native" }));

    const modal = await screen.findByTestId("deadopt-confirm-modal");
    expect(
      within(modal).getByLabelText(/I understand this is irreversible/),
    ).toBeTruthy();
    expect(
      within(modal).getByText(
        "IRREVERSIBLE: the accepted client's pinned snapshot is DESTROYED at close and its pre-adopt original config + secret-literal spellings are discarded without ever being restored.",
      ),
    ).toBeTruthy();
  });

  it("plans and executes eligible de-adopt and renders partial failure reasons", async () => {
    const planRequests: unknown[] = [];
    const executeRequests: unknown[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/deadopt/eligible": () =>
          jsonResponse(200, {
            eligible: true,
            adopt_owned: true,
            gate_on: false,
            gate_on_clients: [],
            blocked_reason: "",
          }),
        "/api/deadopt/plan": (init) => {
          planRequests.push(JSON.parse(String(init?.body ?? "{}")));
          return jsonResponse(200, {
            ManifestName: "adopted-server",
            SourceEntryName: "legacy-server",
            AdoptClients: ["codex-cli", "claude-code"],
            Routing: "FRESH",
            RefusalReason: "",
            Manifest: {
              Present: true,
              AlreadyAbsent: false,
              HashReady: true,
              ExpectedHash: "expected",
              ActualHash: "actual",
              Reason: "",
            },
            Eligibility: {
              AdoptOwned: true,
              GateOn: false,
              Eligible: true,
              GateOnClients: [],
              BlockedReason: "",
            },
            Clients: [
              {
                Client: "codex-cli",
                OriginalState: "present",
                Disposition: "restore-pending",
                AcceptEligible: false,
                Reason: "",
              },
              {
                Client: "claude-code",
                OriginalState: "conflict",
                Disposition: "remove-pending",
                AcceptEligible: true,
                Reason: "native conflict",
              },
            ],
          });
        },
        "/api/deadopt": (init) => {
          executeRequests.push(JSON.parse(String(init?.body ?? "{}")));
          return jsonResponse(200, {
            restored: ["codex-cli"],
            accepted: ["claude-code"],
            failed: [
              { client: "cursor", reason: "write failed" },
              { client: "vscode", reason: "permission denied" },
            ],
          });
        },
        "/api/scan": () => jsonResponse(200, managedDiscoveryScan()),
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
      }) as unknown as typeof fetch,
    );

    render(
      <>
        <DiscoveryScreen />
        <ToastContainer />
      </>,
    );
    const button = await screen.findByRole("button", { name: "De-adopt to native" });
    expect((button as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(button);

    const modal = await screen.findByTestId("deadopt-confirm-modal");
    await vi.waitFor(() => expect((modal as HTMLDialogElement).open).toBe(true));
    expect(planRequests).toEqual([{ server: "adopted-server" }]);
    expect(modal.textContent).toContain("codex-cli: restore-pending");
    fireEvent.click(within(modal).getByLabelText(/accept the current native conflict/i));
    fireEvent.click(within(modal).getByRole("button", { name: "De-adopt to native" }));

    await vi.waitFor(() => expect(executeRequests).toHaveLength(1));
    expect(executeRequests).toEqual([
      {
        server: "adopted-server",
        accept_conflict_clients: ["claude-code"],
      },
    ]);
    await vi.waitFor(() => expect(within(modal).getByText("De-adopt report")).toBeTruthy());
    expect(within(modal).getByText("Restored")).toBeTruthy();
    expect(within(modal).getByText("codex-cli")).toBeTruthy();
    expect(within(modal).getByText("Accepted")).toBeTruthy();
    expect(within(modal).getByText("claude-code")).toBeTruthy();
    expect(within(modal).getByText("Failed")).toBeTruthy();
    expect(within(modal).getByText("cursor — write failed")).toBeTruthy();
    expect(within(modal).getByText("vscode — permission denied")).toBeTruthy();
    const toast = await screen.findByTestId("toast");
    expect(toast.getAttribute("data-toast-variant")).toBe("danger");
    expect(within(toast).getByTestId("toast-message").textContent).toBe(
      "De-adopt incomplete: adopted-server — 2 client(s) failed.",
    );
    expect(within(toast).getByTestId("toast-message").textContent).not.toContain("De-adopted");
  });

  it("surfaces a failed de-adopt execute and refreshes Discovery", async () => {
    let scanCalls = 0;
    let executeCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/deadopt/eligible": () =>
          jsonResponse(200, {
            eligible: true,
            adopt_owned: true,
            gate_on: false,
            gate_on_clients: [],
            blocked_reason: "",
          }),
        "/api/deadopt/plan": () => jsonResponse(200, basicDeAdoptPlan()),
        "/api/deadopt": () => {
          executeCalls += 1;
          return jsonResponse(500, { error: "executor failed after a partial restore" });
        },
        "/api/scan": () => {
          scanCalls += 1;
          return jsonResponse(200, managedDiscoveryScan());
        },
        "/api/dismissed": () => jsonResponse(200, { unknown: [] }),
      }) as unknown as typeof fetch,
    );

    render(<DiscoveryScreen />);
    fireEvent.click(await screen.findByRole("button", { name: "De-adopt to native" }));
    const modal = await screen.findByTestId("deadopt-confirm-modal");
    await vi.waitFor(() => expect((modal as HTMLDialogElement).open).toBe(true));
    const scansBeforeExecute = scanCalls;
    fireEvent.click(within(modal).getByRole("button", { name: "De-adopt to native" }));

    await vi.waitFor(() => expect(executeCalls).toBe(1));
    await vi.waitFor(() =>
      expect(within(modal).getByText(/executor failed after a partial restore/)).toBeTruthy(),
    );
    await vi.waitFor(() => expect(scanCalls).toBeGreaterThan(scansBeforeExecute));
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
