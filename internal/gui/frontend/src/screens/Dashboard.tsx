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
      setBulkInflight(body.action);
      setBulkOutcome(null);
      return;
    }
    setBulkInflight(null);
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

  // Codex bot PR #38 P1 (round 2): the SSE Broadcaster is explicitly
  // lossy — internal/gui/events.go::Publish drops events when a
  // subscriber's buffer is full. If the terminal "completed"/"error"
  // event is dropped, bulkInflight stays set forever and BOTH bulk
  // buttons are disabled with no recovery path. The full fan-out
  // never takes more than a few seconds in practice (restart-all of
  // ~10 daemons completes in <5s on a warm machine); a 30s cap is
  // 6× that headroom and still much shorter than any user's patience.
  useEffect(() => {
    if (bulkInflight === null) return;
    const t = setTimeout(() => {
      setBulkInflight(null);
      // No outcome flash — leave the user a clean idle state to
      // retry from rather than a stale "Failed" that wasn't really
      // a failure (the action may have succeeded; we just lost the
      // SSE event).
    }, 30000);
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
  // Local-fallback wrappers around postBulkAction. Codex bot review on
  // PR #38 P2: a rejected fetch (network-down, connection refused,
  // DNS) means the backend never received the request, so no SSE
  // event will EVER arrive — without a fallback the button stays in
  // idle and the click looks ignored. We catch the rejection and
  // set bulkOutcome to error UNLESS the SSE pipeline already ran
  // (200/207/500 cases all publish SSE before the response returns,
  // so prev is non-null). prev ?? error preserves SSE-driven outcome
  // when both arrive — idempotent for the "error" case where SSE
  // also says error.
  const setLocalErrorFallback = useCallback((action: BulkAction) => {
    setBulkInflight(null);
    setBulkOutcome((prev) => prev ?? { action, state: "error" });
    if (bulkResetTimerRef.current) clearTimeout(bulkResetTimerRef.current);
    bulkResetTimerRef.current = setTimeout(() => {
      setBulkOutcome(null);
      bulkResetTimerRef.current = null;
    }, 1500);
  }, []);
  const runAll = () =>
    postBulkAction("restart").catch(() => setLocalErrorFallback("restart"));
  const stopAll = () =>
    postBulkAction("stop").catch(() => setLocalErrorFallback("stop"));

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
}) {
  const { daemon: d, onRestart, onStop } = props;
  const restartBtn = useActionButton(onRestart);
  const stopBtn = useActionButton(onStop);

  const cls = d.state === "Running" ? "card ok" : "card down";
  const title = d.daemon && d.daemon !== "default" ? `${d.server} (${d.daemon})` : d.server;

  // Guard against concurrent same-daemon ops: while one button is in
  // flight, the other is disabled. Stop is additionally disabled when
  // the daemon is already not running — there is nothing to stop.
  const anyWorking = restartBtn.state === "working" || stopBtn.state === "working";
  const restartDisabled = restartBtn.state !== "idle" || stopBtn.state === "working";
  const stopDisabled =
    stopBtn.state !== "idle" || restartBtn.state === "working" || d.state !== "Running";

  const restartLabel = {
    idle: "Restart",
    working: "Restarting…",
    done: "Restarted",
    error: "Failed",
  }[restartBtn.state];
  const stopLabel = {
    idle: "Stop",
    working: "Stopping…",
    done: "Stopped",
    error: "Failed",
  }[stopBtn.state];

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
