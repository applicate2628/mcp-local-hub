import { useEffect, useRef, useState } from "preact/hooks";
import {
  fetchOrThrow,
  postInitClientConfig,
  InitClientConfigError,
  listWorkspaces,
  postLspRegister,
  type WorkspaceEntryDTO,
  type WorkspacePair,
} from "../api";
import { useEventSource } from "../hooks/useEventSource";
import { collectServers } from "../lib/routing";
import { collectLspRows, type LspRow, LSP_KNOWN_CLIENTS, LSP_MANIFEST_SERVER } from "../lib/lsp-rows";
import { aggregateStatus, stateShape } from "../lib/status";
import { WorkspaceSelector, ALL_WORKSPACES_KEY } from "../components/WorkspaceSelector";
import { EnvDrawer } from "../components/EnvDrawer";
import type {
  ClientConfigState,
  ClientEntry,
  ClientPresence,
  DaemonStatus,
  ScanResult,
  ServerRow,
  Routing,
} from "../types";

const CLIENTS = [
  "claude-code",
  "codex-cli",
  "cursor",
  "vscode",
  "gemini-cli",
  "qwen-cli",
  "antigravity",
] as const;

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

export function ServersScreen() {
  const [servers, setServers] = useState<ServerRow[] | null>(null);
  const [statusByServer, setStatusByServer] = useState<Record<string, { state: string; port: number | null }>>({});
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

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        // /api/workspaces returns {workspaces, entries}. A registry
        // load failure must NOT block the matrix render — the
        // selector falls back to its empty-state placeholder and the
        // LSP matrix surfaces the 9 placeholder rows. Catch isolation
        // is bounded to the workspaces fetch only; /api/scan and
        // /api/status errors continue to fail the whole effect.
        const [scan, status, workspacesResp] = await Promise.all([
          fetchOrThrow<ScanResult>("/api/scan", "object"),
          fetchOrThrow<DaemonStatus[]>("/api/status", "array"),
          listWorkspaces().catch(() => ({ workspaces: [], entries: [] })),
        ]);
        if (cancelled) return;
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
        if (!cancelled) setError((err as Error).message);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [reloadToken]);

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

    if (failed.length === 0) {
      setApplyMsg("Applied. Refreshing…");
      setFailedRows([]);
    } else {
      // Bug-bash B1 closure (#7): each failed entry becomes its own
      // <li> row in the toolbar list (see render below). applyMsg
      // just shows the count + reminder; the wall-of-text in a
      // single string is gone.
      setApplyMsg(`Failed: ${failed.length} row(s); re-toggle and retry below.`);
      setFailedRows(failed);
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

  // v0.5.x Task 4.3 — LSP rows are always 9 (one per language). When
  // a workspace is selected, the helper scopes each row's task_name +
  // presence to that workspace's entries; otherwise every workspace's
  // entries fold into the same row and the matrix surfaces the union
  // (with coexistence rendering as dual badges per cell).
  const lspRows = collectLspRows(scanForLsp, workspaceEntries, selectedWorkspaceKey);
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
      await postLspRegister(lspRegisterWorkspacePath, row.language);
      if (!mountedRef.current) return;
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
      <h1>Servers</h1>
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
        <button onClick={applyChanges} disabled={applyDisabled}>
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
          style="margin:8px 0"
        >
          {initMsg.text}
        </div>
      )}
      <table class="servers-matrix">
        <thead>
          <tr>
            <th>Server</th>
            {CLIENTS.map((c) => {
              const presence = clientConfigPresence[c];
              const canInit = presence === "missing-init-possible";
              const busy = initBusy[c] === true;
              return (
                <th key={c}>
                  <div class="matrix-col-header">
                    <span>{c}</span>
                    {canInit && (
                      <button
                        type="button"
                        class="matrix-col-init-btn"
                        data-testid={`init-client-${c}`}
                        disabled={busy}
                        title={`${c}'s MCP config file is not present on this host, but its parent directory exists. Click to seed an empty stub so this column becomes active.`}
                        onClick={() => initializeClient(c)}
                      >
                        {busy ? "Init…" : "Initialize"}
                      </button>
                    )}
                  </div>
                </th>
              );
            })}
            <th>Port</th>
            <th>State</th>
          </tr>
        </thead>
        <tbody>
          {manifestedServers.map((server) => (
            <ServerRowView
              key={server.name}
              server={server}
              status={statusByServer[server.name]}
              outcomes={outcomes.get(server.name)}
              applyGen={applyGen}
              onToggle={toggleCell}
              applying={applying}
            />
          ))}
        </tbody>
      </table>
      <LspMatrix
        rows={lspRows}
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
    <details class="other-mcp-entries" style="margin-top:24px">
      <summary>
        <strong>Other MCP entries ({servers.length})</strong>
        {" — "}
        legacy or third-party MCP servers detected in client configs;
        no mcphub manifest, so they can't be migrated through this matrix
      </summary>
      <ul style="font-family:monospace; font-size:0.9em; margin-top:8px">
        {servers.map((s) => {
          const clientsWithEntry = Object.entries(s.routing)
            .filter(([, r]) => r === "via-hub" || r === "direct")
            .map(([c]) => c);
          return (
            <li key={s.name}>
              <code>{s.name}</code>
              {clientsWithEntry.length > 0 && (
                <span style="color:#666"> — in: {clientsWithEntry.join(", ")}</span>
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
  status?: { state: string; port: number | null };
  outcomes?: Map<string, Outcome>;
  applyGen: number;
  onToggle: (server: string, client: string, nextChecked: boolean, initialChecked: boolean) => void;
  applying: boolean;
}) {
  const { server, status, outcomes, onToggle, applying } = props;
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
          key={`${client}-${props.applyGen}`}
          server={server}
          client={client}
          lastOutcome={outcomes?.get(client)}
          onToggle={onToggle}
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

function CellView(props: {
  server: ServerRow;
  client: string;
  lastOutcome?: Outcome;
  onToggle: (server: string, client: string, nextChecked: boolean, initialChecked: boolean) => void;
  applying: boolean;
}) {
  const { server, client, lastOutcome, onToggle, applying } = props;
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
    routing === "config-error-symlink";
  let title: string | undefined;
  if (routing === "via-hub") {
    title = `Currently routed through the hub. Uncheck and Apply to roll this binding back to the original ${client} config.`;
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
      {hasLegacyConflict && (
        <span
          class="matrix-cell-legacy-chip"
          data-testid={`legacy-chip-${server.name}-${client}`}
          title={`A legacy non-hub entry for ${server.name} also exists in ${client}'s config alongside the hub binding. Resolve in ${client}'s mcp config directly.`}
        >
          legacy
        </span>
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
  openDrawerFor: LspRow | null;
  onOpenDrawer: (row: LspRow) => void;
  targetWorkspacePath: string;
  registerBusy: Record<string, boolean>;
  registerMsg: { text: string; kind: "ok" | "error" } | null;
  onRegister: (row: LspRow) => void;
}) {
  const {
    rows,
    openDrawerFor,
    onOpenDrawer,
    targetWorkspacePath,
    registerBusy,
    registerMsg,
    onRegister,
  } = props;
  return (
    <section class="lsp-matrix-section" data-testid="lsp-matrix-section" style="margin-top:24px">
      <h2>LSP daemons</h2>
      <p class="lsp-matrix-intro" style="color:#555; margin-bottom:8px">
        Workspace-scoped language servers route through shared hub proxies.
      </p>
      {registerMsg && (
        <div
          class={registerMsg.kind === "error" ? "error" : ""}
          data-testid="lsp-register-msg"
          style="margin:8px 0"
        >
          {registerMsg.text}
        </div>
      )}
      <table class="servers-matrix lsp-matrix" data-testid="lsp-matrix">
        <thead>
          <tr>
            <th>Language</th>
            {LSP_KNOWN_CLIENTS.map((c) => (
              <th key={c}>{c}</th>
            ))}
            <th>Env</th>
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
                <td>
                  <strong>{row.language}</strong>
                  {row.workspaceKey && (
                    <span class="lsp-row-workspace" style="color:#555; font-size:0.9em; margin-left:6px">
                      ({row.workspaceKey})
                    </span>
                  )}
                  {ambiguous && (
                    <span
                      class="lsp-row-ambiguous"
                      data-testid={`lsp-row-ambiguous-${row.language}`}
                      style="color:#bf8700; font-size:0.85em; margin-left:6px"
                    >
                      (multi: {row.ambiguousOwners!.join(", ")})
                    </span>
                  )}
                </td>
                {LSP_KNOWN_CLIENTS.map((client) => (
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
                      style="color:#bf8700; font-size:0.9em"
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
