import { useCallback, useEffect, useState } from "preact/hooks";
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

  async function restart(server: string) {
    // Visual feedback is handled inside the button component below — here
    // we only need to surface errors as needed; on success the SSE stream
    // delivers the State: Running update.
    try {
      const resp = await fetch(`/api/servers/${encodeURIComponent(server)}/restart`, { method: "POST" });
      if (!resp.ok) {
        const body = (await resp.json().catch(() => ({}))) as { error?: string };
        throw new Error(body.error ?? String(resp.status));
      }
    } catch (e) {
      console.error(`restart ${server}: ${(e as Error).message}`);
    }
  }

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
      <h1>Dashboard</h1>
      <div class="cards">
        {sorted.map((d) => (
          <Card key={keyFor(d)} daemon={d} onRestart={restart} />
        ))}
      </div>
    </div>
  );
}

function Card(props: { daemon: DaemonStatus; onRestart: (server: string) => Promise<void> }) {
  const { daemon: d, onRestart } = props;
  const [btnState, setBtnState] = useState<"idle" | "working" | "done" | "error">("idle");
  const cls = d.state === "Running" ? "card ok" : "card down";
  const title = d.daemon && d.daemon !== "default" ? `${d.server} (${d.daemon})` : d.server;
  const btnText = {
    idle: "Restart",
    working: "Restarting…",
    done: "Restarted",
    error: "Failed",
  }[btnState];

  async function click() {
    setBtnState("working");
    try {
      await onRestart(d.server);
      setBtnState("done");
    } catch {
      setBtnState("error");
    }
    setTimeout(() => setBtnState("idle"), 1500);
  }

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
        <button onClick={click} disabled={btnState === "working"}>
          {btnText}
        </button>
      </div>
    </div>
  );
}
