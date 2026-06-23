import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import {
  fetchOrThrow,
  postInitClientConfig,
  InitClientConfigError,
  listWorkspaces,
  postLspRegister,
  resolveClientSymlink,
  writeClientSymlink,
  type ResolveSymlinkResult,
  type WorkspaceEntryDTO,
  type WorkspacePair,
} from "../api";
import { ScanRefreshControls } from "../components/ScanRefreshControls";
import { useAutoScan } from "../hooks/useAutoScan";
import { useEventSource } from "../hooks/useEventSource";
import { collectServers } from "../lib/routing";
import {
  effectiveVisibleClients,
  loadColumnPrefs,
  saveColumnPrefs,
  clearColumnPrefs,
  type ColumnPrefs,
} from "../lib/matrix-columns";
import { collectLspRows, type LspRow, LSP_MANIFEST_SERVER } from "../lib/lsp-rows";
import { aggregateStatus, stateShape } from "../lib/status";
import { pushToast } from "../lib/toast-store";
import { WorkspaceSelector, ALL_WORKSPACES_KEY } from "../components/WorkspaceSelector";
import { MatrixColumnsMenu } from "../components/MatrixColumnsMenu";
import { EnvDrawer } from "../components/EnvDrawer";
import { ServerRowDrawer } from "../components/ServerRowDrawer";
import { ToggleSwitch } from "../components/ToggleSwitch";
import type {
  ClientConfigState,
  ClientEntry,
  ClientPresence,
  DaemonStatus,
  ScanResult,
  ServerRow,
  Routing,
} from "../types";

// The rendered client columns are no longer a fixed constant: they are
// computed per-scan by visibleClients() so the seven core clients always
// show while every non-core opt-in client appears only when detected on the
// host (detection-gated, see routing.ts::visibleClients). The non-core
// universe is derived live from the scan's client_config_presence map (one
// key per clients.SupportedClientNames(), all 46 backend clients), so any
// supported client surfaces when installed — no hardcoded column list to
// drift behind the backend registry.
// On top of that auto-detected base the operator's manual show/hide
// overrides (persisted in localStorage, see lib/matrix-columns.ts) are
// folded in via effectiveVisibleClients() — the "Manage columns" popover
// lets them pin an undetected column visible or hide a noisy one. This is
// a pure VIEW filter: hidden columns simply don't render; apply/migrate/
// demigrate logic is unchanged. The computed list is threaded through
// ServerRowView/CellView as a prop.

// EMPTY_CLIENT_CONFIG_PRESENCE is a stable reference used when a scan
// response omits client_config_presence (e.g. /api/scan mocks in
// Playwright tests). Without a stable reference, every scan refetch
// would call setClientConfigPresence with a fresh `{}`, triggering a
// re-render even when no presence info changed — that in turn
// compresses the "Applied. Refreshing…" visible window enough to
// destabilize the B1 §5 e2e timing assertions. React/Preact bail out
// when Object.is(prev, next) is true, so reusing this constant
// short-circuits the redundant render.
const EMPTY_CLIENT_CONFIG_PRESENCE: Record<string, ClientConfigState> = {};

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

// HoverScope (G1) names the bulk-toggle group the pointer is currently over so
// the matrix can preview which cells a click would flip. `kind: "col"` → key
// is a client (column-header toggle hover); `kind: "row"` → key is a server
// name (row swap-toggle hover). null = no bulk toggle hovered (single-cell
// hover is handled by CSS, not this state).
type HoverScope = { kind: "col"; key: string } | { kind: "row"; key: string };

// cellInteractive is the SINGLE source of truth for whether a matrix data
// cell exposes a live, toggleable checkbox. CellView's `disabled` derivation
// and the whole-column header toggle both consume it so the two can never
// drift: a cell the header toggle would flip must be exactly a cell the
// operator could flip by clicking its checkbox.
//
// Interactive routing states are "via-hub", "direct", and "available"
// (see perClientRouting doc in lib/routing.ts). Everything else
// ("not-installed", "unsupported", "config-error", "config-error-symlink",
// and an undefined/absent client key → treated as "not-installed") is a
// disabled, non-toggleable cell. The `applying` guard is folded in so an
// in-flight Apply freezes both the per-cell checkbox AND the column toggle.
function cellInteractive(routing: Routing, applying: boolean): boolean {
  if (applying) return false;
  return routing === "via-hub" || routing === "direct" || routing === "available";
}

export function ServersScreen() {
  const [servers, setServers] = useState<ServerRow[] | null>(null);
  const [statusByServer, setStatusByServer] = useState<Record<string, { state: string; port: number | null }>>({});
  // Raw per-server DaemonStatus rows from /api/status, kept alongside the
  // aggregate `statusByServer` so the ServerRowDrawer can render per-daemon
  // lifetime stats (PID / uptime / RAM) without a second fetch. Keyed by
  // server name; each value is the list of daemon rows for that server.
  const [statusRowsByServer, setStatusRowsByServer] = useState<Record<string, DaemonStatus[]>>({});
  // The server whose detail drawer is currently open (null = closed).
  const [drawerServer, setDrawerServer] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  // v0.4.5 init-button: client_config_presence drives the per-column
  // "Initialize <client>" header affordance. Stored alongside servers
  // so applyChanges → refresh re-resolves into the new "ok" state and
  // the button disappears once the empty stub lands on disk.
  const [clientConfigPresence, setClientConfigPresence] = useState<
    Record<string, ClientConfigState>
  >({});
  // Per-client "initializing" flag: the operator clicked the
  // Initialize button and the POST is in flight. Used to disable the
  // button + render a spinner-like dim state so double-clicks during
  // the brief network roundtrip do not enqueue redundant inits.
  const [initBusy, setInitBusy] = useState<Record<string, boolean>>({});
  // Banner that surfaces the most recent init result (success or
  // failure). Cleared automatically on the next /api/scan refresh so
  // a successful init doesn't linger across screen interactions; an
  // error message persists until the operator clicks Initialize again
  // or navigates away.
  const [initMsg, setInitMsg] = useState<{ text: string; kind: "ok" | "error" } | null>(null);
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
  // Bug-bash B1 closure (#7): structured per-row failure list instead
  // of the legacy "; "-joined wall-of-text. Each entry is a short
  // "server/client: err" string that renders as one <li> in the toolbar
  // failure list. Empty array = no failures to render.
  const [failedRows, setFailedRows] = useState<string[]>([]);
  const [reloadToken, setReloadToken] = useState<number>(0);
  // applyGen forces a per-cell remount after every applyChanges run so
  // each CellView re-initializes its local `checked` state from
  // initialChecked (authoritative scan). Bug-bash A4 (#17) closure:
  // pre-fix, a failed demigrate left CellView showing the user's last
  // toggle (☐) while the disk still held the entry (☑) — visual lied
  // about state. CellView's useEffect [initialChecked] only fires when
  // initialChecked CHANGES; on a failed Apply the scan is unchanged,
  // so the effect doesn't re-sync and user's local toggle persists.
  // Remounting via key fixes that: the new instance starts with
  // useState(initialChecked) honestly. Retry context survives via the
  // separate red-border outcome map.
  const [applyGen, setApplyGen] = useState<number>(0);

  // v0.5.x Task 4.3 — Workspace registry + LSP matrix state.
  //
  // `workspaces` is the deduplicated (key, path) list rendered by the
  // selector at the top of the screen; `workspaceEntries` is the full
  // (key, language) tuple list the LSP-row helper consumes to map a
  // language → its registered task_name. Both come from
  // /api/workspaces and are refreshed alongside /api/scan + /api/status.
  //
  // `selectedWorkspaceKey` defaults to ALL_WORKSPACES_KEY ("") so the
  // matrix starts with every workspace's rows visible — operators with
  // a single workspace then never need to interact with the selector.
  //
  // `openDrawerFor` carries the LspRow whose drawer is currently open
  // (null = closed). Holding the full row keeps the drawer's initial
  // PATH + label coherent even if scan refreshes mid-edit.
  const [workspaces, setWorkspaces] = useState<WorkspacePair[]>([]);
  const [workspaceEntries, setWorkspaceEntries] = useState<WorkspaceEntryDTO[]>([]);
  const [selectedWorkspaceKey, setSelectedWorkspaceKey] = useState<string>(ALL_WORKSPACES_KEY);
  const [openDrawerFor, setOpenDrawerFor] = useState<LspRow | null>(null);
  const [lspRegisterBusy, setLspRegisterBusy] = useState<Record<string, boolean>>({});
  const [lspRegisterMsg, setLspRegisterMsg] = useState<{ text: string; kind: "ok" | "error" } | null>(null);
  // Keeps the latest /api/scan result around so the LSP-row helper has
  // a fresh source after a respawn or apply. Independent of `servers`
  // because the legacy main-matrix flow already mutates `servers` via
  // collectServers — keeping `scanForLsp` separate avoids re-deriving
  // collectServers on every minor refresh.
  const [scanForLsp, setScanForLsp] = useState<ScanResult | null>(null);
  // Monotonic guard for scan refreshes. Auto-refresh, manual Rescan,
  // reloadToken effects, and SSE can overlap; only the newest request is
  // allowed to publish state so a slow older response cannot overwrite a
  // fresher matrix.
  const latestScanRequestRef = useRef(0);

  // Per-client matrix-column visibility overrides, persisted in
  // localStorage (see lib/matrix-columns.ts). Initialized once from
  // storage via the useState lazy initializer so a reload restores the
  // operator's last show/hide choices on first paint. `true` = pin
  // visible, `false` = hide; an absent client defers to auto-detection.
  // This drives ONLY which columns render — a pure view filter.
  const [columnPrefs, setColumnPrefs] = useState<ColumnPrefs>(() => loadColumnPrefs());

  // G1 hover-scope: which bulk-toggle the pointer is currently over, so the
  // matrix can PREVIEW the click SCOPE before the operator commits. CSS alone
  // cannot light up a whole COLUMN across rows (sibling <td>s in different
  // <tr>s have no common hover ancestor), so a small JS signal carries the
  // hovered scope down to every CellView. `kind: "col"` → key is a client;
  // `kind: "row"` → key is a server name. A CellView highlights itself when
  // it is toggleable AND falls inside this scope. Single-cell hover stays
  // pure CSS (.matrix-cell-label:not(.disabled):hover) — no JS needed there.
  const [hoverScope, setHoverScope] = useState<HoverScope | null>(null);

  // Show or hide one client column. Updates the in-memory prefs AND
  // persists immediately so the choice survives a reload; the matrix
  // re-renders synchronously off the new state.
  function toggleColumn(client: string, show: boolean) {
    setColumnPrefs((prev) => {
      const next = { ...prev, [client]: show };
      saveColumnPrefs(next);
      return next;
    });
  }

  // Clear every override → revert to pure auto-detection. Wipes the
  // persisted record too so a reload also starts clean.
  function resetColumns() {
    clearColumnPrefs();
    setColumnPrefs({});
  }

  // PR #208 deep-sec Lane A round 7 P2 closure: mountedRef guards
  // post-await setState calls inside initializeClient against the
  // "user navigated away mid-POST" race. The scan useEffect already
  // has its own local `cancelled` flag; the click-handler promise
  // needs its own signal because it does not own the effect scope.
  const mountedRef = useRef<boolean>(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  // Tray "Rescan client configs" — backend publishes clients-rescan,
  // every open Servers tab re-fetches. Bumping reloadToken composes
  // with the existing useEffect dep so the same path serves both
  // user-triggered Apply and tray-triggered rescan.
  useEventSource("/api/events", {
    "clients-rescan": () => setReloadToken((n) => n + 1),
  });

  // loadScan is the single fetch path for the matrix. It is driven by
  // three sources, all calling the SAME closure so the rendered baseline
  // reconciles identically regardless of trigger:
  //   - useAutoScan (initial mount fetch + 10s poll + on-visible refresh),
  //   - the reloadToken effect (Apply, Initialize, LSP register, SSE
  //     clients-rescan bumps),
  //   - the "Rescan now" button (via useAutoScan.rescan).
  //
  // CRITICAL — dirty-cell no-clobber: this refetch only replaces the
  // authoritative scan BASELINE (server.routing, statusByServer, presence,
  // workspaces). The operator's pending edits live in the SEPARATE `dirty`
  // map, which this closure never touches. Auto-refresh is additionally
  // PAUSED at the useAutoScan level while `dirty.size > 0` (see below), so
  // a poll tick can't even fire mid-edit; and because each CellView's
  // visible checkbox folds `dirty` over the baseline (effectiveChecked), a
  // baseline-only refresh can never visually drop a pending toggle.
  const loadScan = useCallback(async () => {
    const requestId = latestScanRequestRef.current + 1;
    latestScanRequestRef.current = requestId;
    const isLatest = () => latestScanRequestRef.current === requestId;
    try {
      // /api/workspaces returns {workspaces, entries}. A registry
      // load failure must NOT block the matrix render — the
      // selector falls back to its empty-state placeholder and the
      // LSP matrix surfaces the 9 placeholder rows. Catch isolation
      // is bounded to the workspaces fetch only; /api/scan and
      // /api/status errors continue to fail the whole load.
      const [scan, status, workspacesResp] = await Promise.all([
        fetchOrThrow<ScanResult>("/api/scan", "object"),
        fetchOrThrow<DaemonStatus[]>("/api/status", "array"),
        listWorkspaces().catch(() => ({ workspaces: [], entries: [] })),
      ]);
      if (!isLatest()) return;
      if (scan.entries != null && !Array.isArray(scan.entries)) {
        setError("/api/scan returned malformed entries");
        return;
      }
      setServers(collectServers(scan));
      setScanForLsp(scan);
      setWorkspaces(workspacesResp.workspaces);
      setWorkspaceEntries(workspacesResp.entries);
      setClientConfigPresence(scan.client_config_presence ?? EMPTY_CLIENT_CONFIG_PRESENCE);
      // Clear any success banner once the authoritative refresh lands
      // (the matrix has already redrawn with the new "ok" state).
      // Error banners stay sticky so the operator sees the failure
      // until they retry — refreshing should NOT mask a recent
      // PARENT_MISSING / INIT_FAILED report.
      setInitMsg((msg) => (msg && msg.kind === "ok" ? null : msg));
      setLspRegisterMsg((msg) => (msg && msg.kind === "ok" ? null : msg));
      const agg = aggregateStatus(status);
      const flat: Record<string, { state: string; port: number | null }> = {};
      for (const [name, a] of Object.entries(agg)) {
        flat[name] = { state: a.state, port: a.port };
      }
      setStatusByServer(flat);
      // Group raw daemon rows per server for the row detail drawer. Skip
      // maintenance rows (same filter the aggregate uses) so the drawer
      // never shows a blank weekly-refresh task as a "daemon".
      const rowsByServer: Record<string, DaemonStatus[]> = {};
      for (const row of status.filter((r) => !r.is_maintenance)) {
        (rowsByServer[row.server] ??= []).push(row);
      }
      setStatusRowsByServer(rowsByServer);
      setError(null);
      // PR #186 fix: clear the "Applied. Refreshing…" indicator
      // once the authoritative reload completes. Pre-fix the
      // string sat in applyMsg forever (set by applyChanges
      // on success, never cleared) — user saw the spinner-like
      // wording after every successful Apply with no way to
      // know when the refresh was actually done. Failed-message
      // strings ("Failed: N row(s); re-toggle and retry below.")
      // must NOT be cleared here — they remain visible until
      // the user starts a new Apply cycle.
      setApplyMsg((msg) => (msg === "Applied. Refreshing…" ? "" : msg));
    } catch (err) {
      if (isLatest()) setError((err as Error).message);
    }
  }, []);

  // Auto-refresh: 10s poll while mounted, paused while the tab is hidden,
  // immediate refresh on becoming visible again — AND paused while there
  // are unsaved matrix edits (`dirty.size > 0`). The dirty pause is the
  // no-clobber guarantee: an auto tick never fires mid-edit. The pause
  // releases automatically once Apply/discard empties the dirty map.
  const hasUnsavedEdits = dirty.size > 0;
  const { rescan, agoSeconds } = useAutoScan(loadScan, hasUnsavedEdits);

  // reloadToken-driven refetch for Apply / Initialize / LSP register / SSE.
  // useAutoScan already issues the initial mount fetch, so this effect
  // skips token 0 to avoid a redundant duplicate fetch on first paint.
  // These triggers fire AFTER the dirty set has been pruned (Apply) or are
  // operator-initiated (Initialize/register), so refetching here is safe
  // even though the auto-poll is paused while dirty.
  useEffect(() => {
    if (reloadToken === 0) return;
    void loadScan();
  }, [reloadToken, loadScan]);

  async function initializeClient(client: string) {
    if (initBusy[client]) return;
    setInitBusy((prev) => ({ ...prev, [client]: true }));
    setInitMsg(null);
    try {
      const res = await postInitClientConfig(client);
      // PR #208 deep-sec Lane A round 7 P2 closure: skip every
      // post-await setState if the component has unmounted (user
      // navigated away from the Servers screen between click and
      // POST settle). React/Preact does not raise on unmounted
      // setState in production but the leftover state update is
      // wasted work and the unmount-then-remount flow could pick
      // up the stale banner if React re-uses the fiber slot.
      if (!mountedRef.current) return;
      setInitMsg({
        text: res.created
          ? `Initialized ${client} config at ${res.path}.`
          : `${client} config already existed at ${res.path}; refreshed.`,
        kind: "ok",
      });
      // Trigger /api/scan refresh — collectServers reruns with the new
      // "ok" presence and the column's cells flip to "available"
      // (assuming the affected rows are migratable).
      setReloadToken((n) => n + 1);
    } catch (err) {
      if (!mountedRef.current) return;
      setInitMsg({ text: (err as Error).message, kind: "error" });
      // PR #208 deep-sec Lane B round 4 P2 closure: when the
      // backend says PARENT_MISSING (412), the cached
      // client_config_presence is stale — the parent directory
      // disappeared between scan refresh and click. Trigger a scan
      // refresh so the matrix re-renders without the Initialize
      // affordance (presence flips from "missing-init-possible" to
      // "missing"). Error banners are sticky across scan refresh
      // (see the scan onload setInitMsg passthrough for kind ===
      // "error"), so the operator still sees the failure context.
      if (err instanceof InitClientConfigError && err.code === "PARENT_MISSING") {
        setReloadToken((n) => n + 1);
      }
    } finally {
      // Only clear busy if still mounted; otherwise the next mount
      // starts with a clean state via useState default anyway.
      if (mountedRef.current) {
        setInitBusy((prev) => {
          const next = { ...prev };
          delete next[client];
          return next;
        });
      }
    }
  }

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
    // Clear any stale failures from a prior Apply — new attempt resets
    // the list so retried-cells don't display under both old and new
    // error rows.
    setFailedRows([]);

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
    //
    // Response shapes (bug-bash B1 closure #7):
    //   200 → success (Restored[] populated, Failed[] empty)
    //   207 → partial failure (Failed[] has exactly 1 row for this cell)
    //   500 → setup error (`{error, code: DEMIGRATE_FAILED}`)
    //   204 (legacy) → treat as success for forward compat (test fakes)
    for (const cell of demigrateCells) {
      try {
        const resp = await fetch("/api/demigrate", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ servers: [cell.server], clients: [cell.client] }),
        });
        if (resp.status === 200 || resp.status === 204) {
          outcomes.get(cell.server)!.set(cell.client, "succeeded");
        } else if (resp.status === 207) {
          // Partial-failure body shape: { restored: [...], failed: [{server, client, err}] }.
          // Per-cell POST means failed[] has at most 1 entry; surface its
          // clean err message without the legacy aggregation prefix.
          const body = (await resp.json().catch(() => ({}))) as {
            failed?: { server: string; client: string; err: string }[];
          };
          const row = body.failed?.[0];
          const errMsg = row?.err ?? `HTTP ${resp.status}`;
          failed.push(`${cell.server}/demigrate/${cell.client}: ${errMsg}`);
          outcomes.get(cell.server)!.set(cell.client, "failed");
          failedDemigrateClients.add(cell.client);
        } else {
          // 4xx/5xx error body: { error, code }
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
        // Same shape as /api/demigrate above (B1 #7 symmetry).
        if (resp.status === 200 || resp.status === 204) {
          outcomes.get(cell.server)!.set(cell.client, "succeeded");
        } else if (resp.status === 207) {
          const body = (await resp.json().catch(() => ({}))) as {
            failed?: { server: string; client: string; err: string }[];
          };
          const row = body.failed?.[0];
          const errMsg = row?.err ?? `HTTP ${resp.status}`;
          failed.push(`${cell.server}/migrate/${cell.client}: ${errMsg}`);
          outcomes.get(cell.server)!.set(cell.client, "failed");
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
    // cell's baseline honest.
    setReloadToken((x) => x + 1);
    // Bug-bash A4 (#17): bump applyGen so EVERY CellView remounts and
    // re-initializes from initialChecked. Pre-fix, failed cells kept
    // user's local toggle as the visible state while disk unchanged.
    // Now: after Apply, every cell visually matches authoritative scan
    // (initialChecked); retry context survives via outcomes/red border,
    // not via the checkbox visual.
    setApplyGen((g) => g + 1);

    const total = demigrateCells.length + migrateCells.length;
    if (failed.length === 0) {
      setApplyMsg("Applied. Refreshing…");
      setFailedRows([]);
      // Flowbite success toast — a transient, screen-agnostic confirmation
      // that complements the inline "Applied. Refreshing…" toolbar text.
      pushToast(
        "success",
        `Applied ${total} change${total === 1 ? "" : "s"} to client configs.`,
      );
    } else {
      // Bug-bash B1 closure (#7): each failed entry becomes its own
      // <li> row in the toolbar list (see render below). applyMsg
      // just shows the count + reminder; the wall-of-text in a
      // single string is gone.
      setApplyMsg(`Failed: ${failed.length} row(s); re-toggle and retry below.`);
      setFailedRows(failed);
      // Flowbite danger toast — sticky (no auto-dismiss) so the operator
      // acknowledges the partial-failure explicitly. The per-row detail
      // still lives in the inline failedRows list below.
      pushToast(
        "danger",
        `Apply failed for ${failed.length} of ${total} change${total === 1 ? "" : "s"}; see the list below to retry.`,
      );
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

  // Bug-bash A4 (#9) closure: count cells whose last Apply failed or
  // was gated AND that are still in the dirty queue. Bot r1 P2 fix:
  // counting from `outcomes` alone reads stale entries when the user
  // toggles a failed cell back to its initial state — toggleCell
  // prunes from `dirty` but leaves the entry in `outcomes`, so the
  // button would falsely advertise "Apply changes (incl. retry N)"
  // while Apply is disabled because dirty.size === 0. Intersect both
  // maps so the label reflects actionable state only.
  let retryPendingCount = 0;
  for (const [server, clientOutcomes] of outcomes) {
    const stillDirty = dirty.get(server);
    if (!stillDirty) continue;
    for (const [client, o] of clientOutcomes) {
      if ((o === "failed" || o === "gated") && stillDirty.has(client)) {
        retryPendingCount++;
      }
    }
  }

  // Bug-bash A3 (#11/#12): split rows into mcphub-managed (manifested)
  // and legacy non-mcphub (no manifest). The main matrix only renders
  // manifested servers — checkboxes for non-manifested rows would mean
  // nothing (Apply migrate/demigrate both require a manifest). Legacy
  // entries surface in a read-only "Other MCP entries" expander so the
  // operator can see what's in client configs without confusing them
  // for mcphub-managed.
  // Bot r1 P2 closure: keep any row with a pending dirty edit visible
  // in the main matrix, even if a scan race flipped manifest_exists to
  // false (e.g., manifest deletion between fetch + render). Pre-fix,
  // filtering by `s.manifested` alone could hide a row that still has
  // queued migrate/demigrate work — Apply would fire on an invisible
  // row and the operator couldn't inspect or undo from the UI.
  // Exclude the workspace-scoped LSP server from the top single-daemon
  // matrix: its per-(workspace, language) form is enabled through the
  // "LSP daemons" table below, and its bare matrix checkbox (Port "—")
  // could register nothing — a non-functional trap. See LSP_MANIFEST_SERVER
  // doc in lsp-rows.ts. (serena stays: it has a single router endpoint.)
  const manifestedServers = servers.filter(
    (s) => (s.manifested || dirty.has(s.name)) && s.name !== LSP_MANIFEST_SERVER,
  );
  const otherServers = servers.filter((s) => !s.manifested && !dirty.has(s.name));

  // effectiveChecked computes a cell's CURRENT visual state: a pending
  // dirty edit overrides the authoritative scan baseline (a queued
  // "migrate" means the operator already checked it; "demigrate" means
  // they unchecked it). Used by both the per-cell visual sync and the
  // column-toggle's all-checked computation so the header toggle reflects
  // unsaved edits, not just the last scan.
  const effectiveChecked = (server: ServerRow, client: string): boolean => {
    const pending = dirty.get(server.name)?.get(client);
    if (pending) return pending === "migrate";
    return (server.routing[client] ?? "not-installed") === "via-hub";
  };

  // Whole-column toggle (Feature 2): flip every applicable (toggleable,
  // non-disabled) cell in one client's column at once. Computes the
  // column's effective state across only the interactive cells — if ALL
  // are currently checked, set them all UNchecked; otherwise set them all
  // checked. Each flip routes through the SAME per-cell onToggle path
  // (toggleCell) so migrate/demigrate semantics + the Apply queue stay
  // identical to manual per-cell toggles — no separate apply path.
  const columnInteractiveServers = (client: string): ServerRow[] =>
    manifestedServers.filter((s) =>
      cellInteractive(s.routing[client] ?? "not-installed", applying),
    );
  // flipCellGroup is the SINGLE owner of the bulk enable-all/disable-all
  // logic shared by the whole-column header toggle AND the whole-row
  // server toggle: given a set of (server, client) cells, if ALL are
  // currently checked set them all UNchecked, else set them all checked.
  // Each flip routes through the SAME per-cell toggleCell path so
  // migrate/demigrate semantics + the Apply queue stay identical to a
  // manual per-cell toggle — there is no separate bulk apply path, and no
  // duplicated flip logic between the column and row toggles.
  const flipCellGroup = (cells: { server: ServerRow; client: string }[]) => {
    if (cells.length === 0) return; // no-op: nothing toggleable in the group
    const allChecked = cells.every(({ server, client }) => effectiveChecked(server, client));
    const next = !allChecked;
    for (const { server, client } of cells) {
      // initialChecked is the authoritative scan baseline the dirty map
      // keys direction off of — pass it (NOT the effective state) so
      // toggleCell prunes/creates the dirty entry against the right axis.
      const initialChecked = (server.routing[client] ?? "not-installed") === "via-hub";
      // Only fire when the cell's effective state actually changes, so an
      // already-correct cell isn't churned through the dirty map.
      if (effectiveChecked(server, client) !== next) {
        toggleCell(server.name, client, next, initialChecked);
      }
    }
  };
  const toggleColumnCells = (client: string) => {
    flipCellGroup(columnInteractiveServers(client).map((server) => ({ server, client })));
  };

  // v0.5.x Task 4.3 — LSP rows are always 9 (one per language). When
  // a workspace is selected, the helper scopes each row's task_name +
  // presence to that workspace's entries; otherwise every workspace's
  // entries fold into the same row and the matrix surfaces the union
  // (with coexistence rendering as dual badges per cell).
  const lspRows = collectLspRows(scanForLsp, workspaceEntries, selectedWorkspaceKey);
  // Effective client columns: the detection-gated base (seven core
  // clients always, plus any non-core opt-in client detected on this host)
  // with the operator's manual show/hide overrides folded in. Derived
  // from the live scan + persisted prefs so an uninstalled niche client
  // adds no column unless explicitly pinned, and a noisy detected column
  // can be hidden. View filter only — see effectiveVisibleClients.
  const clientColumns = effectiveVisibleClients(scanForLsp, columnPrefs);
  // Whole-row toggle: flip every applicable (toggleable, non-disabled)
  // cell in ONE server's row at once — the row analog of the column
  // header toggle, fed through the same flipCellGroup owner. Scoped to
  // the currently-visible client columns so a hidden column is never
  // silently mutated by a row toggle.
  const rowInteractiveClients = (server: ServerRow): string[] =>
    clientColumns.filter((client) =>
      cellInteractive(server.routing[client] ?? "not-installed", applying),
    );
  const toggleRowCells = (server: ServerRow) => {
    flipCellGroup(rowInteractiveClients(server).map((client) => ({ server, client })));
  };
  const selectedWorkspace =
    selectedWorkspaceKey !== ALL_WORKSPACES_KEY
      ? workspaces.find((w) => w.workspace_key === selectedWorkspaceKey)
      : workspaces.length === 1
        ? workspaces[0]
        : null;
  const lspRegisterWorkspacePath = selectedWorkspace?.workspace_path ?? "";

  const registerLspRow = async (row: LspRow) => {
    if (!lspRegisterWorkspacePath) {
      setLspRegisterMsg({
        kind: "error",
        text: "Pick one workspace before enabling an LSP daemon.",
      });
      return;
    }
    const key = row.language;
    setLspRegisterBusy((prev) => ({ ...prev, [key]: true }));
    setLspRegisterMsg(null);
    try {
      const response = await postLspRegister(lspRegisterWorkspacePath, row.language);
      if (!mountedRef.current) return;
      const failed = response.results?.find((result) => result.status === "error");
      if (failed) {
        setLspRegisterMsg({
          kind: "error",
          text: `Enable ${row.language} failed: ${failed.error ?? "unknown error"}`,
        });
        return;
      }
      setLspRegisterMsg({ kind: "ok", text: `Enabled ${row.language}. Refreshing…` });
      setReloadToken((n) => n + 1);
    } catch (err) {
      if (!mountedRef.current) return;
      setLspRegisterMsg({
        kind: "error",
        text: `Enable ${row.language} failed: ${(err as Error).message}`,
      });
    } finally {
      if (mountedRef.current) {
        setLspRegisterBusy((prev) => ({ ...prev, [key]: false }));
      }
    }
  };

  return (
    <div>
      <div class="screen-header-row">
        <h1>Servers</h1>
        <ScanRefreshControls
          agoSeconds={agoSeconds}
          onRescan={rescan}
          paused={hasUnsavedEdits}
          pauseReason="auto-refresh paused — unsaved changes"
          disabledReason="Apply or discard your unsaved matrix changes before rescanning"
        />
      </div>
      <WorkspaceSelector
        workspaces={workspaces}
        selectedKey={selectedWorkspaceKey}
        onChange={(key) => {
          // Close any open drawer when the workspace filter changes —
          // the drawer's taskName may no longer match a visible row.
          setOpenDrawerFor(null);
          setSelectedWorkspaceKey(key);
        }}
      />
      <div id="servers-toolbar">
        <button class="btn btn-primary" onClick={applyChanges} disabled={applyDisabled}>
          {retryPendingCount > 0
            ? `Apply changes (incl. retry ${retryPendingCount})`
            : "Apply changes"}
        </button>
        <span style="margin-left:12px" class={applyMsg.startsWith("Failed") ? "error" : ""}>
          {applyMsg}
        </span>
      </div>
      {failedRows.length > 0 && (
        <ul class="apply-failed-rows" data-testid="apply-failed-rows">
          {failedRows.map((row) => (
            <li key={row} class="error">
              {row}
            </li>
          ))}
        </ul>
      )}
      {initMsg && (
        <div
          class={initMsg.kind === "error" ? "error" : ""}
          data-testid="init-client-msg"
          style="margin:var(--gap-xs) 0"
        >
          {initMsg.text}
        </div>
      )}
      <div class="matrix-columns-toolbar" style="margin:var(--gap-xs) 0">
        <MatrixColumnsMenu
          visible={clientColumns}
          onToggle={toggleColumn}
          onReset={resetColumns}
        />
      </div>
      <table class="servers-matrix">
        {/* G15 a11y: a visually-hidden <caption> names the matrix for screen
            readers, and every header carries scope= so AT can associate each
            toggle cell with its server (row) + client (column). */}
        <caption class="visually-hidden">
          MCP servers by client install matrix: each row is an MCP server,
          each column a client, and each cell toggles routing that server
          through the hub for that client.
        </caption>
        <thead>
          <tr>
            <th scope="col">Server</th>
            {clientColumns.map((c) => {
              const presence = clientConfigPresence[c];
              // G17: render Initialize for BOTH the legacy
              // "missing-init-possible" (parent dir exists) and the new
              // "missing-init-creatable" (parent dir absent but securely
              // creatable under the user home) states. The tooltip
              // differs so the operator knows when a config DIRECTORY is
              // being created vs only a stub file.
              const canInit =
                presence === "missing-init-possible" ||
                presence === "missing-init-creatable";
              const willCreateDir = presence === "missing-init-creatable";
              const busy = initBusy[c] === true;
              // Whole-column toggle (Feature 2): the client-NAME span doubles
              // as a clickable enable-all / disable-all control for that
              // column. It is only interactive when the column has at least
              // one toggleable cell; otherwise it renders as plain inert
              // text (no pointer, no role) so an all-disabled column shows no
              // misleading affordance. Kept as a <span> (NOT a <button>) so
              // the existing `.matrix-col-header > span` header assertions
              // and the client-name text content stay intact; the Initialize
              // button remains a separate sibling control.
              const colToggleable = columnInteractiveServers(c).length > 0;
              return (
                <th key={c} scope="col">
                  <div class="matrix-col-header">
                    <span
                      class={
                        colToggleable
                          ? canInit
                            ? "matrix-col-toggle"
                            : "matrix-col-toggle matrix-col-toggle--full"
                          : undefined
                      }
                      data-testid={colToggleable ? `matrix-col-toggle-${c}` : undefined}
                      role={colToggleable ? "button" : undefined}
                      tabIndex={colToggleable ? 0 : undefined}
                      title={colToggleable ? `Toggle all ${c} cells` : undefined}
                      onClick={colToggleable ? () => toggleColumnCells(c) : undefined}
                      onMouseEnter={
                        colToggleable ? () => setHoverScope({ kind: "col", key: c }) : undefined
                      }
                      onMouseLeave={colToggleable ? () => setHoverScope(null) : undefined}
                      onKeyDown={
                        colToggleable
                          ? (ev: KeyboardEvent) => {
                              if (ev.key === "Enter" || ev.key === " ") {
                                ev.preventDefault();
                                toggleColumnCells(c);
                              }
                            }
                          : undefined
                      }
                    >
                      {c}
                    </span>
                    {canInit && (
                      <button
                        type="button"
                        class="matrix-col-init-btn btn btn-sm"
                        data-testid={`init-client-${c}`}
                        disabled={busy}
                        title={
                          willCreateDir
                            ? `${c}'s MCP config directory does not exist yet. Click to create the config directory and seed an empty MCP config so this column becomes active.`
                            : `${c}'s MCP config file is not present on this host, but its parent directory exists. Click to seed an empty stub so this column becomes active.`
                        }
                        onClick={() => initializeClient(c)}
                      >
                        {busy ? "Init…" : "Initialize"}
                      </button>
                    )}
                  </div>
                </th>
              );
            })}
            <th scope="col">Port</th>
            <th scope="col">State</th>
          </tr>
        </thead>
        <tbody>
          {manifestedServers.map((server) => (
            <ServerRowView
              key={server.name}
              server={server}
              clients={clientColumns}
              status={statusByServer[server.name]}
              outcomes={outcomes.get(server.name)}
              pending={dirty.get(server.name)}
              applyGen={applyGen}
              onToggle={toggleCell}
              onRowToggle={toggleRowCells}
              onRowToggleHover={(hovering) =>
                setHoverScope(hovering ? { kind: "row", key: server.name } : null)
              }
              hoverScope={hoverScope}
              onOpenDrawer={() => setDrawerServer(server.name)}
              onSymlinkResolved={() => setReloadToken((n) => n + 1)}
              applying={applying}
            />
          ))}
        </tbody>
      </table>
      <LspMatrix
        rows={lspRows}
        clients={clientColumns}
        openDrawerFor={openDrawerFor}
        onOpenDrawer={(row) => setOpenDrawerFor(row)}
        targetWorkspacePath={lspRegisterWorkspacePath}
        registerBusy={lspRegisterBusy}
        registerMsg={lspRegisterMsg}
        onRegister={registerLspRow}
      />
      {openDrawerFor && openDrawerFor.taskName && (
        <EnvDrawer
          taskName={openDrawerFor.taskName}
          rowLabel={`${openDrawerFor.language} (${openDrawerFor.workspaceKey || "—"})`}
          onClose={() => setOpenDrawerFor(null)}
        />
      )}
      {drawerServer && (
        <ServerRowDrawer
          key={drawerServer}
          serverName={drawerServer}
          daemons={statusRowsByServer[drawerServer] ?? []}
          onClose={() => setDrawerServer(null)}
        />
      )}
      {otherServers.length > 0 && (
        <OtherMCPEntriesSection servers={otherServers} />
      )}
    </div>
  );
}

// OtherMCPEntriesSection renders MCP server entries discovered in
// client configs that have no corresponding manifest under
// servers/<name>/manifest.yaml — e.g. operator's own legacy stdio
// entries like `time-server` in `.cursor/mcp.json`. These rows are
// read-only: migrate/demigrate are no-ops without a manifest.
//
// Bug-bash A3 (#11/#12) closure: pre-fix, these rows mixed into the
// main matrix and rendered live checkboxes that did nothing on Apply,
// confusing operators.
function OtherMCPEntriesSection(props: { servers: ServerRow[] }) {
  const { servers } = props;
  return (
    <details class="other-mcp-entries" style="margin-top:var(--section-gap)">
      <summary>
        <strong>Other MCP entries ({servers.length})</strong>
        {" — "}
        legacy or third-party MCP servers detected in client configs;
        no mcphub manifest, so they can't be migrated through this matrix
      </summary>
      <ul style="font-family:monospace; font-size:0.9em; margin-top:var(--gap-xs)">
        {servers.map((s) => {
          const clientsWithEntry = Object.entries(s.routing)
            .filter(([, r]) => r === "via-hub" || r === "direct")
            .map(([c]) => c);
          return (
            <li key={s.name}>
              <code>{s.name}</code>
              {clientsWithEntry.length > 0 && (
                <span style="color:var(--text-muted)"> — in: {clientsWithEntry.join(", ")}</span>
              )}
            </li>
          );
        })}
      </ul>
    </details>
  );
}

function ServerRowView(props: {
  server: ServerRow;
  clients: readonly string[];
  status?: { state: string; port: number | null };
  outcomes?: Map<string, Outcome>;
  // Per-server dirty direction map (client → "migrate" | "demigrate").
  // Threaded so each CellView's visual `checked` can reflect a pending
  // dirty edit — necessary for the whole-column toggle to visually flip
  // cells it queues, not just the cell the operator clicked directly.
  pending?: Map<string, Direction>;
  applyGen: number;
  onToggle: (server: string, client: string, nextChecked: boolean, initialChecked: boolean) => void;
  // Whole-row enable-all/disable-all for this server, the row analog of the
  // column header toggle. Mutates only the parent dirty map via the shared
  // flipCellGroup owner, so Apply semantics are identical to per-cell edits.
  onRowToggle: (server: ServerRow) => void;
  // G1 hover-scope: pointer entered (true) / left (false) the row toggle, so
  // the parent can set/clear the "row" hover scope that previews which cells a
  // row toggle would flip.
  onRowToggleHover: (hovering: boolean) => void;
  // The currently-hovered bulk-toggle scope (col/row) threaded from the parent
  // so each CellView can light up when it falls inside the previewed group.
  hoverScope: HoverScope | null;
  // Opens the per-row detail drawer (manifest preview + lifetime stats +
  // Stop/Restart). Parent owns the open flag.
  onOpenDrawer: () => void;
  // A3 PR-2: a config-error-symlink cell's "Resolve symlink" write succeeded;
  // the parent rescans so the cell re-classifies to "ok".
  onSymlinkResolved: () => void;
  applying: boolean;
}) {
  const { server, clients, status, outcomes, pending, onToggle, onRowToggle, onRowToggleHover, hoverScope, onOpenDrawer, onSymlinkResolved, applying } = props;
  // Row-toggle affordance state: a cell is "checked" if a pending dirty
  // edit says so, else the scan baseline (via-hub). rowInteractive uses the
  // same cellInteractive gate as the per-cell checkbox + the column toggle,
  // so the row toggle flips exactly the cells the operator could flip by
  // hand. The toggle is only offered when the row has ≥1 toggleable cell.
  const rowEffectiveChecked = (client: string): boolean => {
    const p = pending?.get(client);
    if (p) return p === "migrate";
    return (server.routing[client] ?? "not-installed") === "via-hub";
  };
  const rowInteractive = clients.filter((c) =>
    cellInteractive(server.routing[c] ?? "not-installed", applying),
  );
  const rowToggleable = rowInteractive.length > 0;
  const rowAllChecked = rowToggleable && rowInteractive.every(rowEffectiveChecked);
  return (
    <tr>
      {/* The WHOLE server cell is the row-toggle hover target (not just the
          ⇄ glyph): hovering anywhere in the server's own cell previews the
          horizontal bulk-toggle scope — highlights every toggleable client
          cell in this row. Gated on rowToggleable so a non-interactive row
          lights nothing.
          G15 a11y: this is the row-header cell, so it is a <th scope="row">
          (associates every client toggle in the row with its server name for
          screen readers). CSS normalizes the tbody th back to body-cell
          weight/background so the visual is unchanged. */}
      <th
        scope="row"
        onMouseEnter={rowToggleable ? () => onRowToggleHover(true) : undefined}
        onMouseLeave={rowToggleable ? () => onRowToggleHover(false) : undefined}
      >
        {rowToggleable && (
          <span
            class={`matrix-row-toggle${rowAllChecked ? " matrix-row-toggle--full" : ""}`}
            data-testid={`matrix-row-toggle-${server.name}`}
            role="button"
            tabIndex={0}
            aria-pressed={rowAllChecked}
            title={`Toggle all visible clients for ${server.name}`}
            onClick={() => onRowToggle(server)}
            onKeyDown={(ev: KeyboardEvent) => {
              if (ev.key === "Enter" || ev.key === " ") {
                ev.preventDefault();
                onRowToggle(server);
              }
            }}
          >
            ⇄
          </span>
        )}
        <a
          href={`#/edit-server?name=${encodeURIComponent(server.name)}`}
          data-action="edit-server"
        >
          {server.name}
        </a>
        {/* Per-row detail drawer trigger (Flowbite Drawer): manifest
            preview + lifetime stats + Stop/Restart. Kept a plain text
            button so existing matrix selectors (edit-server link, the
            row/column toggles) are untouched. */}
        <button
          type="button"
          class="matrix-row-details"
          data-testid={`server-row-details-${server.name}`}
          title={`Open details for ${server.name} (manifest, stats, stop/restart)`}
          onClick={onOpenDrawer}
        >
          Details
        </button>
      </th>
      {clients.map((client) => (
        <CellView
          key={`${client}-${props.applyGen}`}
          server={server}
          client={client}
          lastOutcome={outcomes?.get(client)}
          pendingDirection={pending?.get(client)}
          // G1 hover-scope preview: this cell is inside the hovered group when
          // a column-header toggle for this client is hovered, OR the row
          // toggle for THIS server is hovered. CellView still gates the actual
          // highlight on the cell being interactive, so disabled cells in a
          // hovered column/row never light up.
          inHoverScope={
            hoverScope != null &&
            ((hoverScope.kind === "col" && hoverScope.key === client) ||
              (hoverScope.kind === "row" && hoverScope.key === server.name))
          }
          onToggle={onToggle}
          onSymlinkResolved={onSymlinkResolved}
          applying={applying}
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

// SymlinkResolveAffordance is the A3 PR-2 "Resolve symlink → write to real
// target" cell affordance for a `config-error-symlink` cell. It is fully
// self-contained: it owns its busy/modal/error state and drives the two-phase
// POST (resolve → confirm → write) itself. On a successful write it calls
// `onResolved` so the parent rescans (the cell then re-classifies to "ok").
//
// The confirm modal shows the PINNED real path the symlink resolves to — that
// is the path the operator consents to, and the server re-verifies the same
// pin at write time (a swap between confirm and write is refused). Strict mode
// surfaces a WRITE_REFUSED with the canonical hint instead of following.
function SymlinkResolveAffordance(props: {
  client: string;
  onResolved: () => void;
}) {
  const { client, onResolved } = props;
  const [busy, setBusy] = useState(false);
  const [pending, setPending] = useState<ResolveSymlinkResult | null>(null);
  const [err, setErr] = useState<string | null>(null);

  async function openConfirm() {
    if (busy) return;
    setBusy(true);
    setErr(null);
    try {
      const res = await resolveClientSymlink(client);
      setPending(res);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function confirmWrite() {
    if (!pending || busy) return;
    setBusy(true);
    setErr(null);
    try {
      await writeClientSymlink(client, pending.pinned_real_path, pending.content_hash);
      setPending(null);
      onResolved();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <span class="symlink-resolve-affordance">
      <button
        type="button"
        class="symlink-resolve-btn"
        data-testid={`resolve-symlink-${client}`}
        disabled={busy}
        title={`This ${client} config is a symlink. Resolve it and write to the real target (per-config, no restart).`}
        onClick={openConfirm}
      >
        Resolve symlink
      </button>
      {err && (
        <span class="symlink-resolve-error" role="alert" data-testid={`resolve-symlink-error-${client}`}>
          {err}
        </span>
      )}
      {pending && (
        <div
          class="modal-backdrop symlink-resolve-modal"
          role="dialog"
          aria-modal="true"
          aria-label="Confirm symlink resolution"
          data-testid={`resolve-symlink-modal-${client}`}
        >
          <div class="modal-card">
            <h3>Resolve {client} config symlink</h3>
            <p>
              This config is a symlink. Its real target is{" "}
              <code data-testid={`resolve-symlink-pinned-${client}`}>{pending.resolved_target}</code>.
            </p>
            <p>
              mcphub will write to that target (re-stamping it with hardened
              owner-only permissions). Write there?
            </p>
            <div class="modal-actions">
              <button
                type="button"
                class="btn-secondary"
                data-testid={`resolve-symlink-cancel-${client}`}
                disabled={busy}
                onClick={() => setPending(null)}
              >
                Cancel
              </button>
              <button
                type="button"
                class="btn-primary"
                data-testid={`resolve-symlink-confirm-${client}`}
                disabled={busy}
                onClick={confirmWrite}
              >
                Write to real target
              </button>
            </div>
          </div>
        </div>
      )}
    </span>
  );
}

function CellView(props: {
  server: ServerRow;
  client: string;
  onSymlinkResolved?: () => void;
  lastOutcome?: Outcome;
  // Pending dirty direction for THIS cell (from the parent's dirty map).
  // When present it overrides the scan baseline for the visual checkbox:
  // "migrate" → checked, "demigrate" → unchecked. This lets the
  // whole-column header toggle (which only mutates the parent dirty map)
  // visually flip cells it queues, not merely the one the operator
  // clicked directly. A direct per-cell click also flows through dirty,
  // so the same path keeps the visual honest in every case.
  pendingDirection?: Direction;
  // G1 hover-scope: true when a hovered column/row bulk toggle covers this
  // cell. The visual highlight is gated additionally on the cell being
  // interactive (see `disabled` below) so a hovered column/row only lights up
  // the cells a click would actually flip — port/state/disabled cells stay
  // dark.
  inHoverScope?: boolean;
  onToggle: (server: string, client: string, nextChecked: boolean, initialChecked: boolean) => void;
  applying: boolean;
}) {
  const { server, client, onSymlinkResolved, lastOutcome, pendingDirection, inHoverScope, onToggle, applying } = props;
  // Treat undefined routing as "not-installed" — perClientRouting only
  // populates keys present in /api/scan's client_presence map.
  const routing: Routing = server.routing[client] ?? "not-installed";
  // "via-hub-inherited" IS hub-routed (rendered CHECKED) but read-only/disabled
  // — the hub cannot demigrate an inherited (import / below-write-target) entry
  // it never wrote, so the cell shows checked-but-disabled (see `disabled` +
  // cellInteractive below), not a toggleable demigrate switch.
  const initialChecked = routing === "via-hub" || routing === "via-hub-inherited";
  // effectiveChecked folds a pending dirty edit over the scan baseline so a
  // column toggle (or any out-of-band dirty mutation) is reflected visually.
  const effectiveChecked = pendingDirection
    ? pendingDirection === "migrate"
    : initialChecked;
  const [checked, setChecked] = useState(effectiveChecked);
  // Keep local `checked` in sync with effectiveChecked. It changes when the
  // authoritative scan moves the cell (direct→via-hub after a reload), OR
  // when the parent dirty map flips this cell out-of-band (the whole-column
  // toggle). Deps `[effectiveChecked]` means unrelated parent re-renders do
  // not stomp the value, but a genuine baseline/dirty change does re-sync.
  useEffect(() => {
    setChecked(effectiveChecked);
  }, [effectiveChecked]);
  // Disable when cell is meaningless:
  //  - "unsupported"   : this client cannot route this server via the hub
  //  - "not-installed" : this client's config file does not exist on disk
  // "via-hub", "direct", and "available" are all INTERACTIVE.
  // "via-hub" → uncheck + Apply posts /api/demigrate (B1 memo §4 D5).
  // "direct" / "available" → check + Apply posts /api/migrate.
  //
  // Bug-bash A2 (#13) closure: "available" is the cell state for "this
  // client's config file exists but currently has no entry for this
  // server". Pre-fix, that state was missing — clients with empty
  // mcpServers were indistinguishable from "client absent" and the UI
  // disabled the whole column, locking the operator out of re-adding
  // servers via Apply. The new state-machine includes "available" as
  // an enabled-but-unchecked cell.
  const disabled =
    applying ||
    routing === "unsupported" ||
    routing === "not-installed" ||
    routing === "config-error" ||
    routing === "config-error-symlink" ||
    // "via-hub-inherited" is hub-routed but read-only: the hub never wrote the
    // inherited (import / below-write-target) source and cannot demigrate it,
    // so the cell renders checked-but-disabled (kiro disabled-cell precedent).
    routing === "via-hub-inherited";
  let title: string | undefined;
  if (routing === "via-hub") {
    title = `Currently routed through the hub. Uncheck and Apply to roll this binding back to the original ${client} config.`;
  } else if (routing === "via-hub-inherited") {
    // The hub-loopback entry exists but its SOURCE is a layer the hub never
    // wrote — chiefly the ~/.claude.json mcpServers import, or a config.json
    // layer below ${client}'s write target. The hub cannot demigrate what it
    // did not write, so this cell is read-only; the operator edits the source
    // config to remove it (kiro disabled-cell precedent).
    title = `Routed through the hub, but ${client} inherits this entry from a config layer mcphub never wrote (e.g. ~/.claude.json, or a lower config.json layer). mcphub cannot roll it back — edit that source config directly to remove it.`;
  } else if (routing === "direct") {
    title = `${client} has a direct (non-hub) entry for this server. Check and Apply to route it through the hub.`;
  } else if (routing === "available") {
    title = `${client} has no entry for this server yet. Check and Apply to install it.`;
  } else if (routing === "not-installed") {
    title = `${client}'s MCP config file is not present on this host — nothing to install.`;
  } else if (routing === "unsupported") {
    title = `${client} cannot route this server through the hub (e.g., per-session servers).`;
  } else if (routing === "config-error") {
    // v0.4.5 PR #208 deep-sec Lane B follow-up: distinguish "stat
    // returned an error" from "file absent" so the operator sees an
    // actionable diagnostic instead of the misleading "not present"
    // tooltip. Typical causes: parent-directory permissions blocked,
    // antivirus quarantine, or I/O fault on the underlying volume.
    title = `${client}'s MCP config file could not be read (stat error). Check file permissions and disk health, then refresh.`;
  } else if (routing === "config-error-symlink") {
    // 2026-05-19 message-accuracy fix: the config path is a symlink.
    // The prior generic "stat error" tooltip sent operators to inspect
    // disk/permissions instead of their dotfile-symlink setup.
    // 2026-06-02 opt-in-accuracy fix: this status fires ONLY in default
    // mode (env unset) or strict mode. With the
    // MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK opt-in set on a non-strict
    // host, scan reports "ok" and writes resolve the symlink, so the
    // tooltip leads with that supported remediation rather than
    // claiming an unconditional refusal. Strict mode
    // (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) overrides the opt-in and still
    // refuses — the "single-user host" phrasing covers that.
    // 2026-06-03 opt-in-qualification fix: the opt-in flips a symlink to
    // "ok" in probeClientConfigPresence ONLY when os.Stat resolves to a
    // REGULAR file (scan.go ~L154: rst.Mode().IsRegular()). A DANGLING
    // symlink, or one pointing at a directory / special file, stays
    // "error-symlink" even with the env var set, so the opt-in clause is
    // qualified "if the symlink points at a regular file"; the
    // replace/edit fallbacks remain the path for dangling/non-regular
    // targets.
    // 2026-06-03 opt-in-restart fix (Codex PR #258 P3): the opt-in is read
    // per-process at runtime — OperatorAllowsClientConfigSymlink()
    // (client_write_init.go ~L419) calls os.Getenv on every check. A
    // running GUI/server process does NOT observe an env var the operator
    // exports into their shell AFTER startup, so a browser refresh keeps
    // returning error-symlink. The remediation therefore says to RESTART
    // mcphub with the env var set, not merely refresh.
    // work-items/bugs/2026-05-19-codex-config-symlink-blocked-by-pr209.md.
    title = `${client}'s MCP config file is a symlink. By default mcphub refuses symlinked client configs (confused-deputy protection, PR #209). On a single-user host, if the symlink points at a regular file, you can opt in: set MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK=1 and restart mcphub (a running process won't pick up a newly-set env var). Otherwise (e.g. a dangling symlink or one pointing at a directory) replace the symlink with a real file, or edit the symlink target's config directly.`;
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
  // v0.5.x Task 4.3 — dual-badge chip when this cell ALSO has a
  // legacy_conflict entry for the same client (coexistence anomaly:
  // hub URL + direct-stdio both target this client). The checkbox
  // visual stays so the existing migrate/demigrate UX is preserved;
  // the chip is a non-interactive marker that surfaces the anomaly.
  const hasLegacyConflict = Boolean(server.legacyConflict?.[client]);
  return (
    <td class={retryPending ? "matrix-cell-retry-pending" : ""} data-retry-pending={retryPending ? "true" : undefined}>
      {/* Feature 1 — whole-cell click target: a <label> fills the cell so a
          native label-click toggles the wrapped control exactly once (no JS
          double-fire). When the cell is disabled the label carries the
          .disabled modifier (default cursor, no toggle — clicking a label
          bound to a disabled control is a native no-op).

          The control is now a polished ToggleSwitch (shared component) instead
          of a raw checkbox, but it is STILL a real <input type="checkbox"> at
          its core — every existing selector (`input[type="checkbox"]`), the
          `.checked`/`.disabled` mirrors, and the onChange → onToggle path are
          unchanged. `pending` lights the unsaved-edit cue (accent ring/track)
          so a dirty toggle still reads visibly different from a clean one,
          mirroring what the raw-checkbox + retry-outline gave before. The
          title carries the per-cell help text exactly as before. */}
      <label
        class={`matrix-cell-label${disabled ? " disabled" : ""}${
          inHoverScope && !disabled ? " matrix-cell-hover-scope" : ""
        }`}
      >
        <ToggleSwitch
          checked={checked}
          disabled={disabled}
          pending={pendingDirection !== undefined}
          title={cellTitle}
          onChange={(ev) => {
            const next = (ev.currentTarget as HTMLInputElement).checked;
            setChecked(next);
            onToggle(server.name, client, next, initialChecked);
          }}
        />
      </label>
      {hasLegacyConflict && (
        <span
          class="matrix-cell-legacy-chip"
          data-testid={`legacy-chip-${server.name}-${client}`}
          title={`A legacy non-hub entry for ${server.name} also exists in ${client}'s config alongside the hub binding. Resolve in ${client}'s mcp config directly.`}
        >
          legacy
        </span>
      )}
      {/* A3 PR-2: the explicit per-config symlink-follow ENABLE the operator
          asked for. The cell is disabled (the symlinked config refuses writes
          by default), so the affordance is the way to opt in without setting
          the global env var + restarting. */}
      {routing === "config-error-symlink" && onSymlinkResolved && (
        <SymlinkResolveAffordance client={client} onResolved={onSymlinkResolved} />
      )}
    </td>
  );
}

// LspMatrix renders the 9-row LSP daemons table below the main matrix.
// Workspace-scoped LSP rows do not use migrate/demigrate checkboxes,
// but unregistered rows can be enabled from the row action when the
// current workspace filter resolves to exactly one workspace.
// Cells surface presence as badges: `[via-hub]`, `[direct]`, `[legacy]`,
// or `—` for absent.
//
// Row-level affordances: clicking the "Edit env" button opens the
// EnvDrawer for that row's taskName (when registered). Placeholder
// rows (no workspace entry) render a "(register first)" hint instead.
function LspMatrix(props: {
  rows: LspRow[];
  clients: readonly string[];
  openDrawerFor: LspRow | null;
  onOpenDrawer: (row: LspRow) => void;
  targetWorkspacePath: string;
  registerBusy: Record<string, boolean>;
  registerMsg: { text: string; kind: "ok" | "error" } | null;
  onRegister: (row: LspRow) => void;
}) {
  const {
    rows,
    clients,
    openDrawerFor,
    onOpenDrawer,
    targetWorkspacePath,
    registerBusy,
    registerMsg,
    onRegister,
  } = props;
  return (
    <section class="lsp-matrix-section" data-testid="lsp-matrix-section">
      <h2>LSP daemons</h2>
      <p class="lsp-matrix-intro">
        Workspace-scoped language servers route through shared hub proxies.
      </p>
      {registerMsg && (
        <div
          class={registerMsg.kind === "error" ? "error" : ""}
          data-testid="lsp-register-msg"
          style="margin:var(--gap-xs) 0"
        >
          {registerMsg.text}
        </div>
      )}
      <table class="servers-matrix lsp-matrix" data-testid="lsp-matrix">
        {/* G15 a11y: caption + scope= mirror the main servers matrix so the
            LSP daemons table is equally navigable by screen readers. */}
        <caption class="visually-hidden">
          Language servers by client install matrix: each row is a language,
          each column a client, showing how that language server routes for
          the client.
        </caption>
        <thead>
          <tr>
            <th scope="col">Language</th>
            {clients.map((c) => (
              <th key={c} scope="col">{c}</th>
            ))}
            <th scope="col">Env</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => {
            const isOpen = openDrawerFor?.language === row.language && openDrawerFor?.taskName === row.taskName;
            const registered = row.taskName !== null;
            const ambiguous = (row.ambiguousOwners?.length ?? 0) > 1;
            return (
              <tr
                key={`${row.language}-${row.workspaceKey}`}
                class={isOpen ? "lsp-row-open" : ""}
                data-testid={`lsp-row-${row.language}`}
                data-registered={registered ? "true" : "false"}
                data-workspace={row.workspaceKey || undefined}
                data-ambiguous={ambiguous ? "true" : undefined}
              >
                {/* G15 a11y: row-header cell for the LSP matrix. */}
                <th scope="row">
                  <strong>{row.language}</strong>
                  {row.workspaceKey && (
                    <span class="lsp-row-workspace">
                      ({row.workspaceKey})
                    </span>
                  )}
                  {ambiguous && (
                    <span
                      class="lsp-row-ambiguous"
                      data-testid={`lsp-row-ambiguous-${row.language}`}
                    >
                      (multi: {row.ambiguousOwners!.join(", ")})
                    </span>
                  )}
                </th>
                {clients.map((client) => (
                  <LspCellView
                    key={client}
                    language={row.language}
                    client={client}
                    presence={row.clientPresence[client]}
                    legacy={row.legacyConflict[client]}
                  />
                ))}
                <td>
                  {registered ? (
                    <button
                      type="button"
                      onClick={() => onOpenDrawer(row)}
                      data-testid={`lsp-edit-env-${row.language}`}
                    >
                      {isOpen ? "Editing…" : "Edit env"}
                    </button>
                  ) : ambiguous ? (
                    // Ambiguity in ALL-workspaces mode: silently picking
                    // one workspace's task_name (the pre-fix behavior)
                    // could land an Apply in the wrong workspace. Block
                    // Edit-env until the operator narrows the filter via
                    // the WorkspaceSelector at the top of the screen.
                    // Bot review PR #222 P2 (lsp-rows.ts:122).
                    <span
                      class="lsp-row-ambiguous-hint"
                      data-testid={`lsp-row-ambiguous-hint-${row.language}`}
                    >
                      pick a workspace above to edit env
                    </span>
                  ) : (
                    <button
                      type="button"
                      class="lsp-row-enable"
                      data-testid={`lsp-enable-${row.language}`}
                      disabled={!targetWorkspacePath || registerBusy[row.language] === true}
                      title={
                        targetWorkspacePath
                          ? `Register ${row.language} for ${targetWorkspacePath}`
                          : "Pick one workspace above before enabling this LSP daemon."
                      }
                      onClick={() => onRegister(row)}
                    >
                      {registerBusy[row.language] === true ? "Enabling…" : "Enable"}
                    </button>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </section>
  );
}

// LspCellView renders one LSP row's per-client cell. Badge semantics:
//   - presence.transport === "http"  -> [via-hub]
//   - presence.transport === "stdio" -> [direct]
//   - legacy populated               -> [legacy] (stacks under [via-hub] or [direct])
//   - none                           -> "-"
function LspCellView(props: {
  language: string;
  client: string;
  presence: ClientPresence | undefined;
  legacy: ClientEntry | undefined;
}) {
  const { language, client, presence, legacy } = props;
  const t = presence?.transport;
  const hasPrimary = t === "http" || t === "relay" || t === "stdio";
  const dualBadge = hasPrimary && Boolean(legacy);
  return (
    <td
      class="lsp-cell"
      data-testid={`lsp-cell-${language}-${client}`}
      data-dual-badge={dualBadge ? "true" : undefined}
    >
      {hasPrimary && (
        <span
          class={`lsp-chip lsp-chip-${t === "stdio" ? "direct" : "via-hub"}`}
          data-testid={`lsp-chip-primary-${language}-${client}`}
        >
          {t === "stdio" ? "direct" : "via-hub"}
        </span>
      )}
      {legacy && (
        <span
          class="lsp-chip lsp-chip-legacy"
          data-testid={`lsp-chip-legacy-${language}-${client}`}
        >
          legacy
        </span>
      )}
      {!hasPrimary && !legacy && <span class="lsp-cell-empty">—</span>}
    </td>
  );
}
