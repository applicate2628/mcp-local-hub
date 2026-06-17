// DaemonMetrics — the per-daemon process-metrics block of a Dashboard card.
//
// Single owner of the metric `.card-kv` rows so the Dashboard Card stays
// focused on the title/state-badge/actions chrome. Every field rendered
// here comes from data ALREADY present on api.DaemonStatus (mirrored in
// types.ts) — the /api/status HTTP snapshot and the SSE daemon-state
// deltas. This component adds NO new backend surface; it only humanizes
// and lays out fields the supervisor IPC status path already emits:
//
//   - Port       — d.port (loopback TCP port the daemon binds).
//   - PID        — d.pid  (live process id; omitted/0 when not running).
//   - Uptime     — d.uptime_sec, humanized by formatUptime ("2h 14m");
//                  the row is omitted when the value is absent/zero
//                  (just-spawned or non-running daemon).
//   - RAM        — d.ram_bytes, humanized by formatBytes ("48 MB"); the
//                  row is omitted when absent (non-Windows host, port-
//                  stale/Idle daemon, or PID-recycled lookup miss).
//   - Orphan PID — d.orphan_pid (Windows post-create orphan; diagnostic).
//   - Job Protection — d.job_protection === false (orphan-protection
//                  fallback fired; warning badge). nil/true render nothing.
//
// NOTE on restart-count: api.DaemonStatus does NOT carry a restart count.
// The only restart_count on the wire lives on api.DaemonRow (the
// /api/health HealthSnapshot path), where it is hardcoded to 0 with the
// comment "existing DaemonStatus doesn't currently expose them; default
// 0/nil. Future scheduler integration fills them" (internal/api/health.go).
// Rendering a constant 0 here would be a fabricated metric, so the
// restart-count row is intentionally NOT emitted until the backend
// exposes the field on the status path. State is rendered by the Card
// (it carries the interactive Flowbite badge), not here.
//
// The markup, classes, and data-testids match what previously lived
// inline in Dashboard.tsx so the existing Dashboard tests (uptime-row /
// ram-row / orphan-pid-row / job-protection-row, and the 4-button
// non-interactive-rows invariant) keep passing unchanged.

import { formatBytes, formatUptime } from "../lib/format";
import type { DaemonStatus } from "../types";

export function DaemonMetrics({ daemon: d }: { daemon: DaemonStatus }) {
  // Live process metrics. uptime_sec is derived server-side from the
  // supervisor's started_at; ram_bytes from a per-pid GetProcessMemoryInfo
  // lookup. Empty string => omit the row (a non-running daemon carries no
  // uptime; RAM is absent on non-Windows hosts / when the lookup failed).
  const uptimeText = formatUptime(d.uptime_sec);
  const ramText = formatBytes(d.ram_bytes);

  return (
    <>
      <div class="card-kv">
        <span>Port</span>
        <span>{d.port ?? "—"}</span>
      </div>
      <div class="card-kv">
        <span>PID</span>
        <span>{d.pid ?? "—"}</span>
      </div>
      {d.orphan_pid ? (
        <div class="card-kv" data-testid="orphan-pid-row">
          <span>Orphan PID</span>
          <span title="Windows post-create orphan PID; supervisor's best-effort kill failed. Run `taskkill /F /T /PID` for manual cleanup.">
            {d.orphan_pid} ⚠
          </span>
        </div>
      ) : null}
      {d.job_protection === false ? (
        <div class="card-kv" data-testid="job-protection-row">
          <span>Job Protection</span>
          <span title="Per-spawn Windows Job Object allocation FAILED for the current spawn. Supervisor proceeded via the non-fatal cmd.Start fallback documented in ADR #239 — daemon runs without KILL_ON_JOB_CLOSE orphan-protection. On supervisor crash, this daemon's descendant tree (e.g. uvx/python or npx/node wrappers) may survive as orphans. Underlying causes are typically AppLocker / nested-job constraints / handle exhaustion on restrictive corp-managed hosts. Reference: PR #242 (consultant strategic concern #1 on PR #241).">
            UNPROTECTED ⚠
          </span>
        </div>
      ) : null}
      {uptimeText ? (
        <div class="card-kv" data-testid="uptime-row">
          <span>Uptime</span>
          <span>{uptimeText}</span>
        </div>
      ) : null}
      {ramText ? (
        <div class="card-kv" data-testid="ram-row">
          <span>RAM</span>
          <span>{ramText}</span>
        </div>
      ) : null}
    </>
  );
}
