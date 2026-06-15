# Closure — Supervisor lifecycle permanent fix (§5)

Closed: 2026-06-15
Outcome: DELIVERED + pushed + live-proven.

## What shipped (master b0d62f8..a346542, pushed)
- `6ef39e1` Increment 1: PART 1 breakaway-from-job spawn on both AUTOMATIC supervisor-spawn
  sites (GUI startup/respawn; install --upgrade + serena-migrate) via a shared
  breakaway-tolerant helper (flagless retry on ERROR_ACCESS_DENIED); PART 2d event-loop
  panic observability (recover-seam → emit `supervisor-handler-panic` → re-raise).
- `187e95c` Increment 2 (load-bearing): liveness now RECOVERS a dead supervisor under a live
  GUI via a detached standalone `mcphub supervise` spawn (standaloneRelaunchFn) instead of the
  old defer-to-GUI suppression. Closes the §5 deadlock (liveness deferred to GUI + GUI only
  respawned SPAWNED-not-ADOPTED supervisors → permanent wedge).
- `a346542` review fixes: must-fix stale-text (probeGUIOwnerAlive doc still described the removed
  suppression in present tense) + 3 should-fixes (past-tense parenthetical, standalone-relaunch
  failure test, ACCESS_DENIED precedent-verified comment).

## Verification
- Root cause: read-only investigation workflow (9 agents + adversarial). Death-trigger UNCERTAIN
  (Job-cascade likely / panic / kill / singleton race — all leave no supervisor-exit event; live
  IsProcessInJob probe non-discriminating). Fix is DEFENSE-FIRST: recovery works for any cause.
- Review: multi-model panel (opus+sonnet+synth; fable lane unavailable) → ship-after-must-fix.
  Design independently confirmed correct: interlock safe (migrate holds flock → liveness no-ops;
  upgrade swaps binary before reap), no double-supervisor, panic-Emit cannot deadlock.
- Unit: build ./... + vet + targeted tests green; live supervisor-intent backed-up + verified
  untouched on every test run (per the subagent-wiped-intent lesson).
- **LIVE PROOF (the definitive one):** deployed a346542, killed supervisor 180688 (dead-supervisor-
  under-live-GUI = the exact §5 scenario), triggered one liveness tick → within 5s a NEW supervisor
  (172044) + all 26 daemons RECOVERED, with the `liveness-relaunched-supervisor-under-gui` event
  emitted (the NEW code path; the old code would have emitted the suppress event + recovered
  nothing). `claude mcp list` = 26 Connected / 0 Failed. Full redeploy: everything on a346542.

## Residual risk / follow-ups (NOT blocking)
- DEFERRED (review-agreed, out-of-scope): the MANUAL /api/supervisor/restart configureDetached has
  the breakaway flag but NO ERROR_ACCESS_DENIED flagless-retry → now the LESS robust path on a
  locked-down host. Route it through the shared helper in a follow-up.
- Death-trigger attribution still open: PART 2d will now make the NEXT silent death attributable
  (supervisor-handler-panic event); A.4 Sysmon-trace only if churn recurs under single-owner.
- ADJACENT BUG found during GIF capture (separate, unfixed): the GUI Servers screen `/api/scan`
  fails on the zed client — `zed: invalid character '/' looking for beginning of value` (§9 zed
  adapter parse). File separately.

## Archive
work-items/archive/2026-06/2026-06-15-supervisor-lifecycle-permanent-fix/
