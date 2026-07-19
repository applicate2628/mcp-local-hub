import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, waitFor, cleanup, fireEvent, act } from "@testing-library/preact";
import { DashboardScreen, formatUptime, formatBytes } from "./Dashboard";
import type { DaemonStatus, ScanEntry } from "../types";

// happy-dom does not ship EventSource. Dashboard's bulk-action UI state
// is SSE-driven (PR #38: unified pipeline — backend publishes
// bulk-action events, frontend mirrors them). The stub captures
// listeners so tests can dispatch synthetic events that drive the same
// state transitions a real backend would.
type StubListener = (ev: MessageEvent) => void;
type StubLifecycleListener = ((ev: Event) => void) | null;
const stubInstances = new Set<StubEventSource>();
class StubEventSource {
  url: string;
  listeners = new Map<string, Set<StubListener>>();
  onopen: StubLifecycleListener = null;
  onerror: StubLifecycleListener = null;
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
  triggerOpen(): void {
    this.onopen?.(new Event("open"));
  }
  triggerError(): void {
    this.onerror?.(new Event("error"));
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

function activeStubEventSource(): StubEventSource {
  const [source] = stubInstances;
  if (!source) throw new Error("no active StubEventSource");
  return source;
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

function scanResponse(entries: ScanEntry[]): Response {
  return jsonResponse(200, {
    at: "2026-07-09T00:00:00Z",
    entries,
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

  it("polls /api/status every 30 seconds so supervisor-backed rows refresh without SSE deltas", async () => {
    let statusCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") {
        statusCalls++;
        return Promise.resolve(statusResponse(statusCalls === 1 ? [runningRow] : [runningRow, stoppedRow]));
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });

    await vi.advanceTimersByTimeAsync(30_000);

    expect(statusCalls).toBe(2);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(6);
    });
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

  // Codex bot PR #39 P2 ("Let bulk outcome override stale per-card
  // flash state"): a recent local click leaves the per-card button
  // state in "done"/"error" for 1.5s. If a bulk action lands during
  // that flash window, every card MUST show the bulk outcome —
  // otherwise the card with the stale local flash diverges from
  // siblings, breaking the "all cards mirror bulk action" invariant.
  it("bulk outcome overrides stale per-card flash state", async () => {
    vi.useRealTimers();
    let resolveLocal: (r: Response) => void = () => {};
    const localStop = new Promise<Response>((resolve) => {
      resolveLocal = resolve;
    });
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
      if (url === "/api/servers/memory/stop?daemon=default") return localStop;
      return Promise.reject(new Error(`unexpected: ${url}`));
    });

    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
    const buttons = await findAllByRole("button");
    const cardStop = buttons[3] as HTMLButtonElement;

    // Local click on per-card Stop, resolve to "Stopped" flash.
    fireEvent.click(cardStop);
    resolveLocal(jsonResponse(200, { stop_results: [] }));
    await waitFor(() => expect(cardStop.textContent).toBe("Stopped"));

    // Tray triggers Run all DURING the 1.5s "Stopped" flash window.
    // Card's Restart button is also in stale-idle, but its Stop is
    // showing "Stopped" — when the bulk Restart completes, every
    // card's Restart MUST show the bulk outcome ("Restarted").
    dispatchSse("bulk-action", { phase: "started", action: "restart" });
    dispatchSse("bulk-action", { phase: "completed", action: "restart" });
    await waitFor(() => {
      const btns = (Array.from(document.querySelectorAll("button"))) as HTMLButtonElement[];
      expect(btns[2].textContent).toBe("Restarted"); // bulk cascade wins
    });
    // Stop button's local "Stopped" flash is unaffected (different
    // action than the bulk one).
    const cardStopAfter = (await findAllByRole("button"))[3] as HTMLButtonElement;
    expect(cardStopAfter.textContent).toBe("Stopped");
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

// v0.6 Workstream B (§3.1): when the supervisor IPC is down, the backend
// fails loud (/api/status → 500 STATUS_FAILED with the
// "supervisor unreachable — restart the hub" message) instead of falling
// back to the stale scheduler scan and painting Running daemons as
// failed/Restarting. The Dashboard must surface that as an explicit
// degraded state — the "Failed to load status" banner carrying the
// backend message, plus the "Restart supervisor" recovery affordance —
// NOT a misleading row of failed/Restarting cards.
describe("DashboardScreen — supervisor-down fail-loud (Workstream B §3.1)", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  it("renders the degraded banner with the supervisor-down message and NO daemon cards on /api/status 500", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") {
        // The exact envelope writeAPIError emits for the
        // api.ErrSupervisorDown degraded marker.
        return Promise.resolve(
          jsonResponse(500, {
            error: "supervisor unreachable — restart the hub",
            code: "STATUS_FAILED",
          }),
        );
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });

    const { findByTestId, queryByText } = render(<DashboardScreen />);

    // Restart-grace debounce: the FIRST 500 renders the calm Loading state,
    // not the banner — the supervisor may simply be coming up. Advance past
    // the grace window (the 30s poll re-fires each tick; the grace-deadline
    // timer flips persistentlyDegraded once RESTART_GRACE_MS elapses) so the
    // PERSISTENT-down fail-loud banner takes over. The §3.1 assertion below
    // is unchanged — a persistent down MUST still surface the degraded
    // banner; we just exercise it past the grace.
    await vi.advanceTimersByTimeAsync(30_000);
    await vi.advanceTimersByTimeAsync(30_000);
    await vi.advanceTimersByTimeAsync(30_000);

    // The fail-loud banner shows the backend's degraded message verbatim
    // (fetchOrThrow prefixes the path, so the message is a substring).
    const banner = await findByTestId("dashboard-error");
    expect(banner.textContent).toContain("supervisor unreachable — restart the hub");

    // NO daemon cards — the operator must not see Running daemons painted
    // as failed/Restarting from stale scheduler data.
    expect(document.querySelectorAll(".cards .card").length).toBe(0);

    // The recovery affordance must be present and prominent so the
    // operator can act on the "restart the hub" guidance.
    expect(queryByText("Restart supervisor")).not.toBeNull();
  });

  it("initial /api/status failure shows a calm Loading state, not the alarming banner", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") {
        return Promise.resolve(
          jsonResponse(500, {
            error: "supervisor unreachable — restart the hub",
            code: "STATUS_FAILED",
          }),
        );
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });

    const { findByTestId, queryByTestId } = render(<DashboardScreen />);

    // The first failure is inside the startup grace → calm Loading state,
    // NOT the degraded banner. (Flush the initial /api/status promise.)
    await vi.advanceTimersByTimeAsync(0);
    const loading = await findByTestId("dashboard-loading");
    expect(loading.textContent).toContain("Loading status…");
    expect(queryByTestId("dashboard-error")).toBeNull();
  });

  it("does NOT render the degraded banner on the happy path (no false positive)", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([runningRow]));
    const { findAllByRole, queryByTestId } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4); // 2 bulk + 2 per-card → cards rendered
    });
    // The error banner must be absent when the supervisor is up.
    expect(queryByTestId("dashboard-error")).toBeNull();
  });

  // Fix B (bug 2026-07-19): a transient supervisor restart/handoff window
  // (deploy, RestartV3 self-restart) makes /api/status fail for a few
  // seconds AFTER the dashboard has already loaded. That must NOT flash the
  // RED banner — it debounces to a calm reconnecting cue within
  // RESTART_GRACE_MS, and only turns RED once the streak passes the bound
  // (fail-loud on a genuine prolonged outage preserved).
  it("debounces the RED banner across a restart window: calm reconnecting cue within grace, RED past the bound", async () => {
    let statusFails = false;
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") {
        if (statusFails) {
          return Promise.resolve(
            jsonResponse(500, {
              error: "supervisor unreachable — restart the hub",
              code: "STATUS_FAILED",
            }),
          );
        }
        return Promise.resolve(statusResponse([runningRow]));
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });

    const { findAllByRole, findByTestId, queryByTestId, queryByText } = render(
      <DashboardScreen />,
    );

    // One successful /api/status → hasEverLoaded=true, cards render, no error.
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4); // 2 bulk + 2 per-card
    });
    expect(queryByTestId("dashboard-error")).toBeNull();

    // Supervisor enters a restart/handoff window: /api/status now 500s.
    statusFails = true;

    // Advance to the next 30s poll — the first failure marks degradedSince,
    // but we are still WITHIN the grace window (elapsed 0 < RESTART_GRACE_MS).
    // A calm reconnecting cue renders, NOT the RED banner.
    await vi.advanceTimersByTimeAsync(30_000);
    expect(queryByTestId("dashboard-error")).toBeNull();
    const reconnecting = await findByTestId("dashboard-reconnecting");
    expect(reconnecting.textContent).toContain("Reconnecting");

    // Advance PAST RESTART_GRACE_MS (20s) from the first failure → the
    // grace-deadline timer fires, persistentlyDegraded flips, and the RED
    // fail-loud banner takes over (prolonged outage → RED preserved).
    await vi.advanceTimersByTimeAsync(20_001);
    const banner = await findByTestId("dashboard-error");
    expect(banner.textContent).toContain("supervisor unreachable — restart the hub");
    // The reconnecting cue is gone; the RecoveryActions affordance is present.
    expect(queryByTestId("dashboard-reconnecting")).toBeNull();
    expect(queryByText("Restart supervisor")).not.toBeNull();
  });
});

describe("DashboardScreen — unmanaged stdio anti-drift signal", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  it("renders a Discovery adopt banner when /api/scan contains unmanaged stdio entries", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
      if (url === "/api/scan") {
        return Promise.resolve(
          scanResponse([
            {
              name: "local-stdio",
              status: "unknown",
              client_presence: { "claude-code": { transport: "stdio", endpoint: "npx" } },
            },
            {
              name: "local-uvx",
              status: "unknown",
              client_presence: { "codex-cli": { transport: "stdio", endpoint: "uvx" } },
            },
            {
              name: "context7",
              status: "external",
              client_presence: { "claude-code": { transport: "http", endpoint: "https://mcp.context7.com/mcp" } },
            },
          ]),
        );
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });

    const { findByTestId } = render(<DashboardScreen />);
    const banner = await findByTestId("dashboard-unmanaged-stdio");
    expect(banner.textContent).toContain("⚠ 2 unmanaged MCP servers bypassing the hub");
    const link = banner.querySelector("a") as HTMLAnchorElement | null;
    expect(link?.textContent).toBe("Adopt");
    expect(link?.getAttribute("href")).toBe("#/migration");
  });

  it("does not render the unmanaged stdio banner when /api/scan has no drift entries", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
      if (url === "/api/scan") {
        return Promise.resolve(
          scanResponse([
            {
              name: "fetch",
              status: "can-migrate",
              client_presence: { "claude-code": { transport: "stdio", endpoint: "npx" } },
            },
            {
              name: "odd-remote",
              status: "unknown",
              client_presence: { "claude-code": { transport: "http", endpoint: "https://example.test/mcp" } },
            },
          ]),
        );
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });

    const { findAllByRole, queryByTestId } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
    expect(queryByTestId("dashboard-unmanaged-stdio")).toBeNull();
  });
});

// Roadmap §B: per-daemon expanded-card metrics. UPTIME is derived
// server-side from the supervisor's started_at (DaemonStatus.uptime_sec);
// RAM is the live working-set bytes looked up by current_pid
// (DaemonStatus.ram_bytes). Both render as .card-kv rows alongside
// Port/PID/State and are omitted when absent/zero.
describe("formatUptime", () => {
  it("humanizes seconds into the two-largest-unit form", () => {
    expect(formatUptime(0)).toBe("");
    expect(formatUptime(undefined)).toBe("");
    expect(formatUptime(-5)).toBe("");
    expect(formatUptime(47)).toBe("47s");
    expect(formatUptime(60)).toBe("1m");
    expect(formatUptime(90)).toBe("1m 30s");
    expect(formatUptime(3600)).toBe("1h");
    expect(formatUptime(2 * 3600 + 14 * 60)).toBe("2h 14m"); // the spec example
    expect(formatUptime(86400)).toBe("1d");
    expect(formatUptime(3 * 86400 + 5 * 3600)).toBe("3d 5h");
  });
});

describe("formatBytes", () => {
  it("humanizes byte counts in binary units", () => {
    expect(formatBytes(0)).toBe("");
    expect(formatBytes(undefined)).toBe("");
    expect(formatBytes(-1)).toBe("");
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(48 * 1024 * 1024)).toBe("48 MB"); // the spec example
    expect(formatBytes(1024)).toBe("1 KB");
    expect(formatBytes(Math.round(1.2 * 1024 * 1024 * 1024))).toBe("1.2 GB");
  });
});

describe("DashboardScreen — expanded-card metrics (Uptime + RAM)", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  it("renders Uptime and RAM rows when uptime_sec + ram_bytes are present", async () => {
    const row: DaemonStatus = {
      server: "memory",
      daemon: "default",
      port: 9123,
      pid: 12345,
      state: "Running",
      uptime_sec: 2 * 3600 + 14 * 60, // 2h 14m
      ram_bytes: 48 * 1024 * 1024, // 48 MB
    };
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([row]));
    const { findByTestId } = render(<DashboardScreen />);

    const uptimeRow = await findByTestId("uptime-row");
    expect(uptimeRow.textContent).toContain("Uptime");
    expect(uptimeRow.textContent).toContain("2h 14m");

    const ramRow = await findByTestId("ram-row");
    expect(ramRow.textContent).toContain("RAM");
    expect(ramRow.textContent).toContain("48 MB");
  });

  it("omits the Uptime row when uptime_sec is absent/zero", async () => {
    const row: DaemonStatus = {
      server: "memory",
      daemon: "default",
      port: 9123,
      pid: 12345,
      state: "Running",
      ram_bytes: 10 * 1024 * 1024,
      // no uptime_sec
    };
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([row]));
    const { findByTestId, queryByTestId } = render(<DashboardScreen />);
    // RAM row present confirms the card rendered.
    await findByTestId("ram-row");
    expect(queryByTestId("uptime-row")).toBeNull();
  });

  it("omits the RAM row when ram_bytes is absent (non-Windows / lookup failed)", async () => {
    const row: DaemonStatus = {
      server: "memory",
      daemon: "default",
      port: 9123,
      pid: 12345,
      state: "Running",
      uptime_sec: 300,
      // no ram_bytes
    };
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([row]));
    const { findByTestId, queryByTestId } = render(<DashboardScreen />);
    await findByTestId("uptime-row");
    expect(queryByTestId("ram-row")).toBeNull();
  });

  it("does not add extra buttons (metric rows are non-interactive divs)", async () => {
    const row: DaemonStatus = {
      server: "memory",
      daemon: "default",
      port: 9123,
      pid: 12345,
      state: "Running",
      uptime_sec: 300,
      ram_bytes: 10 * 1024 * 1024,
    };
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([row]));
    const { findAllByRole } = render(<DashboardScreen />);
    // 2 bulk header + 2 per-card (Restart + Stop) — metric rows must NOT
    // contribute buttons, so the long-standing count invariant holds.
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      expect(buttons.length).toBe(4);
    });
  });
});

// Flowbite-Card metric cards (Feature 3): each daemon card now carries a
// "View logs" link (an <a>, NOT a button) plus Flowbite Card shell classes
// and a Flowbite state badge.
describe("DashboardScreen — Flowbite card layout + View-logs link", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  it("renders a View-logs link as an anchor to #/logs (not a button)", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([runningRow]));
    const { findByTestId } = render(<DashboardScreen />);
    const link = (await findByTestId("card-view-logs")) as HTMLAnchorElement;
    expect(link.tagName).toBe("A");
    expect(link.getAttribute("href")).toBe("#/logs");
  });

  it("does not add extra buttons (View-logs is a link)", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([runningRow]));
    const { findAllByRole } = render(<DashboardScreen />);
    await waitFor(async () => {
      const buttons = await findAllByRole("button");
      // Still 2 bulk + 2 per-card — the View-logs <a> is link-role, not button.
      expect(buttons.length).toBe(4);
    });
  });

  it("applies Flowbite Card shell classes while keeping the .card test hook", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(statusResponse([runningRow]));
    const { findByTestId } = render(<DashboardScreen />);
    const card = await findByTestId("dashboard-card");
    // Both the legacy class (status coloring + .cards .card selector) and
    // the Flowbite Card vocabulary are present.
    expect(card.className).toContain("card");
    expect(card.className).toContain("rounded-lg");
    expect(card.className).toContain("shadow-sm");
  });
});

describe("DashboardScreen — hub-health banner (Phase-0 item 1)", () => {
  function mockDashboardFetch(hubHealth: Record<string, unknown> = {
    state: "healthy",
    degraded: false,
  }) {
    return vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
      if (url === "/api/hub/health") return Promise.resolve(jsonResponse(200, hubHealth));
      if (url === "/api/scan") return Promise.resolve(scanResponse([]));
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });
  }

  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  it("hydrates a needs-reconcile banner from the initial GET without SSE", async () => {
    mockDashboardFetch({
      state: "needs-reconcile",
      degraded: true,
      operator_action: "mcphub install --reconcile-hub-mode",
    });
    const { findByTestId } = render(<DashboardScreen />);

    const banner = await findByTestId("dashboard-hub-health");
    expect(banner.getAttribute("data-hub-state")).toBe("needs-reconcile");
    expect(banner.textContent).toContain("mcphub install --reconcile-hub-mode");
    expect(banner.textContent).toContain("notice clears when the hub GUI restarts");
  });

  it("re-hydrates hub health on every SSE open, including reconnect", async () => {
    const healthResponses = [
      { state: "healthy", degraded: false },
      {
        state: "needs-reconcile",
        degraded: true,
        operator_action: "mcphub install --reconcile-hub-mode",
      },
      { state: "down", degraded: true },
    ];
    let healthCalls = 0;
    let resolveFirstOpenFetch!: () => void;
    const firstOpenFetch = new Promise<void>((resolve) => {
      resolveFirstOpenFetch = resolve;
    });
    let resolveReconnectOpenFetch!: () => void;
    const reconnectOpenFetch = new Promise<void>((resolve) => {
      resolveReconnectOpenFetch = resolve;
    });
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
      if (url === "/api/hub/health") {
        healthCalls += 1;
        if (healthCalls === 2) resolveFirstOpenFetch();
        if (healthCalls === 3) resolveReconnectOpenFetch();
        const body = healthResponses.shift();
        if (!body) return Promise.reject(new Error("unexpected extra hub-health fetch"));
        return Promise.resolve(jsonResponse(200, body));
      }
      if (url === "/api/scan") return Promise.resolve(scanResponse([]));
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });
    const { findAllByRole, findByTestId } = render(<DashboardScreen />);
    await findAllByRole("button");
    expect(healthCalls).toBe(1);

    const stream = activeStubEventSource();
    act(() => {
      stream.triggerOpen();
    });
    await firstOpenFetch;
    expect(healthCalls).toBe(2);
    let banner = await findByTestId("dashboard-hub-health");
    expect(banner.getAttribute("data-hub-state")).toBe("needs-reconcile");

    act(() => {
      stream.triggerError();
    });
    expect((await findByTestId("connection-badge")).textContent).toContain("reconnecting");
    act(() => {
      stream.triggerOpen();
    });
    await reconnectOpenFetch;
    expect(healthCalls).toBe(3);
    banner = await findByTestId("dashboard-hub-health");
    await waitFor(() => expect(banner.getAttribute("data-hub-state")).toBe("down"));
  });

  it("re-hydrates hub health when the document becomes visible", async () => {
    const healthResponses = [
      { state: "healthy", degraded: false },
      { state: "down", degraded: true },
    ];
    let healthCalls = 0;
    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
      if (url === "/api/hub/health") {
        healthCalls += 1;
        const body = healthResponses.shift();
        if (!body) return Promise.reject(new Error("unexpected extra hub-health fetch"));
        return Promise.resolve(jsonResponse(200, body));
      }
      if (url === "/api/scan") return Promise.resolve(scanResponse([]));
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });
    const { findAllByRole, findByTestId } = render(<DashboardScreen />);
    await findAllByRole("button");
    await waitFor(() => expect(healthCalls).toBe(1));

    act(() => {
      document.dispatchEvent(new Event("visibilitychange"));
    });

    await waitFor(() => expect(healthCalls).toBe(2));
    const banner = await findByTestId("dashboard-hub-health");
    expect(banner.getAttribute("data-hub-state")).toBe("down");
  });

  it("re-hydrates hub health on the 60-second interval", async () => {
    let healthCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
      if (url === "/api/hub/health") {
        healthCalls += 1;
        return Promise.resolve(jsonResponse(200, { state: "healthy", degraded: false }));
      }
      if (url === "/api/scan") return Promise.resolve(scanResponse([]));
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });
    const { findAllByRole } = render(<DashboardScreen />);
    await findAllByRole("button");
    await waitFor(() => expect(healthCalls).toBe(1));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });

    await waitFor(() => expect(healthCalls).toBe(2));
  });

  it("keeps a newer hub-health SSE state when the mount GET resolves stale", async () => {
    let resolveMountHealth!: (response: Response) => void;
    const mountHealth = new Promise<Response>((resolve) => {
      resolveMountHealth = resolve;
    });
    let healthCalls = 0;
    let resolveMountHealthRequested!: () => void;
    const mountHealthRequested = new Promise<void>((resolve) => {
      resolveMountHealthRequested = resolve;
    });
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
      if (url === "/api/hub/health") {
        healthCalls += 1;
        resolveMountHealthRequested();
        return mountHealth;
      }
      if (url === "/api/scan") return Promise.resolve(scanResponse([]));
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });
    const { findAllByRole, findByTestId } = render(<DashboardScreen />);
    await findAllByRole("button");
    await mountHealthRequested;
    expect(healthCalls).toBe(1);

    dispatchSse("hub-health", { state: "recovering", degraded: true });
    let banner = await findByTestId("dashboard-hub-health");
    expect(banner.getAttribute("data-hub-state")).toBe("recovering");

    const staleResponse = jsonResponse(200, { state: "healthy", degraded: false });
    let resolveStaleJSONRead!: () => void;
    const staleJSONRead = new Promise<void>((resolve) => {
      resolveStaleJSONRead = resolve;
    });
    vi.spyOn(staleResponse, "json").mockImplementation(async () => {
      resolveStaleJSONRead();
      return { state: "healthy", degraded: false };
    });
    resolveMountHealth(staleResponse);
    await staleJSONRead;
    await Promise.resolve();
    await Promise.resolve();

    banner = await findByTestId("dashboard-hub-health");
    expect(banner.getAttribute("data-hub-state")).toBe("recovering");
  });

  it("applies an earlier successful mount GET when a later open GET fails", async () => {
    let resolveMountHealth!: (response: Response) => void;
    const mountHealth = new Promise<Response>((resolve) => {
      resolveMountHealth = resolve;
    });
    let healthCalls = 0;
    let resolveOpenHealthRequested!: () => void;
    const openHealthRequested = new Promise<void>((resolve) => {
      resolveOpenHealthRequested = resolve;
    });
    vi.spyOn(globalThis, "fetch").mockImplementation((input: Request | string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url === "/api/status") return Promise.resolve(statusResponse([runningRow]));
      if (url === "/api/hub/health") {
        healthCalls += 1;
        if (healthCalls === 1) return mountHealth;
        if (healthCalls === 2) {
          resolveOpenHealthRequested();
          return Promise.reject(new Error("open resync failed"));
        }
        return Promise.reject(new Error("unexpected extra hub-health fetch"));
      }
      if (url === "/api/scan") return Promise.resolve(scanResponse([]));
      return Promise.reject(new Error(`unexpected fetch: ${url}`));
    });
    const { findAllByRole, findByTestId } = render(<DashboardScreen />);
    await findAllByRole("button");
    await waitFor(() => expect(healthCalls).toBe(1));

    act(() => {
      activeStubEventSource().triggerOpen();
    });
    await openHealthRequested;
    expect(healthCalls).toBe(2);

    resolveMountHealth(jsonResponse(200, { state: "down", degraded: true }));
    const banner = await findByTestId("dashboard-hub-health");
    expect(banner.getAttribute("data-hub-state")).toBe("down");
  });

  it("shows restart guidance for a down hub-health SSE event", async () => {
    mockDashboardFetch();
    const { findAllByRole, findByTestId } = render(<DashboardScreen />);
    await findAllByRole("button");

    dispatchSse("hub-health", { state: "down", degraded: true });
    const banner = await findByTestId("dashboard-hub-health");
    expect(banner.getAttribute("data-hub-state")).toBe("down");
    expect(banner.textContent).toContain("Restart the hub");
  });

  it("describes needs-reconcile as serving with stale client config", async () => {
    mockDashboardFetch();
    const { findAllByRole, findByTestId } = render(<DashboardScreen />);
    await findAllByRole("button");

    dispatchSse("hub-health", {
      state: "needs-reconcile",
      degraded: true,
      operator_action: "mcphub repair --from-health-event",
    });
    const banner = await findByTestId("dashboard-hub-health");
    expect(banner.textContent).toContain("mcphub repair --from-health-event");
    expect(banner.textContent).not.toContain("mcphub install --reconcile-hub-mode");
    expect(banner.textContent).toContain("notice clears when the hub GUI restarts");
    expect(banner.textContent).not.toContain("cannot reach any server");
  });

  it("shows a degraded hub banner on a needs-reconcile hub-health SSE event, then hides it when healthy", async () => {
    mockDashboardFetch();
    const { findAllByRole, queryByTestId, findByTestId } = render(<DashboardScreen />);
    await findAllByRole("button");

    // Healthy/unknown → no banner (the whole point: green cards never hide a dead hub silently,
    // but a healthy hub shows nothing).
    expect(queryByTestId("dashboard-hub-health")).toBeNull();

    // A needs-reconcile hub is serving on a new address, but stale clients need guidance.
    dispatchSse("hub-health", {
      state: "needs-reconcile",
      degraded: true,
      operator_action: "mcphub install --reconcile-hub-mode",
    });
    const banner = await findByTestId("dashboard-hub-health");
    expect(banner.getAttribute("data-hub-state")).toBe("needs-reconcile");
    expect(banner.textContent).not.toContain("cannot reach any server");
    expect(banner.textContent).toContain("reconcile");

    // Recovered → banner gone.
    dispatchSse("hub-health", { state: "healthy", degraded: false });
    await waitFor(() => expect(queryByTestId("dashboard-hub-health")).toBeNull());
  });
});

describe("DashboardScreen — quarantined daemon recovery", () => {
  const quarantinedRow: DaemonStatus = {
    server: "memory",
    daemon: "default",
    port: 9123,
    state: "Quarantined",
    task_name: "mcp-local-hub-memory-default",
    display_name: "Memory daemon",
  };

  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  afterEach(() => {
    cleanup();
  });

  function mockRecoveryFetch(recoverResponse: Response = jsonResponse(200, {
    task_name: quarantinedRow.task_name,
    state: "respawn_accepted",
    reaped: false,
    port_owner_check: "unbound",
    port_wait_outcome: "not_required",
  })) {
    let statusCalls = 0;
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(
      (input: Request | string | URL) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url === "/api/status") {
          statusCalls += 1;
          return Promise.resolve(statusResponse([quarantinedRow]));
        }
        if (url === "/api/scan") return Promise.resolve(scanResponse([]));
        if (url === "/api/hub/health") {
          return Promise.resolve(jsonResponse(200, { state: "healthy", degraded: false }));
        }
        if (url === "/api/daemon/recover") return Promise.resolve(recoverResponse.clone());
        return Promise.reject(new Error(`unexpected fetch: ${url}`));
      },
    );
    return { fetchSpy, statusCalls: () => statusCalls };
  }

  it("shows the quarantine reason and Recover instead of the refused Restart; Cancel sends nothing", async () => {
    const { fetchSpy } = mockRecoveryFetch();
    const view = render(<DashboardScreen />);
    const recover = await view.findByRole("button", { name: "Recover" });

    expect(view.queryByRole("button", { name: "Restart" })).toBeNull();
    expect(view.getByRole("status").textContent).toContain(
      "Automatic restart is paused because this daemon is quarantined after repeated failures.",
    );

    fireEvent.click(recover);
    await waitFor(() => {
      expect((view.getByTestId("daemon-recover-modal") as HTMLDialogElement).open).toBe(true);
    });
    expect(view.getByText("Recover Memory daemon?")).toBeTruthy();
    expect(view.getByText(/It will never stop a foreign or unverifiable process/)).toBeTruthy();
    expect(view.getByText(/Verified lost-child termination is Windows-only in v1/)).toBeTruthy();
    fireEvent.click(view.getByTestId("daemon-recover-modal-cancel"));

    const recoverCalls = fetchSpy.mock.calls.filter((call) => call[0].toString() === "/api/daemon/recover");
    expect(recoverCalls).toHaveLength(0);
  });

  it("confirms the exact task identity, keeps Quarantined pending, and refreshes status immediately", async () => {
    const { fetchSpy, statusCalls } = mockRecoveryFetch();
    const view = render(<DashboardScreen />);
    fireEvent.click(await view.findByRole("button", { name: "Recover" }));
    fireEvent.click(view.getByTestId("daemon-recover-modal-confirm"));

    await view.findByText("Recovery accepted; waiting for supervisor status");
    await waitFor(() => expect(statusCalls()).toBeGreaterThanOrEqual(2));
    expect(view.getByTestId("dashboard-card").className).toContain("card warning");
    expect(view.getByTestId("dashboard-card").textContent).toContain("Quarantined");
    expect(fetchSpy).toHaveBeenCalledWith("/api/daemon/recover", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        task_name: "mcp-local-hub-memory-default",
        confirm: true,
      }),
    });
  });

  it.each([
    [409, "RECOVER_REFUSED_PORT_OWNER", "Recovery was refused: the port owner could not be verified as this daemon's child. No process was stopped."],
    [500, "RECOVER_RESPAWN_FAILED", "The supervisor did not accept recovery. View logs and retry after resolving the failure."],
    [503, "RECOVER_SUPERVISOR_UNAVAILABLE", "The supervisor is unavailable. Restart the hub, then retry recovery."],
    [408, "RECOVER_REQUEST_CANCELED", "Recovery was canceled before any process was stopped. Retry if recovery is still needed."],
    [504, "RECOVER_BOUNDARY_PROBE_TIMEOUT", "Recovery verified the process identity but timed out while rechecking the port owner. No process was stopped."],
    [503, "RECOVER_RESPAWN_BUDGET_INSUFFICIENT", "Recovery could not reserve enough time for a safe restart. No process was stopped; retry when the local system is less busy."],
    [500, "RECOVER_UNCLASSIFIED_FAILURE", "Recovery failed for an unclassified reason. No specific cause can be asserted; check the supervisor logs before retrying."],
  ])("maps HTTP %i code %s to persistent safe inline copy without claiming an undisclosed termination", async (status, code, message) => {
    const raw = "C:\\attacker\\owned.exe --secret-token";
    mockRecoveryFetch(jsonResponse(status, { error: raw, code }));
    const view = render(<DashboardScreen />);
    fireEvent.click(await view.findByRole("button", { name: "Recover" }));
    fireEvent.click(view.getByTestId("daemon-recover-modal-confirm"));

    const alert = await view.findByRole("alert");
    expect(alert.textContent).toBe(message);
    expect(alert.textContent).not.toContain("A process was already stopped during this recovery attempt.");
    expect(view.container.textContent).not.toContain(raw);
    expect(view.getByRole("button", { name: "Recover" })).toBeTruthy();
  });

  it.each([
    [500, "RECOVER_RESPAWN_FAILED", "The supervisor did not accept recovery. View logs and retry after resolving the failure."],
    [503, "RECOVER_SUPERVISOR_UNAVAILABLE", "The supervisor is unavailable. Restart the hub, then retry recovery."],
  ])("discloses a committed termination for HTTP %i code %s", async (status, code, message) => {
    const raw = "C:\\attacker\\owned.exe --secret-token";
    mockRecoveryFetch(jsonResponse(status, {
      error: raw,
      code,
      termination_committed: true,
    }));
    const view = render(<DashboardScreen />);
    fireEvent.click(await view.findByRole("button", { name: "Recover" }));
    fireEvent.click(view.getByTestId("daemon-recover-modal-confirm"));

    const alert = await view.findByRole("alert");
    expect(alert.textContent).toBe(`${message} A process was already stopped during this recovery attempt.`);
    expect(view.container.textContent).not.toContain(raw);
  });
});
