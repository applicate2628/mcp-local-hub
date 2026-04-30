import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import { fetchOrThrow } from "../api";
import { useEventSource } from "../hooks/useEventSource";
import type { DaemonStatus } from "../types";

// Key state map by "<server>/<daemon>" — matches the poller convention.
// A multi-daemon server (serena: claude + codex) would otherwise collide
// on server alone and render one card instead of two.
function keyFor(r: { server: string; daemon?: string }): string {
  return `${r.server}/${r.daemon ?? "default"}`;
}

// BulkAction is the action verb used in /api/{restart,stop}-all and in
// the SSE "bulk-action" lifecycle events. Single source of truth: any
// trigger (Dashboard click, tray menu, future API client) flows through
// the same HTTP endpoint and produces the same SSE events. The UI
// state below is a pure projection of those events.
type BulkAction = "restart" | "stop";
type BulkOutcome = { action: BulkAction; state: "done" | "error" };

export function DashboardScreen() {
  const [state, setState] = useState<Record<string, DaemonStatus>>({});
  const [error, setError] = useState<string | null>(null);
  // Bulk-action state driven ENTIRELY by SSE "bulk-action" events. A
  // local click sends HTTP POST → server publishes "started" → this
  // handler flips inflight; "completed"/"error" clears inflight and
  // sets outcome for the flash. Tray-triggered runs reach the same
  // event stream so any open Dashboard sees the same animation.
  const [bulkInflight, setBulkInflight] = useState<BulkAction | null>(null);
  const [bulkOutcome, setBulkOutcome] = useState<BulkOutcome | null>(null);
  const bulkResetTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(
    () => () => {
      if (bulkResetTimerRef.current) clearTimeout(bulkResetTimerRef.current);
    },
    [],
  );

  // Initial bootstrap. Non-ok status OR non-array body → error state.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const rows = await fetchOrThrow<DaemonStatus[]>("/api/status", "array");
        if (cancelled) return;
        const next: Record<string, DaemonStatus> = {};
        // Scheduler-maintenance rows (weekly-refresh tasks) have no
        // meaningful "Restart" action. Rendering them would produce a
        // blank-name card whose Restart button hits
        // /api/servers//restart → invalid target.
        for (const row of rows.filter((r) => !r.is_maintenance)) {
          next[keyFor(row)] = row;
        }
        setState(next);
      } catch (err) {
        if (!cancelled) setError((err as Error).message);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  // SSE delta handler. Same maintenance filter as bootstrap — otherwise a
  // weekly-refresh transition would re-inject a blank-name card after the
  // initial filter dropped it.
  const onDelta = useCallback((ev: MessageEvent) => {
    const body = JSON.parse(ev.data) as DaemonStatus & { state: string };
    if (body.is_maintenance) return;
    const k = keyFor(body);
    // A valid delta means the backend is reachable — clear any stale
    // bootstrap error so the early-return at render time falls through
    // and cards render from live state. Without this the Dashboard
    // stays locked on "Failed to load status" forever after a transient
    // startup 500, even though /api/events is streaming fine.
    // (GitHub Codex PR #1 R1.)
    setError(null);
    setState((prev) => {
      if (body.state === "Gone") {
        const next = { ...prev };
        delete next[k];
        return next;
      }
      return { ...prev, [k]: { ...(prev[k] ?? { server: body.server, daemon: body.daemon }), ...body } };
    });
  }, []);

  // SSE handler for bulk-action lifecycle (PR #38: unified pipeline).
  // Backend publishes started → completed|error around every fan-out;
  // we mirror that into local UI state. The outcome flash auto-clears
  // after 1.5s so the button label snaps back to "Run all"/"Stop all".
  const onBulkAction = useCallback((ev: MessageEvent) => {
    const body = JSON.parse(ev.data) as {
      phase: "started" | "completed" | "error";
      action: BulkAction;
    };
    if (body.phase === "started") {
      // Idempotent confirmation — local click already optimistically
      // set bulkInflight. SSE re-confirms for tray-triggered fan-out
      // (no local click) and event reordering.
      //
      // Codex bot PR #38 P1 (commit ef0f4ea, "Correlate bulk-action
      // terminal events before unlocking UI"): with the shared SSE
      // pipeline, concurrent triggers (Dashboard + tray, or two
      // Dashboards) can interleave events. Don't OVERWRITE the
      // currently-tracked inflight action from a different
      // started — keep the first-tracked action so terminal-match
      // logic below stays sound.
      setBulkInflight((cur) => cur ?? body.action);
      setBulkOutcome(null);
      return;
    }
    // Terminal phase. Only clear inflight if the event's action
    // matches what we're tracking — otherwise this is a sibling
    // operation's terminal and the locally-tracked operation may
    // still be running.
    setBulkInflight((cur) => (cur === body.action ? null : cur));
    setBulkOutcome({
      action: body.action,
      state: body.phase === "error" ? "error" : "done",
    });
    if (bulkResetTimerRef.current) clearTimeout(bulkResetTimerRef.current);
    bulkResetTimerRef.current = setTimeout(() => {
      setBulkOutcome(null);
      bulkResetTimerRef.current = null;
    }, 1500);
  }, []);

  useEventSource("/api/events", {
    "daemon-state": onDelta,
    "bulk-action": onBulkAction,
  });

  // Codex bot PR #38 P1 (round 3): safety-net for dropped SSE events.
  // The Broadcaster is lossy (internal/gui/events.go::Publish drops on
  // full subscriber buffer), so bulkInflight could stay set forever if
  // the terminal event is dropped.
  //
  // Timeout MUST exceed realistic fan-out duration. api.Restart calls
  // killDaemonByPort(5s) + waitForPortFree(3s) per daemon = up to 8s
  // each, and runs sequentially. With 11 daemons × 8s = 88s worst-case
  // legit. Plus serena spawn-up time (~3-6s each). 5min cap is well
  // beyond any realistic fan-out and short enough that a truly stuck
  // UI doesn't trap the user indefinitely. Codex bot review on commit
  // d92aa2c P1 ("Keep bulk-action lock until terminal SSE event").
  useEffect(() => {
    if (bulkInflight === null) return;
    const t = setTimeout(() => {
      setBulkInflight(null);
    }, 300_000);
    return () => clearTimeout(t);
  }, [bulkInflight]);

  // Backend contract:
  //   POST /api/servers/<server>/<action>             — all daemons
  //   POST /api/servers/<server>/<action>?daemon=<n>  — only that daemon
  //
  //   200 { <action>_results: [...] }            — all OK
  //   207 { <action>_results: [...] }            — partial: some Err non-empty
  //   400                                         — empty/repeated ?daemon
  //   404 { error, code: DAEMON_NOT_FOUND }       — ?daemon matched no task
  //   500 { <action>_results: [...], error, code } — orchestration failure
  //
  // The ?daemon scope is REQUIRED for multi-daemon servers (serena ships
  // claude + codex). Without it, clicking Restart on the codex card was
  // restarting claude too — see PR #32 / 2026-04-30 bug report.
  //
  // Re-throws on failure so the Card button state machine can flash
  // "Failed". Caller logs for operator triage.
  async function postServerAction(
    server: string,
    daemon: string | undefined,
    action: "restart" | "stop",
  ) {
    const resultsKey = `${action}_results` as const;
    let url = `/api/servers/${encodeURIComponent(server)}/${action}`;
    if (daemon) url += `?daemon=${encodeURIComponent(daemon)}`;
    try {
      const resp = await fetch(url, { method: "POST" });
      const body = (await resp.json().catch(() => ({}))) as {
        error?: string;
        code?: string;
        [k: string]: unknown;
      };
      if (resp.status === 500) {
        throw new Error(body.error ?? String(resp.status));
      }
      if (resp.status === 207) {
        const rows = (body[resultsKey] as Array<{ task_name: string; error: string }>) ?? [];
        const failed = rows.filter((r) => r.error !== "");
        const summary = failed.map((r) => `${r.task_name}: ${r.error}`).join("; ");
        throw new Error(`partial ${action} failure: ${summary}`);
      }
      if (!resp.ok) {
        throw new Error(body.error ?? String(resp.status));
      }
    } catch (e) {
      console.error(`${action} ${server}/${daemon ?? "*"}: ${(e as Error).message}`);
      throw e;
    }
  }

  const restart = (server: string, daemon: string | undefined) =>
    postServerAction(server, daemon, "restart");
  const stop = (server: string, daemon: string | undefined) =>
    postServerAction(server, daemon, "stop");

  // Bulk actions back the Dashboard header buttons. Backend routes
  // /api/restart-all and /api/stop-all share the same 200/207/500
  // contract as per-server actions, only without ?daemon scoping.
  async function postBulkAction(action: "restart" | "stop") {
    const resultsKey = `${action}_results` as const;
    try {
      const resp = await fetch(`/api/${action}-all`, { method: "POST" });
      const body = (await resp.json().catch(() => ({}))) as {
        error?: string;
        code?: string;
        [k: string]: unknown;
      };
      if (resp.status === 500) {
        throw new Error(body.error ?? String(resp.status));
      }
      if (resp.status === 207) {
        const rows = (body[resultsKey] as Array<{ task_name: string; error: string }>) ?? [];
        const failed = rows.filter((r) => r.error !== "");
        const summary = failed.map((r) => `${r.task_name}: ${r.error}`).join("; ");
        throw new Error(`partial ${action}-all failure: ${summary}`);
      }
      if (!resp.ok) {
        throw new Error(body.error ?? String(resp.status));
      }
    } catch (e) {
      console.error(`${action}-all: ${(e as Error).message}`);
      throw e;
    }
  }
  // Codex bot PR #38 P2 (rejected fetch fallback) + P1 (re-entrant
  // double-click). Optimistic-update pattern handles BOTH:
  //
  //   click → setBulkInflight("restart") IMMEDIATELY → buttons
  //   disable, re-entrant click is gated by the same state check.
  //   SSE "started" arrives ~50ms later; idempotent setter no-ops.
  //   SSE terminal arrives → clear inflight, set outcome.
  //
  // A rejected fetch (network failure, no SSE will arrive) lands in
  // .catch → setLocalErrorFallback restores idle + flashes Failed.
  // For 207/500 fetch also rejects, but SSE error event already set
  // outcome=error so prev ?? wins (idempotent).
  const setLocalErrorFallback = useCallback((action: BulkAction) => {
    setBulkInflight(null);
    // Codex bot PR #38 P2 (commit ff656fe): prev ?? error preserved
    // STALE outcomes from prior actions. If user clicked Run all
    // (success → outcome=done flash), then clicked Stop all within
    // the 1.5s flash window and Stop's POST rejected → prev=done
    // (restart) ?? would suppress the new error → user sees no
    // feedback for the failed Stop click. Fix: only keep prev if
    // it's for the SAME action (idempotent on real partial-fail
    // where SSE 'error' already arrived); otherwise set new error.
    setBulkOutcome((prev) =>
      prev && prev.action === action ? prev : { action, state: "error" },
    );
    if (bulkResetTimerRef.current) clearTimeout(bulkResetTimerRef.current);
    bulkResetTimerRef.current = setTimeout(() => {
      setBulkOutcome(null);
      bulkResetTimerRef.current = null;
    }, 1500);
  }, []);
  function fireBulk(action: BulkAction): Promise<void> {
    if (bulkInflight !== null) return Promise.resolve();
    setBulkInflight(action); // optimistic: locks UI immediately
    return postBulkAction(action).catch(() => setLocalErrorFallback(action));
  }
  const runAll = () => fireBulk("restart");
  const stopAll = () => fireBulk("stop");

  if (error) {
    return (
      <div>
        <h1>Dashboard</h1>
        <p class="error">Failed to load status: {error}</p>
      </div>
    );
  }

  const sorted = Object.values(state).sort((a, b) => keyFor(a).localeCompare(keyFor(b)));

  return (
    <div>
      <header class="dashboard-header">
        <h1>Dashboard</h1>
        <BulkActionsRow
          runAll={runAll}
          stopAll={stopAll}
          disabled={sorted.length === 0}
          inflight={bulkInflight}
          outcome={bulkOutcome}
        />
      </header>
      <div class="cards">
        {sorted.map((d) => (
          <Card
            key={keyFor(d)}
            daemon={d}
            onRestart={() => restart(d.server, d.daemon)}
            onStop={() => stop(d.server, d.daemon)}
            bulkInflight={bulkInflight}
            bulkOutcome={bulkOutcome}
          />
        ))}
      </div>
    </div>
  );
}

// BulkActionsRow is a pure presentational component driven by SSE-fed
// state from DashboardScreen. inflight + outcome both come from the
// "bulk-action" event stream so any trigger source — Dashboard click,
// tray menu, future API client — produces the same visual feedback.
//
// Click handlers fire HTTP POST and return immediately; they do NOT
// set local state. The visual state (Starting…/Started/Failed) flows
// in via SSE. This is the ONE source of truth: backend is canonical,
// UI is a projection.
//
// Mutual exclusion is preserved by the shared inflight prop — while
// one bulk action is in flight, BOTH buttons are disabled. Codex bot
// review on PR #36 P2 (race-prone independent state machines).
function BulkActionsRow(props: {
  runAll: () => Promise<void>;
  stopAll: () => Promise<void>;
  disabled?: boolean;
  inflight: BulkAction | null;
  outcome: BulkOutcome | null;
}) {
  function labelFor(action: BulkAction, idle: string, working: string, done: string): string {
    if (props.inflight === action) return working;
    if (props.outcome && props.outcome.action === action) {
      return props.outcome.state === "done" ? done : "Failed";
    }
    return idle;
  }

  const lockDisabled = props.inflight !== null || props.disabled;
  return (
    <div class="dashboard-bulk-actions">
      <button
        onClick={() => {
          if (!lockDisabled) void props.runAll();
        }}
        disabled={lockDisabled}
        aria-busy={props.inflight === "restart"}
      >
        {labelFor("restart", "Run all", "Starting…", "Started")}
      </button>
      <button
        onClick={() => {
          if (!lockDisabled) void props.stopAll();
        }}
        disabled={lockDisabled}
        class="btn-stop"
        aria-busy={props.inflight === "stop"}
      >
        {labelFor("stop", "Stop all", "Stopping…", "Stopped")}
      </button>
    </div>
  );
}

type ActionState = "idle" | "working" | "done" | "error";

// useActionButton owns one button's state machine: idle → working →
// done|error → snap-back-to-idle after 1.5s. Stable across the timer
// lifecycle (cancels a pending reset before queueing a new one) and
// cleans up on unmount.
function useActionButton(
  run: () => Promise<void>,
): { state: ActionState; click: () => Promise<void> } {
  const [state, setState] = useState<ActionState>("idle");
  const resetTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(
    () => () => {
      if (resetTimerRef.current) clearTimeout(resetTimerRef.current);
    },
    [],
  );

  async function click() {
    if (state !== "idle") return;
    setState("working");
    try {
      await run();
      setState("done");
    } catch {
      setState("error");
    }
    if (resetTimerRef.current) clearTimeout(resetTimerRef.current);
    resetTimerRef.current = setTimeout(() => {
      setState("idle");
      resetTimerRef.current = null;
    }, 1500);
  }

  return { state, click };
}

function Card(props: {
  daemon: DaemonStatus;
  onRestart: () => Promise<void>;
  onStop: () => Promise<void>;
  // Bulk action signals from the parent — when a Run all / Stop all
  // is in flight, every Card's matching button reflects it. By
  // definition Run all === click each per-card Restart, so the
  // affordance must mirror that.
  bulkInflight: BulkAction | null;
  bulkOutcome: BulkOutcome | null;
}) {
  const { daemon: d, onRestart, onStop, bulkInflight, bulkOutcome } = props;
  const restartBtn = useActionButton(onRestart);
  const stopBtn = useActionButton(onStop);

  const cls = d.state === "Running" ? "card ok" : "card down";
  const title = d.daemon && d.daemon !== "default" ? `${d.server} (${d.daemon})` : d.server;

  // Effective per-button state merges local click-driven state with
  // the parent's bulk-action state. If a bulk Restart is in flight,
  // every Card's Restart button is "working" + disabled regardless
  // of whether THIS card's button was the one clicked. Same for
  // Stop. The bulk outcome flash (Restarted/Stopped/Failed) cascades
  // to all cards' matching button after the SSE terminal event.
  const restartEffective: ActionState =
    bulkInflight === "restart"
      ? "working"
      : bulkOutcome && bulkOutcome.action === "restart" && restartBtn.state === "idle"
        ? bulkOutcome.state
        : restartBtn.state;
  const stopEffective: ActionState =
    bulkInflight === "stop"
      ? "working"
      : bulkOutcome && bulkOutcome.action === "stop" && stopBtn.state === "idle"
        ? bulkOutcome.state
        : stopBtn.state;

  // While a bulk action is in flight, every per-card button is
  // disabled — clicking one would race the global fan-out. The
  // existing per-card mutual exclusion (one button locks the other)
  // is preserved.
  const anyBulk = bulkInflight !== null;
  const anyWorking = restartEffective === "working" || stopEffective === "working";
  const restartDisabled =
    anyBulk || restartBtn.state !== "idle" || stopBtn.state === "working";
  const stopDisabled =
    anyBulk ||
    stopBtn.state !== "idle" ||
    restartBtn.state === "working" ||
    d.state !== "Running";

  const restartLabel = {
    idle: "Restart",
    working: "Restarting…",
    done: "Restarted",
    error: "Failed",
  }[restartEffective];
  const stopLabel = {
    idle: "Stop",
    working: "Stopping…",
    done: "Stopped",
    error: "Failed",
  }[stopEffective];

  return (
    <div class={cls}>
      <div class="card-title">{title}</div>
      <div class="card-kv">
        <span>Port</span>
        <span>{d.port ?? "—"}</span>
      </div>
      <div class="card-kv">
        <span>PID</span>
        <span>{d.pid ?? "—"}</span>
      </div>
      <div class="card-kv">
        <span>State</span>
        <span class="state">{d.state}</span>
      </div>
      <div class="card-actions">
        <button onClick={restartBtn.click} disabled={restartDisabled} aria-busy={anyWorking}>
          {restartLabel}
        </button>
        <button
          onClick={stopBtn.click}
          disabled={stopDisabled}
          aria-busy={anyWorking}
          class="btn-stop"
        >
          {stopLabel}
        </button>
      </div>
    </div>
  );
}
