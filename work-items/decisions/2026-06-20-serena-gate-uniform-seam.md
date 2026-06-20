---
status: accepted
date: 2026-06-20
slug: serena-gate-uniform-seam
deciders: architect (opus) + lead
pr: 386
---

# Serena stop-gate: one parameterized forward seam + a 1-line sweeper invariant (terminate the whack-a-mole)

## Context

The per-workspace serena stop-gate (closing the idle-stop-vs-in-flight + prune-vs-request races) went
through 6 review rounds; each round the bot found the SAME pattern (enter-bounded → recheck-after-wait
→ re-resolve) on yet another daemon-touching path. Blast-radius/whack-a-mole smell confirmed.

## Architect findings (verified, 8 gate sites)

- 4 of the 5 FORWARD sites (DELETE :1634, cancel :1913, tools/list-wake :991, tools/list-fetch :1217)
  ALREADY implement the full recheck/re-resolve correctly — the bot re-flags the hand-copied pattern
  because it cannot judge one path in isolation. Genuine open gaps: the MAIN tool-call (serena_router.go:761)
  uses the UNBOUNDED enter, and the sweeper-side `beginPhase` (serena_idle_sweeper.go:294) does not
  refuse a new phase when requests are WAITING.
- The "mark-in-flight before resolve" sidestep ALREADY exists (inFlight++ at enterCtx + beginPhase
  refuses on inFlight>0); that is exactly why `waitedThroughPrune` recheck is irreducible (it handles
  the opposite ordering — request arrived while a phase was already running). The win is structural
  (one owner), not elimination.

## Decision

1. **Refactor the forward side to ONE parameterized seam** `withSerenaWorkspaceGate(ctx, wsKey, policy,
   resolve, urlFn, onPhaseActive, fn)` in serena_idle_sweeper.go — the single owner of
   enter→interpret(phaseActive/waitedThroughPrune)→re-resolve→guaranteed-exit. It COMPOSES the existing
   low-level enterCtx/tryEnter/exit (kept intact). Each of the 5 forward sites passes per-site policy via
   callbacks; the tools/list candidate loop, the cancel goroutine, and the DELETE 5s ctx stay at the call
   sites (only the gate dance moves in). This also fixes the unbounded main-loop wait.
2. **Fix the sweeper invariant in ONE line**: `beginPhase` adds `|| entry.waiters > 0`, closing the
   "block new stop phases when requests are waiting" finding across both sweepers. (The sweeper is the
   OTHER side of the gate, not a seam consumer — honest "smaller helper, not mega-wrapper" split.)

## Why (over continue-patching)

Converts an O(paths) review into O(1): after the refactor exactly ONE place interprets the gate result,
so path N+1 supplies a callback + body and structurally cannot forget a step. The refactor surface
(3 files, ~5 bodies + 1 engine + 1 invariant line) is SMALLER than the cumulative patch surface already
touched over 4 rounds. No wire/contract/persisted-state change.

## Correctness gate

All existing race-closure tests (serena_router_invariant_d_test.go, serena_router_r6_gate_edges_test.go,
serena_router_lifecycle_test.go, workspace_prune_sweeper_test.go) MUST pass UNCHANGED against the
refactored sites (the falsifier that behavior is preserved) + a new waiters-invariant test + an
engine table-test. All existing audit events move INTO the policy callbacks (not lost).

## Protected (unchanged)

The low-level enter/enterCtx/tryEnter/exit/beginPhase/endPhase signatures; the stop-write ordering;
the cancel 202 / DELETE 204 response shapes; supervisor-intent/registry writers. Full architect package
in the PR #386 thread / this session's reports.
