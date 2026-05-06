// Maintenance — destructive workstation-wide cleanup actions.
// Cleanup-5 per docs/superpowers/specs/2026-05-06-cleanup-buttons-design.md.
//
// Each card follows the same pattern: Preview button (dry-run) lists the
// matched processes inline; the Apply button (with browser-confirm gate)
// kills them. The browser confirm() is a streamlined stand-in for a
// proper ConfirmModal — TODO: upgrade to typed-confirmation modal in a
// follow-up commit, mirroring the secrets D5 escalation flow already
// used in SectionBackups.

import { useState } from "preact/hooks";
import {
  cleanupOrphans,
  cleanupLogWatchers,
  forceKillProbe,
  forceKillApply,
  stopAllDaemons,
  type OrphanProcess,
  type LogWatcher,
  type StopResult,
} from "../../lib/settings-api";

type ActionState =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "preview"; orphans?: OrphanProcess[]; watchers?: LogWatcher[]; verdict?: unknown }
  | { kind: "applied"; killed?: number; skipped?: number; result?: unknown; stopResults?: StopResult[] }
  | { kind: "error"; error: string };

function asError(e: unknown): string {
  if (e instanceof Error) return e.message;
  return String(e);
}

export function SectionMaintenance(): preact.JSX.Element {
  return (
    <section data-section="maintenance" class="settings-section">
      <h2>Maintenance</h2>
      <p class="settings-section-help">
        Reclaim leftover processes from dead client sessions and stuck
        instances. All actions default to a preview before any kill;
        actual termination is gated by an explicit confirmation.
      </p>

      <CardOrphanMcpServers />
      <CardOrphanLogWatchers />
      <CardForceKillInstance />
      <CardStopAllDaemons />
    </section>
  );
}

// --- Card 1: Orphan MCP server processes -----------------------------------

function CardOrphanMcpServers(): preact.JSX.Element {
  const [state, setState] = useState<ActionState>({ kind: "idle" });

  // Codex Cloud bot P2 on PR #131 (commit 99938e7): non-Windows backend
  // returns 501 with `not_supported_on_this_os`. Detect that body and
  // render a clearer "Windows only" message rather than the generic
  // "Error: not_supported_on_this_os" string.
  function friendlyError(e: unknown): string {
    const raw = asError(e);
    if (raw.includes("not_supported_on_this_os")) {
      return "Not supported on this OS yet — Windows only. POSIX support is on the roadmap.";
    }
    return raw;
  }

  async function preview() {
    setState({ kind: "loading" });
    try {
      const r = await cleanupOrphans(true);
      setState({ kind: "preview", orphans: r.orphans });
    } catch (e) {
      setState({ kind: "error", error: friendlyError(e) });
    }
  }

  async function apply() {
    if (state.kind !== "preview" || !state.orphans) return;
    const n = state.orphans.length;
    if (n === 0) return;
    if (!confirm(`Kill ${n} orphan MCP server process${n === 1 ? "" : "es"}?`)) return;
    setState({ kind: "loading" });
    try {
      const r = await cleanupOrphans(false);
      setState({ kind: "applied", killed: r.killed, skipped: r.skipped });
    } catch (e) {
      setState({ kind: "error", error: friendlyError(e) });
    }
  }

  return (
    <div data-card="orphan-mcp-servers" class="maintenance-card">
      <h3>Orphan MCP server processes</h3>
      <p>
        Reclaim uvx/npx/python children left behind by dead client
        sessions (IDE restart, Ctrl-C didn't propagate). Wraps
        <code> mcphub cleanup --confirm</code>.
      </p>
      <div class="maintenance-card-actions">
        <button onClick={preview} disabled={state.kind === "loading"}>
          Preview
        </button>
        {state.kind === "preview" && state.orphans && state.orphans.length > 0 && (
          <button onClick={apply} disabled={false}>
            Clean ({state.orphans.length})
          </button>
        )}
      </div>
      <CardResult state={state} />
      {state.kind === "preview" && state.orphans && (
        <OrphansTable orphans={state.orphans} />
      )}
    </div>
  );
}

function OrphansTable({ orphans }: { orphans: OrphanProcess[] }): preact.JSX.Element {
  if (orphans.length === 0) {
    return <p class="maintenance-empty">No orphan processes found.</p>;
  }
  return (
    <table class="maintenance-table">
      <thead>
        <tr>
          <th>PID</th>
          <th>Server</th>
          <th>Age</th>
          <th>RAM (MB)</th>
          <th>Cmd</th>
        </tr>
      </thead>
      <tbody>
        {orphans.map((o) => (
          <tr key={o.pid}>
            <td>{o.pid}</td>
            <td>{o.server}</td>
            <td>{Math.round(o.age_sec)}s</td>
            <td>{Math.round(o.ram_bytes / (1024 * 1024))}</td>
            <td class="maintenance-cmd">{o.cmdline}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

// --- Card 2: Orphan log watchers (tail/grep/bash) --------------------------

function CardOrphanLogWatchers(): preact.JSX.Element {
  const [state, setState] = useState<ActionState>({ kind: "idle" });
  const [includeLive, setIncludeLive] = useState(false);

  async function preview() {
    setState({ kind: "loading" });
    try {
      const r = await cleanupLogWatchers(true, includeLive);
      setState({ kind: "preview", watchers: r.watchers });
    } catch (e) {
      setState({ kind: "error", error: asError(e) });
    }
  }

  async function apply() {
    if (state.kind !== "preview" || !state.watchers) return;
    const targets = includeLive
      ? state.watchers
      : state.watchers.filter((w) => !w.parent_alive);
    const n = targets.length;
    if (n === 0) return;
    if (!confirm(`Kill ${n} orphan log watcher process${n === 1 ? "" : "es"}?${includeLive ? " (Includes live-parent processes — those are usually CURRENT active agent sessions.)" : ""}`)) return;
    setState({ kind: "loading" });
    try {
      const r = await cleanupLogWatchers(false, includeLive);
      setState({ kind: "applied", killed: r.killed, skipped: r.skipped });
    } catch (e) {
      setState({ kind: "error", error: asError(e) });
    }
  }

  // Codex Cloud bot P3 on PR #131 (commit c0fe229): when all preview
  // rows have parent_alive=true and IncludeLive=false, the action
  // count is 0 but the button rendered as "Clean (0)" with no
  // explanation — clicking returned early silently. Compute the
  // kill-target count once and use it for BOTH the label and the
  // disabled/title state so the UI reads "Clean (0)" disabled with a
  // tooltip explaining the IncludeLive checkbox lever, never
  // a clickable-but-no-op button.
  const watchers = state.kind === "preview" ? (state.watchers ?? []) : [];
  const killCount = includeLive
    ? watchers.length
    : watchers.filter((w) => !w.parent_alive).length;
  const noKillReason =
    state.kind === "preview" && watchers.length > 0 && killCount === 0
      ? `All ${watchers.length} watcher${watchers.length === 1 ? "" : "s"} belong to active sessions (live parent). Toggle "Include live-parent processes" above to clean them anyway.`
      : "";

  return (
    <div data-card="orphan-log-watchers" class="maintenance-card">
      <h3>Orphan log watchers (tail / grep / bash)</h3>
      <p>
        Reclaim <code>tail.exe</code> + <code>grep.exe</code> pipelines
        left behind by agent shell-snapshot launchers (Claude Code,
        codex CLI). See <code>scripts/cleanup-orphan-watchers.ps1</code>.
      </p>
      <label class="maintenance-checkbox">
        <input
          type="checkbox"
          checked={includeLive}
          onChange={(e) => setIncludeLive((e.target as HTMLInputElement).checked)}
        />
        Include live-parent processes (CURRENT active sessions — kills them too)
      </label>
      <div class="maintenance-card-actions">
        <button onClick={preview} disabled={state.kind === "loading"}>
          Preview
        </button>
        {state.kind === "preview" && watchers.length > 0 && (
          <button onClick={apply} disabled={killCount === 0} title={noKillReason}>
            Clean ({killCount})
          </button>
        )}
      </div>
      <CardResult state={state} />
      {state.kind === "preview" && state.watchers && (
        <WatchersTable watchers={state.watchers} />
      )}
    </div>
  );
}

function WatchersTable({ watchers }: { watchers: LogWatcher[] }): preact.JSX.Element {
  if (watchers.length === 0) {
    return <p class="maintenance-empty">No orphan watchers found.</p>;
  }
  return (
    <table class="maintenance-table">
      <thead>
        <tr>
          <th>PID</th>
          <th>Parent</th>
          <th>Name</th>
          <th>Age</th>
          <th>Cmd</th>
        </tr>
      </thead>
      <tbody>
        {watchers.map((w) => (
          <tr key={w.pid}>
            <td>{w.pid}</td>
            <td>{w.parent_pid}{w.parent_alive ? " (alive)" : " (dead)"}</td>
            <td>{w.name}</td>
            <td>{w.age_sec > 0 ? `${Math.round(w.age_sec / 60)}m` : "?"}</td>
            <td class="maintenance-cmd">{w.cmdline}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

// --- Card 3: Stuck mcphub instance recovery --------------------------------

function CardForceKillInstance(): preact.JSX.Element {
  const [state, setState] = useState<ActionState>({ kind: "idle" });

  async function diagnose() {
    setState({ kind: "loading" });
    try {
      const v = await forceKillProbe();
      setState({ kind: "preview", verdict: v });
    } catch (e) {
      setState({ kind: "error", error: asError(e) });
    }
  }

  async function apply() {
    if (!confirm("Force-kill the recorded single-instance lock holder? The 3-part identity gate (executable basename, argv[1]=gui, start-time precedes pidport mtime) will refuse if the recorded PID has been recycled to an unrelated process.")) return;
    setState({ kind: "loading" });
    try {
      const v = await forceKillApply();
      setState({ kind: "applied", result: v });
    } catch (e) {
      setState({ kind: "error", error: asError(e) });
    }
  }

  return (
    <div data-card="force-kill-instance" class="maintenance-card">
      <h3>Stuck mcphub instance</h3>
      <p>
        Force-kill another mcphub gui that holds the single-instance
        lock. Equivalent to <code>mcphub gui --force --kill</code>.
        macOS not yet supported.
      </p>
      <div class="maintenance-card-actions">
        <button onClick={diagnose} disabled={state.kind === "loading"}>
          Diagnose
        </button>
        <button onClick={apply} disabled={state.kind === "loading"}>
          Force-kill
        </button>
      </div>
      <CardResult state={state} />
      {state.kind === "preview" && state.verdict !== undefined && (
        <pre class="maintenance-pre">
          {JSON.stringify(state.verdict, null, 2)}
        </pre>
      )}
    </div>
  );
}

// --- Card 4: Stop all daemons ---------------------------------------------

function CardStopAllDaemons(): preact.JSX.Element {
  const [state, setState] = useState<ActionState>({ kind: "idle" });

  async function apply() {
    if (!confirm("Stop ALL running mcphub daemons? Each daemon's subprocess tree will be tree-killed; clients reconnect on next request.")) return;
    setState({ kind: "loading" });
    try {
      const r = await stopAllDaemons();
      // Codex Cloud bot P1+P2 chain on PR #131 / kosyaks
      // stop-all-card-ignored-multi-status-response.md +
      // third-time-shipped-without-checking-json-tags.md:
      // /api/stop-all returns HTTP 207 + per-daemon stop_results
      // where each row is api.RestartResult with JSON tags
      // `task_name` and `error` (NOT `name`/`err` as the prior fix
      // assumed). Read those exact field names to detect failures.
      const results = r?.stop_results ?? [];
      const failed = results.filter((sr) => sr.error && sr.error !== "");
      setState({
        kind: "applied",
        stopResults: results,
        killed: results.length - failed.length,
        skipped: failed.length,
      });
    } catch (e) {
      setState({ kind: "error", error: asError(e) });
    }
  }

  return (
    <div data-card="stop-all-daemons" class="maintenance-card">
      <h3>Stop all daemons</h3>
      <p>
        Stop every running daemon. Use after multi-daemon zombie
        scenarios; pair with the orphan-MCP cleanup above for a full
        reset. Wraps the existing <code>/api/stop-all</code> endpoint.
      </p>
      <div class="maintenance-card-actions">
        <button onClick={apply} disabled={state.kind === "loading"}>
          Stop all
        </button>
      </div>
      <CardResult state={state} />
      {state.kind === "applied" && state.stopResults && (
        <StopResultsTable results={state.stopResults} />
      )}
    </div>
  );
}

// StopResultsTable lists the daemons /api/stop-all returned, marking
// any with non-empty err as failed. Empty results array (no daemons
// running) renders an explicit "no daemons" line rather than a blank
// table — Codex Cloud bot P2 review feedback.
function StopResultsTable({ results }: { results: StopResult[] }): preact.JSX.Element {
  if (results.length === 0) {
    return <p class="maintenance-empty">No daemons were running.</p>;
  }
  return (
    <table class="maintenance-table">
      <thead>
        <tr>
          <th>Daemon</th>
          <th>Status</th>
        </tr>
      </thead>
      <tbody>
        {results.map((sr) => (
          <tr key={sr.task_name}>
            <td>{sr.task_name}</td>
            <td class={sr.error ? "maintenance-error" : ""}>
              {sr.error ? `Failed: ${sr.error}` : "Stopped"}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

// --- Shared result renderer -----------------------------------------------

function CardResult({ state }: { state: ActionState }): preact.JSX.Element | null {
  switch (state.kind) {
    case "loading":
      return <p class="maintenance-status">Working…</p>;
    case "applied": {
      // Stop-All has its own per-daemon table below; render a banner
      // that distinguishes full success from partial failure (HTTP 207).
      // Codex Cloud bot P2 on PR #131: "Done." alone hid 207 partial
      // failures; the failed count must surface in the summary too.
      if (state.stopResults !== undefined) {
        const total = state.stopResults.length;
        const failed = state.skipped ?? 0;
        if (total === 0) {
          return <p class="maintenance-status">Done. No daemons were running.</p>;
        }
        if (failed === 0) {
          return <p class="maintenance-status">Stopped all {total} daemon{total === 1 ? "" : "s"}.</p>;
        }
        return (
          <p class="maintenance-status maintenance-error">
            Partial: {total - failed} stopped, {failed} failed.
          </p>
        );
      }
      if (state.killed !== undefined || state.skipped !== undefined) {
        return (
          <p class="maintenance-status">
            Done. Killed {state.killed ?? 0}, skipped {state.skipped ?? 0}.
          </p>
        );
      }
      return <p class="maintenance-status">Done.</p>;
    }
    case "error":
      return <p class="maintenance-status maintenance-error">Error: {state.error}</p>;
    default:
      return null;
  }
}
