import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import {
  fetchOrThrow,
  removeClientEntry,
  ClientEntryRemovalError,
  postInitClientConfig,
  InitClientConfigError,
  listLspRouterClientStatuses,
  listWorkspaces,
  postLspRegister,
  resolveClientSymlink,
  writeClientSymlink,
  type LspRegisterResponse,
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
import {
  collectLspRows,
  lspCellState,
  type LspRow,
  LSP_MANIFEST_SERVER,
  LSP_LANGUAGES,
} from "../lib/lsp-rows";
import { aggregateStatus, stateShape } from "../lib/status";
import { pushToast } from "../lib/toast-store";
import { WorkspaceSelector, ALL_WORKSPACES_KEY } from "../components/WorkspaceSelector";
import { MatrixColumnsMenu } from "../components/MatrixColumnsMenu";
import { EnvDrawer } from "../components/EnvDrawer";
import { ServerRowDrawer } from "../components/ServerRowDrawer";
import { ToggleSwitch } from "../components/ToggleSwitch";
import { LoadingState } from "../components/LoadingState";
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
// key per clients.SupportedClientNames(), all 47 backend clients), so any
// supported client surfaces when installed — no hardcoded column list to
// drift behind the backend registry.
// On top of that auto-detected base the operator's manual show/hide
// overrides (persisted in localStorage, see lib/matrix-columns.ts) are
// folded in via effectiveVisibleClients() — the "Manage columns" popover
// lets them pin an undetected column visible or hide a noisy one. This is
// a pure VIEW filter: hidden columns simply don't render. The computed list is threaded through
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

// EMPTY_CLIENT_SCAN_ERRORS is the stable empty reference for the per-client
// parse/read-error map, mirroring EMPTY_CLIENT_CONFIG_PRESENCE so a scan that
// omits client_scan_errors (the common no-error case + older mocks) does not
// churn a fresh `{}` into state on every refresh and trip the render-timing
// bailout above.
const EMPTY_CLIENT_SCAN_ERRORS: Record<string, string> = {};

// One intent per observed server/client. Removal retains the opaque member
// token through refresh/retry; routing checked state is distinct from presence.
type CellIntent = { operation: "migrate" | "undo" } |
  { operation: "remove-client-entry"; sourceToken: string | null; refusal?: string };
type DirtyMap = Map<string, Map<string, CellIntent>>;
type RemovalCell = { server: string; client: string; sourceToken: string | null };
type Outcome = "succeeded" | "failed" | "gated";
type OutcomeMap = Map<string, Map<string, Outcome>>;

function hubChecked(routing: Routing): boolean {
  return routing === "via-hub" || routing === "via-hub-inherited";
}

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
// Hub entries admit OFF removal; inherited entries require removal capability.
// Direct and available entries admit ON only with a manifest. Everything else
// ("not-installed", "unsupported", "config-error", "config-error-symlink",
// and an undefined/absent client key → treated as "not-installed") is a
// disabled, non-toggleable cell. The `applying` guard is folded in so an
// in-flight Apply freezes both the per-cell checkbox AND the column toggle.
function cellInteractive(routing: Routing, applying: boolean, removableInherited = false, manifested = true): boolean {
  if (applying) return false;
  if (routing === "via-hub" || (routing === "via-hub-inherited" && removableInherited)) return true;
  return manifested && (routing === "direct" || routing === "available");
}

function objectRecord(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function apiErrorMessage(body: unknown, fallback: string): string {
  const obj = objectRecord(body);
  return typeof obj?.error === "string" ? obj.error : fallback;
}

type LspRegistrationMessage = {
  text: string;
  kind: "ok" | "warning" | "error";
};

type LspRegistrationClassification = LspRegistrationMessage & {
  refresh: boolean;
};

type LspRegistrationAction =
  | { kind: "enable" }
  | { kind: "register"; workspacePath: string };

function sanitizedLspRegistrationWarnings(response: LspRegisterResponse): string[] {
  if (!Array.isArray(response.warnings)) return [];
  return response.warnings
    .filter((warning): warning is string => typeof warning === "string")
    .map((warning) => warning.replace(/[\u0000-\u001f\u007f]+/g, " ").replace(/\s+/g, " ").trim())
    .filter((warning) => warning.length > 0);
}

// The single response owner for both Servers-screen registration flows. A 2xx
// transport result is not success on its own: only the requested language's
// affirmative result authorizes a refresh. Warnings are partial success and
// remain visible instead of being overwritten by the clean-success copy.
function classifyLspRegistrationResponse(
  action: LspRegistrationAction,
  language: string,
  response: LspRegisterResponse,
): LspRegistrationClassification {
  const warnings = sanitizedLspRegistrationWarnings(response);
  const warningContext = warnings.length > 0 ? ` Warnings: ${warnings.join("; ")}` : "";
  const requestedResult = Array.isArray(response.results)
    ? response.results.find((result) => result?.language === language)
    : undefined;
  const actionVerb = action.kind === "enable" ? "Enable" : "Register";

  if (requestedResult?.status === "error") {
    return {
      kind: "error",
      text: `${actionVerb} ${language} failed: ${requestedResult.error ?? response.error ?? "unknown error"}.${warningContext}`,
      refresh: false,
    };
  }
  if (requestedResult?.status !== "ok") {
    return {
      kind: "error",
      text: `registration-unconfirmed: ${actionVerb} ${language} response did not confirm the requested language.${warningContext}`,
      refresh: false,
    };
  }

  if (warnings.length > 0) {
    const target = action.kind === "register"
      ? ` for ${response.workspace || action.workspacePath}`
      : "";
    return {
      kind: "warning",
      text: `Partial success ${action.kind === "enable" ? "enabling" : "registering"} ${language}${target}: ${warnings.join("; ")} Refreshing…`,
      refresh: true,
    };
  }

  return action.kind === "enable"
    ? { kind: "ok", text: `Enabled ${language}. Refreshing…`, refresh: true }
    : {
        kind: "ok",
        text: `Registered ${language} for ${response.workspace || action.workspacePath}. Refreshing…`,
        refresh: true,
      };
}

function lspRouterFailedCount(body: unknown): number {
  const obj = objectRecord(body);
  const report = objectRecord(obj?.report);
  const failed = report?.Failed ?? report?.failed;
  return Array.isArray(failed) ? failed.length : 0;
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
  // P2 scan per-client isolation: the adapter-level parse/read error message
  // (keyed by client id) recorded when a client's config could not be parsed
  // or read. ScanFrom now isolates such a failure to that one client (presence
  // → "error") and records the message here instead of aborting the whole scan.
  // Threaded into CellView so a config-error cell whose client has an entry
  // here renders a PARSE/read-failure diagnostic with the actual message,
  // distinct from the generic stat/permission anomaly.
  const [clientScanErrors, setClientScanErrors] = useState<
    Record<string, string>
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
  const [removalOpen, setRemovalOpen] = useState(false);
  const removalSelection: RemovalCell[] = [];
  for (const [server, cells] of dirty) {
    for (const [client, intent] of cells) {
      if (intent.operation === "remove-client-entry") removalSelection.push({ server, client, sourceToken: intent.sourceToken });
    }
  }
  const applyBusyRef = useRef(false);
  const [outcomes, setOutcomes] = useState<OutcomeMap>(new Map());
  const [applyMsg, setApplyMsg] = useState<string>("");
  const [applying, setApplying] = useState<boolean>(false);
  // Bug-bash B1 closure (#7): structured per-row failure list instead
  // of the legacy "; "-joined wall-of-text. Each entry is a short
  // "server/client: err" string that renders as one <li> in the toolbar
  // failure list. Empty array = no failures to render.
  const [failedRows, setFailedRows] = useState<string[]>([]);
  const [reloadToken, setReloadToken] = useState<number>(0);
  // Remount settled cells from the scan plus retained intent. Presence badges
  // remain scan-owned, so an unchecked failed removal never claims absence.
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
  const [lspRegisterMsg, setLspRegisterMsg] = useState<LspRegistrationMessage | null>(null);
  const [lspRouterBusy, setLspRouterBusy] = useState<Record<string, boolean>>({});
  const [lspRouterMsg, setLspRouterMsg] = useState<{ text: string; kind: "ok" | "error" } | null>(null);
  const [lspRouterClientOverrides, setLspRouterClientOverrides] = useState<Record<string, boolean>>({});
  const [lspRouterClientDisabled, setLspRouterClientDisabled] = useState<Record<string, boolean>>({});
  // First-workspace registration (fresh-machine connect path): the
  // operator types a workspace ROOT path + picks a language, and the GUI
  // POSTs /api/lsp/register directly — no CLI `mcphub register` step. The
  // LSP-daemons section renders this inline so a host with ZERO registered
  // workspaces is no longer a dead-end (WorkspaceSelector empty state +
  // every row's Enable button were both inert without a pre-selected
  // workspace). Busy guards the in-flight POST against a double-fire.
  const [workspacePathInput, setWorkspacePathInput] = useState<string>("");
  const [workspaceRegisterBusy, setWorkspaceRegisterBusy] = useState<boolean>(false);
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
  const loadScan = useCallback(async (markScanAccepted: () => void) => {
    const requestId = latestScanRequestRef.current + 1;
    latestScanRequestRef.current = requestId;
    const isLatest = () => latestScanRequestRef.current === requestId;
    try {
      // Promise.all is fail-fast, so every member is REQUIRED unless it carries
      // its own catch. Two members are OPTIONAL and must degrade in place:
      //
      //   - /api/workspaces returns {workspaces, entries}. A registry load
      //     failure must NOT block the matrix render — the selector falls back
      //     to its empty-state placeholder and the LSP matrix surfaces the 9
      //     placeholder rows.
      //   - /api/lsp-router/status only decorates the LSP rows with per-client
      //     opt-out state. Its backend (LSPRouterClientStatuses) aborts on a
      //     SINGLE client's GetEntry/config read failure, so one broken client
      //     config would otherwise blank the whole Servers matrix — even though
      //     /api/scan can still render that client as a config-error cell.
      //
      // /api/scan and /api/status stay REQUIRED: they supply the matrix rows
      // and the Port/State columns, so there is nothing left to render without
      // them, and their errors continue to fail the whole load.
      const [scan, status, workspacesResp, routerStatuses] = await Promise.all([
        fetchOrThrow<ScanResult>("/api/scan", "object"),
        fetchOrThrow<DaemonStatus[]>("/api/status", "array"),
        listWorkspaces().catch(() => ({ workspaces: [], entries: [] })),
        listLspRouterClientStatuses().catch(() => null),
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
      setClientScanErrors(scan.client_scan_errors ?? EMPTY_CLIENT_SCAN_ERRORS);
      // A failed status probe knows NOTHING about opt-outs, so it must not
      // overwrite the map with {} — that would read as "no client is opted
      // out" and silently flip a persisted opt-out cell back on. Keep the
      // last-known map (empty on first load) instead. The optimistic
      // per-client overrides sit ON TOP of that map, so they are cleared
      // together with it or not at all: clearing them against a stale
      // baseline would revert a toggle the backend already applied.
      if (routerStatuses !== null) {
        const routerDisabled: Record<string, boolean> = {};
        for (const rs of routerStatuses) {
          if (rs.disabled) routerDisabled[rs.client] = true;
        }
        setLspRouterClientDisabled(routerDisabled);
        setLspRouterClientOverrides({});
      }
      // Clear any success banner once the authoritative refresh lands
      // (the matrix has already redrawn with the new "ok" state).
      // Error banners stay sticky so the operator sees the failure
      // until they retry — refreshing should NOT mask a recent
      // PARENT_MISSING / INIT_FAILED report.
      setInitMsg((msg) => (msg && msg.kind === "ok" ? null : msg));
      setLspRegisterMsg((msg) => (msg && msg.kind === "ok" ? null : msg));
      setLspRouterMsg((msg) => (msg && msg.kind === "ok" ? null : msg));
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
      markScanAccepted();
    } catch (err) {
      if (isLatest()) setError((err as Error).message);
    }
  }, []);

  // Auto-refresh: 10s poll while mounted, paused while the tab is hidden,
  // immediate refresh on becoming visible again — AND paused while there
  // are unsaved matrix edits (`dirty.size > 0`) or a per-client LSP router
  // toggle is in flight. The pause is the no-clobber guarantee: an auto tick
  // never fires while local optimistic state is waiting on an operator POST.
  const hasUnsavedEdits = dirty.size > 0;
  const hasLspRouterToggleInFlight = Object.keys(lspRouterBusy).length > 0;
  const autoScanPaused = hasUnsavedEdits || hasLspRouterToggleInFlight || applying;
  const { rescan, markScanAccepted, agoSeconds } = useAutoScan(loadScan, autoScanPaused);

  // reloadToken-driven refetch for Apply / Initialize / LSP register / SSE.
  // useAutoScan already issues the initial mount fetch, so this effect
  // skips token 0 to avoid a redundant duplicate fetch on first paint.
  // These triggers fire AFTER the dirty set has been pruned (Apply) or are
  // operator-initiated (Initialize/register), so refetching here is safe
  // even though the auto-poll is paused while dirty.
  useEffect(() => {
    if (reloadToken === 0) return;
    void loadScan(markScanAccepted);
  }, [reloadToken, loadScan, markScanAccepted]);

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

  async function toggleLspRouterClient(client: string, enabled: boolean) {
    if (lspRouterBusy[client]) return;
    const endpoint = enabled ? "/api/lsp-router/enable" : "/api/lsp-router/disable";
    const label = enabled ? "Enable" : "Disable";
    setLspRouterBusy((prev) => ({ ...prev, [client]: true }));
    setLspRouterMsg(null);
    setLspRouterClientOverrides((prev) => ({ ...prev, [client]: enabled }));
    try {
      const resp = await fetch(endpoint, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ client }),
      });
      let body: unknown = null;
      try {
        body = await resp.json();
      } catch {
        // Non-JSON responses are handled by status below.
      }
      if (resp.status === 200 || resp.status === 204) {
        if (!mountedRef.current) return;
        setLspRouterMsg({
          kind: "ok",
          text: `${enabled ? "Enabled" : "Disabled"} LSP router entries for ${client}. Refreshing…`,
        });
        pushToast(
          "success",
          `${enabled ? "Enabled" : "Disabled"} LSP router entries for ${client}.`,
        );
        setReloadToken((n) => n + 1);
        return;
      }
      if (resp.status === 207) {
        if (!mountedRef.current) return;
        const failed = lspRouterFailedCount(body);
        setLspRouterMsg({
          kind: "error",
          text: `${label} ${client} partially failed (${failed || "some"} LSP router row(s)); refresh and retry.`,
        });
        setReloadToken((n) => n + 1);
        return;
      }
      throw new Error(`${endpoint}: ${apiErrorMessage(body, resp.statusText || `HTTP ${resp.status}`)}`);
    } catch (err) {
      if (!mountedRef.current) return;
      setLspRouterClientOverrides((prev) => {
        const next = { ...prev };
        delete next[client];
        return next;
      });
      setLspRouterMsg({
        kind: "error",
        text: `${label} ${client} failed: ${(err as Error).message}`,
      });
    } finally {
      if (mountedRef.current) {
        setLspRouterBusy((prev) => {
          const next = { ...prev };
          delete next[client];
          return next;
        });
      }
    }
  }

  // All selection surfaces change only this intent map, clearing only their
  // superseded outcomes. Client configuration is written only by Apply.
  function updateIntents(changes: { server: string; client: string; intent: CellIntent | null }[]) {
    if (applyBusyRef.current || applying) return;
    setDirty((previous) => {
      const next = new Map(previous);
      for (const { server, client, intent } of changes) {
        const row = new Map(next.get(server));
        if (intent) row.set(client, intent);
        else row.delete(client);
        if (row.size > 0) next.set(server, row);
        else next.delete(server);
      }
      return next;
    });
    setOutcomes((previous) => {
      const next = new Map(previous);
      for (const { server, client } of changes) {
        const row = new Map(next.get(server));
        row.delete(client);
        if (row.size > 0) next.set(server, row);
        else next.delete(server);
      }
      return next;
    });
    setFailedRows((previous) => previous.filter((message) => !changes.some(({ server, client }) =>
      message.startsWith(`${server}/${client}:`) ||
      (message.startsWith(`${server}/`) && message.includes(`/${client}:`)))));
    setApplyMsg("");
  }

  function removalIntent(server: string, client: string): CellIntent {
    const presence = removalPresence(scanForLsp, server, client);
    return { operation: "remove-client-entry", sourceToken: removalToken(presence),
      refusal: removalRefusal(presence?.removal?.reason) };
  }

  function toggleCell(server: string, client: string, nextChecked: boolean, initialChecked: boolean) {
    updateIntents([{ server, client, intent: nextChecked === initialChecked ? null :
      nextChecked ? { operation: "migrate" } : removalIntent(server, client) }]);
  }

  function selectRemoval(next: RemovalCell[]) {
    const changes: { server: string; client: string; intent: CellIntent | null }[] = [];
    for (const current of removalSelection) {
      if (!next.some((cell) => cell.server === current.server && cell.client === current.client)) changes.push({ ...current, intent: null });
    }
    for (const cell of next) {
      if (!removalSelection.some((current) => current.server === cell.server && current.client === cell.client)) {
        changes.push({ ...cell, intent: { operation: "remove-client-entry", sourceToken: cell.sourceToken } });
      }
    }
    updateIntents(changes);
  }

  function queueUndo(server: string, client: string) {
    updateIntents([{ server, client, intent: { operation: "undo" } }]);
  }

  // One executor/outcome/mount/in-flight owner for checkbox, bulk, explicit
  // removal and retry. The explicit removal button executes removal intents only.
  async function applyChanges(onlyRemoval = false) {
    if (applyBusyRef.current || applying) return;
    const selected: { server: string; client: string; intent: CellIntent }[] = [];
    for (const [server, row] of dirty) {
      for (const [client, intent] of row) {
        if (!onlyRemoval || intent.operation === "remove-client-entry") selected.push({ server, client, intent });
      }
    }
    if (selected.length === 0) return;
    const cells = [
      ...selected.filter((cell) => cell.intent.operation === "remove-client-entry"),
      ...selected.filter((cell) => cell.intent.operation === "undo"),
      ...selected.filter((cell) => cell.intent.operation === "migrate"),
    ];
    applyBusyRef.current = true;
    setApplying(true);
    setApplyMsg(onlyRemoval ? "Removing selected entries…" : "Applying…");
    const results: OutcomeMap = new Map();
    const failed: string[] = [];
    const blockedClients = new Set<string>();
    let unsettled = false;
    let succeeded = 0;
    function record(server: string, client: string, outcome: Outcome) {
      const row = results.get(server) ?? new Map<string, Outcome>();
      row.set(client, outcome);
      results.set(server, row);
    }
    try {
      for (const { server, client, intent } of cells) {
        if (!mountedRef.current) return;
        if (unsettled || (intent.operation !== "remove-client-entry" && blockedClients.has(client))) {
          record(server, client, "gated");
          failed.push(`${server}/${client}: ${unsettled && intent.operation === "remove-client-entry"
            ? "Not attempted because another removal could not be settled. Refresh and verify the failed entry before retrying."
            : `Not attempted because an earlier change ${unsettled ? "could not be settled" : "failed on this client"}. Refresh and verify before retrying.`}`);
          continue;
        }
        try {
          if (intent.operation === "remove-client-entry") {
            const currentToken = removalToken(removalPresence(scanForLsp, server, client));
            if (!intent.sourceToken) {
              throw new ClientEntryRemovalError(intent.refusal ?? "Removal selection is incomplete. Refresh and select the current entry again.", "REMOVAL_SELECTION_INVALID", 0, "refused");
            }
            if (currentToken !== intent.sourceToken) {
              throw new ClientEntryRemovalError("Source changed or removal is unsupported. Refresh, clear this selection, and select the current entry again.", "REMOVAL_SELECTION_STALE", 0, "refused");
            }
            await removeClientEntry(server, client, intent.sourceToken);
          } else {
            // The explicitly named undo action alone enters legacy demigrate.
            const endpoint = intent.operation === "undo" ? "/api/demigrate" : "/api/migrate";
            const resp = await fetch(endpoint, {
              method: "POST", headers: { "Content-Type": "application/json" },
              body: JSON.stringify({ servers: [server], clients: [client] }),
            });
            if (resp.status !== 200 && resp.status !== 204) {
              const body = await resp.json().catch(() => ({})) as { error?: string; failed?: { err?: string }[] };
              throw new Error(body.failed?.[0]?.err ?? body.error ?? `HTTP ${resp.status}`);
            }
          }
          record(server, client, "succeeded");
          succeeded++;
        } catch (err) {
          record(server, client, "failed");
          failed.push(`${server}/${client}: ${(err as Error).message ?? "unknown"}`);
          if (intent.operation !== "migrate") blockedClients.add(client);
          if (intent.operation === "remove-client-entry" &&
            (!(err instanceof ClientEntryRemovalError) || (err.outcome !== "refused" && err.outcome !== "failed"))) unsettled = true;
        }
      }
      if (!mountedRef.current) return;
      setDirty((previous) => {
        const next = new Map(previous);
        for (const { server, client, intent } of cells) {
          if (results.get(server)?.get(client) !== "succeeded" || next.get(server)?.get(client) !== intent) continue;
          const row = new Map(next.get(server));
          row.delete(client);
          if (row.size > 0) next.set(server, row);
          else next.delete(server);
        }
        return next;
      });
      setOutcomes((previous) => {
        const next = new Map(previous);
        for (const [server, row] of results) next.set(server, new Map([...(next.get(server) ?? []), ...row]));
        return next;
      });
      setFailedRows((previous) => [
        ...previous.filter((message) => !cells.some(({ server, client }) =>
          message.startsWith(`${server}/${client}:`) ||
          (message.startsWith(`${server}/`) && message.includes(`/${client}:`)))),
        ...failed,
      ]);
      setApplyMsg(failed.length > 0
        ? `Failed: ${failed.length} row(s); re-toggle and retry below.`
        : onlyRemoval ? `Removed ${succeeded} selected entr${succeeded === 1 ? "y" : "ies"}. Refreshing…`
        : "Applied. Refreshing…");
      setReloadToken((token) => token + 1);
      setApplyGen((generation) => generation + 1);
      pushToast(failed.length > 0 ? "danger" : "success", failed.length > 0
        ? `Apply failed or was gated for ${failed.length} of ${cells.length} changes; see the list below.`
        : `Applied ${succeeded} changes to client configs.`);
    } finally {
      applyBusyRef.current = false;
      if (mountedRef.current) setApplying(false);
    }
  }

  if (error) {
    return (
      <div>
        <h1>Servers</h1>
        <p class="error" role="alert">Failed to load: {error}</p>
        <button type="button" class="btn" onClick={rescan}>Retry</button>
      </div>
    );
  }

  if (!servers) {
    return (
      <div>
        <h1>Servers</h1>
        <LoadingState label="Loading servers" />
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

  // A Hub entry can be removed without a manifest. Direct legacy entries stay
  // in the explicit removal panel; ON retains its existing manifest requirement.
  const inMainMatrix = (s: ServerRow) => s.name !== LSP_MANIFEST_SERVER &&
    (s.manifested || dirty.has(s.name) || Object.values(s.routing).some(hubChecked));
  const manifestedServers = servers.filter(inMainMatrix);
  const otherServers = servers.filter((s) => !inMainMatrix(s) && !s.manifested && !dirty.has(s.name));

  // effectiveChecked computes a cell's CURRENT visual state: a pending
  // dirty edit overrides the authoritative scan baseline (a queued
  // "migrate" means the operator already checked it; removal means
  // they unchecked it). Used by both the per-cell visual sync and the
  // column-toggle's all-checked computation so the header toggle reflects
  // unsaved edits, not just the last scan.
  const effectiveChecked = (server: ServerRow, client: string): boolean => {
    const pending = dirty.get(server.name)?.get(client);
    if (pending) return pending.operation === "migrate";
    return hubChecked(server.routing[client] ?? "not-installed");
  };

  // Whole-column toggle (Feature 2): flip every applicable (toggleable,
  // non-disabled) cell in one client's column at once. Computes the
  // column's effective state across only the interactive cells — if ALL
  // are currently checked, set them all UNchecked; otherwise set them all
  // checked. Each flip routes through the SAME per-cell onToggle path
  // (toggleCell) so operation capture and the Apply queue stay
  // identical to manual per-cell toggles — no separate apply path.
  const columnInteractiveServers = (client: string): ServerRow[] =>
    manifestedServers.filter((s) =>
      cellInteractive(s.routing[client] ?? "not-installed", applying, removalToken(removalPresence(scanForLsp, s.name, client)) !== null, s.manifested),
    );
  // flipCellGroup is the SINGLE owner of the bulk enable-all/disable-all
  // logic shared by the whole-column header toggle AND the whole-row
  // server toggle: given a set of (server, client) cells, if ALL are
  // currently checked set them all UNchecked, else set them all checked.
  // Each flip routes through the SAME per-cell toggleCell path so
  // operation capture and the Apply queue stay identical to a
  // manual per-cell toggle — there is no separate bulk apply path, and no
  // duplicated flip logic between the column and row toggles.
  const flipCellGroup = (cells: { server: ServerRow; client: string }[]) => {
    if (cells.length === 0) return; // no-op: nothing toggleable in the group
    const allChecked = cells.every(({ server, client }) => effectiveChecked(server, client));
    const next = !allChecked;
    for (const { server, client } of cells) {
      // initialChecked is the authoritative scan baseline the dirty map
      // compares selection against — pass it (NOT the effective state) so
      // toggleCell prunes/creates the dirty entry against the right axis.
      const initialChecked = hubChecked(server.routing[client] ?? "not-installed");
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
      cellInteractive(server.routing[client] ?? "not-installed", applying, removalToken(removalPresence(scanForLsp, server.name, client)) !== null, server.manifested),
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
      const classification = classifyLspRegistrationResponse(
        { kind: "enable" },
        row.language,
        response,
      );
      setLspRegisterMsg(classification);
      if (!classification.refresh) return;
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

  // registerWorkspacePath is the FIRST-workspace register flow (fresh-machine
  // connect path). It takes a workspace ROOT path the operator typed and a
  // language, and POSTs /api/lsp/register directly — the same backend seam the
  // `mcphub register` CLI uses (EnsureLSPRegistered + bless), so no CLI step is
  // needed for the happy path. On success it reloads /api/workspaces so the new
  // workspace appears in the selector + the row flips to registered, and (when
  // the host had zero workspaces) auto-selects the just-registered key so the
  // operator can immediately enable more languages.
  const registerWorkspacePath = async (language: string) => {
    const path = workspacePathInput.trim();
    if (!path) {
      setLspRegisterMsg({ kind: "error", text: "Enter a workspace folder path first." });
      return;
    }
    if (!language) {
      setLspRegisterMsg({ kind: "error", text: "Pick a language to register." });
      return;
    }
    setWorkspaceRegisterBusy(true);
    setLspRegisterMsg(null);
    try {
      const response = await postLspRegister(path, language);
      if (!mountedRef.current) return;
      const classification = classifyLspRegistrationResponse(
        { kind: "register", workspacePath: path },
        language,
        response,
      );
      setLspRegisterMsg(classification);
      if (!classification.refresh) return;
      // Auto-select the newly-registered workspace so the operator can enable
      // more languages against it without first opening the selector.
      if (response.workspace_key) {
        setSelectedWorkspaceKey(response.workspace_key);
      }
      setWorkspacePathInput("");
      setReloadToken((n) => n + 1);
    } catch (err) {
      if (!mountedRef.current) return;
      setLspRegisterMsg({
        kind: "error",
        text: `Register ${language} failed: ${(err as Error).message}`,
      });
    } finally {
      if (mountedRef.current) {
        setWorkspaceRegisterBusy(false);
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
          paused={autoScanPaused}
          pauseReason={
            hasUnsavedEdits
              ? "auto-refresh paused — unsaved changes"
              : removalSelection.length > 0
                ? "auto-refresh paused — removal selection"
                : applying ? "auto-refresh paused — client update in progress" : "auto-refresh paused — LSP router update in progress"
          }
          disabledReason={
            hasUnsavedEdits
              ? "Apply or discard your unsaved matrix changes before rescanning"
              : removalSelection.length > 0
                ? "Use Refresh removal sources to check the selected entries"
                : "Wait for the client update to finish before rescanning"
          }
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
<button type="button" class="btn btn-primary" onClick={() => applyChanges()} disabled={applyDisabled}>
          {retryPendingCount > 0
            ? `Apply changes (incl. retry ${retryPendingCount})`
            : "Apply changes"}
        </button>
        <button
          type="button"
          class="btn"
          aria-expanded={removalOpen}
          aria-controls="servers-removal-selection"
          disabled={applying}
          onClick={() => setRemovalOpen((open) => !open)}
        >
          Select entries to remove{removalSelection.length > 0 ? ` (${removalSelection.length} selected)` : ""}
        </button>
        <span role="status" style="margin-left:12px" class={applyMsg.startsWith("Failed") ? "error" : ""}>
          {applyMsg}
        </span>
      </div>
      <p>Uncheck a Hub entry and Apply changes to remove it from that client. Direct entries remain present while unchecked. Check and Apply to route an entry through the Hub. Restore direct connection is a separate undo action.</p>
      {removalOpen && (
        <section id="servers-removal-selection" aria-label="Remove entries from client configs">
          <h2>Remove from client</h2>
          <p>Select the existing entries to delete from their client configs. This does not restore an older entry. Selecting removal replaces a pending Hub routing change for that entry. Unsupported sources stay visible with their reason.</p>
          <RemovalSelectionTable
            scan={scanForLsp}
            clients={clientColumns}
            selection={removalSelection}
            disabled={applying}
            onSelection={selectRemoval}
          />
          <button type="button" class="btn btn-primary" disabled={applying || removalSelection.length === 0} onClick={() => applyChanges(true)}>Remove from client ({removalSelection.length})</button>
          <button type="button" class="btn" disabled={applying} onClick={() => setReloadToken((token) => token + 1)}>Refresh removal sources</button>
          <button type="button" class="btn" disabled={applying || removalSelection.length === 0} onClick={() => selectRemoval([])}>Clear removal selection</button>
        </section>
      )}
      {failedRows.length > 0 && (
        <ul class="apply-failed-rows" data-testid="apply-failed-rows" role="alert">
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
              scanErrors={clientScanErrors}
              status={statusByServer[server.name]}
              outcomes={outcomes.get(server.name)}
              pending={dirty.get(server.name)}
              applyGen={applyGen}
              onToggle={toggleCell}
              onUndo={queueUndo}
              removalSources={scanForLsp?.entries?.find((entry) => entry.name === server.name)?.client_presence}
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
        clientConfigPresence={clientConfigPresence}
        openDrawerFor={openDrawerFor}
        onOpenDrawer={(row) => setOpenDrawerFor(row)}
        targetWorkspacePath={lspRegisterWorkspacePath}
        registerBusy={lspRegisterBusy}
        registerMsg={lspRegisterMsg}
        routerBusy={lspRouterBusy}
        routerMsg={lspRouterMsg}
        routerClientOverrides={lspRouterClientOverrides}
        routerClientDisabled={lspRouterClientDisabled}
        onRegister={registerLspRow}
        onToggleRouterClient={toggleLspRouterClient}
        hasWorkspaces={workspaces.length > 0}
        workspacePathInput={workspacePathInput}
        onWorkspacePathInput={setWorkspacePathInput}
        onRegisterWorkspacePath={registerWorkspacePath}
        workspaceRegisterBusy={workspaceRegisterBusy}
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

function removalPresence(scan: ScanResult | null, server: string, client: string): ClientPresence | undefined {
  return scan?.entries?.find((entry) => entry.name === server)?.client_presence?.[client];
}

function removalToken(presence: ClientPresence | undefined): string | null {
  const capability = presence?.removal;
  return capability?.version === 1 && capability.supported === true &&
    typeof capability.source_token === "string" && capability.source_token.length > 0
    ? capability.source_token
    : null;
}

function removalRefusal(reason: string | undefined): string {
  switch (reason) {
    case "REMOVAL_SHARED_SOURCE_SCOPE": return "This source is shared with another client. Removing it would affect an unselected client.";
    case "REMOVAL_SOURCE_CONFLICT": return "The source has changed or could not be verified. Refresh before selecting it again.";
    case "REMOVAL_EFFECTIVE_RETAINED": return "Another config layer would keep this entry active after removal.";
    default: return "Removal is not supported for this source.";
  }
}

function RemovalSelectionTable(props: {
  scan: ScanResult | null;
  clients: readonly string[];
  selection: RemovalCell[];
  disabled: boolean;
  onSelection: (selection: RemovalCell[]) => void;
}) {
  const { scan, clients, selection, disabled, onSelection } = props;
  const names = [...new Set([
    ...(scan?.entries ?? []).filter((entry) => Object.values(entry.client_presence ?? {}).some((presence) => presence.transport !== "absent")).map((entry) => entry.name),
    ...selection.map((cell) => cell.server),
  ])].sort();
  const selected = (server: string, client: string) => selection.find((cell) => cell.server === server && cell.client === client);
  const toggle = (server: string, client: string) => {
    if (disabled) return;
    const current = selected(server, client);
    if (current) {
      onSelection(selection.filter((cell) => cell !== current));
      return;
    }
    const token = removalToken(removalPresence(scan, server, client));
    if (token) onSelection([...selection, { server, client, sourceToken: token }]);
  };
  const selectClient = (client: string) => {
    if (disabled) return;
    const additions = names.flatMap((server) => {
      const token = removalToken(removalPresence(scan, server, client));
      return token && !selected(server, client) ? [{ server, client, sourceToken: token }] : [];
    });
    onSelection([...selection, ...additions]);
  };
  return (
    <div style="overflow-x:auto">
      {names.length === 0 ? <p>No client entries available to remove.</p> : (
        <table class="servers-matrix">
          <caption>Choose entries to remove from each client</caption>
          <thead>
            <tr>
              <th scope="col">Server</th>
              {clients.map((client) => (
                <th key={client} scope="col">
                  {client}
                  <button type="button" class="btn btn-sm" disabled={disabled || !names.some((server) => removalToken(removalPresence(scan, server, client)) !== null)} onClick={() => selectClient(client)}>Select all for removal from {client}</button>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {names.map((server) => (
              <tr key={server}>
                <th scope="row">{server}</th>
                {clients.map((client) => {
                  const presence = removalPresence(scan, server, client);
                  const token = removalToken(presence);
                  const current = selected(server, client);
                  const stale = current !== undefined && token !== current.sourceToken;
                  const reason = stale ? "Source changed. Refresh, clear this selection, and select the current entry again." :
                    token ? "Delete this entry from the client config." :
                    presence && presence.transport !== "absent" ? removalRefusal(presence.removal?.reason) : "No entry in this client.";
                  return (
                    <td key={client}>
                      <label>
                        <input type="checkbox" aria-label={`Remove ${server} from ${client}`} checked={Boolean(current)} disabled={disabled || (!token && !current)} onChange={() => toggle(server, client)} />
                        {current ? "Selected for removal" : token ? "Select to remove" : "Unavailable"}
                      </label>
                      <p class={stale ? "error" : ""}>{reason}</p>
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {selection.length > 0 && <p>Selected: {selection.map((cell) => `${cell.server}/${cell.client}`).join(", ")}</p>}
    </div>
  );
}

// OtherMCPEntriesSection renders MCP server entries discovered in
// client configs that have no corresponding manifest under
// servers/<name>/manifest.yaml — e.g. operator's own legacy stdio
// entries like `time-server` in `.cursor/mcp.json`. These rows are
// shown separately because ON migration requires a manifest. Existing direct
// entries can still be selected in the shared explicit removal panel.
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
          // "in: <clients>" lists every client that ACTUALLY HAS an entry for
          // this server — hub-routed (via-hub), inherited hub-routed
          // (via-hub-inherited), or a direct non-hub entry. via-hub-inherited
          // is an entry the operator can see, so it
          // belongs in this presence list; dropping it would hide a real
          // binding the same way the Discovery default-case drop did.
          const clientsWithEntry = Object.entries(s.routing)
            .filter(([, r]) => r === "via-hub" || r === "via-hub-inherited" || r === "direct")
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
  // Per-client parse/read-error messages from ScanResult.client_scan_errors
  // (keyed by client id). Threaded through to each CellView so a config-error
  // cell can render the actual parse-failure message instead of the generic
  // stat-anomaly tooltip. Empty/absent for the common no-error scan.
  scanErrors: Record<string, string>;
  status?: { state: string; port: number | null };
  outcomes?: Map<string, Outcome>;
  // Per-server pending operation map, including captured removal tokens.
  // Threaded so each CellView's visual `checked` can reflect a pending
  // dirty edit — necessary for the whole-column toggle to visually flip
  // cells it queues, not just the cell the operator clicked directly.
  pending?: Map<string, CellIntent>;
  removalSources?: Record<string, ClientPresence>;
  onUndo: (server: string, client: string) => void;
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
  const { server, clients, scanErrors, status, outcomes, pending, removalSources, onUndo, onToggle, onRowToggle, onRowToggleHover, hoverScope, onOpenDrawer, onSymlinkResolved, applying } = props;
  // Row-toggle affordance state: a cell is "checked" if a pending dirty
  // edit says so, else the scan baseline (via-hub). rowInteractive uses the
  // same cellInteractive gate as the per-cell checkbox + the column toggle,
  // so the row toggle flips exactly the cells the operator could flip by
  // hand. The toggle is only offered when the row has ≥1 toggleable cell.
  const rowEffectiveChecked = (client: string): boolean => {
    const p = pending?.get(client);
    if (p) return p.operation === "migrate";
    return hubChecked(server.routing[client] ?? "not-installed");
  };
  const rowInteractive = clients.filter((c) =>
    cellInteractive(server.routing[c] ?? "not-installed", applying, removalToken(removalSources?.[c]) !== null, server.manifested),
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
          scanError={scanErrors[client]}
          lastOutcome={outcomes?.get(client)}
          pendingIntent={pending?.get(client)}
          removalSource={removalSources?.[client]}
          onUndo={onUndo}
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
  // P2 scan per-client isolation: the adapter-level parse/read-error message
  // for THIS cell's client from ScanResult.client_scan_errors, if any. Present
  // only when ScanFrom isolated a malformed/unreadable config for this client
  // (presence → "error"). When set on a config-error cell, the cell renders a
  // PARSE/read-failure diagnostic with this message, distinct from the generic
  // stat/permission anomaly label used when no message is recorded.
  scanError?: string;
  onSymlinkResolved?: () => void;
  lastOutcome?: Outcome;
  // Pending operation for THIS cell (from the parent's intent map).
  // When present it overrides the scan baseline for the visual checkbox:
  // "migrate" → checked, removal/undo → unchecked. This lets the
  // whole-column header toggle (which only mutates the parent dirty map)
  // visually flip cells it queues, not merely the one the operator
  // clicked directly. A direct per-cell click also flows through dirty,
  // so the same path keeps the visual honest in every case.
  pendingIntent?: CellIntent;
  removalSource?: ClientPresence;
  onUndo: (server: string, client: string) => void;
  // G1 hover-scope: true when a hovered column/row bulk toggle covers this
  // cell. The visual highlight is gated additionally on the cell being
  // interactive (see `disabled` below) so a hovered column/row only lights up
  // the cells a click would actually flip — port/state/disabled cells stay
  // dark.
  inHoverScope?: boolean;
  onToggle: (server: string, client: string, nextChecked: boolean, initialChecked: boolean) => void;
  applying: boolean;
}) {
  const { server, client, scanError, onSymlinkResolved, lastOutcome, pendingIntent, removalSource, onUndo, inHoverScope, onToggle, applying } = props;
  // Treat undefined routing as "not-installed" — perClientRouting only
  // populates keys present in /api/scan's client_presence map.
  const routing: Routing = server.routing[client] ?? "not-installed";
  const initialChecked = hubChecked(routing);
  const effectiveChecked = pendingIntent ? pendingIntent.operation === "migrate" : initialChecked;
  const [checked, setChecked] = useState(effectiveChecked);
  // Keep local `checked` in sync with effectiveChecked. It changes when the
  // authoritative scan moves the cell (direct→via-hub after a reload), OR
  // when the parent dirty map flips this cell out-of-band (the whole-column
  // toggle). Deps `[effectiveChecked]` means unrelated parent re-renders do
  // not stomp the value, but a genuine baseline/dirty change does re-sync.
  useEffect(() => {
    setChecked(effectiveChecked);
  }, [effectiveChecked]);
  const disabled = !cellInteractive(routing, applying, removalToken(removalSource) !== null, server.manifested);
  let title: string | undefined;
  if (hubChecked(routing)) {
    title = `Currently present and routed through the Hub. Uncheck and Apply to remove this entry from ${client}.`;
    if (!removalToken(removalSource)) title += ` ${removalRefusal(removalSource?.removal?.reason)}`;
  } else if (routing === "direct") {
    title = `${client} has a direct (non-hub) entry for this server. Check and Apply to route it through the hub.`;
  } else if (routing === "available") {
    title = `${client} has no entry for this server yet. Check and Apply to install it.`;
  } else if (routing === "not-installed") {
    title = `${client}'s MCP config file is not present on this host — nothing to install.`;
  } else if (routing === "unsupported") {
    title = `${client} cannot route this server through the hub (e.g., per-session servers).`;
  } else if (routing === "config-error") {
    // P2 scan per-client isolation: a config-error cell now has TWO distinct
    // causes. The backend records a message in ScanResult.client_scan_errors
    // ONLY for an isolated adapter-level parse/read failure (malformed JSON/
    // TOML, unreadable body) — the pre-existing stat/wrong-shape failure leaves
    // the map empty for this client. Surface them differently so an operator
    // with a malformed config sees the actual parser message instead of the
    // misleading "check permissions and disk health" stat-anomaly hint.
    if (scanError) {
      title = `${client}'s MCP config could not be parsed/read: ${scanError}. Fix the config file (e.g. invalid JSON/TOML), then refresh. Other clients were scanned normally.`;
    } else {
      // v0.4.5 PR #208 deep-sec Lane B follow-up: distinguish "stat
      // returned an error" from "file absent" so the operator sees an
      // actionable diagnostic instead of the misleading "not present"
      // tooltip. Typical causes: parent-directory permissions blocked,
      // antivirus quarantine, or I/O fault on the underlying volume.
      title = `${client}'s MCP config file could not be read (stat error). Check file permissions and disk health, then refresh.`;
    }
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
  // visual stays on the routing axis;
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
          pending={pendingIntent !== undefined}
          title={cellTitle}
          onChange={(ev) => {
            const next = (ev.currentTarget as HTMLInputElement).checked;
            setChecked(next);
            onToggle(server.name, client, next, initialChecked);
          }}
        />
      </label>
      {routing === "direct" && <span class="text-muted">Direct entry present</span>}
      {initialChecked && <span class="text-muted">Hub entry present</span>}
      {pendingIntent && <span class="text-muted">{pendingIntent.operation === "undo" ? "Undo Hub migration pending" : pendingIntent.operation === "migrate" ? "Hub routing pending" : "Removal pending"}</span>}
      {initialChecked && !removalToken(removalSource) && (
        <span class="error" role="note">{removalRefusal(removalSource?.removal?.reason)}</span>
      )}
      {pendingIntent && (
        <button type="button" class="btn btn-sm" disabled={applying}
          aria-label={`Cancel pending change for ${server.name} in ${client}`}
          onClick={() => onToggle(server.name, client, initialChecked, initialChecked)}>Cancel pending change</button>
      )}
      {routing === "via-hub" && (
        <button type="button" class="btn btn-sm" disabled={applying}
          aria-label={`Restore direct connection for ${server.name} in ${client}`}
          onClick={() => onUndo(server.name, client)}>Undo Hub migration</button>
      )}
      {hasLegacyConflict && (
        <span
          class="matrix-cell-legacy-chip"
          data-testid={`legacy-chip-${server.name}-${client}`}
          title={`A legacy non-hub entry for ${server.name} also exists in ${client}'s config alongside the hub binding. Resolve in ${client}'s mcp config directly.`}
        >
          legacy
        </span>
      )}
      {/* P2 scan per-client isolation: an isolated adapter-level parse/read
          failure for THIS client (presence "error" + a client_scan_errors
          message). Renders a DISTINCT inline note — the actual parser message,
          not the generic stat-anomaly label — so the operator sees "config
          unreadable: <message>". role="note" + the message in both the visible
          text and the title keeps it accessible. A bare stat-error cell
          (routing config-error with no scanError) renders nothing here and
          keeps its generic tooltip. */}
      {routing === "config-error" && scanError && (
        <span
          class="matrix-cell-parse-error"
          role="note"
          data-testid={`scan-error-${server.name}-${client}`}
          title={`${client}'s MCP config could not be parsed/read: ${scanError}`}
        >
          config unreadable: {scanError}
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

// RegisterWorkspacePanel is the GUI-discoverable FIRST-workspace register
// affordance (fresh-machine connect path). A fresh operator with the
// toolchains installed but ZERO registered workspaces previously hit a
// dead-end: the WorkspaceSelector pointed at the CLI and every row's Enable
// button was disabled until a workspace already existed. This panel lets the
// operator type a workspace ROOT folder, pick a language, and register it
// straight from the GUI (POST /api/lsp/register) — no `mcphub register` CLI
// step. It is rendered ALWAYS (not only on the empty host): once a workspace
// exists it stays available as the "register another workspace / language"
// surface, but its copy emphasizes the first-run path when hasWorkspaces is
// false. Language list mirrors the canonical LSP_LANGUAGES (the 9-language
// manifest set the rows below render).
function RegisterWorkspacePanel(props: {
  hasWorkspaces: boolean;
  pathValue: string;
  onPathInput: (value: string) => void;
  onRegister: (language: string) => void;
  busy: boolean;
}) {
  const { hasWorkspaces, pathValue, onPathInput, onRegister, busy } = props;
  const [language, setLanguage] = useState<string>(LSP_LANGUAGES[0]);
  const canSubmit = pathValue.trim().length > 0 && !busy;
  return (
    <div class="lsp-register-workspace" data-testid="lsp-register-workspace">
      <p class="lsp-register-workspace-intro" data-testid="lsp-register-workspace-intro">
        {hasWorkspaces
          ? "Register another workspace folder, then enable its languages below."
          : "No workspace registered yet. Enter a project folder and pick a language to connect a language server — no command line needed."}
      </p>
      <div class="lsp-register-workspace-controls">
        <label class="lsp-register-workspace-field">
          <span class="visually-hidden">Workspace folder path</span>
          <input
            type="text"
            class="field-ctl"
            data-testid="lsp-register-workspace-path"
            placeholder="Workspace folder path"
            value={pathValue}
            disabled={busy}
            onInput={(ev) => onPathInput((ev.currentTarget as HTMLInputElement).value)}
          />
        </label>
        <label class="lsp-register-workspace-field">
          <span class="visually-hidden">Language</span>
          <select
            class="field-ctl"
            data-testid="lsp-register-workspace-language"
            value={language}
            disabled={busy}
            onChange={(ev) => setLanguage((ev.currentTarget as HTMLSelectElement).value)}
          >
            {LSP_LANGUAGES.map((lang) => (
              <option key={lang} value={lang}>
                {lang}
              </option>
            ))}
          </select>
        </label>
        <button
          type="button"
          class="btn btn-primary"
          data-testid="lsp-register-workspace-submit"
          disabled={!canSubmit}
          title={
            canSubmit
              ? `Register ${language} for ${pathValue.trim()}`
              : "Enter a workspace folder path first."
          }
          onClick={() => onRegister(language)}
        >
          {busy ? "Registering…" : "Register workspace"}
        </button>
      </div>
    </div>
  );
}

// LspMatrix renders the 9-row LSP daemons table below the main matrix.
// Workspace-scoped LSP rows do not use the main matrix migrate/demigrate
// queue. Their per-client checkbox drives the shared LSP-router enable/disable
// path, while unregistered rows can be registered from the row action when the
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
  // Per-client config-file usability (ScanResult.client_config_presence),
  // the SAME map the main matrix uses. Threaded so each LSP toggle can gate
  // its disabled state on whether the client has a usable config, instead of
  // rendering an enabled toggle for a missing/error config (finding 2).
  clientConfigPresence: Record<string, ClientConfigState>;
  openDrawerFor: LspRow | null;
  onOpenDrawer: (row: LspRow) => void;
  targetWorkspacePath: string;
  registerBusy: Record<string, boolean>;
  registerMsg: LspRegistrationMessage | null;
  routerBusy: Record<string, boolean>;
  routerMsg: { text: string; kind: "ok" | "error" } | null;
  routerClientOverrides: Record<string, boolean>;
  routerClientDisabled: Record<string, boolean>;
  onRegister: (row: LspRow) => void;
  onToggleRouterClient: (client: string, enabled: boolean) => void;
  // First-workspace register flow (fresh-machine connect path).
  hasWorkspaces: boolean;
  workspacePathInput: string;
  onWorkspacePathInput: (value: string) => void;
  onRegisterWorkspacePath: (language: string) => void;
  workspaceRegisterBusy: boolean;
}) {
  const {
    rows,
    clients,
    clientConfigPresence,
    openDrawerFor,
    onOpenDrawer,
    targetWorkspacePath,
    registerBusy,
    registerMsg,
    routerBusy,
    routerMsg,
    routerClientOverrides,
    routerClientDisabled,
    onRegister,
    onToggleRouterClient,
    hasWorkspaces,
    workspacePathInput,
    onWorkspacePathInput,
    onRegisterWorkspacePath,
    workspaceRegisterBusy,
  } = props;
  return (
    <section class="lsp-matrix-section" data-testid="lsp-matrix-section">
      <h2>LSP daemons</h2>
      <p class="lsp-matrix-intro">
        Workspace-scoped language servers route through shared hub proxies.
      </p>
      <RegisterWorkspacePanel
        hasWorkspaces={hasWorkspaces}
        pathValue={workspacePathInput}
        onPathInput={onWorkspacePathInput}
        onRegister={onRegisterWorkspacePath}
        busy={workspaceRegisterBusy}
      />
      {registerMsg && (
        <div
          class={
            registerMsg.kind === "error"
              ? "error"
              : registerMsg.kind === "warning"
                ? "lsp-register-warning"
                : ""
          }
          data-testid="lsp-register-msg"
          role={registerMsg.kind === "error" ? "alert" : "status"}
          style="margin:var(--gap-xs) 0"
        >
          {registerMsg.text}
        </div>
      )}
      {routerMsg && (
        <div
          class={routerMsg.kind === "error" ? "error" : ""}
          data-testid="lsp-router-msg"
          style="margin:var(--gap-xs) 0"
        >
          {routerMsg.text}
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
                    entryName={row.clientPresenceEntryName[client]}
                    canEnableFromLegacy={row.clientPresenceCanEnableFromLegacy[client] === true}
                    legacy={row.legacyConflict[client]}
                    configState={clientConfigPresence[client]}
                    checkedOverride={routerClientOverrides[client]}
                    clientRouterDisabled={routerClientDisabled[client] === true}
                    busy={routerBusy[client] === true}
                    onToggle={onToggleRouterClient}
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

// LspCellView renders one LSP row's per-client cell. Badge semantics (chips,
// unchanged) are driven by the entry's transport:
//   - presence.transport === "http"  -> [via-hub]
//   - presence.transport === "stdio" -> [direct]
//   - legacy populated               -> [legacy] (stacks under [via-hub] or [direct])
//   - none                           -> "-"
//
// The TOGGLE (checked / disabled / read-only) and the OPT-OUT affordance are
// both derived by the single owner lspCellState (lib/lsp-rows.ts). This view
// re-derives none of it: the checkbox describes the ROUTER ENTRY, while the
// opt-out persists a CLIENT PREFERENCE and is therefore reachable for every
// entry shape a usable client config can be in. See lspCellState's contract
// comment for the full rationale and the disable-path side effects.
function LspCellView(props: {
  language: string;
  client: string;
  presence: ClientPresence | undefined;
  entryName: string | undefined;
  canEnableFromLegacy: boolean;
  legacy: ClientEntry | undefined;
  // Per-client config-file usability from ScanResult.client_config_presence —
  // the SAME map the main matrix consumes. Undefined when the scan omitted the
  // key (client absent / older mock) → treated as no usable config.
  configState: ClientConfigState | undefined;
  checkedOverride: boolean | undefined;
  clientRouterDisabled: boolean;
  busy: boolean;
  onToggle: (client: string, enabled: boolean) => void;
}) {
  const {
    language,
    client,
    presence,
    entryName,
    canEnableFromLegacy,
    legacy,
    configState,
    checkedOverride,
    clientRouterDisabled,
    busy,
    onToggle,
  } = props;
  const {
    transport: t,
    hasPrimary,
    hasRouterEntry,
    hasUnreplaceablePrimary,
    inherited,
    configUsable,
    checked,
    checkboxDisabled: disabled,
    optedOut: clientOptedOut,
    showOptOutButton,
  } = lspCellState({
    language,
    presence,
    entryName,
    canEnableFromLegacy,
    configState,
    checkedOverride,
    clientRouterDisabled,
    busy,
  });
  const dualBadge = hasPrimary && Boolean(legacy);
  let title: string;
  let ariaLabel: string;
  if (inherited) {
    title = `Routed through the shared LSP router, but ${client} inherits this entry from a config layer mcphub never wrote (e.g. ~/.claude.json, or a lower config.json layer). mcphub cannot roll it back — edit that source config directly to remove it.`;
    ariaLabel = `Shared LSP router entry for ${client} is inherited and read-only`;
  } else if (!hasRouterEntry && hasUnreplaceablePrimary) {
    title = `${client} already has a direct LSP entry here. mcphub cannot safely replace it from this checkbox; edit that client entry directly or remove it before enabling the shared router.`;
    ariaLabel = `Shared LSP router toggle for ${client} unavailable — direct ${client} entry already exists`;
  } else if (!hasRouterEntry && !configUsable) {
    title = `${client} has no usable MCP config on this host, so its shared LSP router entries can't be changed here.`;
    ariaLabel = `Shared LSP router toggle for ${client} unavailable — no usable ${client} config`;
  } else if (checked) {
    title = `Uncheck to disable shared LSP router entries for ${client}.`;
    ariaLabel = `Disable shared LSP router entries for ${client}`;
  } else if (clientOptedOut) {
    title = `${client} is opted out of automatic shared LSP router entries. Check to enable them again.`;
    ariaLabel = `Enable shared LSP router entries for ${client}`;
  } else {
    title = `Check to enable shared LSP router entries for ${client}.`;
    ariaLabel = `Enable shared LSP router entries for ${client}`;
  }
  return (
    <td
      class="lsp-cell"
      data-testid={`lsp-cell-${language}-${client}`}
      data-dual-badge={dualBadge ? "true" : undefined}
      data-inherited={inherited ? "true" : undefined}
    >
      <label class="lsp-router-toggle" title={title}>
        <input
          type="checkbox"
          data-testid={`lsp-toggle-${language}-${client}`}
          aria-label={ariaLabel}
          checked={checked}
          disabled={disabled}
          onChange={(event) => onToggle(client, (event.currentTarget as HTMLInputElement).checked)}
        />
      </label>
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
      {clientOptedOut && (
        <span
          class="lsp-chip lsp-chip-off"
          data-testid={`lsp-chip-opt-out-${language}-${client}`}
        >
          off
        </span>
      )}
      {showOptOutButton && (
        <button
          type="button"
          class="lsp-router-opt-out"
          data-testid={`lsp-opt-out-${language}-${client}`}
          title={`Persist ${client}'s shared LSP router opt-out. mcphub stops adding router entries for ${client} on setup, and removes the router entries it owns.`}
          aria-label={`Opt ${client} out of shared LSP router entries`}
          onClick={() => onToggle(client, false)}
        >
          Off
        </button>
      )}
      {!hasPrimary && !legacy && !clientOptedOut && !showOptOutButton && (
        <span class="lsp-cell-empty">—</span>
      )}
    </td>
  );
}
