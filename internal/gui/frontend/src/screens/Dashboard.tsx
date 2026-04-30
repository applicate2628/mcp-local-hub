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

export function DashboardScreen() {
  const [state, setState] = useState<Record<string, DaemonStatus>>({});
  const [error, setError] = useState<string | null>(null);

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

  useEventSource("/api/events", { "daemon-state": onDelta });

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
  const runAll = () => postBulkAction("restart");
  const stopAll = () => postBulkAction("stop");

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
        <div class="dashboard-bulk-actions">
          <BulkActionButton onClick={runAll} idleLabel="Run all" workingLabel="Starting…" doneLabel="Started" disabled={sorted.length === 0} />
          <BulkActionButton onClick={stopAll} idleLabel="Stop all" workingLabel="Stopping…" doneLabel="Stopped" disabled={sorted.length === 0} variant="danger" />
        </div>
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

// BulkActionButton reuses the same idle → working → done|error → idle
// state machine as per-card useActionButton, but as a standalone
// component because the bulk buttons live outside any Card. Keeping
// the state machine identical ensures consistent UX: the same 1.5s
// flash of "Started" / "Stopped" / "Failed" before snapping back.
function BulkActionButton(props: {
  onClick: () => Promise<void>;
  idleLabel: string;
  workingLabel: string;
  doneLabel: string;
  disabled?: boolean;
  variant?: "danger";
}) {
  const btn = useActionButton(props.onClick);
  const label = {
    idle: props.idleLabel,
    working: props.workingLabel,
    done: props.doneLabel,
    error: "Failed",
  }[btn.state];
  const cls = props.variant === "danger" ? "btn-stop" : "";
  return (
    <button
      onClick={btn.click}
      disabled={props.disabled || btn.state !== "idle"}
      class={cls}
      aria-busy={btn.state === "working"}
    >
      {label}
    </button>
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
