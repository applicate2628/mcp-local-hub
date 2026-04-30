import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, waitFor, cleanup, fireEvent } from "@testing-library/preact";
import { DashboardScreen } from "./Dashboard";
import type { DaemonStatus } from "../types";

// happy-dom does not ship EventSource. Dashboard's bulk-action UI state
// is SSE-driven (PR #38: unified pipeline — backend publishes
// bulk-action events, frontend mirrors them). The stub captures
// listeners so tests can dispatch synthetic events that drive the same
// state transitions a real backend would.
type StubListener = (ev: MessageEvent) => void;
const stubInstances = new Set<StubEventSource>();
class StubEventSource {
  url: string;
  listeners = new Map<string, Set<StubListener>>();
  constructor(url: string) {
    this.url = url;
    stubInstances.add(this);
  }
  addEventListener(name: string, handler: StubListener): void {
    let bucket = this.listeners.get(name);
    if (!bucket) {
      bucket = new Set();
      this.listeners.set(name, bucket);
    }
    bucket.add(handler);
  }
  removeEventListener(name: string, handler: StubListener): void {
    this.listeners.get(name)?.delete(handler);
  }
  close(): void {
    stubInstances.delete(this);
  }
}
(globalThis as unknown as { EventSource: typeof StubEventSource }).EventSource = StubEventSource;

// dispatchSse fires a synthetic SSE event into every live
// StubEventSource. Used by bulk-action tests to drive UI state the
// way a real backend would.
function dispatchSse(eventName: string, data: unknown) {
  const ev = new MessageEvent(eventName, { data: JSON.stringify(data) });
  for (const inst of stubInstances) {
    inst.listeners.get(eventName)?.forEach((h) => h(ev));
  }
}

const runningRow: DaemonStatus = {
  server: "memory",
  daemon: "default",
  port: 9123,
  pid: 12345,
  state: "Running",
};

const stoppedRow: DaemonStatus = {
  server: "gdb",
  daemon: "default",
  port: 9129,
  state: "Stopped",
};

function statusResponse(rows: DaemonStatus[]): Response {
  return new Response(JSON.stringify(rows), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("DashboardScreen — Stop button", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  // Bulk action buttons (Run all + Stop all) live in the dashboard
  // header — index 0 and 1. Per-card Restart and Stop start at index 2.
  // Total button count = 2 (header) + 2 × cards.
  it("renders Stop button alongside Restart for a Running daemon", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([runningRow]));
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4); // 2 bulk + 2 per-card
    });
    const buttons = await findAllByRole("button");
    expect(buttons[2]?.textContent).toBe("Restart");
    expect(buttons[3]?.textContent).toBe("Stop");
  });

  it("disables Stop when daemon state is Stopped (nothing to stop)", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([stoppedRow]));
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
    const buttons = await findAllByRole("button");
    const stopBtn = buttons[3] as HTMLButtonElement;
    expect(stopBtn.textContent).toBe("Stop");
    expect(stopBtn.disabled).toBe(true);
  });

  it("posts to /api/servers/<name>/stop on click and flashes Stopped", async () => {
    const fetchSpy = vi
      .spyOn(globalThis, "fetch")
      .mockImplementation((input: Request | string | URL) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
        if (url === "/api/servers/memory/stop?daemon=default") {
          return Promise.resolve(jsonResponse(200, { stop_results: [] }));
        }
        return Promise.reject(new Error(`unexpected fetch: ${url}`));
      });
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
    const buttons = await findAllByRole("button");
    const stopBtn = buttons[3] as HTMLButtonElement;

    fireEvent.click(stopBtn);

    await waitFor(() => {
      expect(stopBtn.textContent).toBe("Stopped");
    });
    expect(fetchSpy).toHaveBeenCalledWith(
      "/api/servers/memory/stop?daemon=default",
      expect.objectContaining({ method: "POST" }),
    );
  });

  it("flashes Failed on /stop 500", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
      if (url === "/api/servers/memory/stop?daemon=default") {
        return Promise.resolve(
          jsonResponse(500, { stop_results: [], error: "scheduler unavailable", code: "STOP_FAILED" }),
        );
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });
    // Suppress expected console.error from the failing path.
    vi.spyOn(console, "error").mockImplementation(() => {});

    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
    const buttons = await findAllByRole("button");
    const stopBtn = buttons[3] as HTMLButtonElement;

    fireEvent.click(stopBtn);

    await waitFor(() => {
      expect(stopBtn.textContent).toBe("Failed");
    });
  });

  // Multi-daemon regression: serena ships claude (9121) + codex (9122).
  // Clicking Restart on the codex card MUST NOT restart claude. The bug
  // was that the request fired POST /api/servers/serena/restart with no
  // daemon filter — backend interpreted that as "all daemons" and
  // restarted both. Frontend must include ?daemon=<daemon-name> in the
  // URL so the backend can narrow the restart to the clicked card only.
  it("multi-daemon: Restart on codex card sends ?daemon=codex (not all)", async () => {
    const serenaClaude: DaemonStatus = {
      server: "serena",
      daemon: "claude",
      port: 9121,
      pid: 1001,
      state: "Running",
    };
    const serenaCodex: DaemonStatus = {
      server: "serena",
      daemon: "codex",
      port: 9122,
      pid: 1002,
      state: "Running",
    };
    const fetchSpy = vi
      .spyOn(globalThis, "fetch")
      .mockImplementation((input: Request | string | URL) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url === "/api/status") {
          return Promise.resolve(statusResponse([serenaClaude, serenaCodex]));
        }
        if (url === "/api/servers/serena/restart?daemon=codex") {
          return Promise.resolve(
            jsonResponse(200, { restart_results: [{ task_name: "mcp-local-hub-serena-codex", error: "" }] }),
          );
        }
        return Promise.reject(new Error(`unexpected fetch: ${url}`));
      });

    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(6); // 2 bulk header + 2 cards × (Restart + Stop)
    });
    const buttons = await findAllByRole("button");
    // Indexes: [0] Run all, [1] Stop all (header).
    // Cards sort by keyFor(): "serena/claude" < "serena/codex". Per-card
    // buttons render Restart-then-Stop in document order.
    // [2] claude Restart, [3] claude Stop, [4] codex Restart, [5] codex Stop.
    const codexRestartBtn = buttons[4] as HTMLButtonElement;

    fireEvent.click(codexRestartBtn);
    await waitFor(() => expect(codexRestartBtn.textContent).toBe("Restarted"));

    // The request MUST carry ?daemon=codex. A bare /restart would (per
    // backend api.Restart with empty daemonFilter) restart claude too —
    // the regression we're guarding against.
    const calls = fetchSpy.mock.calls.map((c) => (typeof c[0] === "string" ? c[0] : c[0]?.toString()));
    expect(calls).toContain("/api/servers/serena/restart?daemon=codex");
    expect(calls).not.toContain("/api/servers/serena/restart");
  });

  it("renders Run all and Stop all bulk buttons in dashboard header", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([runningRow]));
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      // 2 bulk (Run all + Stop all) + 2 per-card (Restart + Stop) = 4
      expect(buttons.length).toBe(4);
    });
    const buttons = await findAllByRole("button");
    expect(buttons[0]?.textContent).toBe("Run all");
    expect(buttons[1]?.textContent).toBe("Stop all");
  });

  // PR #38 unified pipeline: bulk-action UI state is driven by SSE
  // events, not local onClick. Click → POST /api/restart-all →
  // backend publishes "started" → frontend animates. The test
  // simulates the SSE round-trip with dispatchSse so the assertion
  // mirrors how a real backend drives the UI.
  it("Run all posts to /api/restart-all and flashes Started on SSE completion", async () => {
    const fetchSpy = vi
      .spyOn(globalThis, "fetch")
      .mockImplementation((input: Request | string | URL) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
        if (url === "/api/restart-all") {
          return Promise.resolve(jsonResponse(200, { restart_results: [] }));
        }
        return Promise.reject(new Error(`unexpected fetch: ${url}`));
      });
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
    const buttons = await findAllByRole("button");
    const runAllBtn = buttons[0] as HTMLButtonElement;

    fireEvent.click(runAllBtn);
    expect(fetchSpy).toHaveBeenCalledWith(
      "/api/restart-all",
      expect.objectContaining({ method: "POST" }),
    );

    // Backend would publish these — synthesize the round-trip.
    dispatchSse("bulk-action", { phase: "started", action: "restart" });
    await waitFor(() => expect(runAllBtn.textContent).toBe("Starting…"));

    dispatchSse("bulk-action", { phase: "completed", action: "restart" });
    await waitFor(() => expect(runAllBtn.textContent).toBe("Started"));
  });

  it("Stop all posts to /api/stop-all and flashes Stopped on SSE completion", async () => {
    const fetchSpy = vi
      .spyOn(globalThis, "fetch")
      .mockImplementation((input: Request | string | URL) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
        if (url === "/api/stop-all") {
          return Promise.resolve(jsonResponse(200, { stop_results: [] }));
        }
        return Promise.reject(new Error(`unexpected fetch: ${url}`));
      });
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
    const buttons = await findAllByRole("button");
    const stopAllBtn = buttons[1] as HTMLButtonElement;

    fireEvent.click(stopAllBtn);
    expect(fetchSpy).toHaveBeenCalledWith(
      "/api/stop-all",
      expect.objectContaining({ method: "POST" }),
    );

    dispatchSse("bulk-action", { phase: "started", action: "stop" });
    await waitFor(() => expect(stopAllBtn.textContent).toBe("Stopping…"));

    dispatchSse("bulk-action", { phase: "completed", action: "stop" });
    await waitFor(() => expect(stopAllBtn.textContent).toBe("Stopped"));
  });

  // Tray-triggered fan-out goes through the SAME pipeline: tray POSTs
  // /api/restart-all → backend publishes "started" → any open Dashboard
  // animates. This guards the unified-pipeline contract: an SSE event
  // alone (no local fetch) should drive the UI.
  it("tray-triggered: bulk-action SSE alone animates the buttons (no local fetch needed)", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([runningRow]));
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
    const buttons = await findAllByRole("button");
    const runAllBtn = buttons[0] as HTMLButtonElement;
    const stopAllBtn = buttons[1] as HTMLButtonElement;

    // Simulate a tray click somewhere else → backend published.
    dispatchSse("bulk-action", { phase: "started", action: "restart" });
    await waitFor(() => expect(runAllBtn.textContent).toBe("Starting…"));
    // Lock applies — Stop all must also be disabled.
    expect(stopAllBtn.disabled).toBe(true);

    dispatchSse("bulk-action", { phase: "completed", action: "restart" });
    await waitFor(() => expect(runAllBtn.textContent).toBe("Started"));
    expect(stopAllBtn.disabled).toBe(false);
  });

  it("disables Run all and Stop all when no daemons are listed", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([]));
    const { findAllByRole } = render(<DashboardScreen />);
    // With empty list there are no Cards, only the 2 bulk header buttons.
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(2);
    });
    const buttons = await findAllByRole("button");
    expect((buttons[0] as HTMLButtonElement).disabled).toBe(true);
    expect((buttons[1] as HTMLButtonElement).disabled).toBe(true);
  });

  // Codex bot PR #36 P2: bulk actions are global; clicking Stop all
  // while Run all is in flight (or vice versa) would race
  // /api/restart-all with /api/stop-all against every daemon and the
  // final state would depend on request timing rather than user intent.
  // BulkActionsRow holds a shared in-flight lock so the second click
  // is a no-op until the first completes.
  it("bulk-action lock: Stop all is disabled while Run all is in flight (SSE-driven)", async () => {
    const fetchSpy = vi
      .spyOn(globalThis, "fetch")
      .mockImplementation((input: Request | string | URL) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
        if (url === "/api/restart-all") return Promise.resolve(jsonResponse(200, { restart_results: [] }));
        if (url === "/api/stop-all") return Promise.resolve(jsonResponse(200, { stop_results: [] }));
        return Promise.reject(new Error(`unexpected fetch: ${url}`));
      });
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
    const buttons = await findAllByRole("button");
    const runAllBtn = buttons[0] as HTMLButtonElement;
    const stopAllBtn = buttons[1] as HTMLButtonElement;

    // Simulate the start of a Run all (no completion yet).
    dispatchSse("bulk-action", { phase: "started", action: "restart" });
    await waitFor(() => expect(runAllBtn.textContent).toBe("Starting…"));

    // Stop all MUST be disabled — the lock keeps overlapping fan-outs out.
    expect(stopAllBtn.disabled).toBe(true);

    // Defensive click on Stop all must NOT smuggle a fetch through.
    fireEvent.click(stopAllBtn);
    expect(fetchSpy.mock.calls.find((c) => {
      const url = typeof c[0] === "string" ? c[0] : c[0]?.toString();
      return url === "/api/stop-all";
    })).toBeUndefined();

    // Backend completes; lock releases.
    dispatchSse("bulk-action", { phase: "completed", action: "restart" });
    await waitFor(() => expect(runAllBtn.textContent).toBe("Started"));
    expect(stopAllBtn.disabled).toBe(false);
  });

  it("disables Restart while Stop is in flight (mutual exclusion per card)", async () => {
    let resolveStop: (r: Response) => void = () => {};
    const stopInFlight = new Promise<Response>((resolve) => {
      resolveStop = resolve;
    });
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
      if (url === "/api/servers/memory/stop?daemon=default") return stopInFlight;
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });

    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4); // 2 bulk + 2 per-card
    });
    const buttons = await findAllByRole("button");
    const restartBtn = buttons[2] as HTMLButtonElement;
    const stopBtn = buttons[3] as HTMLButtonElement;

    fireEvent.click(stopBtn);
    await waitFor(() => expect(stopBtn.textContent).toBe("Stopping…"));
    // Restart must be locked while Stop is in flight.
    expect(restartBtn.disabled).toBe(true);

    resolveStop(jsonResponse(200, { stop_results: [] }));
    await waitFor(() => expect(stopBtn.textContent).toBe("Stopped"));
  });
});
