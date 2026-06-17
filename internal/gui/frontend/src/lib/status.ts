import type { DaemonStatus, ServerAggregate } from "../types";

// stateShape returns the shape glyph that augments color for daemon-state
// rendering. Spec §5.7 line 289 requires status cells to include a shape
// indicator so red/green color is not the sole carrier of meaning — users
// with a red-green color deficit can still read state at a glance.
//
// The earlier dichotomy (Running → ●, everything else → ○) made the three
// most-confused states — Stopped (benign idle), Failed/Quarantined (hard
// error), and Partial (one of several daemons down) — render the identical
// open-circle glyph, so a color-blind user could NOT tell a benign stop
// from a crash-looped failure without relying on hue. The glyph now varies
// by SEMANTIC GROUP, with distinct silhouettes that survive a grayscale
// render:
//
//   ●  Running          — filled circle: healthy, fully up
//   ◐  Partial          — half-filled circle: some-but-not-all daemons up
//   ◓  Starting/recover — half-filled (top): transient, supervisor working
//   ✕  Failed/quarantine — cross: hard error
//   ○  Stopped/idle      — open circle: benign, deliberately not running
//
// The semantic grouping mirrors the backend's authoritative
// normalizeDaemonState classifier (internal/api/health.go) so the visual
// meaning has a single conceptual owner: "Starting"/"Restarting"/"Backoff"/
// "Spawning" are the recovering group, "Failed"/"Quarantined" are the
// terminal-failure group, "Ready"/"Scheduled"/"Stopped" are the benign-idle
// group. "Partial" is a GUI-only aggregate (mixed multi-daemon, see
// aggregateStatus below) and gets its own glyph. Any unrecognized state
// falls back to the open circle. The text label still carries the precise
// state word, so the shape only has to convey the coarse health group at a
// glance; the label remains the authoritative source for screen readers.
export function stateShape(state: string): string {
  switch (state) {
    case "Running":
      return "●";
    case "Partial":
      return "◐";
    case "Starting":
    case "Restarting":
    case "Backoff":
    case "Spawning":
      return "◓";
    case "Failed":
    case "Quarantined":
      return "✕";
    case "Ready":
    case "Scheduled":
    case "Stopped":
      return "○";
    default:
      // Unrecognized / blank vocabulary (e.g. "Disabled", "Queued", "")
      // reads as the benign open circle — it is not a known failure, so it
      // must not borrow the error cross.
      return "○";
  }
}

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
  const grouped: Record<string, DaemonStatus[]> = Object.create(null);
  for (const r of (rows ?? []).filter((x) => !x.is_maintenance)) {
    if (!grouped[r.server]) grouped[r.server] = [];
    grouped[r.server].push(r);
  }
  const out: Record<string, ServerAggregate> = Object.create(null);
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
