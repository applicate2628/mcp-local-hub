import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, waitFor, cleanup, fireEvent } from "@testing-library/preact";
import { DashboardScreen } from "./Dashboard";
import type { DaemonStatus } from "../types";

// happy-dom does not ship EventSource. Dashboard mounts useEventSource
// for /api/events, so we install an inert stub: construct + addEventListener +
// close are no-ops. These tests drive state changes via direct fetch
// responses, not SSE, so the stub never needs to dispatch.
class StubEventSource {
  url: string;
  constructor(url: string) {
    this.url = url;
  }
  addEventListener(_name: string, _handler: (ev: MessageEvent) => void): void {}
  removeEventListener(_name: string, _handler: (ev: MessageEvent) => void): void {}
  close(): void {}
}
(globalThis as unknown as { EventSource: typeof StubEventSource }).EventSource = StubEventSource;

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

  it("renders Stop button alongside Restart for a Running daemon", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([runningRow]));
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(2);
    });
    const buttons = await findAllByRole("button");
    expect(buttons[0]?.textContent).toBe("Restart");
    expect(buttons[1]?.textContent).toBe("Stop");
  });

  it("disables Stop when daemon state is Stopped (nothing to stop)", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([stoppedRow]));
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(2);
    });
    const buttons = await findAllByRole("button");
    const stopBtn = buttons[1] as HTMLButtonElement;
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
      expect(buttons.length).toBe(2);
    });
    const buttons = await findAllByRole("button");
    const stopBtn = buttons[1] as HTMLButtonElement;

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
      expect(buttons.length).toBe(2);
    });
    const buttons = await findAllByRole("button");
    const stopBtn = buttons[1] as HTMLButtonElement;

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
      expect(buttons.length).toBe(4); // 2 cards × (Restart + Stop)
    });
    const buttons = await findAllByRole("button");
    // Cards are sorted by keyFor(): "serena/claude" < "serena/codex".
    // Buttons render Restart-then-Stop per card in document order.
    // Indexes: [0] claude Restart, [1] claude Stop, [2] codex Restart, [3] codex Stop.
    const codexRestartBtn = buttons[2] as HTMLButtonElement;

    fireEvent.click(codexRestartBtn);
    await waitFor(() => expect(codexRestartBtn.textContent).toBe("Restarted"));

    // The request MUST carry ?daemon=codex. A bare /restart would (per
    // backend api.Restart with empty daemonFilter) restart claude too —
    // the regression we're guarding against.
    const calls = fetchSpy.mock.calls.map((c) => (typeof c[0] === "string" ? c[0] : c[0]?.toString()));
    expect(calls).toContain("/api/servers/serena/restart?daemon=codex");
    expect(calls).not.toContain("/api/servers/serena/restart");
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
      expect(buttons.length).toBe(2);
    });
    const buttons = await findAllByRole("button");
    const restartBtn = buttons[0] as HTMLButtonElement;
    const stopBtn = buttons[1] as HTMLButtonElement;

    fireEvent.click(stopBtn);
    await waitFor(() => expect(stopBtn.textContent).toBe("Stopping…"));
    // Restart must be locked while Stop is in flight.
    expect(restartBtn.disabled).toBe(true);

    resolveStop(jsonResponse(200, { stop_results: [] }));
    await waitFor(() => expect(stopBtn.textContent).toBe("Stopped"));
  });
});
