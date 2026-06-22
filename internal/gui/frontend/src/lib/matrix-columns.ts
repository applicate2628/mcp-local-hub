import { ALL_CLIENTS, orderClientsForColumns, visibleClients } from "./routing";
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
//     wants pinned visible);
//   - a client with no pref keeps its auto-detected visibility.
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
  const universe = new Set<string>(ALL_CLIENTS);
  for (const c of auto) universe.add(c);
  for (const c of Object.keys(prefs)) universe.add(c);
  const out: string[] = [];
  for (const client of orderClientsForColumns(universe)) {
    const pref = prefs[client];
    let show: boolean;
    if (pref === true) {
      show = true;
    } else if (pref === false) {
      show = false;
    } else {
      show = auto.has(client);
    }
    if (show) out.push(client);
  }
  return out;
}
