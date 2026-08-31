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
//   - Cannot start — d.spawn_hold_reason (pre-spawn existence gate, P1.1):
//                  the supervisor is HOLDING this daemon because a path it
//                  needs is absent. Plain-language cause + remedy, because the
//                  operator this exists for does not read log files. Absent
//                  reason renders nothing. Copy lives in lib/spawnHold.ts.
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
// inline in Dashboard.tsx so the existing Dashboard tests keep passing
// unchanged.
//
// ACTUAL TEST COVERAGE (verified 2026-07-20 by grepping every *.test.* under
// frontend/src and internal/gui/e2e — do not restate this from memory):
//   COVERED   uptime-row, ram-row        — Dashboard.test.tsx
//   COVERED   spawn-hold-row             — Dashboard.test.tsx
//   COVERED   the non-interactive-rows (button-count) invariant
//   NOT COVERED  orphan-pid-row, job-protection-row — no test anywhere
//                references these testids. They render only on rare
//                failure paths (a post-create orphan whose kill failed; a
//                per-spawn Job-Object allocation failure), which is exactly
//                why nobody exercised them — and exactly why a silent
//                regression there would go unnoticed.
//
// An earlier revision of this comment claimed all four were covered. They
// were not. A wrong coverage claim is worse than no claim: it is the reason
// a reviewer stops looking. Tracked in
// work-items/bugs/2026-07-20-spawn-hold-delivery-seams-untested.md.

import { formatBytes, formatUptime } from "../lib/format";
import { spawnHoldBadge, spawnHoldMessage } from "../lib/spawnHold";
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
      {d.spawn_hold_reason ? (
        <div class="card-kv card-kv-hold" data-testid="spawn-hold-row" data-hold-reason={d.spawn_hold_reason}>
          <span>Cannot start</span>
          <span title={spawnHoldMessage(d.spawn_hold_reason, d.spawn_hold_path ?? "")}>
            {spawnHoldBadge(d.spawn_hold_reason)} ⚠
          </span>
        </div>
      ) : null}
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
      {d.stop_settlement ? (
        <div class="card-kv" data-testid="stop-settlement-row">
          <span>Stop settlement</span>
          <span title={stopSettlementTitle(d.stop_settlement)}>
            {d.stop_settlement.phase === "failed" ? "FAILED" : "PENDING"} ⚠
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

function stopSettlementTitle(receipt: NonNullable<DaemonStatus["stop_settlement"]>): string {
  const owner = receipt.observed_port_owner_pid;
  const observation = owner
    ? ` The expected port is currently observed as owned by PID ${owner}; this is diagnostic only, not a hub-ownership claim.`
    : "";
  const failure = receipt.failure ? ` Failure: ${receipt.failure}.` : "";
  return `Stop receipt ${receipt.phase} (epoch ${receipt.epoch}, generation ${receipt.pid_generation}).${failure}${observation}`;
}
