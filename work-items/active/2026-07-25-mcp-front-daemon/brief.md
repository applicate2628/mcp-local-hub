# Brief — PR #588 live Codex-bot finding closure

## Scope

- Inspect the fourteen reported rows as ten deduplicated defect classes.
- Compare remote head `origin/feat/mcp-front-daemon` with local commit
  `3872ee16`; classify local closure claims from current code and tests.
- Sweep every named defect class across sibling paths and state shapes.
- Fix any class still open at its owning boundary.
- Mutation-prove each real regression guard and run required verification.
- Commit only task-owned changes and do not push.

## Out of scope

- Unrelated features, refactors, and previously filed adjacent defects.
- GUI, tray, supervisor, or scheduler launch.
- Real operator state, live process fleet, or production state paths.
- Unscoped tests and destructive Git operations.

## Acceptance criteria

1. Every reported row maps to a unique defect class and one of
   `ALREADY FIXED`, `REAL, open`, or `WRONG`, with current evidence.
2. Each class sweep enumerates every participant, including retry, rollback,
   pending/unreachable, legacy, newly appearing client, and session-lifetime
   paths.
3. Every open class is corrected at its single owner without fix layering.
4. Each regression guard has a controlled failing mutation and restored pass.
5. All protected-package tests use the required tag and fresh state directory.
6. `go build ./...` and `go vet ./...` exit 0.
7. A local commit names every closure mechanism; no push occurs.

## Required roles

- `$analyst`: current-code classification and complete class inventory.
- `$architect`: correction seams and single-owner claims if any class is open.
- `$reliability-engineer`: rollback, retry, concurrency, and cleanup constraints.
- `$planner`: bounded implementation and verification plan.
- `$backend-engineer`: integration owner for any required Go changes.
- `$qa-engineer`: filtered tests, mutation evidence, build, and vet.
- `$architecture-reviewer`: independent claim verification and anti-layering.

## Critical risks and owners

- Live-fleet damage: lead owns command admission; protected tests require the
  build tag plus fresh state override.
- Recovery-data loss and stale overwrite: `$reliability-engineer`.
- Transaction interleaving and lock lifetime: `$reliability-engineer`.
- Incorrect command-mode dispatch: `$backend-engineer` and `$qa-engineer`.
- Session-resource leakage: `$backend-engineer` and `$qa-engineer`.
- Vacuous regression tests: `$qa-engineer`.
- Fix layering and blast radius: `$architecture-reviewer`.

## Diff-invisible invariants

- Rollback never overwrites state not owned by the active generation.
- Forward retry preserves the original baseline while tracking its latest
  writes.
- One reconcile operation owns the report/client mutation lifecycle at a time.
- Read-only `--check` never dispatches a mutating reconcile mode.
- Route-owned sessions are expired by the route process itself.

## Named regression guards

- Exact tests selected from commit `3872ee16` and the current test tree,
  always using anchored `-run` filters.
- Controlled source mutations are bounded, recorded, and restored without
  checkout, hard reset, or stash.
- Expected mutation result: the specific guard fails for the reverted defect
  mechanism, then passes after restoration.
