# Commission round 3 — verdict: REVISE (Sol + Terra converge on match-guard TOCTOU)

Date 2026-07-13. Round-3 = revert FIX-2 + FIX-1 whole-file match-guard + doc softening.

## CLOSED
- Round-2 P1 (GC deletes another adopt's routed key): **CLOSED** — FIX-2 reverted; `gcOrphanedAdoptingProvenance` reaps row+snapshot only; emitter gone; no routed-secret deletion in GC. (Sol + Terra)
- Round-2 P2 (reap-first/delete-second crash-safety): **CLOSED** — autonomous secret deletion removed. (Terra)
- Round-1 P1 (compensation after mutation): remains CLOSED (FIX-1). Match-guard NO-OP test genuinely load-bearing (both confirm; I proved it too). Snapshot byte-compares + Test-8 keys-REMAIN confirmed.

## OPEN (round-3)
### Match-guard TOCTOU (P2 — BOTH reviewers, install.go:2804)
The unlocked `liveConfigMatchesBackup` read + the separately-locked restore are NOT atomic. A
lock-respecting concurrent writer can change the config (a) after matched=true, before return → a
now-mutated client is SKIPPED; or (b) after mismatch, before restore → this rollback overwrites the
other writer. Unlocked read avoids self-deadlock but doesn't make "provably unmutated" durable.
**Fix (both):** take the per-path lock ONCE, re-check live-vs-backup under it, conditionally restore
via a lock-held (non-relocking) primitive. Add a barrier-based race test.
### Sol P2 — round-2 unmutated-damage STILL-OPEN via sibling edit
Whole-file byte equality: an external edit to a DIFFERENT entry in the config (a sibling) during the
window makes live≠backup → forces the redundant restore (the very damage the guard was meant to avoid).
**Fix:** entry-scoped compare (compare only entryName's value), not whole-file.
### Sol P3 — doc/telemetry softening INCOMPLETE
Missed: `adopt_provenance_events.go:122-129` (emitter comment "had rewritten to the hub relay [and]
could not be restored"), CLAUDE.md:1400-1405 ("un-restored client NAMES"), and the new test file's
opening comment. **Fix:** consistently "clients whose pre-adopt restoration could not be confirmed".

## DISPOSITION (orchestrator) — round-4, the reviewers' prescribed fix (cleanly solvable, NOT an edge-mine)
Replace the unlocked whole-file match-guard with an ATOMIC, ENTRY-SCOPED, lock-held skip-if-unchanged
FOLDED INTO the restore primitive: the restore already holds `withConfigLock` and already reads
live+backup entries — add "if live entryName value == backup entryName value ⇒ return nil WITHOUT
writing". Under one lock ⇒ no TOCTOU. Entry-scoped ⇒ sibling edits don't force a restore (closes the
STILL-OPEN P2). Returns nil on skip ⇒ transparent to the closure (no append, no false sentinel) ⇒
the install closure DROPS the separate guard + `liveConfigMatchesBackup` helper. Then finish the P3
doc softening at the 3 missed sites. FIX-1 (register-before) + the reverted FIX-2 stay.
Architect designs the exact primitive (shared restore body skip-if-equal vs a dedicated rollback
variant; which adapters; the serena-migrate-rollback co-caller impact). Then backend impl; re-run Sol+Terra.
