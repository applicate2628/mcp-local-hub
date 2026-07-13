# Closure — daemon-port ephemeral-collision self-heal

Closed: 2026-07-13

## Outcome
DELIVERED. Merged as PR #535 (squash commit `b351a5cc` on master) and DEPLOYED (canonical `~/.local/bin/mcphub.exe` upgraded to `b351a5cc`; full daemon fleet cold-restarted and verified Running via `mcphub status`; hub no longer "partial").

The supervisor now self-heals a dynamic-pool proxy (serena / workspace-LSP) whose loopback pool port is stolen by a foreign process (the WSL2/Hyper-V-widened Windows TCP ephemeral range overlapping mcphub's pools). Three layers:
- L1 runtime self-heal: off-loop atomic realloc to a fresh OS-probed pool port + respawn; bounded by a separate reallocation cap + StRunning dwell gate; covers BOTH bind-fail-after-spawn AND port-held-at-pre-spawn shapes from one owner; stale LSP registry rows self-heal via exit-3; persistent Failed reallocations escalate to quarantine; coincident respawn timers collapse to a global-epoch single arm.
- L2 setup detect + `--fix-ephemeral-range` (admin, before the elevation gate).
- L3 redacted (PID+basename) observability events.

## Verification
7-round commission (Sol acceptance + Terra concurrency + fable arbiter) + Codex Cloud bot PASS on the final HEAD. fable arbiter empirically closed every P1 (loop self-deadlock, partial-write LSP brick, equal-timestamp common-path regression). Pre-push gate clean (build/vet/test untagged+tagged/race). Live supervisor-intent byte-identical across all subagent test runs.

## Residual risk (all P3, tracked in work-items/backlog/2026-07-13-daemon-port-self-heal-followups.md)
1. Monotonic intent revision (trap-free ordering; round-4b's targeted patch resolves the defect without it).
2. FIX-6 worker lease/epoch (a permanently-wedged reallocation worker silent-flaps at ~30s; bounded, restart-recoverable).
3. Respawn-guard loop-side move (a ~1e-8 Add→Store window degrades to the accepted-P3 redundant timer; fable-adjudicated non-blocker; fix has a `replayDeferredBackoffTimerIfPending` caveat → deliberate follow-up).
4. Pre-existing load-induced TestVerifyProxyReady*/QuiesceTimers flake (proven NOT this feature; independent hardening).

## Archive location
work-items/archive/2026-07/2026-07-13-daemon-port-ephemeral-self-heal/

## Retrospective (what the 7 rounds taught)
- Architecture review alone (round-1 arch PASS) MISSED the runtime concurrency P1s; the full commission (Sol+Terra+fable) with a code-embedded arbiter caught them. Static diff-analysis finds the SHAPE of a bug; a runtime-ordering trace (fable) decides reachability/severity — several Sol/Terra findings (stale-snapshot lost-update, timer-amplification, F3 stale-classification, Add→Store race) were REAL as interleavings but fable-refuted as non-blocking because they degrade to an already-accepted bounded state.
- The single-timer-owner polish (r6→r7) was chasing a fable-declared-P3 and each round's fix introduced a new bounded issue (ABA, then Add→Store window) — a signal that polishing a non-blocking P3 in delicate concurrent code spirals; the arbiter's ship-verdict correctly ended it.
- The bot is slow (~27 min) but caught 3 REAL P2 the commission's per-round (not whole-final-diff) review missed — the two gates are complementary.
