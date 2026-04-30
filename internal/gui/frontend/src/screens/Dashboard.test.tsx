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
  // Codex bot PR #38 P2: rejected fetch (network down, connection
  // refused, DNS failure) means backend never receives request → no
  // SSE arrives → button stays idle without this fallback.
  it("local fallback: rejected fetch sets bulk outcome to Failed when no SSE arrives", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
      if (url === "/api/restart-all") return Promise.reject(new Error("net::ERR_CONNECTION_REFUSED"));
      return Promise.reject(new Error(`unexpected: ${url}`));
    });
    vi.spyOn(console, "error").mockImplementation(() => {});

    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
    const buttons = await findAllByRole("button");
    const runAllBtn = buttons[0] as HTMLButtonElement;

    fireEvent.click(runAllBtn);
    // No SSE event will ever arrive — only the local catch fallback
    // can set the button to Failed.
    await waitFor(() => expect(runAllBtn.textContent).toBe("Failed"));
  });

  // Codex bot PR #38 P1 (round 2): backpressure-dropped SSE event
  // recovery. The implementation is a 5min safety-net useEffect in
  // DashboardScreen. End-to-end verification is brittle in vitest +
  // happy-dom (fake-timer + Preact-microtask interplay), so the
  // contract is enforced by code review of the useEffect block.
  // The other bulk-action tests guard the normal SSE-driven path.

  // Codex bot PR #38 P1 (round 3): re-entrant double-click guard.
  // props.inflight only flips after SSE "started" round-trip; two
  // rapid clicks before that fire two POSTs and reintroduce
  // overlapping fan-outs. Optimistic-set on click closes the window:
  // the second click sees bulkInflight!==null and is gated.
  // Per-card buttons must cascade with bulk-action state. Run all
  // is by definition "click each per-card Restart" so every card's
  // Restart button must show "Restarting…" + disabled while the
  // bulk operation is in flight. Without this the Dashboard
  // showed bulk header animation but per-card buttons looked
  // idle/clickable, which lies about the state.
  it("bulk cascade: Run all puts every Card's Restart button into Restarting…", async () => {
    const serenaClaude: DaemonStatus = {
      server: "serena",
      daemon: "claude",
      port: 9121,
      pid: 1,
      state: "Running",
    };
    const serenaCodex: DaemonStatus = {
      server: "serena",
      daemon: "codex",
      port: 9122,
      pid: 2,
      state: "Running",
    };
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([serenaClaude, serenaCodex]));
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      // 2 bulk header + 2 cards × (Restart + Stop) = 6
      expect(buttons.length).toBe(6);
    });
    // Indexes: [0] Run all, [1] Stop all, [2/4] Restart per card,
    // [3/5] Stop per card.
    let buttons = await findAllByRole("button");
    expect(buttons[2]?.textContent).toBe("Restart");
    expect(buttons[4]?.textContent).toBe("Restart");

    // Tray (or anyone) triggers a bulk Restart — SSE arrives.
    dispatchSse("bulk-action", { phase: "started", action: "restart" });
    await waitFor(() => {
      const btns = (Array.from(document.querySelectorAll("button"))) as HTMLButtonElement[];
      expect(btns[0].textContent).toBe("Starting…");
    });
    buttons = await findAllByRole("button");
    // Both per-card Restart buttons MUST cascade to Restarting…
    expect(buttons[2].textContent).toBe("Restarting…");
    expect(buttons[4].textContent).toBe("Restarting…");
    // ALL buttons disabled (bulk-in-flight gates everything).
    for (const b of buttons) {
      expect((b as HTMLButtonElement).disabled).toBe(true);
    }

    // Bulk completes — outcome flash cascades to every Restart.
    dispatchSse("bulk-action", { phase: "completed", action: "restart" });
    buttons = await findAllByRole("button");
    await waitFor(() => {
      const btns = (Array.from(document.querySelectorAll("button"))) as HTMLButtonElement[];
      expect(btns[2].textContent).toBe("Restarted");
      expect(btns[4].textContent).toBe("Restarted");
    });
  });

  // Codex bot PR #38 P1 (commit ef0f4ea, "Correlate bulk-action
  // terminal events before unlocking UI"): concurrent triggers
  // (Dashboard + tray, or two Dashboards) can interleave SSE events.
  // Sibling operation's terminal must NOT clear the locally-tracked
  // inflight, otherwise the UI re-enables buttons mid-action.
  it("event correlation: sibling terminal does not clear locally-tracked inflight", async () => {
    vi.useRealTimers();
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([runningRow]));
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
    const buttons = await findAllByRole("button");
    const runAllBtn = buttons[0] as HTMLButtonElement;
    const stopAllBtn = buttons[1] as HTMLButtonElement;

    // Track restart locally (started for restart sets inflight).
    dispatchSse("bulk-action", { phase: "started", action: "restart" });
    await waitFor(() => expect(runAllBtn.textContent).toBe("Starting…"));

    // Sibling stop also fires (someone else triggered it). Started
    // for stop must NOT overwrite — first-tracked wins so terminal
    // matching stays consistent.
    dispatchSse("bulk-action", { phase: "started", action: "stop" });
    expect(runAllBtn.textContent).toBe("Starting…"); // unchanged
    expect(stopAllBtn.disabled).toBe(true);          // both disabled

    // Stop's completed arrives FIRST. Since we tracked restart,
    // this terminal must NOT unlock our UI — restart still running.
    dispatchSse("bulk-action", { phase: "completed", action: "stop" });
    expect(runAllBtn.textContent).toBe("Starting…"); // STILL starting
    expect(runAllBtn.disabled).toBe(true);

    // Restart finally completes — NOW our tracked terminal arrives.
    dispatchSse("bulk-action", { phase: "completed", action: "restart" });
    await waitFor(() => expect(runAllBtn.textContent).toBe("Started"));
    expect(runAllBtn.disabled).toBe(false);
    expect(stopAllBtn.disabled).toBe(false);
  });

  // Codex bot PR #38 P2 (commit ff656fe): "Report failed click even
  // when prior bulk outcome is visible". prev ?? error suppressed
  // new failures when a previous outcome was still in its 1.5s flash.
  // Fix: only preserve prev if prev.action === action (same action,
  // SSE-driven error overrides idempotently). For different actions,
  // the new error wins.
  it("local fallback: stale prior outcome does NOT mask a new failed click", async () => {
    vi.useRealTimers();
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
      if (url === "/api/restart-all") return Promise.resolve(jsonResponse(200, { restart_results: [] }));
      if (url === "/api/stop-all") return Promise.reject(new Error("net::ERR"));
      return Promise.reject(new Error(`unexpected: ${url}`));
    });
    vi.spyOn(console, "error").mockImplementation(() => {});

    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
    let buttons = await findAllByRole("button");
    let runAllBtn = buttons[0] as HTMLButtonElement;
    let stopAllBtn = buttons[1] as HTMLButtonElement;

    // First action: Run all succeeds via SSE.
    fireEvent.click(runAllBtn);
    dispatchSse("bulk-action", { phase: "started", action: "restart" });
    dispatchSse("bulk-action", { phase: "completed", action: "restart" });
    await waitFor(() => expect(runAllBtn.textContent).toBe("Started"));

    // Second action: Stop all REJECTS — different action than the
    // stale outcome (which is restart=done). New error MUST win;
    // prior `Started` flash on Run all gets cleared.
    fireEvent.click(stopAllBtn);
    await waitFor(() => expect(stopAllBtn.textContent).toBe("Failed"));
    // Run all button should NOT still show "Started" — outcome is now
    // for stop, so it falls back to idle.
    buttons = await findAllByRole("button");
    runAllBtn = buttons[0] as HTMLButtonElement;
    expect(runAllBtn.textContent).toBe("Run all");
  });

  it("re-entrant guard: rapid second click does not fire a second POST", async () => {
    vi.useRealTimers();
    let restartFireCount = 0;
    let resolveFirst: (r: Response) => void = () => {};
    const firstInFlight = new Promise<Response>((resolve) => {
      resolveFirst = resolve;
    });
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
      if (url === "/api/restart-all") {
        restartFireCount++;
        return firstInFlight;
      }
      return Promise.reject(new Error(`unexpected: ${url}`));
    });
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
    const buttons = await findAllByRole("button");
    const runAllBtn = buttons[0] as HTMLButtonElement;

    // Click 1 — optimistic state flips bulkInflight immediately;
    // button becomes disabled + "Starting…" before SSE arrives.
    fireEvent.click(runAllBtn);
    await waitFor(() => expect(runAllBtn.textContent).toBe("Starting…"));
    expect(restartFireCount).toBe(1);

    // Click 2/3 — must be gated. Without the optimistic update, these
    // would fire additional /api/restart-all before SSE "started"
    // updated bulkInflight.
    fireEvent.click(runAllBtn);
    fireEvent.click(runAllBtn);
    expect(restartFireCount).toBe(1);

    // Cleanup — resolve the in-flight fetch so it doesn't leak.
    resolveFirst(jsonResponse(200, { restart_results: [] }));
  });

  // P1 verification: when backend publishes phase=error (the partial-
  // failure 207 path now does this), Run all flashes Failed not
  // Started. Drives the same SSE handler the real backend would.
  it("partial failure on /api/restart-all: SSE phase=error → button shows Failed", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([runningRow]));
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
    const buttons = await findAllByRole("button");
    const runAllBtn = buttons[0] as HTMLButtonElement;

    dispatchSse("bulk-action", { phase: "started", action: "restart" });
    await waitFor(() => expect(runAllBtn.textContent).toBe("Starting…"));
    dispatchSse("bulk-action", {
      phase: "error",
      action: "restart",
      results: [{ task_name: "x", error: "kill timeout" }],
    });
    await waitFor(() => expect(runAllBtn.textContent).toBe("Failed"));
  });

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
