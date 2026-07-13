# D commission round 3 (final gate) — fable PASS (arbiter, real code); Terra REVISE (partial bundle, refuted); Sol D3 polluted→re-run

Date 2026-07-13.

## fable D3 — PASS, fleet-safe, READY FOR MERGE BOT (authoritative — read the REAL code + fresh build/vet exit0 + -race -count=5 green)
- ALL round-2 findings CLOSED (code-traced):
  - NEW-1 (on-loop 5s foreign-probe) CLOSED: foreignHolderPort=0, gate is `>0 && fn!=nil`; off-loop event still carries holder.
  - NEW-2 (unbounded netsh) CLOSED: 3s CommandContext + non-vacuous off-loop pre-warm (sync.Once); residual ≤3s once-per-process.
  - P1-2 (a)(b)(c) ALL CLOSED by FIX-3: traced each shape through classifyLSPPortMismatch. argv is spawned FROM intent, so the inversion (intent=new/registry=old) → argv==intent(new), registry disagrees → exit-3 heals; revert-fail leaves registry=new/intent=old = shape (a) → heals. Exit-3 fires before Bind (mismatch ~:195 precedes Bind ~:281; EvHealthOK priority-drain → StRunning before fast-exit). No genuine-mis-registration sweep (4 fail-closed branches tested). No infinite loop (each re-drive burns a cap slot → quarantine at 10). No deadlock (proxy reads intent LOCKLESSLY under its held registry flock). §E doc + register-remedy corrected.
  - FIX-4: fable CONCEDES its round-2 FIFO argument was WRONG — the ~5s emit sits between the off-loop read and PostCtx, so a newer evReapScan can post ahead → the guard is right. Guard CORRECT+complete: time.Time parse (dodges RFC3339 string trap), strict After, fail-open = round-2 baseline (watcher ≤60s rescue), check+apply both on loop = no TOCTOU, skip-then-rearm lands on the RIGHT port (fresher cache from flock-serialized RMW), handleReapScan(...,nil) PRESERVES stops cache (no operator-stop revival).
- New-hole hunt: NO fleet-corruption hole. Stale-generation exit-3 vector guarded (P1a gen-guard runs before maybeHandleBindRefusedExit, tested).
- New findings ALL P3 (follow-up, NOT blockers): (1) no test pins FIX-1 (nil resolver in sync controllers); (2) malformed-body test wouldn't fail on zero=Reallocated; (3) on-loop warm-range-check + flock events.Emit (pre-existing controller-wide pattern); (4) LSP proxy lacks the MCPHUB_SUPERVISOR_INTENT_PATH channel (serena-only; Windows GA env-immune; POSIX HOME-redirect → nil → exit-1 fail-closed, never wrong-heal).
- Sol/Terra round-2 P2s (worker-wedge cap, dwell leave/re-enter, ABA/alias): deliberately not re-touched; BOUNDED per-daemon degradations backstopped by quarantine, NOT fleet corruption.

## Terra D3 — REVISE, but on a PARTIAL bundle (my error — omitted handleReallocApplied/CommandContext/dwell code); fable adjudicated each P1 on real code:
- "persist-success+read-fail brick" P1 → fable: argv spawned from intent → heals (Terra didn't see argv-from-intent). NOT a residual.
- "stale-snapshot equal-timestamp/parse-fail applies" P1 → fable: fail-open = round-2 baseline, bounded, watcher rescue. Terra wants monotonic epoch (stronger hardening); bounded-not-corrupting → follow-up.
- dwell-ABA + worker-lease re-raise → fable: bounded, quarantine-backstopped. Follow-up.
- "not verifiable" items = bundle-gaps, resolved by fable reading real code.

## Sol D3 — output polluted with role-md/graphify boilerplate (the `feedback_codex_review_no_graphify` failure mode); DISCARDED. Sol D3b clean re-run (complete inline bundle, hard anti-role-md pin) pending.

## Disposition (pending Sol D3b)
fable (deepest arbiter, real code) = PASS/ship. The MUST-fix fleet-corruption set is CLOSED. Bounded residuals (dwell-reset-on-leave ABA, worker-lease wedge-recovery, monotonic-epoch hardening) + 4 P3s remain, all fable-confirmed bounded + quarantine-backstopped. LEAN: quick round-4 for the CHEAP real ones (dwell-reset-on-leave — a genuine safety-gate ABA Terra flagged twice, ~5 lines; FIX-1 test pin; malformed-body test strength), DEFER worker-lease-epoch + monotonic-epoch as documented follow-up work-items (bounded, non-corrupting), THEN ship to bot. Confirm scope with Sol D3b (mandatory acceptance).
