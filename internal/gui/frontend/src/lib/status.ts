import type { DaemonStatus, ServerAggregate } from "../types";

// aggregateStatus collapses /api/status's per-(server, daemon) rows into one
// row per server for the matrix display. Multi-daemon servers (serena ships
// claude + codex) otherwise had the second iterated daemon overwrite the
// first in a server-keyed derivation, masking a case where one daemon was
// down while the other was Running.
//
// The aggregate state is:
//   - the shared state when every daemon reports the exact same state
//   - "Partial" otherwise — including mixed non-Running states like
//     Failed + Stopped. Surfacing a single state in that case would hide
//     that the daemons are in different failure modes.
// The representative port is the lowest non-zero port for stability and so
// one running daemon's port stays visible even when another daemon is down.
export function aggregateStatus(rows: DaemonStatus[] | null): Record<string, ServerAggregate> {
  const grouped: Record<string, DaemonStatus[]> = {};
  for (const r of (rows ?? []).filter((x) => !x.is_maintenance)) {
    if (!grouped[r.server]) grouped[r.server] = [];
    grouped[r.server].push(r);
  }
  const out: Record<string, ServerAggregate> = {};
  for (const [server, daemons] of Object.entries(grouped)) {
    const states = daemons.map((d) => d.state);
    const unique = [...new Set(states)];
    const aggregate = unique.length === 1 ? unique[0] : "Partial";
    const ports = daemons
      .map((d) => d.port ?? 0)
      .filter((p) => p > 0)
      .sort((a, b) => a - b);
    out[server] = {
      server,
      state: aggregate,
      port: ports[0] ?? null,
      daemonCount: daemons.length,
    };
  }
  return out;
}
