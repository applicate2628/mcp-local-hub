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

import { useRef, useState } from "preact/hooks";
import {
  cleanupOrphans,
  cleanupLogWatchers,
  cleanupAggressive,
  CLEANUP_AGGRESSIVE_TOKEN_MISMATCH,
  forceKillProbe,
  forceKillApply,
  stopAllDaemons,
  type OrphanProcess,
  type LogWatcher,
  type StopResult,
  type AggressiveCleanupScope,
  type AggressiveCleanupResponse,
} from "../../lib/settings-api";
import { ConfirmModal } from "../ConfirmModal";
import { InfoTip } from "../InfoTip";
import { SettingsCard } from "./SettingsCard";

type ActionState =
  | { kind: "idle" }
  | { kind: "loading" }
  // `token` (aggressive card only) is the confirm token bound to the
  // previewed candidate set (bot #373 R2 Finding 1). apply() replays it so
  // the backend can refuse (409) if the set drifted since Preview.
  // `notice` (aggressive card only) carries the "candidate set changed —
  // re-Preview" message when a 409 token-mismatch re-renders the FRESH
  // candidate set as a new preview the operator must explicitly re-confirm.
  | { kind: "preview"; orphans?: OrphanProcess[]; watchers?: LogWatcher[]; verdict?: unknown; token?: string; notice?: string }
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
    <SettingsCard
      section="maintenance"
      title="Maintenance"
      infoTipLabel="About this section"
      infoTip="Reclaim leftover processes from dead client sessions and stuck instances. All actions default to a preview before any kill; actual termination is gated by an explicit confirmation."
      subtitle="Preview, then confirm — reclaim leftover processes and recover stuck instances."
    >
      <div class="flex flex-col gap-4">
        <CardOrphanMcpServers />
        <CardAggressiveCleanup />
        <CardOrphanLogWatchers />
        <CardForceKillInstance />
        <CardStopAllDaemons />
      </div>
    </SettingsCard>
  );
}

// Shared card chrome. The `.maintenance-card` class name is retained as a
// structural hook; visual styling rides Tailwind utilities (the class itself
// carries no CSS). data-card is preserved so the tests' card-scoping queries
// keep matching.
const CARD_CLASS =
  "maintenance-card rounded-lg border border-app-border/70 bg-app-card/40 p-4";
const CARD_TITLE_CLASS = "m-0 text-sm font-semibold text-app-text";
const CARD_DESC_CLASS = "m-0 mt-1 text-sm leading-relaxed text-app-muted";
const CARD_ACTIONS_CLASS = "maintenance-card-actions mt-3 flex flex-wrap gap-2";

// --- Card 1: Orphan MCP server processes -----------------------------------

function CardOrphanMcpServers(): preact.JSX.Element {
  const [state, setState] = useState<ActionState>({ kind: "idle" });
  // Cleanup-6: replaced native confirm() with ConfirmModal. Open state
  // is tracked separately from action state so a Cancel keeps the
  // preview rows visible (the modal closes without mutating state).
  const [confirmOpen, setConfirmOpen] = useState(false);
  // Codex deep-sec PR #143 round 4 finding R1: capture the orphan list
  // at modal-open time and render the modal from THIS snapshot, not
  // live `state.orphans`. Pre-fix, the modal title/body derived from
  // live state — if the user clicked Preview again (or any concurrent
  // path mutated state) while the modal was open, the modal data went
  // stale and Confirm would apply against fresh, unconfirmed data.
  // Post-fix, the snapshot is the only source of truth for the open
  // modal; mutations to `state.orphans` after open do NOT bleed into
  // the visible confirm body.
  const [orphansSnapshot, setOrphansSnapshot] =
    useState<OrphanProcess[] | null>(null);

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
    // Defense-in-depth: if a Preview is requested while a modal is
    // open, close the modal and clear the snapshot. The user must
    // re-confirm against the new preview. This guarantees Confirm
    // always applies to a snapshot the user just acknowledged
    // (codex deep-sec finding R1).
    if (confirmOpen) {
      setConfirmOpen(false);
      setOrphansSnapshot(null);
    }
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
    // Snapshot the orphan list AT the moment the modal opens. The modal
    // renders exclusively from this snapshot, so post-open state
    // mutations cannot change what the user is confirming (codex
    // deep-sec finding R1). Shallow-copy so a future setState on the
    // same row reference can't bleed in either.
    setOrphansSnapshot([...state.orphans]);
    setConfirmOpen(true);
  }

  function cancelConfirm() {
    setConfirmOpen(false);
    setOrphansSnapshot(null);
  }

  async function apply() {
    // Apply uses the SNAPSHOT, not live state. If the snapshot is
    // somehow null (cancelled mid-flight, defensive guard) we abort.
    if (!orphansSnapshot || orphansSnapshot.length === 0) {
      cancelConfirm();
      return;
    }
    setConfirmOpen(false);
    setState({ kind: "loading" });
    try {
      // apply=true → explicit destructive opt-in. Backend re-resolves
      // the orphan set from a fresh process snapshot; the per-row
      // identity gate (PID-reuse, start-time precedes snapshot, etc.)
      // is the authoritative kill filter. The frontend snapshot is
      // about UI consent, not about pinning the kill set.
      const r = await cleanupOrphans(true);
      // Retain the row list so the post-apply table can render per-row
      // kill_err. Bot P2 on commit 72757c6 / kosyak
      // 2026-05-07-startime-zero-fail-open-bypasses-pid-reuse-guard.md
      // (mitigation visibility): without the rows the operator sees
      // only "Done. Killed N, skipped M." with no actionable diagnostic
      // for the very revalidation skips the kill loop now produces.
      setState({ kind: "applied", killed: r.killed, skipped: r.skipped, orphans: r.orphans });
      // Clear the snapshot after a successful apply so a stale list
      // can't be used by any subsequent path.
      setOrphansSnapshot(null);
    } catch (e) {
      setState({ kind: "error", error: friendlyError(e) });
      setOrphansSnapshot(null);
    }
  }

  // Codex deep-sec finding R1: the modal renders from the SNAPSHOT
  // captured at modal-open, not from live state.orphans. While the
  // modal is closed snapshot is null and the body collapses to an
  // empty list (the dialog is hidden anyway).
  const confirmOrphans = orphansSnapshot ?? [];
  const confirmCount = confirmOrphans.length;
  return (
    <div data-card="orphan-mcp-servers" class={CARD_CLASS}>
      <div class="flex items-center gap-1.5">
        <h3 class={CARD_TITLE_CLASS}>Orphan MCP server processes</h3>
        <InfoTip text="Reclaim uvx/npx/python MCP-server children left behind by dead client sessions (an IDE restart, a Ctrl-C that didn't propagate). Preview lists the orphans, then Clean kills them. Wraps mcphub cleanup --confirm." />
      </div>
      <p class={CARD_DESC_CLASS}>
        Reclaim uvx/npx/python children left behind by dead client
        sessions (IDE restart, Ctrl-C didn't propagate). Wraps
        <code> mcphub cleanup --confirm</code>.
      </p>
      <div class={CARD_ACTIONS_CLASS}>
        <button onClick={preview} disabled={state.kind === "loading"}>
          Preview
        </button>
        {state.kind === "preview" && state.orphans && state.orphans.length > 0 && (
          <button
            onClick={openConfirm}
            disabled={false}
            class="btn-danger"
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
          <ul class="maintenance-confirm-list m-0 list-none space-y-1 p-0 text-sm" data-testid="orphan-mcp-confirm-list">
            {confirmOrphans.map((o) => (
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
        onCancel={cancelConfirm}
      />
    </div>
  );
}

// Cleanup-6: the orphans table renders the redacted cmdline_display
// field (basename only — no path, no args).
//
// Codex deep-sec PR #143 round 4 finding A2: the deprecated `cmdline`
// fallback was removed. If a future backend response (or a test fixture)
// omits cmdline_display, render `<unknown>` rather than re-exposing the
// raw command line — full cmdlines carry workspace paths, username
// segments, and possible API keys/tokens in argv. Production wire ALWAYS
// carries cmdline_display; the fallback could only matter in a regression
// scenario, and a regression scenario is exactly when re-leaking the raw
// field would be most damaging. Fail closed: opt-out beats opt-in here.
function cmdlineDisplayOf(o: OrphanProcess): string {
  if (o.cmdline_display && o.cmdline_display.length > 0) return o.cmdline_display;
  return "<unknown>";
}

function OrphansTable({ orphans }: { orphans: OrphanProcess[] }): preact.JSX.Element {
  if (orphans.length === 0) {
    return <p class="maintenance-empty mt-3 text-sm text-app-muted">No orphan processes found.</p>;
  }
  // Codex Cloud bot P2 on PR #131 commit 72757c6: per-row kill_err
  // was invisible in apply state, hiding revalidation skips
  // (PID-reuse, exited-PID, snapshot start-time unknown), access
  // denials, and other partial failures. Render a Result column
  // whenever any row carries a non-empty kill_err.
  const showResult = orphans.some((o) => !!o.kill_err);
  return (
    <table class="maintenance-table mt-3 w-full border-collapse text-sm">
      <thead>
        <tr class="text-left text-app-muted">
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">PID</th>
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Server</th>
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Age</th>
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">RAM (MB)</th>
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Cmd</th>
          {showResult && <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Result</th>}
        </tr>
      </thead>
      <tbody>
        {orphans.map((o) => (
          <tr key={o.pid} class="border-b border-app-border/50">
            <td class="py-1.5 pr-4">{o.pid}</td>
            <td class="py-1.5 pr-4">{o.server}</td>
            <td class="py-1.5 pr-4">{Math.round(o.age_sec)}s</td>
            <td class="py-1.5 pr-4">{Math.round(o.ram_bytes / (1024 * 1024))}</td>
            {/* Cleanup-6: render the redacted basename via cmdline_display.
                Full cmdlines often carry workspace paths, username
                segments, and possible API-keys-in-args; the wire now
                hides the raw `cmdline` field (`json:"-"` server-side). */}
            <td class="maintenance-cmd py-1.5 pr-4 font-mono text-xs">{cmdlineDisplayOf(o)}</td>
            {showResult && (
              <td class={`py-1.5 pr-4 ${o.kill_err ? "maintenance-error text-app-danger" : ""}`}>
                {o.kill_err || "killed"}
              </td>
            )}
          </tr>
        ))}
      </tbody>
    </table>
  );
}

// --- Card 1b: Aggressive cleanup (operator-confirmed override) -------------

// The recognized client launchers the backend allowlists for a --client
// aggressive sweep (knownClientLauncherBasenames in
// internal/api/cleanup.go). Kept in product-name order for the dropdown.
const AGGRESSIVE_CLIENTS = [
  "claude",
  "codex",
  "gemini",
  "qwen",
  "cursor",
  "code",
  "cascade",
  "antigravity",
] as const;

// The default-EXCLUDED dangerous classes the operator can opt back into
// the kill set (aggressiveDenyClasses in internal/api/cleanup.go):
// operator terminals + Playwright browser sessions.
const AGGRESSIVE_DANGER_CLASSES = [
  "cmd",
  "conhost",
  "pwsh",
  "powershell",
  "chrome",
] as const;

// CardAggressiveCleanup is the GUI surface over `mcphub cleanup
// aggressive` — the operator-confirmed override that kills the
// live-rooted MCP-stdio fan-out the default safe sweep (CardOrphanMcpServers
// above) CORRECTLY refuses to touch: a single live client (e.g. codex)
// spawns N subagents that each leak their own stdio MCP children which
// never get reaped on subagent finish.
//
// It clones the orphan card's preview→ConfirmModal→apply flow but adds:
//   - a SCOPE selector (exactly one of By-client / By-root-PID) — Preview
//     is disabled until a valid scope is chosen (no implicit "all
//     live-rooted" mode, per spec H.1);
//   - optional DANGER-class checkboxes (cmd/conhost/pwsh/powershell/chrome)
//     that the ConfirmModal surfaces prominently before the kill.
//
// Unlike the CLI it uses the same-origin + CSRF + ConfirmModal guard
// (per the design memo at cleanup.go:12-18), NOT the recompute-token
// protocol.
function CardAggressiveCleanup(): preact.JSX.Element {
  const [state, setState] = useState<ActionState>({ kind: "idle" });
  const [confirmOpen, setConfirmOpen] = useState(false);
  // Scope: "client" with a chosen launcher, or "root-pid" with a number.
  const [scopeKind, setScopeKind] = useState<"client" | "root-pid">("client");
  const [client, setClient] = useState<string>(AGGRESSIVE_CLIENTS[0]);
  const [rootPidText, setRootPidText] = useState<string>("");
  // Dangerous classes the operator opted back in.
  const [includeClasses, setIncludeClasses] = useState<string[]>([]);
  // Snapshot the candidate list AND the resolved scope/include-class set
  // at modal-open time so Confirm applies against exactly what the user
  // acknowledged (same R1 discipline as CardOrphanMcpServers). The scope
  // selector + class checkboxes stay live while the modal is open, so
  // reading them at apply-time would be a TOCTOU: the operator could widen
  // the scope under the open modal and kill a different process set than
  // the previewed/acknowledged one (security + sonnet review, converged).
  const [orphansSnapshot, setOrphansSnapshot] = useState<OrphanProcess[] | null>(null);
  const [scopeSnapshot, setScopeSnapshot] = useState<AggressiveCleanupScope | null>(null);
  const [classesSnapshot, setClassesSnapshot] = useState<string[]>([]);
  // tokenSnapshot is the confirm token captured at openConfirm — the token
  // that was bound to the acknowledged candidate set. apply() replays THIS,
  // not a live token, so a scope/preview change can't slip a different
  // token under the open modal (bot #373 R2 Finding 1).
  const [tokenSnapshot, setTokenSnapshot] = useState<string | null>(null);
  // previewGen monotonically increments on every preview() launch and on
  // every invalidatePreview(). A preview captures its gen BEFORE the await
  // and discards its own response if the gen advanced while it was in
  // flight — so a scope change (or a newer Preview) during a pending
  // request cannot publish stale candidates for the OLD scope (bot #373 R2
  // Finding 2).
  const previewGen = useRef(0);

  function friendlyError(e: unknown): string {
    const raw = asError(e);
    if (raw.includes("not_supported_on_this_os")) {
      return "Not supported on this OS yet — Windows only. POSIX support is on the roadmap.";
    }
    return raw;
  }

  // The current scope as a typed value, or null when invalid (no
  // client chosen, or a non-positive / unparseable root PID). Preview is
  // gated on this being non-null.
  function currentScope(): AggressiveCleanupScope | null {
    if (scopeKind === "client") {
      return client ? { kind: "client", client } : null;
    }
    // Strict integer: parseInt would TRUNCATE "123.9"→123 / "1e3"→1 and
    // preview/kill a DIFFERENT pid than the operator typed (bot #373 P2).
    // Require a pure-digit string, then a finite positive integer.
    const trimmed = rootPidText.trim();
    if (!/^\d+$/.test(trimmed)) return null;
    const pid = Number(trimmed);
    if (!Number.isInteger(pid) || pid <= 0) return null;
    return { kind: "root-pid", rootPid: pid };
  }

  // invalidatePreview clears any standing preview/confirm + snapshots. A scope
  // or danger-class change makes the previewed candidate list stale, so the
  // operator must re-Preview — the confirm list then always matches the scope
  // that apply() will actually send (bot #373 P1: preview codex, switch to
  // claude, confirm → would have killed claude's tree, never previewed).
  //
  // Bumping previewGen here ALSO discards any preview request still in
  // flight (bot #373 R2 Finding 2): a request that resolves after the
  // invalidation captured an older gen and bails instead of publishing
  // candidates for the now-stale scope.
  function invalidatePreview() {
    previewGen.current++;
    setState((s) => (s.kind === "preview" ? { kind: "idle" } : s));
    setConfirmOpen(false);
    setOrphansSnapshot(null);
    setScopeSnapshot(null);
    setClassesSnapshot([]);
    setTokenSnapshot(null);
  }
  function changeScopeKind(k: "client" | "root-pid") {
    setScopeKind(k);
    invalidatePreview();
  }
  function changeClient(v: string) {
    setClient(v);
    invalidatePreview();
  }
  function changeRootPidText(v: string) {
    setRootPidText(v);
    invalidatePreview();
  }

  function toggleClass(cls: string, on: boolean) {
    setIncludeClasses((prev) =>
      on ? [...prev, cls] : prev.filter((c) => c !== cls),
    );
    invalidatePreview();
  }

  async function preview() {
    const scope = currentScope();
    if (!scope) return;
    if (confirmOpen) {
      setConfirmOpen(false);
      setOrphansSnapshot(null);
      setScopeSnapshot(null);
      setClassesSnapshot([]);
      setTokenSnapshot(null);
    }
    // Capture the generation BEFORE the await. If the scope/classes change
    // (invalidatePreview) or a newer Preview launches while this request is
    // in flight, previewGen advances and this response is discarded so it
    // can't publish stale candidates for the old scope (bot #373 R2 F2).
    const gen = ++previewGen.current;
    setState({ kind: "loading" });
    try {
      const r = await cleanupAggressive(false, scope, includeClasses);
      if (gen !== previewGen.current) return; // stale — superseded in flight
      setState({ kind: "preview", orphans: r.orphans, token: r.token });
    } catch (e) {
      if (gen !== previewGen.current) return; // stale — discard the error too
      setState({ kind: "error", error: friendlyError(e) });
    }
  }

  function openConfirm() {
    if (state.kind !== "preview" || !state.orphans) return;
    if (state.orphans.length === 0) return;
    const scope = currentScope();
    if (!scope) return;
    setOrphansSnapshot([...state.orphans]);
    setScopeSnapshot(scope);
    setClassesSnapshot([...includeClasses]);
    // Pin the preview token alongside the candidate snapshot so apply()
    // replays exactly the token bound to the acknowledged set (bot #373 R2
    // Finding 1). May be undefined for a legacy/empty backend response;
    // apply still sends it (the backend then 400s an empty token).
    setTokenSnapshot(state.token ?? null);
    setConfirmOpen(true);
  }

  function cancelConfirm() {
    setConfirmOpen(false);
    setOrphansSnapshot(null);
    setScopeSnapshot(null);
    setClassesSnapshot([]);
    setTokenSnapshot(null);
  }

  async function apply() {
    // Apply against the SNAPSHOT taken at openConfirm — NOT live scope/classes
    // — so a scope change under the open modal can't redirect the kill.
    if (!orphansSnapshot || orphansSnapshot.length === 0 || !scopeSnapshot) {
      cancelConfirm();
      return;
    }
    setConfirmOpen(false);
    // Capture the scope/token snapshots locally before we clear them so the
    // 409-retry branch (which re-publishes a fresh preview) starts clean.
    const scope = scopeSnapshot;
    const token = tokenSnapshot ?? undefined;
    setState({ kind: "loading" });
    try {
      const r = await cleanupAggressive(true, scope, classesSnapshot, token);
      setState({ kind: "applied", killed: r.killed, skipped: r.skipped, orphans: r.orphans });
      setOrphansSnapshot(null);
      setScopeSnapshot(null);
      setClassesSnapshot([]);
      setTokenSnapshot(null);
    } catch (e) {
      // Token mismatch (409): the candidate set drifted between Preview and
      // Confirm. The backend refused the kill and returned the FRESH
      // candidate set + a new token. Re-render that fresh set as a NEW
      // preview the operator must explicitly re-Preview/re-confirm — nothing
      // was killed (bot #373 R2 Finding 1). previewGen is bumped so this
      // re-publish wins over any older in-flight preview.
      const err = e as { status?: number; body?: AggressiveCleanupResponse };
      if (err?.status === 409 && err.body?.code === CLEANUP_AGGRESSIVE_TOKEN_MISMATCH) {
        previewGen.current++;
        setState({
          kind: "preview",
          orphans: err.body.orphans ?? [],
          token: err.body.token,
          notice:
            "The candidate set changed since Preview — nothing was killed. Review the refreshed list and click Preview again to re-confirm.",
        });
      } else {
        setState({ kind: "error", error: friendlyError(e) });
      }
      setOrphansSnapshot(null);
      setScopeSnapshot(null);
      setClassesSnapshot([]);
      setTokenSnapshot(null);
    }
  }

  const scope = currentScope();
  const confirmOrphans = orphansSnapshot ?? [];
  const confirmCount = confirmOrphans.length;
  // Modal label comes from the SNAPSHOT (what was acknowledged), not live scope.
  const scopeLabel =
    scopeSnapshot == null
      ? ""
      : scopeSnapshot.kind === "client"
        ? `client ${scopeSnapshot.client}`
        : `root PID ${scopeSnapshot.rootPid}`;

  return (
    <div data-card="aggressive-cleanup" class={CARD_CLASS}>
      <div class="flex items-center gap-1.5">
        <h3 class={CARD_TITLE_CLASS}>Aggressive cleanup (live-rooted)</h3>
        <InfoTip text="Operator-confirmed override: kill the live-rooted MCP-stdio children the default safe sweep refuses to touch — the per-subagent fan-out where one live client (codex, claude, …) spawns N subagents that each leak their own stdio MCP children. REQUIRES exactly one scope (a client launcher OR a root PID). Dangerous classes (terminals + Playwright chrome) are excluded unless you opt them back in. mcphub.exe daemons are always spared. Wraps mcphub cleanup aggressive." />
      </div>
      <p class={CARD_DESC_CLASS}>
        Kill the live-rooted MCP-stdio fan-out the default sweep above
        spares (a live client's leaked subagent children). Pick exactly
        one scope. Wraps <code>mcphub cleanup aggressive</code>.
      </p>

      {/* Scope selector — exactly one of By-client / By-root-PID. */}
      <fieldset class="maintenance-scope mt-3 m-0 border-0 p-0">
        <legend class="sr-only">Aggressive cleanup scope</legend>
        <label class="flex items-center gap-2 text-sm text-app-text">
          <input
            type="radio"
            name="aggressive-scope"
            class="h-4 w-4 accent-app-accent"
            checked={scopeKind === "client"}
            data-testid="aggressive-scope-client"
            onChange={() => changeScopeKind("client")}
          />
          By client
          <select
            class="field-ctl"
            value={client}
            disabled={scopeKind !== "client"}
            data-testid="aggressive-client-select"
            aria-label="Client launcher"
            onChange={(e) => changeClient((e.target as HTMLSelectElement).value)}
          >
            {AGGRESSIVE_CLIENTS.map((c) => (
              <option key={c} value={c}>{c}</option>
            ))}
          </select>
        </label>
        <label class="mt-2 flex items-center gap-2 text-sm text-app-text">
          <input
            type="radio"
            name="aggressive-scope"
            class="h-4 w-4 accent-app-accent"
            checked={scopeKind === "root-pid"}
            data-testid="aggressive-scope-root-pid"
            onChange={() => changeScopeKind("root-pid")}
          />
          By root PID
          <input
            type="number"
            min="1"
            class="field-ctl w-32"
            value={rootPidText}
            placeholder="e.g. 12345"
            disabled={scopeKind !== "root-pid"}
            data-testid="aggressive-root-pid-input"
            aria-label="Root PID"
            onInput={(e) => changeRootPidText((e.target as HTMLInputElement).value)}
          />
        </label>
      </fieldset>

      {/* Optional danger-class opt-ins. */}
      <div class="maintenance-danger-classes mt-3">
        <p class="m-0 mb-1 text-xs font-semibold uppercase tracking-wide text-app-muted">
          Also kill (dangerous — off by default)
        </p>
        <div class="flex flex-wrap gap-x-4 gap-y-1">
          {AGGRESSIVE_DANGER_CLASSES.map((cls) => (
            <label key={cls} class="flex items-center gap-1.5 text-sm text-app-text">
              <input
                type="checkbox"
                class="h-4 w-4 accent-app-accent"
                checked={includeClasses.includes(cls)}
                data-testid={`aggressive-class-${cls}`}
                onChange={(e) => toggleClass(cls, (e.target as HTMLInputElement).checked)}
              />
              {cls}
            </label>
          ))}
        </div>
        {includeClasses.length > 0 && (
          <p class="maintenance-danger-warning m-0 mt-1 text-xs text-app-danger">
            Including {includeClasses.join(", ")} may terminate operator
            terminals or live Playwright sessions.
          </p>
        )}
      </div>

      <div class={CARD_ACTIONS_CLASS}>
        <button onClick={preview} disabled={state.kind === "loading" || scope == null} data-testid="aggressive-preview-button">
          Preview
        </button>
        {state.kind === "preview" && state.orphans && state.orphans.length > 0 && (
          <button
            onClick={openConfirm}
            class="btn-danger"
            data-testid="aggressive-clean-button"
          >
            Clean ({state.orphans.length})
          </button>
        )}
      </div>
      <CardResult state={state} />
      {state.kind === "preview" && state.notice && (
        <p class="maintenance-status maintenance-error mt-3 text-sm text-app-danger" data-testid="aggressive-token-mismatch-notice">
          {state.notice}
        </p>
      )}
      {(state.kind === "preview" || state.kind === "applied") && state.orphans && (
        <AggressiveTable orphans={state.orphans} />
      )}
      <ConfirmModal
        open={confirmOpen}
        title={`Aggressively kill ${confirmCount} live-rooted process${confirmCount === 1 ? "" : "es"}?`}
        body={
          <>
            <p class="m-0 mb-2 text-sm">
              Scope: <code>{scopeLabel}</code>. These processes are children
              of a STILL-RUNNING client — killing them is an explicit
              override of the safe-sweep guard.
            </p>
            {classesSnapshot.length > 0 && (
              <p class="maintenance-danger-warning m-0 mb-2 text-sm text-app-danger" data-testid="aggressive-confirm-danger">
                Also killing dangerous classes:{" "}
                <strong>{classesSnapshot.join(", ")}</strong> — operator
                terminals or live Playwright browser sessions may be
                terminated.
              </p>
            )}
            <ul class="maintenance-confirm-list m-0 list-none space-y-1 p-0 text-sm" data-testid="aggressive-confirm-list">
              {confirmOrphans.map((o) => (
                <li key={o.pid}>
                  <code>{cmdlineDisplayOf(o)}</code>
                  {" "}PID {o.pid}
                  {o.match_source ? <> — match <code>{o.match_source}</code></> : null}
                </li>
              ))}
            </ul>
          </>
        }
        confirmLabel="Kill"
        danger
        onConfirm={apply}
        onCancel={cancelConfirm}
      />
    </div>
  );
}

// AggressiveTable renders the aggressive candidates. Like OrphansTable it
// shows the redacted cmdline_display, but it adds a Match column (the
// backend's match_source explaining WHY each candidate was included) and
// drops the Server column (aggressive candidates are matched by ancestor
// scope, not by manifest).
function AggressiveTable({ orphans }: { orphans: OrphanProcess[] }): preact.JSX.Element {
  if (orphans.length === 0) {
    return <p class="maintenance-empty mt-3 text-sm text-app-muted">No aggressive candidates found.</p>;
  }
  const showResult = orphans.some((o) => !!o.kill_err);
  return (
    <table class="maintenance-table mt-3 w-full border-collapse text-sm">
      <thead>
        <tr class="text-left text-app-muted">
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">PID</th>
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Match</th>
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Age</th>
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">RAM (MB)</th>
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Cmd</th>
          {showResult && <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Result</th>}
        </tr>
      </thead>
      <tbody>
        {orphans.map((o) => (
          <tr key={o.pid} class="border-b border-app-border/50">
            <td class="py-1.5 pr-4">{o.pid}</td>
            <td class="maintenance-match py-1.5 pr-4">{o.match_source || "—"}</td>
            <td class="py-1.5 pr-4">{Math.round(o.age_sec)}s</td>
            <td class="py-1.5 pr-4">{Math.round(o.ram_bytes / (1024 * 1024))}</td>
            <td class="maintenance-cmd py-1.5 pr-4 font-mono text-xs">{cmdlineDisplayOf(o)}</td>
            {showResult && (
              <td class={`py-1.5 pr-4 ${o.kill_err ? "maintenance-error text-app-danger" : ""}`}>
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
    <div data-card="orphan-log-watchers" class={CARD_CLASS}>
      <div class="flex items-center gap-1.5">
        <h3 class={CARD_TITLE_CLASS}>Orphan log watchers (tail / grep / bash)</h3>
        <InfoTip text="Reclaim tail.exe + grep.exe pipelines left behind by agent shell-snapshot launchers (Claude Code, codex CLI). Preview lists them, then Clean kills them. Optionally include live-parent processes to also kill watchers of currently active sessions. See scripts/cleanup-orphan-watchers.ps1." />
      </div>
      <p class={CARD_DESC_CLASS}>
        Reclaim <code>tail.exe</code> + <code>grep.exe</code> pipelines
        left behind by agent shell-snapshot launchers (Claude Code,
        codex CLI). See <code>scripts/cleanup-orphan-watchers.ps1</code>.
      </p>
      <label class="maintenance-checkbox mt-3 flex items-center gap-2 text-sm text-app-text">
        <input
          type="checkbox"
          class="h-4 w-4 accent-app-accent"
          checked={includeLive}
          onChange={(e) => setIncludeLive((e.target as HTMLInputElement).checked)}
        />
        Include live-parent processes (CURRENT active sessions — kills them too)
      </label>
      <div class={CARD_ACTIONS_CLASS}>
        <button onClick={preview} disabled={state.kind === "loading"}>
          Preview
        </button>
        {state.kind === "preview" && watchers.length > 0 && (
          <button
            onClick={openConfirm}
            disabled={killCount === 0}
            title={noKillReason}
            class="btn-danger"
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
            <p class="m-0 mb-2 text-sm">
              {includeLive
                ? "Includes live-parent processes — those are usually CURRENT active agent sessions and will be killed."
                : "Only dead-parent watchers will be killed."}
            </p>
            <ul class="maintenance-confirm-list m-0 list-none space-y-1 p-0 text-sm" data-testid="orphan-log-watchers-confirm-list">
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
    return <p class="maintenance-empty mt-3 text-sm text-app-muted">No orphan watchers found.</p>;
  }
  // Same Result column rule as OrphansTable — visible only when at
  // least one row has a non-empty kill_err.
  const showResult = watchers.some((w) => !!w.kill_err);
  return (
    <table class="maintenance-table mt-3 w-full border-collapse text-sm">
      <thead>
        <tr class="text-left text-app-muted">
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">PID</th>
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Parent</th>
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Name</th>
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Age</th>
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Cmd</th>
          {showResult && <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Result</th>}
        </tr>
      </thead>
      <tbody>
        {watchers.map((w) => (
          <tr key={w.pid} class="border-b border-app-border/50">
            <td class="py-1.5 pr-4">{w.pid}</td>
            <td class="py-1.5 pr-4">{w.parent_pid}{w.parent_alive ? " (alive)" : " (dead)"}</td>
            <td class="py-1.5 pr-4">{w.name}</td>
            <td class="py-1.5 pr-4">{w.age_sec > 0 ? `${Math.round(w.age_sec / 60)}m` : "?"}</td>
            <td class="maintenance-cmd py-1.5 pr-4 font-mono text-xs">{w.cmdline}</td>
            {showResult && (
              <td class={`py-1.5 pr-4 ${w.kill_err ? "maintenance-error text-app-danger" : ""}`}>
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
    <div data-card="force-kill-instance" class={CARD_CLASS}>
      <div class="flex items-center gap-1.5">
        <h3 class={CARD_TITLE_CLASS}>Stuck mcphub instance</h3>
        <InfoTip text="Force-kill another mcphub gui process that is holding the single-instance lock so a new GUI can start. Diagnose inspects the lock holder first; Force-kill terminates it after an identity gate. Equivalent to mcphub gui --force --kill. macOS not yet supported." />
      </div>
      <p class={CARD_DESC_CLASS}>
        Force-kill another mcphub gui that holds the single-instance
        lock. Equivalent to <code>mcphub gui --force --kill</code>.
        macOS not yet supported.
      </p>
      <div class={CARD_ACTIONS_CLASS}>
        <button onClick={diagnose} disabled={state.kind === "loading"}>
          Diagnose
        </button>
        <button
          onClick={() => setConfirmOpen(true)}
          disabled={state.kind === "loading"}
          class="btn-danger"
          data-testid="force-kill-button"
        >
          Force-kill
        </button>
      </div>
      <CardResult state={state} />
      {state.kind === "preview" && state.verdict !== undefined && (
        <pre class="maintenance-pre mt-3 overflow-x-auto rounded-md border border-app-border bg-app-card/60 p-3 text-xs">
          {JSON.stringify(state.verdict, null, 2)}
        </pre>
      )}
      <ConfirmModal
        open={confirmOpen}
        title="Force-kill the single-instance lock holder?"
        body={
          <p class="m-0 text-sm">
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
    <div data-card="stop-all-daemons" class={CARD_CLASS}>
      <div class="flex items-center gap-1.5">
        <h3 class={CARD_TITLE_CLASS}>Stop all daemons</h3>
        <InfoTip text="Stop every running daemon at once. Use after a multi-daemon zombie scenario; pair with the orphan-MCP cleanup above for a full reset. Wraps the /api/stop-all endpoint." />
      </div>
      <p class={CARD_DESC_CLASS}>
        Stop every running daemon. Use after multi-daemon zombie
        scenarios; pair with the orphan-MCP cleanup above for a full
        reset. Wraps the existing <code>/api/stop-all</code> endpoint.
      </p>
      <div class={CARD_ACTIONS_CLASS}>
        <button
          onClick={() => setConfirmOpen(true)}
          disabled={state.kind === "loading"}
          class="btn-danger"
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
          <p class="m-0 text-sm">
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
    return <p class="maintenance-empty mt-3 text-sm text-app-muted">No daemons were running.</p>;
  }
  return (
    <table class="maintenance-table mt-3 w-full border-collapse text-sm">
      <thead>
        <tr class="text-left text-app-muted">
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Daemon</th>
          <th class="border-b border-app-border py-1.5 pr-4 font-semibold">Status</th>
        </tr>
      </thead>
      <tbody>
        {results.map((sr) => (
          <tr key={sr.task_name} class="border-b border-app-border/50">
            <td class="py-1.5 pr-4">{sr.task_name}</td>
            <td class={`py-1.5 pr-4 ${sr.error ? "maintenance-error text-app-danger" : ""}`}>
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
      return <p class="maintenance-status mt-3 text-sm text-app-muted">Working…</p>;
    case "applied": {
      // Stop-All has its own per-daemon table below; render a banner
      // that distinguishes full success from partial failure (HTTP 207).
      // Codex Cloud bot P2 on PR #131: "Done." alone hid 207 partial
      // failures; the failed count must surface in the summary too.
      if (state.stopResults !== undefined) {
        const total = state.stopResults.length;
        const failed = state.skipped ?? 0;
        if (total === 0) {
          return <p class="maintenance-status mt-3 text-sm text-app-muted">Done. No daemons were running.</p>;
        }
        if (failed === 0) {
          return <p class="maintenance-status mt-3 text-sm text-app-muted">Stopped all {total} daemon{total === 1 ? "" : "s"}.</p>;
        }
        return (
          <p class="maintenance-status maintenance-error mt-3 text-sm text-app-danger">
            Partial: {total - failed} stopped, {failed} failed.
          </p>
        );
      }
      if (state.killed !== undefined || state.skipped !== undefined) {
        return (
          <p class="maintenance-status mt-3 text-sm text-app-muted">
            Done. Killed {state.killed ?? 0}, skipped {state.skipped ?? 0}.
          </p>
        );
      }
      return <p class="maintenance-status mt-3 text-sm text-app-muted">Done.</p>;
    }
    case "error":
      return <p class="maintenance-status maintenance-error mt-3 text-sm text-app-danger">Error: {state.error}</p>;
    default:
      return null;
  }
}
