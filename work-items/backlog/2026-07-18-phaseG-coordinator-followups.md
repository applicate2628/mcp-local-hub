# Backlog: Phase G restart-coordinator follow-ups (2 non-blocking P3s)

Filed 2026-07-18 by $lead from the fable round-3 convergence review of item-3 Unit B Phase G
(`feat/gui-restart-unitb-gated`, default-OFF). Both fail SAFE via proved rollback and did not block the
gated commit; address before/at Phase J (the gate flip) since they affect gate-ON restart robustness.

## P3-1 — post-Reserve rollback arm attempts an unreachable proved-clear
`internal/gui/gui_restart_protocol.go` (~391-395, the `responseFlushed` select's `ctx.Done` arm). The only
rollback initiated AFTER `Reserve` succeeds can never take the proved-clear path:
`ClearAfterProvedPreReleaseRollback` requires `Phase==InProgress` (`gui_restart_record.go:303`) but the
marker is `Reserved`, so this arm always escalates to the terminal Interrupt branch; its `rollbackCtx`
derives from the already-canceled `deps.Context`, so terminate's bounded reap is skipped after Kill.
Scenario: GUI process shutdown racing a restart between Reserve and ack. Benign — the process was exiting,
the marker terminalizes as Interrupted (correct for ensure-alive), and the child's Commit revalidation
refuses if the Kill lost the race. **Fix (polish):** in that arm call `Interrupt` directly instead of the
unreachable proved-clear.

## P3-2 — child same-port bind budget < parent grace-drain ceiling
`internal/cli/gui.go:234` (child `Bind`=2s) vs `gui_restart_record.go:55-56` (parent `Quiesce`=5s). On a
same-port restart, a mutating request that drains >~1.5s starves the child's bind window → confirm exhausts
→ restart fails via safe proved rollback (GUI intact, error surfaced). Tuning-only. **Fix:** give the child
same-port bind budget `Bind >= Quiesce` (so the child can wait out the full parent grace drain) — validate
the timeout-ordering invariant (`RestartDeadlines`) still holds.

## Also considered + REFUTED (no action) — Terra R3 "unsafe nonce/lock sweep" P1
Terra flagged `sweepRestartNonceResidue` (gui_restart_protocol.go:611) as a P1 (concurrent-generation
deletion + `.lock` unlink under a live gofrs holder). $lead + fable refuted it 2-way: the sweep runs under
the single-instance flock, before this generation's `WriteNonce`, with generation-bound leaves; a live-handle
sharing violation fails the sweep cleanly. Recorded here so a future reader does not re-open it. See the
Phase G review record for the full refutation.
