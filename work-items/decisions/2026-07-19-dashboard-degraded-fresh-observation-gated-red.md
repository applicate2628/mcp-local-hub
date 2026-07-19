# Decision: Dashboard degraded RED banner — fresh-observation-gated (deadline recheck + confirmation flag)

status: accepted (2026-07-19, $architect GATE PASS)
work-item: 2026-07-19-supervisor-ipc-poll-flood (Fix B)
supersedes: the reverted adaptive-5s-poll fix (fail-unsafe per fable D1)

## Problem
The RED "supervisor unreachable" banner gate was purely ELAPSED-time-driven
(`degradedSince !== null && performance.now()-degradedSince >= threshold`). A supervisor that
RECOVERS within the grace window but emits NO SSE daemon-state delta (unchanged rows; poller.go
emits only on a changed row) is not re-observed until the next 30s HTTP poll (lands AFTER the bound)
→ the grace-deadline timer flips RED on a STALE failure sample = false RED (bot #564 P1).

The reverted "fix" (5s fast-poll while degraded) was FAIL-UNSAFE (fable D1): ~x6 sampling density
gives a flapping supervisor ~50%/cycle streak-reset → RED never fires for a persistent outage.

## Design (accepted)
RED is NEVER elapsed-driven again — it is POSITIVELY EARNED by a fresh failing /api/status
observation taken AT/after the grace bound. ONE recheck per streak (no continuous polling).
- New state: `redConfirmed` (sole RED driver), `recheckIssuedRef` (per-streak once-guard = anti-D1),
  `mountedRef` (replaces effect-local `cancelled` after loadStatus hoist).
- Hoist `loadStatus` → `useCallback([])` returning `Promise<boolean>` (single fetch owner).
- Poll effect: 30s baseline, NO fast poll.
- Grace-deadline effect: at `remaining<=0`, fire ONE recheck (guarded by recheckIssuedRef);
  failing → setRedConfirmed(true); succeeding → setDegradedSince(null) (clears streak).
- Reset effect: degradedSince===null → reset recheckIssuedRef + redConfirmed.
- Render gate: `persistentlyDegraded = degradedSince !== null && redConfirmed`.

## Why it closes bot-P1 + avoids D1 + D2
- bot-P1: bound recheck succeeds on a silent recovery → clears → no RED.
- D1: 30s baseline (bound 20s < 30s → no intervening reset) + exactly ONE bound recheck/streak → can
  only VETO ~20% of streaks, retried next → RED fires within ~1-3 grace episodes (P(no RED)≈0.2^k).
- D2: RED requires redConfirmed (set only by a RESOLVED failing recheck) → no elapsed-only path → no
  RTT flash; strictly better under background-tab throttling (late recheck is still FRESH).

## Protected / must-not-touch
RESTART_GRACE_MS/STARTUP_GRACE_MS/graceThresholdMs; monotonic performance.now() (never Date.now());
degradedSince single-owner; poller.go backend contract; onDelta/onPollerError bodies.

## Test plan (Vitest fake timers)
T1 recovery-in-grace no-SSE → no RED (FAILS on HEAD); T2a persistent → RED; T2b sampling-density guard
(call count==3, FAILS on the reverted fast-poll); T3 SSE-delta recovery clears streak; T4 never-loaded
STARTUP_GRACE_MS. + full suite + typecheck + embed smoke + reconcile 2 e2e RED-timing specs.

## Scope: SPLIT (accepted by $lead)
Fix A (backend audit-skip: supervisor_ipc.go + supervise.go + test + ipc-audit decision) → own PR, land now
(clean, bot-approved). Fix B (this redesign: Dashboard.tsx + test + app.js + 2 e2e) → own PR. Zero shared
source files. Rationale: risk isolation of a multi-round-adversarial change > split overhead.

## Correction (2026-07-19 round 2): boolean+recheck-confirmer → streak-scoped timestamp gate + per-request abort deadline
status: accepted ($architect GATE PASS round 2). Supersedes the round-1 mechanism (redConfirmed boolean + .then confirmer),
which shipped 2 P1 concurrency defects (both repro-confirmed): Defect 1 (hung recheck = SPOF → RED never fires) +
Defect 2 (stale recheck after recovery → RED stamped on a new age-0 streak, no grace).

**Root:** RED confirmation carried by a LATCHED BOOLEAN written by ONE async writer (recheck .then) with NO client deadline.
A boolean can't say "for which streak" (Defect 2); one writer + no timeout = SPOF (Defect 1).

**Corrected mechanism (pure render-time comparison of two committed observation timestamps):**
- Replace `redConfirmed:boolean` with `lastFailAt:number|null` = monotonic performance.now() of the MOST RECENT
  RESOLVED failing /api/status observation (500, ABORT, or SSE poller-error). Written by loadStatus catch (poll AND
  recheck) + onPollerError. Cleared by the reset effect (degradedSince===null) + on success.
- Render gate: `persistentlyDegraded = degradedSince !== null && lastFailAt !== null && lastFailAt - degradedSince >= graceThresholdMs(hasEverLoaded)`.
  Streak identity = the degradedSince VALUE (monotonic, unique per streak — re-latch needs an intervening success = real
  wall time). A confirmation can NEVER outlive its streak (age-0 re-latch → 0 >= bound false → no RED).
- Every /api/status fetch gets `REQUEST_TIMEOUT_MS=8000` abort (AbortController+setTimeout, NOT AbortSignal.timeout —
  fake-timer testable) threaded via fetchOrThrow's EXISTING `init.signal` (api.ts:17-22 — no fetchOrThrow change).
  8s > 5s backend IPC deadline (health.go:424, real 500 not aborted) < 30s poll backstop. Timeout = confirming failure.
- Recheck becomes fire-and-forget (drop the .then SPOF); recheckIssuedRef KEPT as a fail-safe issue-throttle (anti-D1
  one-recheck/streak, off the RED-decision path).
- SPOF removed: ANY resolved-failing past-bound observation (recheck OR next poll OR SSE) confirms → no single writer.
  Anti-D1 held (30s cadence + recheckIssuedRef unchanged; T2b statusCalls===3 still holds).

**Tests:** existing suite unchanged (asserts observable behavior, not internals) + ADD R1 (Defect 2 repro: SSE recovery +
stale failing recheck → NO false RED; fails on round-1) + R2 (Defect 1 repro: hung recheck + failing polls → RED still
fires; fails on round-1). Defect 3 (P3 amber flash) disclosed + deferred (amber-only, self-healing, <=1 poll interval).
