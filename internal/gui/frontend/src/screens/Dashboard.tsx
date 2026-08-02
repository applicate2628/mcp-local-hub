import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import {
  acknowledgeDaemonRecoverReceipt,
  cleanupOrphans,
  fetchOrThrow,
  getDaemonRecoverAuditLockState,
  getHubHealth,
  isAuditLockAuthorization,
  isAuditLockReceiptStatus,
  isAuditLockTerminationState,
  isDaemonRecoverErrorCode,
  newDaemonRecoverCorrelation,
  postDaemonRecover,
  restartSupervisor,
} from "../api";
import type {
  APIError,
  AuditLockAuthorization,
  AuditLockExpectedPhysical,
  AuditLockSnapshot,
  DaemonRecoverCorrelation,
  DaemonRecoverErrorCode,
  HubHealth,
} from "../api";
import { useEventSource } from "../hooks/useEventSource";
import { unmanagedStdioCount as countUnmanagedStdio } from "../lib/unmanaged-stdio";
import { daemonStateVisual, isRecoveryEligibleState, stateShape } from "../lib/status";
import { formatBytes, formatUptime } from "../lib/format";
import { deriveSpawnHoldBanners } from "../lib/spawnHold";
import { DaemonMetrics } from "../components/DaemonMetrics";
import { ConnectionBadge } from "../components/ConnectionBadge";
import { ConfirmModal } from "../components/ConfirmModal";
import { pushToast } from "../lib/toast-store";
import type { DaemonStatus, ScanResult } from "../types";

// formatUptime + formatBytes now live in lib/format (single owner shared
// with the Servers row drawer). Re-exported here so the existing
// Dashboard.test.tsx imports (`import { ..., formatUptime, formatBytes }
// from "./Dashboard"`) keep resolving unchanged.
export { formatBytes, formatUptime };

// Key state map by "<server>/<daemon>" — matches the poller convention.
// A multi-daemon server (serena: claude + codex) would otherwise collide
// on server alone and render one card instead of two.
function keyFor(r: { server: string; daemon?: string }): string {
  return `${r.server}/${r.daemon ?? "default"}`;
}

// hubHealthMessage renders plain-language guidance for a degraded gate-ON hub
// aggregate (Phase-0 item 1) — a user must understand that ALL aggregated MCP
// traffic is affected, not just one daemon card.
function hubHealthMessage(h: HubHealth): string {
  switch (h.state) {
    case "recovering":
      return "The aggregated hub is not responding — MCP clients cannot reach any server; auto-recovery is in progress.";
    case "needs-reconcile": {
      const action = h.operator_action?.trim() || "mcphub install --reconcile-hub-mode";
      return `The aggregated hub restarted on a new address — installed MCP clients get errors until their config is refreshed. Run \`${action}\`, then re-copy any Group URLs from the Groups screen. This notice clears when the hub GUI restarts.`;
    }
    case "down":
      return "The aggregated hub is down and did not self-heal — MCP clients cannot reach any server. Restart the hub (close the tray/window and relaunch, or `mcphub gui --force --kill --yes` then relaunch).";
    default:
      return "The aggregated hub is degraded — MCP clients may be unable to reach servers.";
  }
}

// BulkAction is the action verb used in /api/{restart,stop}-all and in
// the SSE "bulk-action" lifecycle events. Single source of truth: any
// trigger (Dashboard click, tray menu, future API client) flows through
// the same HTTP endpoint and produces the same SSE events. The UI
// state below is a pure projection of those events.
type BulkAction = "restart" | "stop";
type BulkOutcome = { action: BulkAction; state: "done" | "error" };

type AuditLockWarning = "pending" | "stranded";

const auditLockAuthorizationCanWarn: Record<AuditLockAuthorization, boolean> = {
  none: false,
  current_truth: true,
  uncertain: false,
};

export interface AuditLockView {
  serverInstance: string;
  revision: number;
  state: AuditLockSnapshot["state"];
  warning: AuditLockWarning | null;
}

// Every POST, lookup, baseline GET, and settlement SSE enters this one fold.
// Older revisions cannot overwrite newer truth, and a new server instance
// starts a new ordering domain instead of relabeling an old recovery receipt.
export function nextAuditLockView(
  current: AuditLockView | null,
  incoming: AuditLockSnapshot,
): AuditLockView {
  const sameInstance = current?.serverInstance === incoming.server_instance;
  if (sameInstance && incoming.revision < current.revision) return current;

  let warning: AuditLockWarning | null = null;
  if (sameInstance && current?.warning === "stranded") {
    warning = "stranded";
  } else if (incoming.state === "stranded") {
    warning = "stranded";
  } else if (incoming.state === "outstanding") {
    const receiptAuthorizesWarning = incoming.recovery_receipt
      ? auditLockAuthorizationCanWarn[incoming.recovery_receipt.lock_authorization]
      : false;
    warning =
      (sameInstance && current?.warning === "pending") ||
      receiptAuthorizesWarning
        ? "pending"
        : null;
  }

  return {
    serverInstance: incoming.server_instance,
    revision: incoming.revision,
    state: incoming.state,
    warning,
  };
}

type RecoverySettlement =
  | "missing"
  | "in_flight"
  | "committed_success"
  | "committed_error"
  | "not_committed"
  | "uncertain"
  | "consumed"
  | "conflict";

interface RetainedRecoveryReceipt {
  taskName: string;
  correlation: DaemonRecoverCorrelation;
  status: "committed_error" | "not_committed" | "uncertain";
}

class RecoveryFlowError extends Error {}

function apiErrorCode(error: unknown): string | undefined {
  return (error as APIError | undefined)?.code;
}

function isAuditLockReceipt(value: unknown): value is AuditLockSnapshot["recovery_receipts"][number] {
  if (typeof value !== "object" || value === null) return false;
  const receipt = value as Partial<AuditLockSnapshot["recovery_receipts"][number]>;
  return (
    typeof receipt.attempt_id === "string" &&
    typeof receipt.occurrence_id === "string" &&
    typeof receipt.server_instance === "string" &&
    typeof receipt.task_name === "string" &&
    isAuditLockReceiptStatus(receipt.status) &&
    isAuditLockAuthorization(receipt.lock_authorization) &&
    isAuditLockTerminationState(receipt.termination_commit_state)
  );
}

function isAuditLockSnapshot(value: unknown): value is AuditLockSnapshot {
  if (typeof value !== "object" || value === null) return false;
  const snapshot = value as Partial<AuditLockSnapshot>;
  return (
    snapshot.scope === "supervisor_events_log" &&
    typeof snapshot.server_instance === "string" &&
    Number.isSafeInteger(snapshot.revision) &&
    (snapshot.state === "released" ||
      snapshot.state === "outstanding" ||
      snapshot.state === "stranded") &&
    (snapshot.recovery_receipt === null ||
      isAuditLockReceipt(snapshot.recovery_receipt)) &&
    Array.isArray(snapshot.recovery_receipts) &&
    snapshot.recovery_receipts.every(isAuditLockReceipt)
  );
}

function auditLockSnapshotFromError(error: unknown): AuditLockSnapshot | null {
  const body = (error as APIError | undefined)?.body;
  if (typeof body !== "object" || body === null || !("audit_lock" in body)) {
    return null;
  }
  const snapshot = (body as { audit_lock?: unknown }).audit_lock;
  return isAuditLockSnapshot(snapshot) ? snapshot : null;
}

// RESTART_GRACE_MS bounds the restart/handoff-window debounce for the RED
// `dashboard-error` banner AFTER the dashboard has already loaded real data.
// A mid-session supervisor restart (deploy, RestartV3 self-restart) makes
// /api/status fail transiently for a few seconds — an EXPECTED, self-healing
// event — so we hold the RED banner until we've been degraded for at least
// this long. ~20s covers the RestartV3 readiness/handoff envelope (supervisor
// waitFor=15s + phase budgets) yet stays below an operator-action horizon, so
// a genuine prolonged outage still turns RED shortly after (fail-loud
// preserved).
const RESTART_GRACE_MS = 20_000;

// STARTUP_GRACE_MS is the WIDER tolerance used only while the dashboard has
// NEVER loaded real data (`!hasEverLoaded`). A never-loaded dashboard is in
// the initial supervisor-IPC bind window, which on a logon autostart-storm
// (many daemons cold-starting at once) legitimately takes longer than a
// mid-session restart. ~45s tolerates that cold-start bind without flashing
// RED, while still turning RED on a genuinely stuck first bind. This is NOT a
// second parallel grace mechanism: the SAME `degradedSince` owner is compared
// against this threshold instead of RESTART_GRACE_MS, branched on
// `hasEverLoaded`.
const STARTUP_GRACE_MS = 45_000;

// graceThresholdMs picks the applicable grace bound for the CURRENT load
// state — the single place the two thresholds are selected, so the render
// gate and the deadline timer never diverge.
function graceThresholdMs(hasEverLoaded: boolean): number {
  return hasEverLoaded ? RESTART_GRACE_MS : STARTUP_GRACE_MS;
}

// REQUEST_TIMEOUT_MS bounds EVERY /api/status fetch with a client-side abort.
// A supervisor whose HTTP handler accepts the TCP connection but never writes
// a response (a wedged goroutine, a deadlocked IPC dial) makes the browser
// `fetch` hang FOREVER — a fail-unsafe class the render-time RED gate cannot
// close on its own, because a request that never resolves never produces a
// failing observation to time-stamp. The abort turns that silent hang into a
// resolved failing observation (an AbortError → `lastFailAt`), so a genuinely
// wedged supervisor still earns RED. 8s is chosen to sit ABOVE the 5s backend
// IPC deadline (internal/api/health.go:424 — a real 500 STATUS_FAILED resolves
// in <=5s and is NOT aborted, so its message still reaches the banner) and
// BELOW the 30s poll backstop (so a hung poll is abandoned before the next
// poll would fire).
const REQUEST_TIMEOUT_MS = 8_000;

export function DashboardScreen() {
  const [state, setState] = useState<Record<string, DaemonStatus>>({});
  const [error, setError] = useState<string | null>(null);
  // Restart-grace debounce — the SINGLE owner of the transient-vs-persistent
  // decision. A supervisor restart/handoff window (deploy, RestartV3
  // self-restart) makes /api/status fail for a few seconds; that is an
  // EXPECTED, self-healing event, NOT a hard failure. `degradedSince` records
  // a MONOTONIC `performance.now()` timestamp of the healthy→failing
  // transition (the FIRST failure of a streak) from EITHER failure source —
  // the ~30s HTTP-poll catch AND the SSE `poller-error` handler (the raw poll
  // count is the wrong signal because the SSE path never bumps it). A
  // monotonic clock is used deliberately: a backward wall-clock step (NTP / VM
  // time correction) during the grace window must NOT be able to disarm the
  // banner. ANY success — a successful /api/status OR a live SSE delta —
  // clears it back to null. The RED `dashboard-error` banner only fires once
  // the streak has lasted >= the applicable threshold (RESTART_GRACE_MS once
  // loaded, STARTUP_GRACE_MS while never-loaded — see `persistentlyDegraded`
  // at the render gate); within the grace we show a calm reconnecting cue.
  // This is the ONE time-based grace owner — there is no second parallel grace
  // mechanism; the two thresholds are just a `hasEverLoaded` branch over the
  // same owner.
  const [hasEverLoaded, setHasEverLoaded] = useState(false);
  const [degradedSince, setDegradedSince] = useState<number | null>(null);
  // lastFailAt is the monotonic `performance.now()` at which the MOST RECENT
  // RESOLVED FAILING /api/status observation resolved — a 500, a client abort
  // (AbortError from REQUEST_TIMEOUT_MS), or an SSE `poller-error`. It is the
  // ONLY thing that earns RED, and it is a COMMITTED observation timestamp, not
  // a latched boolean and not elapsed time. RED is decided at render time by
  // comparing this against `degradedSince` (see `persistentlyDegraded`): the
  // streak (identified by its unique monotonic `degradedSince` value) only goes
  // RED once a fresh failing observation lands AT/after the grace bound. Any of
  // the three failing sources (the deadline recheck, the next 30s poll, the SSE
  // poller-error) can write it, so there is NO single async writer that a hang
  // could wedge (round-1's SPOF). It is cleared to null on any success and by
  // the streak-reset effect when `degradedSince` returns to null.
  const [lastFailAt, setLastFailAt] = useState<number | null>(null);
  // graceTick is incremented by the grace-deadline timer to (a) force a
  // re-render exactly when the grace elapses, so the banner appears at the
  // bound even if no further poll or SSE event arrives (the 30s poll would
  // otherwise delay it), and (b) re-run the deadline effect so it SELF-RE-ARMS
  // a fresh timer if the bound has not actually been reached yet (belt-and-
  // suspenders against timer/clock skew) — see the effect below.
  const [graceTick, setGraceTick] = useState(0);
  // reloadTrigger is bumped by RecoveryActions after a successful
  // cleanup/restart so /api/status refetches immediately instead of
  // waiting for the 30 s poll interval. Pure state-bump pattern —
  // the value itself never affects rendering, only the useEffect
  // dependency that consults it on every bump.
  const [reloadTrigger, setReloadTrigger] = useState<number>(0);
  // Bulk-action state driven ENTIRELY by SSE "bulk-action" events. A
  // local click sends HTTP POST → server publishes "started" → this
  // handler flips inflight; "completed"/"error" clears inflight and
  // sets outcome for the flash. Tray-triggered runs reach the same
  // event stream so any open Dashboard sees the same animation.
  const [bulkInflight, setBulkInflight] = useState<BulkAction | null>(null);
  const [bulkOutcome, setBulkOutcome] = useState<BulkOutcome | null>(null);
  const [unmanagedStdioCount, setUnmanagedStdioCount] = useState(0);
  // Phase-0 item 1: honest hub-aggregate health. Fetched on mount, then updated
  // live by the `hub-health` SSE event so a hung/dead/needs-reconcile hub is
  // visible instead of every daemon card silently painting green.
  // The audit-lock projection is screen-level because both warnings report a
  // PROCESS-scoped supervisor-events.log flock, not a daemon-card condition.
  const [auditLockView, setAuditLockView] = useState<AuditLockView | null>(null);
  const auditLockViewRef = useRef<AuditLockView | null>(null);
  const [auditLockNotice, setAuditLockNotice] = useState<string | null>(null);
  const [retainedRecoveryReceipts, setRetainedRecoveryReceipts] = useState<
    RetainedRecoveryReceipt[]
  >([]);
  const pendingRecoveriesRef = useRef(new Map<string, DaemonRecoverCorrelation>());
  const acknowledgedReceiptsRef = useRef(new Set<string>());
  const acknowledgingReceiptsRef = useRef(new Set<string>());
  const auditLockSseSeqRef = useRef(0);
  const auditLockGetIssueRef = useRef(0);
  const auditLockMountedRef = useRef(false);
  const auditLockControllersRef = useRef(new Set<AbortController>());
  const loadStatusRef = useRef<() => Promise<boolean>>(async () => false);
  const refreshAuditLockBaselineRef = useRef<
    (surfaceFailure: boolean) => Promise<AuditLockView | null>
  >(async () => null);
  const [hubHealth, setHubHealth] = useState<HubHealth | null>(null);
  const hubHealthSseSeqRef = useRef(0);
  const hubHealthFetchSeqRef = useRef(0);
  const hubHealthAppliedSeqRef = useRef(0);
  const hubHealthMountedRef = useRef(false);
  const bulkResetTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(
    () => () => {
      if (bulkResetTimerRef.current) clearTimeout(bulkResetTimerRef.current);
    },
    [],
  );

  const applyAuditLockSnapshot = useCallback((snapshot: AuditLockSnapshot) => {
    setAuditLockView((current) => {
      const next = nextAuditLockView(current, snapshot);
      auditLockViewRef.current = next;
      return next;
    });
  }, []);

  const forgetPendingCorrelation = useCallback((correlation: DaemonRecoverCorrelation) => {
    for (const [taskName, retained] of pendingRecoveriesRef.current) {
      if (
        retained.attempt_id === correlation.attempt_id &&
        retained.occurrence_id === correlation.occurrence_id &&
        retained.server_instance === correlation.server_instance
      ) {
        pendingRecoveriesRef.current.delete(taskName);
      }
    }
  }, []);

  const hydrateDurableReceipts = useCallback((snapshot: AuditLockSnapshot) => {
    const pending = new Map<string, DaemonRecoverCorrelation>();
    const manual: RetainedRecoveryReceipt[] = [];
    for (const receipt of snapshot.recovery_receipts) {
      const correlation = {
        attempt_id: receipt.attempt_id,
        occurrence_id: receipt.occurrence_id,
        server_instance: receipt.server_instance,
      };
      pending.set(receipt.task_name, correlation);
      if (
        receipt.status === "committed_error" ||
        receipt.status === "not_committed" ||
        receipt.status === "uncertain"
      ) {
        manual.push({
          taskName: receipt.task_name,
          correlation,
          status: receipt.status,
        });
      }
    }
    pendingRecoveriesRef.current = pending;
    setRetainedRecoveryReceipts(manual);
  }, []);

  const getFreshAuditLockSnapshot = useCallback(async (
    correlation?: DaemonRecoverCorrelation,
  ): Promise<AuditLockSnapshot | null> => {
    const issue = ++auditLockGetIssueRef.current;
    const sseSeqAtIssue = auditLockSseSeqRef.current;
    const controller = new AbortController();
    auditLockControllersRef.current.add(controller);
    const timeout = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);
    try {
      const snapshot = await getDaemonRecoverAuditLockState(
        correlation,
        controller.signal,
      );
      if (!isAuditLockSnapshot(snapshot)) {
        throw new Error("invalid audit-lock state");
      }
      if (
        !auditLockMountedRef.current ||
        issue !== auditLockGetIssueRef.current ||
        sseSeqAtIssue !== auditLockSseSeqRef.current
      ) {
        return null;
      }
      const current = auditLockViewRef.current;
      if (
        current?.serverInstance === snapshot.server_instance &&
        snapshot.revision < current.revision
      ) {
        return null;
      }
      return snapshot;
    } finally {
      clearTimeout(timeout);
      auditLockControllersRef.current.delete(controller);
    }
  }, []);

  const acknowledgeTerminalReceipt = useCallback(async (
    taskName: string,
    correlation: DaemonRecoverCorrelation,
    snapshot: AuditLockSnapshot,
  ): Promise<RecoverySettlement> => {
    const receipt = snapshot.recovery_receipt;
    // A response/lookup is only an acknowledgement authority when it is at
    // least as new as the projection already observed from SSE.  In
    // particular, a delayed "released" response must not consume a receipt
    // after a newer "outstanding" physical-lock update has re-fenced it.
    if (nextAuditLockView(auditLockViewRef.current, snapshot) === auditLockViewRef.current) {
      return receipt?.status ?? "missing";
    }
    applyAuditLockSnapshot(snapshot);
    if (receipt === null) return "missing";
    if (
      receipt.attempt_id !== correlation.attempt_id ||
      receipt.occurrence_id !== correlation.occurrence_id ||
      receipt.server_instance !== correlation.server_instance
    ) {
      setAuditLockNotice(
        "Recovery receipt did not match the action that created it. It was not acknowledged or relabeled.",
      );
      return "conflict";
    }
    if (receipt.status === "in_flight") return "in_flight";
    if (receipt.status === "consumed") {
      forgetPendingCorrelation(correlation);
      return "consumed";
    }
    if (
      receipt.status === "committed_error" ||
      receipt.status === "not_committed" ||
      receipt.status === "uncertain"
    ) {
      const manualStatus = receipt.status;
      setRetainedRecoveryReceipts((current) => {
        const retained = current.filter(
          (item) =>
            item.correlation.attempt_id !== correlation.attempt_id ||
            item.correlation.occurrence_id !== correlation.occurrence_id ||
            item.correlation.server_instance !== correlation.server_instance,
        );
        retained.push({ taskName, correlation, status: manualStatus });
        return retained;
      });
      return receipt.status;
    }
    // A daemon-status refresh proves only that the supervisor answered.  The
    // receipt is allowed to be consumed only after the backend's physical-lock
    // snapshot confirms that the GUI writer released its cross-process fence.
    // Keep the exact task/correlation in pendingRecoveriesRef while the
    // transient release_pending state is outstanding; reconnect and SSE
    // reconciliation re-query this same tuple rather than issuing another POST.
    if (snapshot.state !== "released") {
      setAuditLockNotice(
        "Recovery completed, but the supervisor event-log lock is still settling. Its receipt remains retained; the dashboard will reconcile it after the physical lock releases.",
      );
      return receipt.status;
    }
    const expectedPhysical: AuditLockExpectedPhysical = {
      server_instance: snapshot.server_instance,
      revision: snapshot.revision,
      state: "released",
    };
    if (!await loadStatusRef.current()) {
      setAuditLockNotice(
        "Recovery finished, but current daemon status could not be refreshed. Its receipt remains retained and no retry was started.",
      );
      return receipt.status;
    }

    // Status refresh is an async gap. SSE may advance the physical owner while
    // it is pending, so compare the exact projection again immediately before
    // dispatch. The backend repeats this check atomically at commit time for
    // transitions the browser has not observed yet.
    const currentPhysical = auditLockViewRef.current;
    if (
      currentPhysical?.serverInstance !== expectedPhysical.server_instance ||
      currentPhysical.revision !== expectedPhysical.revision ||
      currentPhysical.state !== expectedPhysical.state
    ) {
      setAuditLockNotice(
        "Recovery finished, but the physical lock state changed before acknowledgement. Its receipt remains retained and is being reconciled.",
      );
      await refreshAuditLockBaselineRef.current(false);
      return receipt.status;
    }

    const receiptKey = `${correlation.attempt_id}/${correlation.occurrence_id}`;
    if (!acknowledgedReceiptsRef.current.has(receiptKey)) {
      if (!acknowledgingReceiptsRef.current.has(receiptKey)) {
        acknowledgingReceiptsRef.current.add(receiptKey);
        try {
          await acknowledgeDaemonRecoverReceipt(correlation, expectedPhysical);
          acknowledgedReceiptsRef.current.add(receiptKey);
        } catch (error) {
          const code = apiErrorCode(error);
          if (
            code === "RECOVER_ACK_PRECONDITION_REQUIRED" ||
            code === "RECOVER_ACK_PHYSICAL_STATE_CHANGED"
          ) {
            setAuditLockNotice(
              "Recovery finished, but the backend physical lock state changed before acknowledgement. Its receipt remains retained and is being reconciled.",
            );
            await refreshAuditLockBaselineRef.current(false);
            return receipt.status;
          }
          setAuditLockNotice(
            "Recovery finished, but its receipt could not be acknowledged. The dashboard will retry the safe acknowledgement after reconnecting.",
          );
          return receipt.status;
        } finally {
          acknowledgingReceiptsRef.current.delete(receiptKey);
        }
      } else {
        return receipt.status;
      }
    }
    forgetPendingCorrelation(correlation);
    await refreshAuditLockBaselineRef.current(false);
    return receipt.status;
  }, [applyAuditLockSnapshot, forgetPendingCorrelation]);

  const refreshAuditLockBaseline = useCallback(async (
    surfaceFailure: boolean,
  ): Promise<AuditLockView | null> => {
    try {
      const snapshot = await getFreshAuditLockSnapshot();
      if (snapshot === null) return auditLockViewRef.current;
      const next = nextAuditLockView(auditLockViewRef.current, snapshot);
      applyAuditLockSnapshot(snapshot);
      hydrateDurableReceipts(snapshot);
      return next;
    } catch (error) {
      if (surfaceFailure) {
        setAuditLockNotice(
          "Recovery state is unavailable. No recovery request was sent; wait for the dashboard to reconnect.",
        );
      }
      return null;
    }
  }, [applyAuditLockSnapshot, getFreshAuditLockSnapshot, hydrateDurableReceipts]);
  refreshAuditLockBaselineRef.current = refreshAuditLockBaseline;

  const reconcileRecovery = useCallback(async (
    taskName: string,
    correlation: DaemonRecoverCorrelation,
  ): Promise<RecoverySettlement> => {
    try {
      const snapshot = await getFreshAuditLockSnapshot(correlation);
      if (snapshot === null) return "missing";
      return await acknowledgeTerminalReceipt(taskName, correlation, snapshot);
    } catch (error) {
      if (apiErrorCode(error) === "RECOVER_BASELINE_STALE") {
        pendingRecoveriesRef.current.delete(taskName);
        setAuditLockNotice(
          "The recovery server restarted before this action could be reconciled. Its old identifiers were not relabeled; check daemon status before starting another recovery.",
        );
      }
      return "missing";
    }
  }, [acknowledgeTerminalReceipt]);

  const reconcilePendingRecoveries = useCallback(async () => {
    const pending = [...pendingRecoveriesRef.current.entries()];
    for (const [taskName, correlation] of pending) {
      const settlement = await reconcileRecovery(taskName, correlation);
      if (settlement === "committed_success") {
        setAuditLockNotice("Recovery result restored after reconnecting.");
        setReloadTrigger((n) => n + 1);
      } else if (settlement === "committed_error") {
        setAuditLockNotice(
          "Recovery stopped a process but completed with an error. Do NOT repeat it; check daemon status and supervisor logs.",
        );
      } else if (settlement === "not_committed") {
        setAuditLockNotice(
          "Recovery did not commit. Check current daemon status before deciding whether to start a new action.",
        );
      } else if (settlement === "uncertain") {
        setAuditLockNotice(
          "Recovery outcome is uncertain. Do NOT repeat it; refresh daemon status, inspect logs, then explicitly acknowledge this warning.",
        );
      }
    }
  }, [reconcileRecovery]);

  const reconcileAuditLockState = useCallback(async () => {
    await refreshAuditLockBaseline(false);
    await reconcilePendingRecoveries();
  }, [reconcilePendingRecoveries, refreshAuditLockBaseline]);

  // mountedRef gates every `loadStatus` state-apply. `loadStatus` is now a
  // hoisted `useCallback` (single fetch+apply owner) that can resolve after the
  // component unmounts — so this ref replaces the old effect-local `cancelled`
  // flag. It starts `true` so the very first mount-time `loadStatus` (issued by
  // the poll effect below before this effect runs) is never dropped.
  const mountedRef = useRef(true);
  // recheckIssuedRef is a per-streak once-guard: the grace-deadline effect fires
  // AT MOST ONE `/api/status` recheck per degraded streak (anti-D1 — no fast
  // polling, so a flapping supervisor cannot streak-reset RED away). It is a
  // fail-safe ISSUE throttle only; it is OFF the RED-decision path (RED reads
  // committed timestamps, never this flag), so the recheck can safely be
  // fire-and-forget. Reset by the streak-reset effect when the streak clears.
  const recheckIssuedRef = useRef(false);
  // loadStatus issue-freshness guard. loadSeqRef is a monotonic per-call issue
  // counter (++ at call time); appliedSeqRef is the highest issue seq that has
  // committed a state apply. Two /api/status calls can be in flight at once — the
  // bound recheck vs the 30s poll, or a reloadTrigger reload vs a prior in-flight
  // poll — and resolve OUT OF ISSUE ORDER. Without this, a stale-by-issue success
  // can clear degradedSince/lastFailAt that a fresher-issued failure already
  // committed → RED falsely cleared/delayed. Every apply (success AND catch)
  // drops if a later-issued call already applied (last-issued-wins), mirroring the
  // resyncHubHealth fetch-seq idiom — but deliberately NOT its SSE-seq clause: a
  // fresh HTTP success is authoritative over a stale SSE failing signal here, so
  // this guard is scoped to HTTP-vs-HTTP ordering only. Refs, not state: they
  // ORDER applies, never render.
  const loadSeqRef = useRef(0);
  const appliedSeqRef = useRef(0);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  // loadStatus is the SINGLE /api/status fetch+apply owner (hoisted out of the
  // poll effect so the grace-deadline recheck can share it). It returns a
  // boolean so a caller that awaits it can branch on success/failure, but the
  // RED decision NEVER depends on its return value — it depends only on the two
  // committed timestamps it writes (`degradedSince`, `lastFailAt`).
  const loadStatus = useCallback(async (): Promise<boolean> => {
    // Capture this call's issue order FIRST (before the fetch), so an
    // out-of-order resolution can be dropped by the apply-gates below.
    const seq = ++loadSeqRef.current;
    // Bound the fetch with a client abort (see REQUEST_TIMEOUT_MS). Uses
    // AbortController + setTimeout deliberately (NOT AbortSignal.timeout) so the
    // deadline is drivable under Vitest fake timers. The signal threads through
    // fetchOrThrow's existing `init` argument (api.ts) — fetchOrThrow itself is
    // unchanged.
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);
    try {
      const rows = await fetchOrThrow<DaemonStatus[]>("/api/status", "array", {
        signal: controller.signal,
      });
      if (!mountedRef.current) return false;
      // Drop a stale-by-issue apply: a later-issued call already committed.
      if (seq < appliedSeqRef.current) return true;
      appliedSeqRef.current = seq;
      const next: Record<string, DaemonStatus> = {};
      // Scheduler-maintenance rows (weekly-refresh tasks) have no
      // meaningful "Restart" action. Rendering them would produce a
      // blank-name card whose Restart button hits
      // /api/servers//restart → invalid target.
      for (const row of rows.filter((r) => !r.is_maintenance)) {
        next[keyFor(row)] = row;
      }
      setState(next);
      setError(null);
      setHasEverLoaded(true);
      // Success clears the streak: both the start-of-streak marker and the
      // failing-observation timestamp. (The streak-reset effect also nulls
      // lastFailAt when degradedSince goes null — this is the direct clear.)
      setDegradedSince(null);
      setLastFailAt(null);
      return true;
    } catch (err) {
      if (!mountedRef.current) return false;
      // Drop a stale-by-issue apply: a later-issued call already committed.
      if (seq < appliedSeqRef.current) return false;
      appliedSeqRef.current = seq;
      const e = err as Error;
      // An abort means the supervisor accepted the connection but never
      // answered within the deadline — a distinct, actionable failure from a
      // backend-reported 500. Map it to a plain-language message; any other
      // error carries the backend/degraded message fetchOrThrow surfaced.
      const message =
        e && e.name === "AbortError"
          ? "supervisor not responding (timed out)"
          : e.message;
      setError(message);
      // Mark the healthy→failing transition once per streak — keep the FIRST
      // failure's MONOTONIC timestamp (prev ?? performance.now()) so the grace
      // window measures from the start of the outage, not the latest poll, and
      // is immune to a wall-clock step.
      setDegradedSince((prev) => prev ?? performance.now());
      // Record this RESOLVED failing observation. Compared against degradedSince
      // at render time, it is what earns RED once it lands at/after the bound.
      setLastFailAt(performance.now());
      return false;
    } finally {
      clearTimeout(timer);
    }
  }, []);
  loadStatusRef.current = loadStatus;

  const acknowledgeRetainedRecoveryReceipts = useCallback(async (): Promise<void> => {
    if (retainedRecoveryReceipts.length === 0) return;
    if (!await loadStatus()) {
      setAuditLockNotice(
        "Current daemon status could not be refreshed. The recovery warning remains retained and was not acknowledged.",
      );
      return;
    }
    const remaining: RetainedRecoveryReceipt[] = [];
    for (const retained of retainedRecoveryReceipts) {
      try {
        await acknowledgeDaemonRecoverReceipt(retained.correlation);
        forgetPendingCorrelation(retained.correlation);
      } catch {
        remaining.push(retained);
      }
    }
    setRetainedRecoveryReceipts(remaining);
    if (remaining.length > 0) {
      setAuditLockNotice(
        "One or more recovery warnings could not be acknowledged. They remain retained for safe reconciliation.",
      );
      return;
    }
    setAuditLockNotice(null);
    await refreshAuditLockBaseline(false);
  }, [
    forgetPendingCorrelation,
    loadStatus,
    refreshAuditLockBaseline,
    retainedRecoveryReceipts,
  ]);

  // Status bootstrap + polling. The 30s poll backs the supervisor IPC
  // status path while live daemon-state SSE deltas are still on the
  // legacy scheduler stream. NO fast poll while degraded (anti-D1: a higher
  // sampling density would let a flapping supervisor streak-reset RED away).
  useEffect(() => {
    void loadStatus();
    const poll = setInterval(() => {
      void loadStatus();
    }, 30_000);
    return () => {
      clearInterval(poll);
    };
  }, [reloadTrigger, loadStatus]);

  // Grace-deadline effect: while a streak is within grace, self-re-arm a timer
  // that bumps `graceTick` exactly when the bound elapses (so this effect
  // re-runs at the bound even if no poll/SSE arrives). AT the bound, fire ONE
  // fresh `/api/status` recheck — a POSITIVE fresh observation, so a supervisor
  // that recovered silently within grace (no SSE delta, no interim poll) is
  // re-observed as HEALTHY at the bound and never flips a false RED on a stale
  // sample (bot #564 P1). The recheck is FIRE-AND-FORGET: RED is decided purely
  // by the render-time timestamp compare, so a hung recheck cannot wedge the
  // banner — the 30s poll and the SSE poller-error remain independent failing-
  // observation sources (no single-writer SPOF). recheckIssuedRef throttles it
  // to once per streak. Uses the SAME monotonic clock as degradedSince (never
  // Date.now()) and the hasEverLoaded-branched threshold.
  useEffect(() => {
    if (degradedSince === null) return;
    const remaining =
      graceThresholdMs(hasEverLoaded) - (performance.now() - degradedSince);
    if (remaining <= 0) {
      if (recheckIssuedRef.current) return;
      recheckIssuedRef.current = true;
      void loadStatus();
      return;
    }
    const t = setTimeout(() => setGraceTick((n: number) => n + 1), remaining);
    return () => clearTimeout(t);
  }, [degradedSince, graceTick, hasEverLoaded, loadStatus]);

  // Streak-reset effect: the SINGLE clearer of the per-streak recheck guard and
  // the failing-observation timestamp. Reached whenever `degradedSince` returns
  // to null — by BOTH recovery sources: a successful `loadStatus` and an SSE
  // daemon-state delta (`onDelta` nulls degradedSince). Nulling lastFailAt here
  // guarantees a NEW streak starts with no stale failing timestamp, and
  // re-arming recheckIssuedRef lets the next streak issue its own one recheck.
  useEffect(() => {
    if (degradedSince === null) {
      recheckIssuedRef.current = false;
      setLastFailAt(null);
    }
  }, [degradedSince]);

  useEffect(() => {
    let cancelled = false;
    async function loadScan() {
      try {
        const scan = await fetchOrThrow<ScanResult>("/api/scan", "object");
        if (!cancelled) {
          setUnmanagedStdioCount(countUnmanagedStdio(scan.entries));
        }
      } catch {
        if (!cancelled) {
          setUnmanagedStdioCount(0);
        }
      }
    }
    void loadScan();
    const poll = setInterval(() => {
      void loadScan();
    }, 30_000);
    return () => {
      cancelled = true;
      clearInterval(poll);
    };
  }, []);

  // SSE delta handler. Same maintenance filter as bootstrap — otherwise a
  // weekly-refresh transition would re-inject a blank-name card after the
  // initial filter dropped it.
  const onDelta = useCallback((ev: MessageEvent) => {
    const body = JSON.parse(ev.data) as DaemonStatus & { state: string };
    if (body.is_maintenance) return;
    const k = keyFor(body);
    // A valid delta means the backend is reachable — clear any stale
    // bootstrap error so the early-return at render time falls through
    // and cards render from live state. Without this the Dashboard
    // stays locked on "Failed to load status" forever after a transient
    // startup 500, even though /api/events is streaming fine.
    // (GitHub Codex PR #1 R1.)
    setError(null);
    // A live delta is a success from the SSE source: clear the restart-grace
    // timestamp (the single owner both failure sources feed) and record that
    // we have loaded real data.
    setHasEverLoaded(true);
    setDegradedSince(null);
    setState((prev) => {
      if (body.state === "Gone") {
        const next = { ...prev };
        delete next[k];
        return next;
      }
      return { ...prev, [k]: { ...(prev[k] ?? { server: body.server, daemon: body.daemon }), ...body } };
    });
  }, []);

  // SSE handler for bulk-action lifecycle (PR #38: unified pipeline).
  // Backend publishes started → completed|error around every fan-out;
  // we mirror that into local UI state. The outcome flash auto-clears
  // after 1.5s so the button label snaps back to "Run all"/"Stop all".
  const onBulkAction = useCallback((ev: MessageEvent) => {
    const body = JSON.parse(ev.data) as {
      phase: "started" | "completed" | "error";
      action: BulkAction;
    };
    if (body.phase === "started") {
      // Idempotent confirmation — local click already optimistically
      // set bulkInflight. SSE re-confirms for tray-triggered fan-out
      // (no local click) and event reordering.
      //
      // Codex bot PR #38 P1 (commit ef0f4ea, "Correlate bulk-action
      // terminal events before unlocking UI"): with the shared SSE
      // pipeline, concurrent triggers (Dashboard + tray, or two
      // Dashboards) can interleave events. Don't OVERWRITE the
      // currently-tracked inflight action from a different
      // started — keep the first-tracked action so terminal-match
      // logic below stays sound.
      setBulkInflight((cur) => cur ?? body.action);
      setBulkOutcome(null);
      return;
    }
    // Terminal phase. Only clear inflight if the event's action
    // matches what we're tracking — otherwise this is a sibling
    // operation's terminal and the locally-tracked operation may
    // still be running.
    setBulkInflight((cur) => (cur === body.action ? null : cur));
    setBulkOutcome({
      action: body.action,
      state: body.phase === "error" ? "error" : "done",
    });
    if (bulkResetTimerRef.current) clearTimeout(bulkResetTimerRef.current);
    bulkResetTimerRef.current = setTimeout(() => {
      setBulkOutcome(null);
      bulkResetTimerRef.current = null;
    }, 1500);
  }, []);

  // SSE handler for poller-error. The backend StatusPoller routes
  // through the fail-loud supervisor-IPC snapshot; when the supervisor
  // is unreachable it emits a `poller-error` event carrying the error
  // string (internal/gui/poller.go). Surfacing it here sets the degraded
  // banner within one poll cycle (5s) instead of waiting up to 30s for
  // the separate /api/status 500 poll below. This is the POSITIVE SSE
  // degraded path: the round-1 fix only stopped the poller from CLEARING
  // the banner (it no longer emits banner-clearing daemon-state deltas on
  // a down supervisor); this listener makes the SSE channel actively SET
  // it. The 30s HTTP poll remains the durable backstop. (PR #281 P3.)
  const onPollerError = useCallback((ev: MessageEvent) => {
    const body = JSON.parse(ev.data) as { err?: string };
    setError(body.err ?? "supervisor unreachable");
    // Source-agnostic degraded marker: the SSE path never bumps the HTTP poll
    // count, so `degradedSince` is the single owner both sources feed. Keep the
    // first failure's MONOTONIC `performance.now()` timestamp across the streak
    // — the SAME clock the HTTP catch records and the grace timer/compare read,
    // so a wall-clock step can't skew the debounce (bot #564 P1).
    setDegradedSince((prev) => prev ?? performance.now());
    // The SSE poller-error IS a resolved failing observation — record its
    // timestamp so it can earn RED once it lands at/after the grace bound, the
    // same as a failing HTTP poll or the deadline recheck (no single writer).
    setLastFailAt(performance.now());
  }, []);

  // SSE handler for daemon-failed (backend rising-edge failure event,
  // internal/gui/poller.go). Fires a sticky danger toast so the operator
  // gets an explicit, must-acknowledge alert the moment a daemon trips the
  // failure predicate — the same gate that turns the tray icon red. The
  // daemon-state delta still updates the card; this is the attention-grabbing
  // overlay on top of it. Edge-triggered upstream, so no toast spam while a
  // daemon stays failed.
  const onDaemonFailed = useCallback((ev: MessageEvent) => {
    const body = JSON.parse(ev.data) as {
      server: string;
      daemon?: string;
      state?: string;
      last_result?: number;
    };
    const who = body.daemon && body.daemon !== "default"
      ? `${body.server}/${body.daemon}`
      : body.server;
    const code = typeof body.last_result === "number" && body.last_result !== 0
      ? ` (exit ${body.last_result})`
      : "";
    pushToast("danger", `Daemon ${who} failed${code}`);
  }, []);

  // SSE handler for daemon-recovered (poller falling edge). The all-clear
  // paired with daemon-failed: a success toast (auto-dismisses by variant
  // default) so the operator who saw the sticky failure also sees the
  // recovery without having to re-read the cards.
  const onDaemonRecovered = useCallback((ev: MessageEvent) => {
    const body = JSON.parse(ev.data) as { server: string; daemon?: string };
    const who = body.daemon && body.daemon !== "default"
      ? `${body.server}/${body.daemon}`
      : body.server;
    pushToast("success", `Daemon ${who} recovered`);
  }, []);

  const onHubHealth = useCallback((ev: MessageEvent) => {
    const body = JSON.parse(ev.data) as HubHealth;
    hubHealthSseSeqRef.current += 1;
    setHubHealth(body);
  }, []);

  const onAuditLockState = useCallback((ev: MessageEvent) => {
    const body = JSON.parse(ev.data) as unknown;
    if (!isAuditLockSnapshot(body)) {
      setAuditLockNotice("The dashboard received an invalid audit-lock update and ignored it.");
      return;
    }
    auditLockSseSeqRef.current += 1;
    applyAuditLockSnapshot(body);
    void reconcilePendingRecoveries();
  }, [applyAuditLockSnapshot, reconcilePendingRecoveries]);

  const resyncHubHealth = useCallback((): (() => void) => {
    let cancelled = false;
    const mySeq = ++hubHealthFetchSeqRef.current;
    const sseSeqAtIssue = hubHealthSseSeqRef.current;
    getHubHealth()
      .then((h) => {
        if (
          !cancelled &&
          hubHealthMountedRef.current &&
          mySeq > hubHealthAppliedSeqRef.current &&
          sseSeqAtIssue === hubHealthSseSeqRef.current
        ) {
          setHubHealth(h);
          hubHealthAppliedSeqRef.current = mySeq;
        }
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, []);

  // connectionState surfaces the live SSE transport status so the
  // Dashboard header can show a "live / reconnecting…" cue. When the
  // supervisor/GUI drops, native EventSource retries silently; without
  // this cue the cards would keep showing the last snapshot with no
  // signal the data is stale. (G13 resilience.)
  const connectionState = useEventSource("/api/events", {
    "daemon-state": onDelta,
    "bulk-action": onBulkAction,
    "poller-error": onPollerError,
    "daemon-failed": onDaemonFailed,
    "daemon-recovered": onDaemonRecovered,
    "hub-health": onHubHealth,
    "audit-lock-state": onAuditLockState,
  });

  useEffect(() => {
    auditLockMountedRef.current = true;
    void reconcileAuditLockState();
    const onVisibilityChange = () => {
      if (document.visibilityState === "visible") {
        void reconcileAuditLockState();
      }
    };
    document.addEventListener("visibilitychange", onVisibilityChange);
    const poll = setInterval(() => {
      void reconcileAuditLockState();
    }, 60_000);
    return () => {
      auditLockMountedRef.current = false;
      auditLockGetIssueRef.current += 1;
      document.removeEventListener("visibilitychange", onVisibilityChange);
      clearInterval(poll);
      for (const controller of auditLockControllersRef.current) {
        controller.abort();
      }
      auditLockControllersRef.current.clear();
    };
  }, [reconcileAuditLockState]);

  // Initial hub-aggregate health plus connected-stream backstops. SSE only
  // pushes transitions and can drop without reconnecting, so resync on mount,
  // foreground visibility, and a low-frequency interval. Non-fatal: a failed
  // probe leaves the last health state in place.
  useEffect(() => {
    hubHealthMountedRef.current = true;
    const cancelInitial = resyncHubHealth();
    const onVisibilityChange = () => {
      if (document.visibilityState === "visible") {
        void resyncHubHealth();
      }
    };
    document.addEventListener("visibilitychange", onVisibilityChange);
    const poll = setInterval(() => {
      void resyncHubHealth();
    }, 60_000);
    return () => {
      hubHealthMountedRef.current = false;
      cancelInitial();
      document.removeEventListener("visibilitychange", onVisibilityChange);
      clearInterval(poll);
    };
  }, [resyncHubHealth]);

  // The hub-health stream is transition-only and lossy. Re-hydrate whenever
  // EventSource opens (including after reconnect), while rejecting a response
  // if a newer fetch or SSE transition landed after this request was issued.
  useEffect(() => {
    if (connectionState !== "open") return;
    return resyncHubHealth();
  }, [connectionState, resyncHubHealth]);

  // Settlement SSE is transition-only and lossy. Every initial connection and
  // reconnect repairs both the process-wide state and any retained exact receipt
  // with GET; the destructive POST itself is never retried.
  useEffect(() => {
    if (connectionState !== "open") return;
    void reconcileAuditLockState();
  }, [connectionState, reconcileAuditLockState]);

  // Codex bot PR #38 P1 (round 3): safety-net for dropped SSE events.
  // The Broadcaster is lossy (internal/gui/events.go::Publish drops on
  // full subscriber buffer), so bulkInflight could stay set forever if
  // the terminal event is dropped.
  //
  // Timeout MUST exceed realistic fan-out duration. api.Restart calls
  // killDaemonByPort(5s) + waitForPortFree(3s) per daemon = up to 8s
  // each, and runs sequentially. With 11 daemons × 8s = 88s worst-case
  // legit. Plus serena spawn-up time (~3-6s each). 5min cap is well
  // beyond any realistic fan-out and short enough that a truly stuck
  // UI doesn't trap the user indefinitely. Codex bot review on commit
  // d92aa2c P1 ("Keep bulk-action lock until terminal SSE event").
  useEffect(() => {
    if (bulkInflight === null) return;
    const t = setTimeout(() => {
      setBulkInflight(null);
    }, 300_000);
    return () => clearTimeout(t);
  }, [bulkInflight]);

  // Backend contract:
  //   POST /api/servers/<server>/<action>             — all daemons
  //   POST /api/servers/<server>/<action>?daemon=<n>  — only that daemon
  //
  //   200 { <action>_results: [...] }            — all OK
  //   207 { <action>_results: [...] }            — partial: some Err non-empty
  //   400                                         — empty/repeated ?daemon
  //   404 { error, code: DAEMON_NOT_FOUND }       — ?daemon matched no task
  //   500 { <action>_results: [...], error, code } — orchestration failure
  //
  // The ?daemon scope is REQUIRED for multi-daemon servers (serena ships
  // claude + codex). Without it, clicking Restart on the codex card was
  // restarting claude too — see PR #32 / 2026-04-30 bug report.
  //
  // Re-throws on failure so the Card button state machine can flash
  // "Failed". Caller logs for operator triage.
  async function postServerAction(
    server: string,
    daemon: string | undefined,
    action: "restart" | "stop",
  ) {
    const resultsKey = `${action}_results` as const;
    let url = `/api/servers/${encodeURIComponent(server)}/${action}`;
    if (daemon) url += `?daemon=${encodeURIComponent(daemon)}`;
    try {
      const resp = await fetch(url, { method: "POST" });
      const body = (await resp.json().catch(() => ({}))) as {
        error?: string;
        code?: string;
        [k: string]: unknown;
      };
      if (resp.status === 500) {
        throw new Error(body.error ?? String(resp.status));
      }
      if (resp.status === 207) {
        const rows = (body[resultsKey] as Array<{ task_name: string; error: string }>) ?? [];
        const failed = rows.filter((r) => r.error !== "");
        const summary = failed.map((r) => `${r.task_name}: ${r.error}`).join("; ");
        throw new Error(`partial ${action} failure: ${summary}`);
      }
      if (!resp.ok) {
        throw new Error(body.error ?? String(resp.status));
      }
    } catch (e) {
      console.error(`${action} ${server}/${daemon ?? "*"}: ${(e as Error).message}`);
      throw e;
    }
  }

  const restart = (server: string, daemon: string | undefined) =>
    postServerAction(server, daemon, "restart");
  const stop = (server: string, daemon: string | undefined) =>
    postServerAction(server, daemon, "stop");

  // Bulk actions back the Dashboard header buttons. Backend routes
  // /api/restart-all and /api/stop-all share the same 200/207/500
  // contract as per-server actions, only without ?daemon scoping.
  async function postBulkAction(action: "restart" | "stop") {
    const resultsKey = `${action}_results` as const;
    try {
      const resp = await fetch(`/api/${action}-all`, { method: "POST" });
      const body = (await resp.json().catch(() => ({}))) as {
        error?: string;
        code?: string;
        [k: string]: unknown;
      };
      if (resp.status === 500) {
        throw new Error(body.error ?? String(resp.status));
      }
      if (resp.status === 207) {
        const rows = (body[resultsKey] as Array<{ task_name: string; error: string }>) ?? [];
        const failed = rows.filter((r) => r.error !== "");
        const summary = failed.map((r) => `${r.task_name}: ${r.error}`).join("; ");
        throw new Error(`partial ${action}-all failure: ${summary}`);
      }
      if (!resp.ok) {
        throw new Error(body.error ?? String(resp.status));
      }
    } catch (e) {
      console.error(`${action}-all: ${(e as Error).message}`);
      throw e;
    }
  }
  // Codex bot PR #38 P2 (rejected fetch fallback) + P1 (re-entrant
  // double-click). Optimistic-update pattern handles BOTH:
  //
  //   click → setBulkInflight("restart") IMMEDIATELY → buttons
  //   disable, re-entrant click is gated by the same state check.
  //   SSE "started" arrives ~50ms later; idempotent setter no-ops.
  //   SSE terminal arrives → clear inflight, set outcome.
  //
  // A rejected fetch (network failure, no SSE will arrive) lands in
  // .catch → setLocalErrorFallback restores idle + flashes Failed.
  // For 207/500 fetch also rejects, but SSE error event already set
  // outcome=error so prev ?? wins (idempotent).
  const setLocalErrorFallback = useCallback((action: BulkAction) => {
    setBulkInflight(null);
    // Codex bot PR #38 P2 (commit ff656fe): prev ?? error preserved
    // STALE outcomes from prior actions. If user clicked Run all
    // (success → outcome=done flash), then clicked Stop all within
    // the 1.5s flash window and Stop's POST rejected → prev=done
    // (restart) ?? would suppress the new error → user sees no
    // feedback for the failed Stop click. Fix: only keep prev if
    // it's for the SAME action (idempotent on real partial-fail
    // where SSE 'error' already arrived); otherwise set new error.
    setBulkOutcome((prev) =>
      prev && prev.action === action ? prev : { action, state: "error" },
    );
    if (bulkResetTimerRef.current) clearTimeout(bulkResetTimerRef.current);
    bulkResetTimerRef.current = setTimeout(() => {
      setBulkOutcome(null);
      bulkResetTimerRef.current = null;
    }, 1500);
  }, []);
  function fireBulk(action: BulkAction): Promise<void> {
    if (bulkInflight !== null) return Promise.resolve();
    setBulkInflight(action); // optimistic: locks UI immediately
    return postBulkAction(action).catch(() => setLocalErrorFallback(action));
  }
  const runAll = () => fireBulk("restart");
  const stopAll = () => fireBulk("stop");

  async function recoverDaemon(taskName: string): Promise<void> {
    const retained = pendingRecoveriesRef.current.get(taskName);
    if (retained) {
      const settlement = await reconcileRecovery(taskName, retained);
      if (settlement === "committed_success" || settlement === "consumed") {
        setReloadTrigger((n) => n + 1);
        return;
      }
      throw new RecoveryFlowError(
        settlement === "committed_error"
          ? "Recovery already stopped a process but completed with an error. Do NOT retry; check current daemon status and supervisor logs."
          : settlement === "not_committed"
            ? "The prior recovery did not commit. Check current daemon status before starting another recovery."
            : settlement === "uncertain"
              ? "The prior recovery outcome is uncertain. Do NOT retry; refresh status, inspect logs, then acknowledge the retained warning."
            : "The prior recovery action is still unresolved. Do NOT submit it again; the dashboard will reconcile it after reconnecting.",
      );
    }
    let baseline = auditLockViewRef.current;
    if (!baseline) {
      baseline = await refreshAuditLockBaseline(true);
    }
    if (!baseline) {
      throw new RecoveryFlowError(
        "Recovery state is unavailable. No recovery request was sent; wait for the dashboard to reconnect.",
      );
    }

    // A POST starts a new occurrence epoch. Abort and invalidate baseline GETs
    // issued before the reservation so an older empty response cannot erase
    // the newly retained correlation.
    auditLockGetIssueRef.current += 1;
    for (const controller of auditLockControllersRef.current) {
      controller.abort();
    }
    auditLockControllersRef.current.clear();
    const correlation = newDaemonRecoverCorrelation(baseline.serverInstance);
    pendingRecoveriesRef.current.set(taskName, correlation);
    setAuditLockNotice(null);

    try {
      const result = await postDaemonRecover(taskName, correlation);
      const settlement = await acknowledgeTerminalReceipt(
        taskName,
        correlation,
        result.audit_lock,
      );
      if (result.state === "recovery_in_flight" || settlement === "in_flight") {
        throw new RecoveryFlowError(
          "Recovery is still running. Do NOT submit it again; the dashboard will reconcile the retained receipt.",
        );
      }
      if (settlement !== "committed_success" && settlement !== "consumed") {
        throw new RecoveryFlowError(
          "Recovery returned without a committed success receipt. Do NOT repeat it; check current daemon status.",
        );
      }
      setReloadTrigger((n) => n + 1);
    } catch (error) {
      if (error instanceof RecoveryFlowError) throw error;

      const snapshot = auditLockSnapshotFromError(error);
      if (snapshot) {
        await acknowledgeTerminalReceipt(taskName, correlation, snapshot);
        throw error;
      }

      const code = apiErrorCode(error);
      if (code) {
        // Reservation-level failures happen before daemon recovery. They have no
        // receipt to retain and must never mutate/relabel the correlation.
        pendingRecoveriesRef.current.delete(taskName);
        if (code === "RECOVER_BASELINE_STALE") {
          await refreshAuditLockBaseline(false);
        }
        throw error;
      }

      // The POST response was lost. Repair with the exact retained tuple; never
      // issue a second destructive POST.
      const settlement = await reconcileRecovery(taskName, correlation);
      if (settlement === "committed_success" || settlement === "consumed") {
        setReloadTrigger((n) => n + 1);
        return;
      }
      throw new RecoveryFlowError(
        settlement === "committed_error"
          ? "Recovery already stopped a process but completed with an error. Do NOT retry; check current daemon status and supervisor logs."
          : settlement === "not_committed"
            ? "Recovery did not commit. Check current daemon status before starting a new action."
            : settlement === "uncertain"
              ? "The recovery outcome is uncertain. Do NOT retry; refresh status, inspect logs, then explicitly acknowledge the warning."
            : "The recovery response was lost and its exact receipt is not terminal yet. Do NOT retry; the dashboard will reconcile it after reconnecting.",
      );
    }
  }

  // Restart-grace debounce, as a PURE render-time comparison of two committed
  // observation timestamps — never elapsed wall-time, never a latched boolean.
  // RED requires a fresh RESOLVED failing observation (`lastFailAt`) that landed
  // AT/after the grace bound MEASURED FROM this streak's start (`degradedSince`).
  // Streak identity is the monotonic `degradedSince` value: re-latching a streak
  // needs an intervening success (real wall time), so a stale failing recheck
  // that resolves after a recovery re-latches an AGE-0 streak
  // (lastFailAt - degradedSince = 0 < bound) and can NEVER stamp RED on it
  // (round-1 Defect 2). A degraded streak shorter than the bound stays a calm
  // reconnecting cue (transient restart/handoff window); a genuine prolonged
  // outage still turns RED (fail-loud preserved).
  const persistentlyDegraded =
    degradedSince !== null &&
    lastFailAt !== null &&
    lastFailAt - degradedSince >= graceThresholdMs(hasEverLoaded);

  // Never loaded + still within grace → calm "supervisor is starting"
  // Loading. Covers both the pristine mount (no failure yet) and the startup
  // window where the first /api/status calls fail while the supervisor binds
  // its IPC.
  if (!hasEverLoaded && !persistentlyDegraded) {
    return (
      <div>
        <h1>Dashboard</h1>
        <p class="dashboard-loading" data-testid="dashboard-loading">
          Loading status… <span class="dashboard-loading-note">the supervisor is starting</span>
        </p>
      </div>
    );
  }

  if (error) {
    // Within grace (already loaded, or the startup grace elapsed into a
    // post-load reconnect) → a CALM amber reconnecting cue, NOT the RED
    // banner. RecoveryActions stays reachable so the operator can still act
    // during a stuck restart.
    if (!persistentlyDegraded) {
      return (
        <div>
          <h1>Dashboard</h1>
          <p
            class="dashboard-reconnecting"
            data-testid="dashboard-reconnecting"
            role="status"
            style="color: var(--warning, #bf8700)"
          >
            <span aria-hidden="true">○</span> Reconnecting…{" "}
            <span class="dashboard-loading-note">the supervisor is restarting</span>
          </p>
          <RecoveryActions
            context="error"
            onReloadStatus={() => setReloadTrigger((n) => n + 1)}
          />
        </div>
      );
    }
    // Past grace → the EXISTING RED fail-loud banner + RecoveryActions.
    return (
      <div>
        <h1>Dashboard</h1>
        <p class="error" data-testid="dashboard-error">Failed to load status: {error}</p>
        <RecoveryActions
          context="error"
          onReloadStatus={() => setReloadTrigger((n) => n + 1)}
        />
      </div>
    );
  }

  const sorted = Object.values(state).sort((a, b) => keyFor(a).localeCompare(keyFor(b)));

  // Pre-spawn existence gate (P1.1). One banner per distinct cause, above the
  // cards. Every server is started from ONE mcphub.exe, so when that file goes
  // missing all of them fail identically — the real incident held 12 daemons at
  // once, and twelve identical red cards is worse for an operator than one
  // sentence naming the cause and the remedy. Grouping keeps that say-it-once
  // property while ALSO guaranteeing the remedy is visible without hover when
  // only one server is held (the card carries the remedy in a `title` tooltip
  // the operator has no reason to open). Derived from data already on the wire;
  // no extra backend surface.
  const spawnHoldBanners = deriveSpawnHoldBanners(sorted);

  return (
    <div>
      <header class="dashboard-header">
        <div class="dashboard-header-title" style="display: flex; align-items: baseline; gap: var(--gap-sm)">
          <h1>Dashboard</h1>
          <ConnectionBadge state={connectionState} />
        </div>
        <BulkActionsRow
          runAll={runAll}
          stopAll={stopAll}
          disabled={sorted.length === 0}
          inflight={bulkInflight}
          outcome={bulkOutcome}
        />
      </header>
      <RecoveryActions
        context="normal"
        onReloadStatus={() => setReloadTrigger((n) => n + 1)}
      />
      {spawnHoldBanners.map((hold) => (
        <p
          key={JSON.stringify([hold.reason, hold.path])}
          class="dashboard-fleet-hold"
          data-testid="dashboard-fleet-hold"
          data-hold-reason={hold.reason}
          data-hold-count={String(hold.count)}
          role="alert"
        >
          ⚠ {hold.headline}
        </p>
      ))}
      {hubHealth?.degraded && (
        <p
          class={`dashboard-hub-health dashboard-hub-health-${hubHealth.state}`}
          data-testid="dashboard-hub-health"
          data-hub-state={hubHealth.state}
          role="alert"
        >
          ⚠ {hubHealthMessage(hubHealth)}
        </p>
      )}
      {auditLockView?.warning === "pending" && (
        <p
          class="dashboard-audit-lock-pending"
          data-testid="dashboard-audit-lock-pending"
          role="alert"
        >
          ⚠ Recovery finished while a background audit writer still holds the
          supervisor-events.log lock. This normally clears on its own; no restart needed.
          While it is pending, the supervisor and <code>mcphub install</code> cannot
          write their event logs. Do NOT re-run recovery — it already completed.
        </p>
      )}
      {auditLockView?.warning === "stranded" && (
        <p
          class="dashboard-audit-lock-stranded"
          data-testid="dashboard-audit-lock-stranded"
          role="alert"
        >
          ⚠ Recovery finished, but this app could not confirm it released the
          supervisor-events.log lock. While that lock is held, the supervisor and{" "}
          <code>mcphub install</code> cannot write their event logs. Do NOT re-run
          recovery — it already completed. Restart mcphub to release the lock.
        </p>
      )}
      {auditLockNotice && (
        <p
          class="dashboard-error"
          data-testid="dashboard-audit-lock-notice"
          role="alert"
        >
          {auditLockNotice}
        </p>
      )}
      {retainedRecoveryReceipts.length > 0 && (
        <div
          class="dashboard-error"
          data-testid="dashboard-recovery-receipt-ack"
          role="alert"
        >
          <p>
            {retainedRecoveryReceipts.some((receipt) => receipt.status === "uncertain")
              ? "A recovery outcome is uncertain. Do NOT repeat recovery. Refresh daemon status and inspect logs before acknowledging this warning."
              : retainedRecoveryReceipts.some((receipt) => receipt.status === "committed_error")
                ? "A committed recovery completed with an error. Do NOT repeat recovery. Refresh daemon status and inspect logs before dismissing this receipt."
                : "Recovery did not commit. Check current daemon status before acknowledging this retained receipt or starting another recovery."}
          </p>
          <button
            type="button"
            onClick={() => void acknowledgeRetainedRecoveryReceipts()}
          >
            I checked current status — acknowledge
          </button>
        </div>
      )}
      {unmanagedStdioCount > 0 && (
        <p
          class="dashboard-unmanaged-stdio"
          data-testid="dashboard-unmanaged-stdio"
          role="status"
        >
          ⚠ {unmanagedStdioCount} unmanaged MCP server{unmanagedStdioCount === 1 ? "" : "s"} bypassing the hub{" "}
          <a href="#/migration">Adopt</a>
        </p>
      )}
      <div class="cards">
        {sorted.map((d) => (
          <Card
            key={keyFor(d)}
            daemon={d}
            onRestart={() => restart(d.server, d.daemon)}
            onRecover={() => recoverDaemon(d.task_name!)}
            onStop={() => stop(d.server, d.daemon)}
            bulkInflight={bulkInflight}
            bulkOutcome={bulkOutcome}
          />
        ))}
      </div>
    </div>
  );
}

// BulkActionsRow is a pure presentational component driven by SSE-fed
// state from DashboardScreen. inflight + outcome both come from the
// "bulk-action" event stream so any trigger source — Dashboard click,
// tray menu, future API client — produces the same visual feedback.
//
// Click handlers fire HTTP POST and return immediately; they do NOT
// set local state. The visual state (Starting…/Started/Failed) flows
// in via SSE. This is the ONE source of truth: backend is canonical,
// UI is a projection.
//
// Mutual exclusion is preserved by the shared inflight prop — while
// one bulk action is in flight, BOTH buttons are disabled. Codex bot
// review on PR #36 P2 (race-prone independent state machines).
function BulkActionsRow(props: {
  runAll: () => Promise<void>;
  stopAll: () => Promise<void>;
  disabled?: boolean;
  inflight: BulkAction | null;
  outcome: BulkOutcome | null;
}) {
  function labelFor(action: BulkAction, idle: string, working: string, done: string): string {
    if (props.inflight === action) return working;
    if (props.outcome && props.outcome.action === action) {
      return props.outcome.state === "done" ? done : "Failed";
    }
    return idle;
  }

  const lockDisabled = props.inflight !== null || props.disabled;
  return (
    <div class="dashboard-bulk-actions">
      <button
        class="btn btn-secondary"
        onClick={() => {
          if (!lockDisabled) void props.runAll();
        }}
        disabled={lockDisabled}
        aria-busy={props.inflight === "restart"}
      >
        {labelFor("restart", "Run all", "Starting…", "Started")}
      </button>
      <button
        onClick={() => {
          if (!lockDisabled) void props.stopAll();
        }}
        disabled={lockDisabled}
        class="btn btn-danger btn-stop"
        aria-busy={props.inflight === "stop"}
      >
        {labelFor("stop", "Stop all", "Stopping…", "Stopped")}
      </button>
    </div>
  );
}

type ActionState = "idle" | "working" | "done" | "error";

// useActionButton owns one button's state machine: idle → working →
// done|error → snap-back-to-idle after 1.5s. Stable across the timer
// lifecycle (cancels a pending reset before queueing a new one) and
// cleans up on unmount.
function useActionButton(
  run: () => Promise<void>,
): { state: ActionState; click: () => Promise<void> } {
  const [state, setState] = useState<ActionState>("idle");
  const resetTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(
    () => () => {
      if (resetTimerRef.current) clearTimeout(resetTimerRef.current);
    },
    [],
  );

  async function click() {
    if (state !== "idle") return;
    setState("working");
    try {
      await run();
      setState("done");
    } catch {
      setState("error");
    }
    if (resetTimerRef.current) clearTimeout(resetTimerRef.current);
    resetTimerRef.current = setTimeout(() => {
      setState("idle");
      resetTimerRef.current = null;
    }, 1500);
  }

  return { state, click };
}

function Card(props: {
  daemon: DaemonStatus;
  onRestart: () => Promise<void>;
  onRecover: () => Promise<void>;
  onStop: () => Promise<void>;
  // Bulk action signals from the parent — when a Run all / Stop all
  // is in flight, every Card's matching button reflects it. By
  // definition Run all === click each per-card Restart, so the
  // affordance must mirror that.
  bulkInflight: BulkAction | null;
  bulkOutcome: BulkOutcome | null;
}) {
  const { daemon: d, onRestart, onStop, bulkInflight, bulkOutcome } = props;
  const restartBtn = useActionButton(onRestart);
  const stopBtn = useActionButton(onStop);
  const [recoverOpen, setRecoverOpen] = useState(false);
  const [recoverBusy, setRecoverBusy] = useState(false);
  const [recoverFeedback, setRecoverFeedback] = useState<
    { kind: "pending" | "error"; message: string } | null
  >(null);
  const previousDaemonState = useRef(d.state);
  useEffect(() => {
    if (previousDaemonState.current !== d.state) {
      setRecoverFeedback(null);
      previousDaemonState.current = d.state;
    }
  }, [d.state]);

  // Flowbite Card shell classes (the documented `p-6 bg-white border
  // border-gray-200 rounded-lg shadow-sm dark:bg-gray-800 dark:border-gray-700`
  // vocabulary) layered on top of the existing `card ok`/`card down`
  // classes the Dashboard tests select on. Keeping both means the metric
  // cards read as Flowbite Cards while the status-color (`.card.ok .state`)
  // and `.cards .card` selectors stay intact.
  const flowbiteCard =
    "p-6 bg-white border border-gray-200 rounded-lg shadow-sm dark:bg-gray-800 dark:border-gray-700";
  const stateVisual = daemonStateVisual(d.state);
  const cls = `${stateVisual.cardClass} ${flowbiteCard}`;
  const stateBadgeClass = stateVisual.badgeClass;
  // Prefer the backend-computed human-readable name ("serena · <project>",
  // "<lang> @ <workspace>") when present; it replaces the hash-suffixed
  // task name for workspace-scoped daemons. Global daemons carry no
  // display_name, so the existing "<server> (<daemon>)" / "<server>"
  // fallback is unchanged for them.
  const title = d.display_name
    ? d.display_name
    : d.daemon && d.daemon !== "default"
      ? `${d.server} (${d.daemon})`
      : d.server;
  const recoveryEligible = isRecoveryEligibleState(d.state);
  const canRecover = recoveryEligible && (d.task_name?.trim().length ?? 0) > 0;

  async function confirmRecovery() {
    if (recoverBusy || !canRecover) return;
    setRecoverBusy(true);
    setRecoverFeedback(null);
    try {
      await props.onRecover();
      setRecoverFeedback({
        kind: "pending",
        message: "Recovery accepted; waiting for supervisor status",
      });
    } catch (error) {
      setRecoverFeedback({ kind: "error", message: daemonRecoveryMessage(error) });
    } finally {
      setRecoverBusy(false);
      setRecoverOpen(false);
    }
  }

  // Live process metrics (Port / PID / Uptime / RAM + the orphan-PID and
  // job-protection diagnostics) are rendered by DaemonMetrics, the single
  // owner of the per-daemon metric rows. It humanizes uptime_sec / ram_bytes
  // via formatUptime / formatBytes and omits a row when its source field is
  // absent/zero. See internal/gui/frontend/src/components/DaemonMetrics.tsx.

  // Effective per-button state merges local click-driven state with
  // the parent's bulk-action state. Precedence (top wins):
  //   1. local "working"        — local click in flight; preserve
  //   2. bulkInflight match     — bulk in flight → "working"
  //   3. bulkOutcome match      — bulk terminal flash overrides stale
  //                                local "done"/"error" so every card
  //                                shows the bulk result uniformly
  //   4. local state            — idle / done / error from prior local
  //                                click outside any bulk window
  //
  // Codex bot PR #39 P2 ("Let bulk outcome override stale per-card
  // flash state"): an `=== "idle"` guard on rule 3 was wrong — a
  // recent local click leaves state "done"/"error" for 1.5s, and
  // during that window a bulk outcome would NOT cascade to that
  // card, breaking the "all cards mirror bulk action" invariant.
  function effective(local: ActionState, action: BulkAction): ActionState {
    if (local === "working") return "working";
    if (bulkInflight === action) return "working";
    if (bulkOutcome && bulkOutcome.action === action) return bulkOutcome.state;
    return local;
  }
  const restartEffective = effective(restartBtn.state, "restart");
  const stopEffective = effective(stopBtn.state, "stop");

  // While a bulk action is in flight, every per-card button is
  // disabled — clicking one would race the global fan-out. The
  // existing per-card mutual exclusion (one button locks the other)
  // is preserved.
  const anyBulk = bulkInflight !== null;
  const anyWorking = restartEffective === "working" || stopEffective === "working";
  const restartDisabled =
    anyBulk || restartBtn.state !== "idle" || stopBtn.state === "working";
  const stopDisabled =
    anyBulk ||
    stopBtn.state !== "idle" ||
    restartBtn.state === "working" ||
    d.state !== "Running";

  const restartLabel = {
    idle: "Restart",
    working: "Restarting…",
    done: "Restarted",
    error: "Failed",
  }[restartEffective];
  const stopLabel = {
    idle: "Stop",
    working: "Stopping…",
    done: "Stopped",
    error: "Failed",
  }[stopEffective];

  return (
    <div class={cls} data-testid="dashboard-card">
      <div class="card-title text-lg font-semibold tracking-tight text-gray-900 dark:text-white">
        {title}
      </div>
      <div class="card-kv">
        <span>State</span>
        <span class="state">
          {/* Flowbite Badge for the state chip — color + the shape glyph
              so meaning survives a red-green color deficit. */}
          <span class={`text-xs font-medium px-2.5 py-0.5 rounded-full ${stateBadgeClass}`}>
            <span class="state-shape" aria-hidden="true">{stateShape(d.state)}</span>{" "}
            {d.state}
          </span>
        </span>
      </div>
      {/* Process-metrics block: Port / PID / Uptime / RAM + orphan-PID and
          job-protection diagnostics. Single owner so the Card stays focused
          on header/state/actions chrome. Reads only fields already present
          on DaemonStatus (no new backend surface). */}
      <DaemonMetrics daemon={d} />
      {recoveryEligible && (
        <p class="daemon-recovery-reason" role="status">
          Automatic restart is paused because this daemon is quarantined after repeated failures. Recover checks for a lost child on its port, may stop only a verified hub child, then requests a forced respawn.
        </p>
      )}
      {recoverFeedback && (
        <p
          class={`daemon-recovery-feedback daemon-recovery-feedback-${recoverFeedback.kind}`}
          role={recoverFeedback.kind === "error" ? "alert" : "status"}
        >
          {recoverFeedback.message}
        </p>
      )}
      {/* View-logs link — an <a> (link role), NOT a <button>, so the
          long-standing per-card button-count invariant (2 bulk + 2 per
          card) the Dashboard tests assert is preserved. Navigates to the
          Logs screen where the operator picks this server's log file. */}
      <div class="card-logs-link" style="margin-top: var(--gap-xs)">
        <a
          href="#/logs"
          class="inline-flex items-center text-sm font-medium text-blue-600 hover:underline dark:text-blue-500"
          data-testid="card-view-logs"
          title={`View logs for ${title}`}
        >
          View logs
          <svg class="w-3.5 h-3.5 ms-1.5 rtl:rotate-180" aria-hidden="true" xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 14 10">
            <path stroke="currentColor" stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M1 5h12m0 0L9 1m4 4L9 9" />
          </svg>
        </a>
      </div>
      <div class="card-actions">
        {recoveryEligible ? (
          canRecover ? (
            <button
              class="btn btn-secondary"
              onClick={() => setRecoverOpen(true)}
              disabled={recoverBusy || anyBulk}
              aria-busy={recoverBusy}
            >
              Recover
            </button>
          ) : null
        ) : (
          <button class="btn btn-secondary" onClick={restartBtn.click} disabled={restartDisabled} aria-busy={anyWorking}>
            {restartLabel}
          </button>
        )}
        <button
          onClick={stopBtn.click}
          disabled={stopDisabled}
          aria-busy={anyWorking}
          class="btn btn-danger btn-stop"
        >
          {stopLabel}
        </button>
      </div>
      {canRecover && (
        <ConfirmModal
          open={recoverOpen}
          title={`Recover ${title}?`}
          body={
            <p>
              Recovery checks the daemon's configured port. If it is occupied, mcphub will stop the process only after it proves that it is this daemon's own lost child. It will never stop a foreign or unverifiable process. Then it asks the supervisor to force a respawn. Verified lost-child termination is Windows-only in v1; on other platforms recovery refuses a bound owner without stopping it.
            </p>
          }
          confirmLabel="Recover daemon"
          danger
          onConfirm={confirmRecovery}
          onCancel={() => setRecoverOpen(false)}
          testId="daemon-recover-modal"
        />
      )}
    </div>
  );
}

function daemonRecoveryMessage(error: unknown): string {
  if (error instanceof RecoveryFlowError) return error.message;

  const apiError = error as APIError | undefined;
  const body = apiError?.body;
  const terminationCommitted =
    typeof body === "object" &&
    body !== null &&
    "termination_committed" in body &&
    (body as { termination_committed?: unknown }).termination_committed === true;

  const code = apiError?.code;
  const message = isDaemonRecoverErrorCode(code)
    ? daemonRecoverCodeMessage(code)
    : // Not a recover-route code at all (transport failure, HTTP_<status>,
      // a thrown non-APIError). Nothing specific can be asserted.
      "Recovery failed. Check the supervisor logs and try again.";

  return terminationCommitted
    ? `${message} A process was already stopped during this recovery attempt.`
    : message;
}

// daemonRecoverCodeMessage maps EVERY DaemonRecoverErrorCode to operator copy.
//
// The `const unhandled: never = code` in the default arm is the exhaustiveness
// guard: adding a member to DAEMON_RECOVER_ERROR_CODES (api.ts) without an arm
// here becomes a COMPILE error, instead of silently inheriting a generic "try
// again" message. That silent inheritance is exactly how
// RECOVER_AUDIT_DURABILITY_FAILED — the one code whose entire purpose is to
// stop a retry — came to render "Recovery failed. Check the supervisor logs and
// try again."
function daemonRecoverCodeMessage(code: DaemonRecoverErrorCode): string {
  switch (code) {
    case "INVALID_ARGS":
      return "Recovery was rejected as malformed. No process was stopped. Refresh status and try again.";
    case "RECOVER_CONFIRMATION_REQUIRED":
      return "Recovery requires explicit confirmation and did not proceed. No process was stopped.";
    case "RECOVER_REFUSED_PORT_OWNER":
      return "Recovery was refused: the port owner could not be verified as this daemon's child. No process was stopped.";
    case "RECOVER_RESPAWN_FAILED":
      return "The supervisor did not accept recovery. View logs and retry after resolving the failure.";
    case "RECOVER_SUPERVISOR_UNAVAILABLE":
      return "The supervisor is unavailable. Restart the hub, then retry recovery.";
    case "RECOVER_REQUEST_CANCELED":
      return "Recovery was canceled before any process was stopped. Retry if recovery is still needed.";
    case "RECOVER_BOUNDARY_PROBE_TIMEOUT":
      return "Recovery verified the process identity but timed out while rechecking the port owner. No process was stopped.";
    case "RECOVER_RESPAWN_BUDGET_INSUFFICIENT":
      return "Recovery could not reserve enough time for a safe restart. No process was stopped; retry when the local system is less busy.";
    case "RECOVER_UNKNOWN_TASK":
      return "This daemon is no longer known to the supervisor. Refresh status and try again.";
    case "RECOVER_STATE_READ_FAILED":
      return "Recovery could not read the current daemon state. Check the supervisor logs and try again.";
    case "RECOVER_AUDIT_DURABILITY_FAILED":
      // The destructive step ALREADY RAN — the process was stopped and the
      // respawn was requested. Only the audit record could not be preserved.
      // Never offer a retry: a re-run would stop a second, freshly respawned
      // process. This is the entire reason the code is distinct from
      // RECOVER_UNCLASSIFIED_FAILURE.
      return "Recovery already stopped the process and requested a respawn, but its audit record could not be preserved. Do NOT retry — the destructive step has already run. Check this daemon's state below and inspect supervisor-events.log.";
    case "RECOVER_UNCLASSIFIED_FAILURE":
      return "Recovery failed for an unclassified reason. No specific cause can be asserted; check the supervisor logs before retrying.";
    case "AUDIT_LOCK_ADAPTER_INIT_FAILED":
      return "Recovery state is unavailable. No recovery request was sent; wait for the dashboard to reconnect.";
    case "RECOVER_CORRELATION_INVALID":
      return "Recovery identifiers were rejected before recovery began. No process was stopped.";
    case "RECOVER_BASELINE_STALE":
      return "The recovery server restarted before this action began. No process was stopped; open Recover again after the dashboard reconnects.";
    case "RECOVER_ATTEMPT_CONFLICT":
      return "Recovery identifiers are already bound to a different action. Nothing was relabeled or retried.";
    case "RECOVER_OCCURRENCE_CONSUMED":
      return "This recovery receipt was already consumed. The action was not repeated; refresh daemon status.";
    case "RECOVER_OCCURRENCE_CAPACITY_EXCEEDED":
      return "Recovery cannot start because the receipt registry is full. No process was stopped.";
    case "RECOVER_RECEIPT_IN_FLIGHT":
      return "Recovery is still running. Its receipt cannot be acknowledged yet, and the action was not repeated.";
    case "RECOVER_OUTCOME_UNCERTAIN":
      return "Recovery may already have stopped the process, but its committed outcome could not be persisted. Do NOT retry. Refresh status, inspect logs, then acknowledge the retained warning.";
    case "RECOVER_ACK_PRECONDITION_REQUIRED":
      return "The recovery receipt needs a current released-lock snapshot before it can be acknowledged. The receipt remains retained; refresh status instead of repeating recovery.";
    case "RECOVER_ACK_PHYSICAL_STATE_CHANGED":
      return "The physical lock state changed before the recovery receipt could be acknowledged. The receipt remains retained; refresh status instead of repeating recovery.";
    default: {
      const unhandled: never = code;
      return `Recovery failed (${String(unhandled)}). Check the supervisor logs before retrying.`;
    }
  }
}

// RecoveryActions surfaces the two ops affordances the Dashboard
// previously lacked when the supervisor IPC went silent:
//
//   1. **Clean up orphans** — calls POST /api/cleanup/orphans with
//      apply=true. Equivalent to running `mcphub cleanup` from a
//      terminal, but reachable from the same Dashboard the operator
//      already has open. Useful when un-migrated agent client configs
//      have re-spawned dozens of mcp-language-server.exe orphans that
//      mcphub did not manage (PR #222 post-mortem: 22-52 orphans
//      across a few agent reconnects).
//
//   2. **Restart supervisor** — calls POST /api/supervisor/restart.
//      Reads supervisor.lock.owner.json to find the current
//      supervisor PID, kills it via taskkill /F (graceful path is
//      gone — the supervisor's IPC may already be wedged when this
//      button is needed), then spawns a fresh detached
//      `mcphub supervise`. The new supervisor inherits the GUI's
//      env vars (including MCPHUB_ALLOW_UNHARDENED_STATE_READ for
//      corp-managed hosts). Recovers the post-mortem case where
//      Dashboard rendered "Failed to load" with no actionable
//      affordance.
//
// `context` controls placement: "error" appears under the
// Failed-to-load banner (prominent recovery surface); "normal"
// appears as an unobtrusive ops toolbar so the operator can run
// cleanup without inducing an error first.
function RecoveryActions(props: {
  context: "error" | "normal";
  onReloadStatus: () => void;
}) {
  const [cleanupBusy, setCleanupBusy] = useState(false);
  const [restartBusy, setRestartBusy] = useState(false);
  const [msg, setMsg] = useState<{ text: string; kind: "ok" | "error" } | null>(null);
  // In normal mode the recovery buttons start collapsed so existing
  // Dashboard tests that do `document.querySelectorAll("button")` to
  // count action buttons don't see the recovery ones in their initial
  // DOM snapshot. The operator expands the section explicitly when
  // they need it. In error mode the buttons are always rendered
  // because the error surface IS the recovery surface.
  const [expanded, setExpanded] = useState(props.context === "error");

  async function handleCleanup() {
    if (cleanupBusy || restartBusy) return;
    setCleanupBusy(true);
    setMsg(null);
    try {
      const res = await cleanupOrphans();
      const skippedNote = res.skipped > 0 ? ` (${res.skipped} skipped)` : "";
      setMsg({
        text: `Killed ${res.killed} orphan process${res.killed === 1 ? "" : "es"}${skippedNote}.`,
        kind: "ok",
      });
      // Refetch status so any orphaned-process-driven anomaly clears.
      props.onReloadStatus();
    } catch (err) {
      setMsg({ text: (err as Error).message, kind: "error" });
    } finally {
      setCleanupBusy(false);
    }
  }

  async function handleRestart() {
    if (cleanupBusy || restartBusy) return;
    setRestartBusy(true);
    setMsg(null);
    try {
      const res = await restartSupervisor();
      const parts: string[] = [];
      if (res.killed) parts.push(`killed PID ${res.killed_pid}`);
      if (res.spawned) parts.push(`spawned PID ${res.spawned_pid}`);
      if (parts.length === 0) parts.push("no-op");
      const stepErrs = Object.entries(res.per_step_error ?? {});
      const errSuffix = stepErrs.length > 0
        ? `; step errors: ${stepErrs.map(([k, v]) => `${k}=${v}`).join(", ")}`
        : "";
      setMsg({
        text: `Supervisor: ${parts.join(", ")}${errSuffix}.`,
        kind: res.spawned ? "ok" : "error",
      });
      // Give the new supervisor a moment to bind IPC, then refetch.
      setTimeout(() => props.onReloadStatus(), 1500);
    } catch (err) {
      setMsg({ text: (err as Error).message, kind: "error" });
    } finally {
      setRestartBusy(false);
    }
  }

  const inner = (
    <div
      class={`dashboard-recovery dashboard-recovery-${props.context}`}
      data-testid="dashboard-recovery"
      style="margin: var(--gap-xs) 0; display: flex; gap: var(--gap-xs); align-items: center; flex-wrap: wrap"
    >
      <button
        type="button"
        onClick={handleCleanup}
        disabled={cleanupBusy || restartBusy}
        data-testid="recovery-cleanup"
        title="Kill orphan MCP subprocesses that no longer have a managing client (mcphub cleanup --apply)."
      >
        {cleanupBusy ? "Cleaning up…" : "Clean up orphans"}
      </button>
      <button
        type="button"
        onClick={handleRestart}
        disabled={cleanupBusy || restartBusy}
        data-testid="recovery-restart-supervisor"
        title="Kill the current supervisor process and spawn a fresh one. Use when /api/status times out or returns 'IPC failed'."
      >
        {restartBusy ? "Restarting…" : "Restart supervisor"}
      </button>
      {msg && (
        <span
          class={msg.kind === "error" ? "error" : "info"}
          data-testid="recovery-msg"
          style="margin-left: 8px"
        >
          {msg.text}
        </span>
      )}
    </div>
  );

  // Error state: render inline & prominent — this is the recovery
  // surface the operator looks for when Dashboard says "Failed to
  // load: i/o timeout".
  if (props.context === "error") {
    return inner;
  }
  // Normal state: keep the buttons OUT of the initial DOM (not just
  // hidden) so existing Dashboard tests that index
  // `document.querySelectorAll("button")` to walk action buttons
  // don't break. The operator clicks the disclosure to reveal them.
  return (
    <div
      class="dashboard-recovery-summary"
      data-testid="dashboard-recovery-summary"
      style="margin: var(--gap-xs) 0"
    >
      {/* Anchor element (plain <a>, no role override) so existing
          Dashboard tests that walk document.querySelectorAll("button")
          OR findAllByRole("button") to enumerate action buttons
          don't pick up the disclosure trigger. Default anchor role
          is "link" which is correct semantically: this control
          reveals more content, it's not a state-changing action.
          href="#" makes it tab-focusable; onClick handles the
          actual expand/collapse. */}
      <a
        href="#"
        class="recovery-toggle"
        data-testid="recovery-toggle"
        onClick={(ev) => {
          ev.preventDefault();
          setExpanded((v) => !v);
        }}
        style="cursor: pointer; color: var(--text-muted); font-size: 0.9em; text-decoration: underline"
      >
        {expanded ? "▼ Ops actions" : "▶ Ops actions (cleanup, restart supervisor)"}
      </a>
      {expanded && inner}
    </div>
  );
}
