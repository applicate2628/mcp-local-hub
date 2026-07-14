# Commission round 2 — verdict: REVISE (Sol P1 + Terra P2)

Date 2026-07-12. Round-2 diff (FIX-1 register-before, FIX-2 GC key cleanup, FIX-3 wording).

## Round-1 findings — status
- P1 (compensation after mutation): **CLOSED** — FIX-1 (closure at install.go:2750-2762 before AddEntry:2763). Sol confirms P1-repro genuinely load-bearing.
- wording P3: **CLOSED** (adopt.go:339 "could not be confirmed").
- redaction P2: **REFUTED by accepted scope** (cause = operator stderr; event names-only; GUI redacts). Both Sol+Terra agree.
- lifecycle tests P2: **STILL-OPEN, narrowed** — exact routed-keys/unrelated-survival/GC-states now covered, BUT snapshot assertions (test:789-805) prove file EXISTS, never compare bytes/parsed-entry vs known pre-adopt config; restore-failure seam omits post-write failure modes.

## NEW findings
### Sol P1 (BLOCKER) — FIX-2 GC can delete a routed key OWNED BY ANOTHER LIVE ADOPT
`adopted_entries.go:1178-1180` blindly deletes every key the reaped row recorded. Manifest-absence
+ byte-frozen clients prove only THIS row no longer references them — NOT that no other provenance
row / manifest shares the key. Failure: dead row A + live adopted row B both record key K (normalization
collision / legacy state / corrupted provenance) → reaping A deletes K → breaks B. Lease doesn't protect B.
### Terra P2 — FIX-2 reap-first/delete-second not crash-safe
Crash / failed vault delete after row+snapshots reaped → orphaned key set, no record, no retry, no
crash-warn. Event carries only manifest+key_count (not names) → operator can't identify orphans.
### Sol P2 — FIX-1 unconditional restore can DAMAGE an AddEntry-UNmutated client
Closure at install.go:2755 runs even when AddEntry failed pre-write. Restore uses the same secure
writer → its own post-rename verify failure can remove/alter a previously-untouched config = new
client outage (recoverable — provenance preserved — but not "over-preserve at worst"). Seam models
restore-failure only as "return error, no write"; doesn't model restore-then-error/file-removed.
### Sol P3 — internal doc/telemetry overstate mutation
CLAUDE.md:1399, adopt.go:325-327, install.go:2564 say sentinel ⇒ Install already rewrote to relay;
FAIL_UNMUTATED × restore-failure also produces the sentinel. Soften to "may have been mutated".

## DISPOSITION (orchestrator)
1. **REVERT FIX-2 entirely.** Both reviewers condemn it (Sol P1 cross-manifest key deletion; Terra P2
   crash-safety). It violates the subsystem's own rule (bug 2026-07-12-adopt-preinstall-crash-orphan-triple:
   background GC must NEVER autonomously delete secret material a live adopt needs — a manifest-present
   row can be a LIVE committed adopt). Route routed-key cleanup to de-adopt (hash-gated, operator-driven,
   proper owner). Keep a test asserting keys REMAIN after GC reap; file the orphaned-key cleanup as
   adjacent to de-adopt / fold into the triple-orphan bug. `Vault.Delete` errors on missing key
   (NOT idempotent) so a crash-safe in-GC retry isn't cheap anyway.
2. **FIX-1 match-guard (Sol P2):** restore closure skips the write when the live entry already matches
   the pre-adopt backup (unmutated ⇒ no-op, no destructive-restore risk). Restore runs only when live
   differs OR can't be read (mutated / removed ⇒ revert; fail ⇒ sentinel). Preserves the P1 fix.
3. **Lifecycle test hardening (Sol/round-1 P2):** compare restored/preserved snapshot BYTES/parsed-entry
   vs known pre-adopt config (not just existence); model restore-then-error / file-removed seam outcomes
   incl FAIL_UNMUTATED × restore-failure.
4. **Doc/telemetry wording (Sol P3):** "restoration could not be confirmed; the client may have been mutated".

Round-3: architect designs match-guard + FIX-2 revert + test/doc; backend implements; re-run Sol+Terra.
