// Maintenance — destructive workstation-wide cleanup actions.
// Cleanup-5 per docs/superpowers/specs/2026-05-06-cleanup-buttons-design.md.
//
// Each card follows the same pattern: Preview button (dry-run) lists the
// matched processes inline; the Apply button opens a <ConfirmModal>
// whose Confirm action kills them. Cleanup-6 swapped the prior native
// browser confirm() for the in-app ConfirmModal so destructive actions
// share the same a11y/theme/dirty-guard semantics as SectionBackups
// (clean-now) and SectionAdvancedDiagnostics (force-kill probe). The
// modal also gives us room to surface per-orphan context (basename + PID
// + Server) on the confirm screen so the operator can sanity-check
// before clicking Clean.

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
import { ConfirmModal } from "../ConfirmModal";

type ActionState =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "preview"; orphans?: OrphanProcess[]; watchers?: LogWatcher[]; verdict?: unknown }
  // applied carries the post-kill row list so the table can render
  // per-row kill_err. Codex Cloud bot P2 on PR #131 commit 72757c6
  // (escalates QA F1): apply state previously stored only counts and
  // the table was gated on kind==="preview", so revalidation skips
  // (PID-reuse, exited-PID, snapshot start-time unknown), access
  // denials, and other partial failures were invisible in production.
  //
  // appliedIncludeLive is the includeLive value at the moment the
  // apply request was issued (only meaningful for the log-watchers
  // card). Codex Cloud bot P2 on PR #135 round 2: deriving the
  // skipped/killed label from the LIVE checkbox state would re-label
  // already-applied rows whenever the user toggled the checkbox after
  // apply — making the post-action audit trail inaccurate. Pin to
  // the apply-time flag so the rendered label reflects the request
  // that was actually executed.
  | { kind: "applied"; killed?: number; skipped?: number; result?: unknown; stopResults?: StopResult[]; orphans?: OrphanProcess[]; watchers?: LogWatcher[]; appliedIncludeLive?: boolean }
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
  // Cleanup-6: replaced native confirm() with ConfirmModal. Open state
  // is tracked separately from action state so a Cancel keeps the
  // preview rows visible (the modal closes without mutating state).
  const [confirmOpen, setConfirmOpen] = useState(false);

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
      // apply=false → dry-run / preview path on the server. Wire-shape
      // change per Codex bot P2 on PR #131 (kosyak
      // 2026-05-07-destructive-endpoint-with-unsafe-zero-value-default.md):
      // safe zero-value polarity.
      const r = await cleanupOrphans(false);
      setState({ kind: "preview", orphans: r.orphans });
    } catch (e) {
      setState({ kind: "error", error: friendlyError(e) });
    }
  }

  function openConfirm() {
    if (state.kind !== "preview" || !state.orphans) return;
    if (state.orphans.length === 0) return;
    setConfirmOpen(true);
  }

  async function apply() {
    if (state.kind !== "preview" || !state.orphans) return;
    const n = state.orphans.length;
    if (n === 0) return;
    setConfirmOpen(false);
    setState({ kind: "loading" });
    try {
      // apply=true → explicit destructive opt-in.
      const r = await cleanupOrphans(true);
      // Retain the row list so the post-apply table can render per-row
      // kill_err. Bot P2 on commit 72757c6 / kosyak
      // 2026-05-07-startime-zero-fail-open-bypasses-pid-reuse-guard.md
      // (mitigation visibility): without the rows the operator sees
      // only "Done. Killed N, skipped M." with no actionable diagnostic
      // for the very revalidation skips the kill loop now produces.
      setState({ kind: "applied", killed: r.killed, skipped: r.skipped, orphans: r.orphans });
    } catch (e) {
      setState({ kind: "error", error: friendlyError(e) });
    }
  }

  // Build the confirmation body once based on the current preview rows.
  // Cleanup-6: the orphans table renders cmdline_display (basename); the
  // confirm body uses the same redacted field — full cmdlines are NEVER
  // exposed to the GUI surface (workspace paths / argv-borne secrets).
  const previewOrphans = state.kind === "preview" ? (state.orphans ?? []) : [];
  const confirmCount = previewOrphans.length;
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
          <button
            onClick={openConfirm}
            disabled={false}
            data-testid="orphan-mcp-clean-button"
          >
            Clean ({state.orphans.length})
          </button>
        )}
      </div>
      <CardResult state={state} />
      {(state.kind === "preview" || state.kind === "applied") && state.orphans && (
        <OrphansTable orphans={state.orphans} />
      )}
      <ConfirmModal
        open={confirmOpen}
        title={`Clean ${confirmCount} orphan MCP process${confirmCount === 1 ? "" : "es"}?`}
        body={
          <ul class="maintenance-confirm-list" data-testid="orphan-mcp-confirm-list">
            {previewOrphans.map((o) => (
              <li key={o.pid}>
                <code>{cmdlineDisplayOf(o)}</code>
                {" "}PID {o.pid}
                {o.server ? <> — server <code>{o.server}</code></> : null}
              </li>
            ))}
          </ul>
        }
        confirmLabel="Clean"
        danger
        onConfirm={apply}
        onCancel={() => setConfirmOpen(false)}
      />
    </div>
  );
}

// Cleanup-6: the orphans table renders the redacted cmdline_display
// field (basename only — no path, no args). For backward compatibility
// with in-flight test fixtures that still set the deprecated `cmdline`,
// fall back to it when cmdline_display is missing. Production wire
// always carries cmdline_display.
function cmdlineDisplayOf(o: OrphanProcess): string {
  if (o.cmdline_display && o.cmdline_display.length > 0) return o.cmdline_display;
  if (o.cmdline && o.cmdline.length > 0) return o.cmdline;
  return "<unknown>";
}

function OrphansTable({ orphans }: { orphans: OrphanProcess[] }): preact.JSX.Element {
  if (orphans.length === 0) {
    return <p class="maintenance-empty">No orphan processes found.</p>;
  }
  // Codex Cloud bot P2 on PR #131 commit 72757c6: per-row kill_err
  // was invisible in apply state, hiding revalidation skips
  // (PID-reuse, exited-PID, snapshot start-time unknown), access
  // denials, and other partial failures. Render a Result column
  // whenever any row carries a non-empty kill_err.
  const showResult = orphans.some((o) => !!o.kill_err);
  return (
    <table class="maintenance-table">
      <thead>
        <tr>
          <th>PID</th>
          <th>Server</th>
          <th>Age</th>
          <th>RAM (MB)</th>
          <th>Cmd</th>
          {showResult && <th>Result</th>}
        </tr>
      </thead>
      <tbody>
        {orphans.map((o) => (
          <tr key={o.pid}>
            <td>{o.pid}</td>
            <td>{o.server}</td>
            <td>{Math.round(o.age_sec)}s</td>
            <td>{Math.round(o.ram_bytes / (1024 * 1024))}</td>
            {/* Cleanup-6: render the redacted basename via cmdline_display.
                Full cmdlines often carry workspace paths, username
                segments, and possible API-keys-in-args; the wire now
                hides the raw `cmdline` field (`json:"-"` server-side). */}
            <td class="maintenance-cmd">{cmdlineDisplayOf(o)}</td>
            {showResult && (
              <td class={o.kill_err ? "maintenance-error" : ""}>
                {o.kill_err || "killed"}
              </td>
            )}
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
  // Cleanup-6: replaced native confirm() with ConfirmModal, mirroring
  // the orphan-MCP card.
  const [confirmOpen, setConfirmOpen] = useState(false);

  async function preview() {
    setState({ kind: "loading" });
    try {
      // apply=false → preview / dry-run. Same wire-shape change as the
      // orphan-MCP card per Codex bot P2 / kosyak
      // 2026-05-07-destructive-endpoint-with-unsafe-zero-value-default.md.
      const r = await cleanupLogWatchers(false, includeLive);
      setState({ kind: "preview", watchers: r.watchers });
    } catch (e) {
      setState({ kind: "error", error: asError(e) });
    }
  }

  function openConfirm() {
    if (state.kind !== "preview" || !state.watchers) return;
    const targets = includeLive
      ? state.watchers
      : state.watchers.filter((w) => !w.parent_alive);
    if (targets.length === 0) return;
    setConfirmOpen(true);
  }

  async function apply() {
    if (state.kind !== "preview" || !state.watchers) return;
    const targets = includeLive
      ? state.watchers
      : state.watchers.filter((w) => !w.parent_alive);
    const n = targets.length;
    if (n === 0) return;
    setConfirmOpen(false);
    // Capture the apply-time includeLive lever so the post-apply
    // label rendering is independent of subsequent checkbox toggles.
    // Codex Cloud bot P2 on PR #135 round 2 — see ActionState comment.
    const appliedIncludeLive = includeLive;
    setState({ kind: "loading" });
    try {
      // apply=true → explicit destructive opt-in.
      const r = await cleanupLogWatchers(true, appliedIncludeLive);
      // Retain the row list so the post-apply table renders per-row
      // kill_err. Same fix as CardOrphanMcpServers above (Codex Cloud
      // bot P2 on commit 72757c6).
      setState({ kind: "applied", killed: r.killed, skipped: r.skipped, watchers: r.watchers, appliedIncludeLive });
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
          <button
            onClick={openConfirm}
            disabled={killCount === 0}
            title={noKillReason}
            data-testid="orphan-log-watchers-clean-button"
          >
            Clean ({killCount})
          </button>
        )}
      </div>
      <CardResult state={state} />
      {(state.kind === "preview" || state.kind === "applied") && state.watchers && (
        <WatchersTable
          watchers={state.watchers}
          includeLive={
            // Pin the label-rendering lever to the apply-time
            // includeLive value so post-apply audit rows don't
            // re-label when the user toggles the checkbox afterwards
            // (Codex bot P2 on PR #135 round 2). The preview path
            // stays live — preview rows are recomputed by the
            // backend on every Preview click anyway.
            state.kind === "applied"
              ? state.appliedIncludeLive ?? includeLive
              : includeLive
          }
        />
      )}
      <ConfirmModal
        open={confirmOpen}
        title={`Clean ${killCount} orphan log watcher${killCount === 1 ? "" : "s"}?`}
        body={
          <>
            <p>
              {includeLive
                ? "Includes live-parent processes — those are usually CURRENT active agent sessions and will be killed."
                : "Only dead-parent watchers will be killed."}
            </p>
            <ul class="maintenance-confirm-list" data-testid="orphan-log-watchers-confirm-list">
              {(includeLive ? watchers : watchers.filter((w) => !w.parent_alive)).map((w) => (
                <li key={w.pid}>
                  <code>{w.name}</code>{" "}PID {w.pid}
                  {" "}— parent {w.parent_pid}{w.parent_alive ? " (alive)" : " (dead)"}
                </li>
              ))}
            </ul>
          </>
        }
        confirmLabel="Clean"
        danger
        onConfirm={apply}
        onCancel={() => setConfirmOpen(false)}
      />
    </div>
  );
}

function WatchersTable(
  { watchers, includeLive }: { watchers: LogWatcher[]; includeLive: boolean },
): preact.JSX.Element {
  if (watchers.length === 0) {
    return <p class="maintenance-empty">No orphan watchers found.</p>;
  }
  // Same Result column rule as OrphansTable — visible only when at
  // least one row has a non-empty kill_err.
  const showResult = watchers.some((w) => !!w.kill_err);
  return (
    <table class="maintenance-table">
      <thead>
        <tr>
          <th>PID</th>
          <th>Parent</th>
          <th>Name</th>
          <th>Age</th>
          <th>Cmd</th>
          {showResult && <th>Result</th>}
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
            {showResult && (
              <td class={w.kill_err ? "maintenance-error" : ""}>
                {w.kill_err || (w.parent_alive && !includeLive ? "skipped (live parent)" : "killed")}
              </td>
            )}
          </tr>
        ))}
      </tbody>
    </table>
  );
}

// --- Card 3: Stuck mcphub instance recovery --------------------------------

function CardForceKillInstance(): preact.JSX.Element {
  const [state, setState] = useState<ActionState>({ kind: "idle" });
  // Cleanup-6: replaced native confirm() with ConfirmModal.
  const [confirmOpen, setConfirmOpen] = useState(false);

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
    setConfirmOpen(false);
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
        <button
          onClick={() => setConfirmOpen(true)}
          disabled={state.kind === "loading"}
          data-testid="force-kill-button"
        >
          Force-kill
        </button>
      </div>
      <CardResult state={state} />
      {state.kind === "preview" && state.verdict !== undefined && (
        <pre class="maintenance-pre">
          {JSON.stringify(state.verdict, null, 2)}
        </pre>
      )}
      <ConfirmModal
        open={confirmOpen}
        title="Force-kill the single-instance lock holder?"
        body={
          <p>
            The 3-part identity gate (executable basename, argv[1]=gui,
            start-time precedes pidport mtime) will refuse if the
            recorded PID has been recycled to an unrelated process.
          </p>
        }
        confirmLabel="Force-kill"
        danger
        onConfirm={apply}
        onCancel={() => setConfirmOpen(false)}
      />
    </div>
  );
}

// --- Card 4: Stop all daemons ---------------------------------------------

function CardStopAllDaemons(): preact.JSX.Element {
  const [state, setState] = useState<ActionState>({ kind: "idle" });
  // Cleanup-6: replaced native confirm() with ConfirmModal.
  const [confirmOpen, setConfirmOpen] = useState(false);

  async function apply() {
    setConfirmOpen(false);
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
        <button
          onClick={() => setConfirmOpen(true)}
          disabled={state.kind === "loading"}
          data-testid="stop-all-button"
        >
          Stop all
        </button>
      </div>
      <CardResult state={state} />
      {state.kind === "applied" && state.stopResults && (
        <StopResultsTable results={state.stopResults} />
      )}
      <ConfirmModal
        open={confirmOpen}
        title="Stop ALL running mcphub daemons?"
        body={
          <p>
            Each daemon's subprocess tree will be tree-killed; clients
            reconnect on next request.
          </p>
        }
        confirmLabel="Stop all"
        danger
        onConfirm={apply}
        onCancel={() => setConfirmOpen(false)}
      />
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
