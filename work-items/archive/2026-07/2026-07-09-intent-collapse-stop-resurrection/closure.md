# Closure - intent collapse stop resurrection

Closed: 2026-07-09

## Outcome

DELIVERED. PR #525 (`fix(supervisor): legacy_stop_watermarks blocks stale
daemon-intent stop-resurrection`) merged as squash commit `5d8ab063`. The
deployed installed binary reports commit `7898c148`. Live host verification
confirmed the absent-only state shape after the stop/restart sequence: the final
state has `stops=0`, `legacy_stop_watermarks=1`, no stop/watermark overlap, and
no bare task keys. A read-only status check showed the expected fleet shape:
21 daemon rows Running, the weekly refresh timer Stopped, and no quarantined
rows.

## What shipped

The delivered fix closes the P1 found after the original EAGER representation:
that design wrote a watermark beside every active stop, duplicating the whole
`stops` map, so a large legacy `daemon-intent.json` could push
`supervisor-intent.json` over the shared 16 MiB `maxIntentFileBytes` cap and make
it unreadable. The accepted decision in
`work-items/decisions/2026-07-09-absent-only-legacy-stop-watermarks.md` chose the
ABSENT-ONLY representation: `LegacyStopWatermarks` holds only canonical task keys
ABSENT from `Stops` (cleared tombstones); for a present task, the `Stops` entry
is the watermark. Enforcement moved from scattered per-site checks to two
boundaries: every production decode canonicalizes both maps so no mutator sees a
bare key, and `normalizeAbsentOnlyStopWatermarks` drops any watermark whose key
is present in `Stops` immediately before marshal. The bare/canonical
key-collision merge rule keeps the record with the later `UpdatedAt`; on an
exact timestamp tie it keeps the canonical record.

## Review findings and residual risk

Four independent adversarial audits ran. Three found real defects that the bot
and the fixing agent's first green gates did not surface:

1. Per-site enforcement leaked three ways: an unrelated-task watermark survived
   an unrelated write, whole-intent writers copied prior watermarks verbatim
   (also the old-eager upgrade path), and bare keys were never canonicalized.
2. A P1 operator-stop-loss path existed because mutators worked in raw key space.
   Against a bare-keyed on-disk file, a clear silently missed the entry and
   write-side canonicalization wrote the stop back active; the idle guard could
   then overwrite and destroy an operator's stop.
3. A P2 merge-rule stop-loss existed because positional "canonical wins"
   dropped the operator's newer stop after a binary downgrade -> upgrade cycle,
   where the older binary writes a bare key last. The shipped rule orders by
   `UpdatedAt` instead.

Two pre-existing bare-key rollback tests had also become vacuous once writes
normalized. They were reseeded with raw JSON, and the now-unreachable bare-key
fallback in `supervisorStopForTask` was deleted so those tests exercise the read
boundary rather than a second rescue path.

Residual risk to persisted state: the `UpdatedAt` tie-break assumes writers set
`UpdatedAt` monotonically. A foreign or hand-edited file with equal timestamps
falls back to preferring the canonical record.

## Retrospective

What went well:

- The P1 was fixed at the representation level rather than by shrinking or
  special-casing the eager duplicate map.
- The accepted decision gave the implementation a small invariant:
  watermarks are cleared tombstones only, and present stops own their own
  durable watermark semantics.
- The final gates included both GitHub verification and a real host state-file
  check, not only unit-test success.

What did not go well:

- A workflow/commission lane that died was initially too easy to treat as clean.
  Re-running it found three real holes.
- "Unreachable" triage that reasoned only about the current code missed states
  produced by the history of binaries; downgrade -> upgrade made the merge-rule
  stop-loss reachable.
- Bot PASS signals must be checked through GitHub GraphQL `reviewThreads`, not
  only inline comments filtered by the current head. During this PR, the summary
  path could say there were no major current-head issues while older review
  threads still carried the real defects.

No `work-items/lessons/` registry exists in this repository, so these lessons are
recorded here only.

## Artifacts

- PR #525: merged as squash `5d8ab063`.
- Deployed installed binary: `7898c148`.
- Decision: `work-items/decisions/2026-07-09-absent-only-legacy-stop-watermarks.md`.
- Related bug: `work-items/bugs/2026-07-04-intent-collapse-cleared-stop-resurrection.md`.
