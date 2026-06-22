import {
  ALL_CLIENTS,
  orderClientsForColumns,
  scannableClients,
  visibleClients,
} from "./routing";
import type { ScanResult } from "../types";

// COLUMN_PREFS_KEY is the localStorage key holding the operator's manual
// per-client column-visibility overrides for the Servers matrix. Shape:
//   { [clientId: string]: boolean }
// where true = force-show that column, false = force-hide it, and an
// ABSENT key means "defer to auto-detection" (visibleClients()). This is
// a pure VIEW filter — it never touches apply/migrate/demigrate logic.
export const COLUMN_PREFS_KEY = "mcphub.servers.column-visibility";

// ColumnPrefs is the parsed pref record. Only `true`/`false` values are
// meaningful; any non-boolean is dropped on load so a hand-corrupted or
// stale entry can't smuggle a non-boolean into the effective-columns math.
export type ColumnPrefs = Record<string, boolean>;

// loadColumnPrefs reads + validates the persisted override record.
// localStorage may be unavailable (private mode, SSR, sandbox) or hold
// corrupt JSON; both cases fall back to an empty record ("use auto
// detection for every client"), matching the same defensive posture as
// app.tsx's readCachedLayout/readCachedDefaultScreen helpers.
export function loadColumnPrefs(): ColumnPrefs {
  let raw: string | null = null;
  try {
    raw = localStorage.getItem(COLUMN_PREFS_KEY);
  } catch {
    return {};
  }
  if (!raw) return {};
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return {};
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return {};
  }
  const out: ColumnPrefs = {};
  for (const [client, value] of Object.entries(parsed as Record<string, unknown>)) {
    // Drop any non-boolean entry, and any client id outside the known
    // superset so a renamed/removed client can't leave a phantom column
    // override that effectiveVisibleClients() would then act on.
    if (typeof value === "boolean" && (ALL_CLIENTS as readonly string[]).includes(client)) {
      out[client] = value;
    }
  }
  return out;
}

// saveColumnPrefs persists the override record. Best-effort: a quota or
// disabled-storage failure is swallowed (the in-memory prefs still drive
// the current session's render; only persistence across reloads is lost).
export function saveColumnPrefs(prefs: ColumnPrefs): void {
  try {
    localStorage.setItem(COLUMN_PREFS_KEY, JSON.stringify(prefs));
  } catch {
    /* ignore — quota / disabled storage */
  }
}

// clearColumnPrefs removes the override record entirely so the matrix
// reverts to pure auto-detection. Backs the popover's "Reset to auto"
// action. Best-effort like saveColumnPrefs.
export function clearColumnPrefs(): void {
  try {
    localStorage.removeItem(COLUMN_PREFS_KEY);
  } catch {
    /* ignore */
  }
}

// effectiveVisibleClients folds the operator's manual overrides onto the
// auto-detected default column set. The auto-detected set from
// visibleClients(scan) is the BASE; then, walking the ordering universe in
// stable order so the column order never depends on pref insertion order:
//   - a client explicitly hidden (pref === false) is removed even if it
//     was auto-detected;
//   - a client explicitly shown (pref === true) is added even if it was
//     NOT auto-detected (e.g. an undetected non-core client the operator
//     wants pinned visible) — UNLESS it is a non-core client the backend
//     reports as NON-SCANNABLE (no clientScanners() parser), in which case
//     the pin is DROPPED (see the scannable re-check below);
//   - a client with no pref keeps its auto-detected visibility.
//
// SCANNABLE re-check on a `pref === true` pin (Finding 1): visibleClients()
// already gates the AUTO-detected non-core columns on the scannable capability
// so an unscannable client never auto-shows. But a persisted/checkbox pin
// (pref === true) previously bypassed that gate, letting an operator force a
// column for an unscannable no-scanner client (e.g. aider). That column's cells
// would render interactive "available" (an "ok" config) yet could never be
// reconciled after a migrate — /api/scan reports no per-entry presence for an
// unparsed client — so the cell never becomes "via-hub" and can't be
// demigrated. So a pinned NON-CORE client is dropped here unless it is
// scannable. CORE clients are ALWAYS shown (and always scannable in
// production), so the re-check is scoped to non-core pins. When the backend
// omits client_capabilities (empty scannable set) the re-check is INERT —
// every client is treated as scannable so legacy pin behavior is preserved
// (the visibleClients() side already falls back to a conservative core-only
// auto set in that case).
//
// The ordering universe is ALL_CLIENTS (the registry mirror) UNIONED with
// the auto-detected set and any pref keys, so a detected client newer than
// the static ALL_CLIENTS list (a backend client the frontend list hasn't
// caught up to) is still ordered + shown rather than silently dropped by an
// ALL_CLIENTS-only loop. orderClientsForColumns keeps CORE first, then
// registry order, then alphabetical extras.
//
// Pure + side-effect-free so it is trivially unit-testable.
export function effectiveVisibleClients(
  scan: ScanResult | null | undefined,
  prefs: ColumnPrefs,
): string[] {
  const auto = new Set(visibleClients(scan));
  // CORE_CLIENT_SET-equivalent: orderClientsForColumns always emits the core
  // set first, and visibleClients() always includes the core set, so any client
  // in `auto` that is also a core client is trivially core. Rather than
  // re-import CORE_CLIENTS, derive the "always-shown core" membership from the
  // auto set's leading core entries via visibleClients(null) (which returns
  // exactly the core set on a null scan) — a pure, dependency-free core probe.
  const core = new Set(visibleClients(null));
  const scannable = scannableClients(scan);
  // gateInert: with no capability info (older backend) the scannable set is
  // empty, so honor every pin as before (the re-check must not silently hide a
  // pinned column just because capabilities are unavailable).
  const gateInert = scannable.size === 0;
  const universe = new Set<string>(ALL_CLIENTS);
  for (const c of auto) universe.add(c);
  for (const c of Object.keys(prefs)) universe.add(c);
  const out: string[] = [];
  for (const client of orderClientsForColumns(universe)) {
    const pref = prefs[client];
    let show: boolean;
    if (pref === true) {
      // A pinned NON-CORE client must still be scannable to render an
      // interactive column (Finding 1). Core clients are exempt (always shown);
      // the gate is inert when capabilities are absent.
      show = core.has(client) || gateInert || scannable.has(client);
    } else if (pref === false) {
      show = false;
    } else {
      show = auto.has(client);
    }
    if (show) out.push(client);
  }
  return out;
}
