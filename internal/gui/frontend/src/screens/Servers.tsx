import { useEffect, useState } from "preact/hooks";
import { fetchOrThrow } from "../api";
import { useEventSource } from "../hooks/useEventSource";
import { collectServers } from "../lib/routing";
import { aggregateStatus, stateShape } from "../lib/status";
import type { DaemonStatus, ScanResult, ServerRow, Routing } from "../types";

const CLIENTS = ["claude-code", "codex-cli", "gemini-cli", "antigravity"] as const;

// Per-cell dirty tracking with direction preserved. Outer key: server name.
// Inner map: client → Direction.
//
// Direction is captured at toggle time because the cell's initialChecked
// (scan state, authoritative) is the only honest source of truth for
// "which endpoint should Apply call for this cell" — by the time
// applyChanges runs, routing may have reloaded. Storing Direction in the
// dirty map keeps endpoint selection stable across reloads.
//
// Prune invariant (see B1 memo §4 D4): on toggle-back (user re-flips a
// dirty cell to its initial state), delete the client entry AND delete
// the server entry if the inner map becomes empty. With the invariant
// enforced at every update, `dirty.size === 0` remains a correct
// "nothing pending" predicate without a deep-empty scan.
type Direction = "migrate" | "demigrate";
type DirtyMap = Map<string, Map<string, Direction>>;

// Per-entry outcome from one applyChanges run. Drives the success-prune /
// retain-failed-or-gated semantic in B1 memo §4 D6:
//   - "succeeded"  : POST fired, got 2xx → prune from dirty
//   - "failed"     : POST fired, got non-2xx → retain (user retries)
//   - "gated"      : POST never fired because phase-1 demigrate on the
//                    same client failed; the §4 D4 per-client gate
//                    removed this client from the phase-2 migrate batch.
//                    Retain (user retries; entry will fire once the
//                    blocking demigrate succeeds).
type Outcome = "succeeded" | "failed" | "gated";
type OutcomeMap = Map<string, Map<string, Outcome>>;

export function ServersScreen() {
  const [servers, setServers] = useState<ServerRow[] | null>(null);
  const [statusByServer, setStatusByServer] = useState<Record<string, { state: string; port: number | null }>>({});
  const [error, setError] = useState<string | null>(null);
  const [dirty, setDirty] = useState<DirtyMap>(new Map());
  // PR #22 retry-queue UX fix: persist last-Apply outcomes so toggleCell
  // can detect cells with a pending failed/gated retry and preserve
  // their dirty entry instead of pruning on toggle-back. Without this,
  // unticking a via-hub cell + Apply-fails + re-ticking would silently
  // drop the failed retry from the queue (Apply gauge would gray out
  // even though backend state never changed). The user lost their
  // ability to re-Apply without leaving and reloading the page.
  const [outcomes, setOutcomes] = useState<OutcomeMap>(new Map());
  const [applyMsg, setApplyMsg] = useState<string>("");
  const [applying, setApplying] = useState<boolean>(false);
  const [reloadToken, setReloadToken] = useState<number>(0);

  // Tray "Rescan client configs" — backend publishes clients-rescan,
  // every open Servers tab re-fetches. Bumping reloadToken composes
  // with the existing useEffect dep so the same path serves both
  // user-triggered Apply and tray-triggered rescan.
  useEventSource("/api/events", {
    "clients-rescan": () => setReloadToken((n) => n + 1),
  });

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const [scan, status] = await Promise.all([
          fetchOrThrow<ScanResult>("/api/scan", "object"),
          fetchOrThrow<DaemonStatus[]>("/api/status", "array"),
        ]);
        if (cancelled) return;
        if (scan.entries != null && !Array.isArray(scan.entries)) {
          setError("/api/scan returned malformed entries");
          return;
        }
        setServers(collectServers(scan));
        const agg = aggregateStatus(status);
        const flat: Record<string, { state: string; port: number | null }> = {};
        for (const [name, a] of Object.entries(agg)) {
          flat[name] = { state: a.state, port: a.port };
        }
        setStatusByServer(flat);
        setError(null);
      } catch (err) {
        if (!cancelled) setError((err as Error).message);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [reloadToken]);

  function toggleCell(server: string, client: string, nextChecked: boolean, initialChecked: boolean) {
    setDirty((prev) => {
      const next = new Map(prev);
      if (nextChecked !== initialChecked) {
        // Dirty: capture direction from initialChecked (authoritative scan
        // state). A cell that started `via-hub` (initialChecked=true) and
        // is now unchecked flips to "demigrate"; a direct cell (false) that
        // just got checked flips to "migrate".
        const direction: Direction = initialChecked ? "demigrate" : "migrate";
        let clients = next.get(server);
        if (!clients) {
          clients = new Map();
          next.set(server, clients);
        }
        clients.set(client, direction);
      } else {
        // Toggle-back: prune invariant (DirtyMap doc).
        //
        // Earlier draft (Codex PR #22 r3 P1 fix) tried to preserve
        // the dirty entry when there was a retained-failure outcome,
        // hoping to keep the retry queue alive across an accidental
        // re-tick. That was WRONG: the visual checkbox is already at
        // `initialChecked`, but the preserved dirty entry still
        // carries the OLD direction (e.g., "demigrate"). A subsequent
        // Apply then fires /api/demigrate against a cell that the
        // user has visually returned to via-hub — UI and intent
        // diverge.
        //
        // Retry affordance is preserved through the outcome map's
        // visual indicator (`.matrix-cell-retry-pending` red outline
        // — see CellView). When the user re-toggles the cell, the
        // dirty entry is recreated with a direction that matches the
        // current visual state, and Apply re-runs the failed action
        // honestly. The outline keeps the failure context visible
        // across the toggle-back so the user knows there was a prior
        // problem on this cell even if Apply is currently inactive.
        const clients = next.get(server);
        if (clients) {
          clients.delete(client);
          if (clients.size === 0) next.delete(server);
        }
      }
      return next;
    });
  }

  async function applyChanges() {
    if (dirty.size === 0) return;
    setApplying(true);
    setApplyMsg(`Applying…`);

    // Per-cell POST granularity (memo §4 D2). Each (server, client, direction)
    // cell fires its OWN /api/migrate or /api/demigrate POST with a single-
    // element clients array. Batching multiple clients into one POST would
    // be collapsed by the handlers into a single 500 on any row failure,
    // corrupting per-cell outcome tracking — a batch containing one failed
    // row and one succeeded row would mark BOTH failed, leaving the actually-
    // successful row dirty and replaying it on retry (which reads the now-
    // polluted backup and hits the R5 sentinel bug). Per-cell POSTs keep
    // outcome 1:1 with cell state. [Codex plan-R4 P1 on this plan.]
    type Cell = { server: string; client: string };
    const demigrateCells: Cell[] = [];
    const migrateCells: Cell[] = [];
    for (const [server, clientMap] of dirty.entries()) {
      for (const [client, direction] of clientMap.entries()) {
        if (direction === "demigrate") demigrateCells.push({ server, client });
        else migrateCells.push({ server, client });
      }
    }

    // Per-entry outcomes — seed every entry as "gated" (will upgrade to
    // "succeeded" or "failed" once its POST fires; gated only remains for
    // cells skipped by the phase-2 per-client gate).
    const outcomes: OutcomeMap = new Map();
    for (const [server, clientMap] of dirty.entries()) {
      const row: Map<string, Outcome> = new Map();
      for (const [client] of clientMap.entries()) row.set(client, "gated");
      outcomes.set(server, row);
    }

    const failed: string[] = [];
    // Clients whose phase-1 demigrate failed. Phase 2 skips every migrate
    // cell targeting such a client (per-client gate, §4 D4). Gated cells
    // stay "gated" in outcomes and retain in dirty for retry.
    const failedDemigrateClients = new Set<string>();

    // PHASE 1 — demigrate (one POST per cell).
    for (const cell of demigrateCells) {
      try {
        const resp = await fetch("/api/demigrate", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ servers: [cell.server], clients: [cell.client] }),
        });
        if (resp.ok || resp.status === 204) {
          outcomes.get(cell.server)!.set(cell.client, "succeeded");
        } else {
          const body = (await resp.json().catch(() => ({}))) as { error?: string };
          failed.push(`${cell.server}/demigrate/${cell.client}: ${body.error ?? resp.status}`);
          outcomes.get(cell.server)!.set(cell.client, "failed");
          failedDemigrateClients.add(cell.client);
        }
      } catch (e) {
        failed.push(`${cell.server}/demigrate/${cell.client}: ${(e as Error).message ?? "unknown"}`);
        outcomes.get(cell.server)!.set(cell.client, "failed");
        failedDemigrateClients.add(cell.client);
      }
    }

    // PHASE 2 — migrate (one POST per cell, with per-client gate).
    for (const cell of migrateCells) {
      if (failedDemigrateClients.has(cell.client)) {
        // Gated: a phase-1 demigrate on this client failed. Do NOT fire
        // the migrate — it would write a polluted post-migrate backup
        // that the user's retry of the failed demigrate would then
        // misread. Outcome stays "gated"; entry retains in dirty.
        continue;
      }
      try {
        const resp = await fetch("/api/migrate", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ servers: [cell.server], clients: [cell.client] }),
        });
        if (resp.ok || resp.status === 204) {
          outcomes.get(cell.server)!.set(cell.client, "succeeded");
        } else {
          const body = (await resp.json().catch(() => ({}))) as { error?: string };
          failed.push(`${cell.server}/migrate/${cell.client}: ${body.error ?? resp.status}`);
          outcomes.get(cell.server)!.set(cell.client, "failed");
        }
      } catch (e) {
        failed.push(`${cell.server}/migrate/${cell.client}: ${(e as Error).message ?? "unknown"}`);
        outcomes.get(cell.server)!.set(cell.client, "failed");
      }
    }

    // Prune "succeeded" outcomes from dirty; retain "failed" and "gated".
    // §4 D6 rationale: successful entries would silently replay on retry
    // and re-read the now-polluted latest backup (R5/R6/R7). Gated entries
    // represent unfulfilled user intent that must retry (R10).
    setDirty((prev) => {
      const next = new Map(prev);
      for (const [server, outcomeRow] of outcomes.entries()) {
        const clientMap = next.get(server);
        if (!clientMap) continue;
        for (const [client, outcome] of outcomeRow.entries()) {
          if (outcome === "succeeded") clientMap.delete(client);
        }
        if (clientMap.size === 0) next.delete(server);
      }
      return next;
    });
    // PR #22 retry-queue fix: hoist outcomes to state so toggleCell
    // can detect retained-failure cells. Without this, outcomes
    // (local var) is GC'd at end of applyChanges and the toggle-back
    // path can't tell "user dismissing" from "user dismissing a
    // pending retry".
    setOutcomes(outcomes);

    // Always reload, regardless of failure count. §4 D6 rationale: the
    // Checkbox useEffect syncs local `checked` from `initialChecked`
    // derived from server.routing; without a reload, successful demigrate
    // cells stay with stale "via-hub" initialChecked and the next toggle
    // fires the wrong direction. Reloading unconditionally keeps every
    // cell's baseline honest. Failed cells retain their local-flipped
    // state via a no-op useEffect sync (their initialChecked is unchanged
    // because backend rejected the POST).
    setReloadToken((x) => x + 1);

    if (failed.length === 0) {
      setApplyMsg("Applied. Refreshing…");
    } else {
      setApplyMsg(`Failed: ${failed.join("; ")}`);
    }
    setApplying(false);
  }

  if (error) {
    return (
      <div>
        <h1>Servers</h1>
        <p class="error">Failed to load: {error}</p>
      </div>
    );
  }

  if (!servers) {
    return (
      <div>
        <h1>Servers</h1>
        <p>Loading…</p>
      </div>
    );
  }

  const applyDisabled = applying || dirty.size === 0;

  return (
    <div>
      <h1>Servers</h1>
      <div id="servers-toolbar">
        <button onClick={applyChanges} disabled={applyDisabled}>
          Apply changes
        </button>
        <span style="margin-left:12px" class={applyMsg.startsWith("Failed") ? "error" : ""}>
          {applyMsg}
        </span>
      </div>
      <table class="servers-matrix">
        <thead>
          <tr>
            <th>Server</th>
            {CLIENTS.map((c) => (
              <th key={c}>{c}</th>
            ))}
            <th>Port</th>
            <th>State</th>
          </tr>
        </thead>
        <tbody>
          {servers.map((server) => (
            <ServerRowView
              key={server.name}
              server={server}
              status={statusByServer[server.name]}
              outcomes={outcomes.get(server.name)}
              onToggle={toggleCell}
            />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function ServerRowView(props: {
  server: ServerRow;
  status?: { state: string; port: number | null };
  outcomes?: Map<string, Outcome>;
  onToggle: (server: string, client: string, nextChecked: boolean, initialChecked: boolean) => void;
}) {
  const { server, status, outcomes, onToggle } = props;
  return (
    <tr>
      <td>
        <a
          href={`#/edit-server?name=${encodeURIComponent(server.name)}`}
          data-action="edit-server"
        >
          {server.name}
        </a>
      </td>
      {CLIENTS.map((client) => (
        <CellView
          key={client}
          server={server}
          client={client}
          lastOutcome={outcomes?.get(client)}
          onToggle={onToggle}
        />
      ))}
      <td>{status?.port ?? "—"}</td>
      <td class={status ? `state-cell ${status.state === "Running" ? "state-ok" : "state-down"}` : ""}>
        {status ? (
          <>
            <span class="state-shape" aria-hidden="true">{stateShape(status.state)}</span>{" "}
            {status.state}
          </>
        ) : (
          "—"
        )}
      </td>
    </tr>
  );
}

function CellView(props: {
  server: ServerRow;
  client: string;
  lastOutcome?: Outcome;
  onToggle: (server: string, client: string, nextChecked: boolean, initialChecked: boolean) => void;
}) {
  const { server, client, lastOutcome, onToggle } = props;
  // Treat undefined routing as "not-installed" — perClientRouting only
  // populates keys present in /api/scan's client_presence map.
  const routing: Routing = server.routing[client] ?? "not-installed";
  const initialChecked = routing === "via-hub";
  const [checked, setChecked] = useState(initialChecked);
  // Keep local `checked` in sync with the authoritative initialChecked
  // when routing actually changes (a scan reload moving a cell from
  // direct→via-hub, an external config change, etc.). Deps `[initialChecked]`
  // means unrelated parent re-renders do not stomp an in-progress user edit.
  useEffect(() => {
    setChecked(initialChecked);
  }, [initialChecked]);
  // Disable when cell is meaningless:
  //  - "unsupported"   : this client cannot route this server via the hub
  //  - "not-installed" : this client is not installed on this machine
  // "via-hub" is now INTERACTIVE (B1): uncheck + Apply posts
  // /api/demigrate for this (server, client) pair. See B1 memo §4 D5.
  const disabled = routing === "unsupported" || routing === "not-installed";
  let title: string | undefined;
  if (routing === "via-hub") {
    title = `Currently routed through the hub. Uncheck and Apply to roll this binding back to the original ${client} config.`;
  } else if (routing === "not-installed") {
    title = `${client} is not installed on this machine.`;
  } else if (routing === "unsupported") {
    title = `${client} cannot route this server through the hub (e.g., per-session servers).`;
  }
  // PR #22 retry-queue fix: cell with a retained failure from the
  // last applyChanges renders a red outline so the user sees the
  // pending retry. The cell's checkbox visual still reflects the
  // last user toggle; the outline is the "click Apply again to
  // retry" affordance.
  const retryPending = lastOutcome === "failed" || lastOutcome === "gated";
  const cellTitle = retryPending
    ? `${title ?? ""}\n\nLast Apply for this cell ${lastOutcome === "gated" ? "was gated by another failure on this client" : "failed"}; click Apply changes to retry.`.trim()
    : title;
  return (
    <td class={retryPending ? "matrix-cell-retry-pending" : ""} data-retry-pending={retryPending ? "true" : undefined}>
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        title={cellTitle}
        onChange={(ev) => {
          const next = (ev.currentTarget as HTMLInputElement).checked;
          setChecked(next);
          onToggle(server.name, client, next, initialChecked);
        }}
      />
    </td>
  );
}
