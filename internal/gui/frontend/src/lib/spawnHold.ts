// Pre-spawn existence gate (P1.1) — the frontend's single owner of hold copy
// and of the fleet-wide collapse rule.
//
// WHY THIS FILE EXISTS. An operator lost a working day to this exact failure:
// every daemon went red, the only explanation was a threshold message
// ("10+ failures in 30-min sliding window") buried in a log file, and nothing
// anywhere said the mcphub program file was missing or that reinstalling would
// fix it. The person on the affected machine does not read log files. So the
// cause and the remedy have to appear on the Dashboard, in plain words.
//
// Copy rules, all load-bearing:
//   - Name the REMEDY, not the internal state. "Reinstall mcphub", not
//     "SpawnHoldReason=missing-binary".
//   - Say it starts by itself. This is a HOLD, not a quarantine: no crash
//     budget was spent and the daemon starts on its own within ~30s of the
//     file reappearing. Never tell the operator to run a recovery command;
//     that would be wrong AND scarier than the truth.
//   - Collapse the fleet-wide case. All daemons are started from one
//     mcphub.exe, so when it goes missing every daemon fails identically.
//     Twelve identical red cards saying "binary missing" is worse than one
//     sentence saying the mcphub installation is broken.
//
// The Go side owns the same collapse rule for the CLI
// (DeriveFleetWideSpawnHold, internal/cli/supervise_prespawn_path_gate.go).
// That duplication is deliberate and confined: it spans a hard process
// boundary (browser vs supervisor), the two are kept honest by matching test
// expectations, and the wire contract between them is the stable reason id.

import type { DaemonStatus } from "../types";

export const MISSING_BINARY = "missing-binary";
export const MISSING_WORKSPACE = "missing-workspace";
// Same hold, different REMEDY: the path's whole volume is unreachable (a
// disconnected mapped network drive, an unmounted removable volume) rather than
// the file being deleted. Go folds ERROR_BAD_NETPATH into fs.ErrNotExist, so
// the supervisor separates the two with a volume-root probe. Telling this
// operator to reinstall would be wrong — their install is fine and reinstalling
// cannot bring a share back online.
export const UNAVAILABLE_BINARY = "unavailable-binary";
export const UNAVAILABLE_WORKSPACE = "unavailable-workspace";

/** Short label for the per-daemon card row. Kept to a few words. */
export function spawnHoldBadge(reason: string): string {
  switch (reason) {
    case MISSING_BINARY:
      return "mcphub program file missing";
    case MISSING_WORKSPACE:
      return "Project folder missing";
    case UNAVAILABLE_BINARY:
      return "mcphub drive unavailable";
    case UNAVAILABLE_WORKSPACE:
      return "Project drive unavailable";
    default:
      // Unknown id from a newer supervisor: degrade to something true and
      // useful rather than rendering blank or leaking the raw id.
      return "Required file missing";
  }
}

/** Full sentence for the tooltip / banner. Names the path and the remedy. */
export function spawnHoldMessage(reason: string, path: string): string {
  const where = path ? ` (${path})` : "";
  switch (reason) {
    case MISSING_BINARY:
      return `The mcphub program file is missing${where}. Reinstall or update mcphub to restore it. Every server is started from this one file, so while it is missing none of them can run. Nothing else is needed — they start again by themselves once the file is back.`;
    case MISSING_WORKSPACE:
      return `This server's project folder is missing${where}. Restore the folder, or remove this server from mcphub if the project is gone. It starts again by itself once the folder is back.`;
    case UNAVAILABLE_BINARY:
      return `The drive holding the mcphub program file is not available right now${where}. Reconnect that drive or network location. Nothing is wrong with your installation and reinstalling will not help. Servers start again by themselves once the drive is back; if it is not coming back, reinstall mcphub to a local folder.`;
    case UNAVAILABLE_WORKSPACE:
      return `The drive holding this server's project folder is not available right now${where}. Reconnect that drive or network location. It starts again by itself once the drive is back.`;
    default:
      return `A file this server needs is missing${where}. It starts again by itself once the file is back.`;
  }
}

export interface SpawnHoldBanner {
  reason: string;
  path: string;
  count: number;
  message: string;
  /** Ready-to-render sentence, including the "N servers cannot start" lead. */
  headline: string;
}

/**
 * Returns one banner per DISTINCT (reason, path) among the held daemons, in
 * first-appearance order. Empty array when nothing is held.
 *
 * WHY GROUPS RATHER THAN A SINGLE ALL-OR-NOTHING HEADLINE. The remedy lives in
 * the banner; the per-daemon card carries only a short badge plus a `title`
 * tooltip. An earlier revision emitted a banner only when TWO OR MORE daemons
 * shared one cause, which meant that on a one-server host — and in any
 * mixed-cause fleet — the operator saw the CAUSE but could reach the REMEDY
 * only by hovering, which they have no reason to do. The original rationale for
 * collapsing was "do not render twelve identical cards"; with one card there is
 * nothing to collapse, so that rationale never argued against showing the
 * remedy. Grouping keeps the say-it-once property per cause AND guarantees the
 * remedy is visible without hover in every case.
 *
 * DELIBERATE DIVERGENCE FROM GO. DeriveFleetWideSpawnHold
 * (internal/cli/supervise_prespawn_path_gate.go) still requires >= 2 for its
 * headline, because the CLI has no card to identify the daemon and therefore
 * falls back to naming each held daemon on its own line — which already carries
 * the remedy there. The two surfaces share the FACT (the stable reason id) and
 * differ only in presentation, which is the point of shipping the id rather
 * than prose over the wire.
 */
export function deriveSpawnHoldBanners(rows: DaemonStatus[]): SpawnHoldBanner[] {
  const order: string[] = [];
  const groups = new Map<string, { reason: string; path: string; count: number }>();
  for (const r of rows) {
    if (!r.spawn_hold_reason) continue;
    const reason = r.spawn_hold_reason;
    const path = r.spawn_hold_path ?? "";
    // JSON, not string concatenation: a separator character would have to be
    // one no reason id and no path can contain, and that reasoning is easy to
    // get wrong (an earlier revision used a raw byte here, which made git treat
    // this source file as binary). JSON.stringify has no such ambiguity.
    const key = JSON.stringify([reason, path]);
    const existing = groups.get(key);
    if (existing) {
      existing.count++;
    } else {
      groups.set(key, { reason, path, count: 1 });
      order.push(key);
    }
  }
  return order.map((key) => {
    const g = groups.get(key)!;
    const message = spawnHoldMessage(g.reason, g.path);
    const lead =
      g.count === 1
        ? "1 server cannot start."
        : `${g.count} servers cannot start, all for the same reason.`;
    return { ...g, message, headline: `${lead} ${message}` };
  });
}
