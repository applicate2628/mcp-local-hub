---
status: open
severity: low
filed: 2026-07-04
context: deep-audit finding (multi-agent audit, install-migrate-intent × correctness lens, STRONG). Call-path verified 2026-07-04 — the suggested fix is WRONG (breaks initial migration); reclassified from quick-fix to design-decision.
---

# `mergeDaemonIntentStops` re-adds a deliberately-cleared stop from a lingering legacy `daemon-intent.json` (transitional-window only)

## Finding

`mergeDaemonIntentStops` (`internal/api/intent_collapse.go:151`) is the pure
core of the Phase 4-E2 collapse: it folds the legacy `daemon-intent.json`
active stops into the unified `supervisor-intent.json` `stops` sub-block. Its
per-task rule (doc §143):

> legacy ACTIVE + absent in sub-block → ADD the full legacy record.

The `!hadPrior` branch (`:179-183`) unconditionally re-adds any legacy-active
stop that is absent from the sub-block. If an operator DELIBERATELY CLEARED a
stop from the sub-block (a new-binary "start"/un-stop) while the legacy
`daemon-intent.json` still carries that stop as active, a subsequent collapse
re-adds it → the deliberately-cleared stop is **resurrected**.

## Failure scenario (narrow — mixed-binary transitional window ONLY)

1. Old binary's `mcphub stop X` writes an active stop for X into `daemon-intent.json`.
2. New binary's collapse merges it into the sub-block, but the E2 ordered
   delete of `daemon-intent.json` is REFUSED/FAILS (or an old binary re-creates
   the file), so the legacy file lingers with X still active.
3. Operator un-stops X via the new binary → X removed from the sub-block.
4. Next collapse: legacy X still active + absent in sub-block → `!hadPrior`
   re-ADDs X. X is stopped again against the operator's intent.

Once `daemon-intent.json` is deleted (the normal post-collapse steady state —
`runDaemonIntentCollapse:483` deletes it after a confirmed merge), there is no
legacy file to merge, so the resurrection is **unreachable**. The window is the
mixed old+new binary transition where the legacy writer is still present.

## Why the audit's suggested fix ("make sub-block authoritative — drop, don't re-add") is WRONG

The suggested one-line fix — treat sub-block absence as authoritative and DROP
instead of ADD — **breaks the initial migration**. On the FIRST collapse the
sub-block is EMPTY: every legacy stop is "legacy-active + absent in sub-block".
Under the suggested fix, the initial collapse would ADD nothing → **every
legacy stop is lost**, which is the exact opposite of the collapse's purpose.

"legacy-active + absent-in-sub-block" is **ambiguous** between two cases that
look byte-identical to `mergeDaemonIntentStops`:

- **never-migrated** (initial collapse, or a stop added by the old binary after
  the last merge) → MUST add.
- **deliberately-cleared-after-migration** (this bug) → MUST NOT re-add.

## Correct fix (design decision, not a code patch)

Distinguishing the two cases requires a **migration watermark / tombstone**: a
persisted record of which legacy stops have ALREADY been migrated, so a later
absence means "cleared" (skip) rather than "never migrated" (add). Options to
weigh (needs `$architect`):

- A per-task "migrated-at" marker in the sub-block or a sidecar, compared
  against the legacy record's `UpdatedAt` — only re-add when the legacy record
  is NEWER than the last migration of that task (the legacy writer genuinely
  re-stopped it), not merely present.
- Delete the specific legacy task entry from `daemon-intent.json` immediately on
  a successful per-task merge (partial-file rewrite under the held flock),
  rather than deleting the whole file only at the end — so a cleared stop cannot
  be re-sourced from a lingering whole-file legacy record.
- Accept the transitional-window risk as documented and rely on the file
  deletion closing the window quickly (do nothing in code).

## Disposition

Low severity, transitional-window-only, and the naive fix is actively harmful.
NOT a quick-fix. Route to `$architect` for the watermark-vs-per-task-delete
decision before any code change. Do not apply the "drop don't re-add" patch.
