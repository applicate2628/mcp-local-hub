# Brief — seven-finding fix-round re-verification

## Scope

- Review `git diff origin/master..HEAD` on `feat/mcp-front-daemon`.
- Re-verify the seven findings named by the operator.
- Treat commits `3f72365d` and `d6c0501f` as untrusted fix claims.
- Trace every write reachable from a request served by `RouteHandler()`.
- Verify every writer to the workspace registry publishes atomically.
- Mutation-test the claimed new read-only controls with exact `-run` filters.

## Out of scope

- Product-code or test-code edits.
- Unrelated packages.
- CodeGraph or repository-wide semantic indexing.
- Starting `mcphub`, killing processes, touching the real operator state.
- Whole-package or unscoped Go test execution for protected packages.

## Acceptance criteria

1. Each original finding is classified `CLOSED`, `PARTIALLY CLOSED`, or
   `NOT CLOSED` with current `file:line` evidence.
2. Any still-reachable shared-state write from `RouteHandler()` is enumerated.
3. Every workspace-registry writer is checked for atomic-rename publication.
4. Seeder persist/adopt/event ordering is checked on all return paths.
5. Descriptor equality is checked for nil/empty and hidden-field churn.
6. Strict-port resolution failure is checked both with and without an existing
   built-in route row.
7. Each claimed falsifying test has a baseline run and controlled mutation
   result, or an explicit evidence gap.

## Required roles

- `$knowledge-archivist`: repair the drifted recovery index only.
- `$planner`: bounded review and verification plan.
- `$analyst`: factual diff, flow, writer, and contract map.
- `$qa-engineer`: filtered tests and mutation evidence.
- `$architecture-reviewer`: adversarial re-review and anti-layering gate.

## Critical risks and owners

- Live-fleet damage: lead owns command admission; only exact filtered tests.
- Vacuous regression tests: `$qa-engineer`.
- Hidden write path and torn unlocked reads: `$analyst` and
  `$architecture-reviewer`.
- Scope drift: lead; no unrelated packages.

## Diff-invisible invariants

- Route requests do not create or modify registry, intent, trusted-roots,
  event-log, lock, or directory artifacts.
- An unlocked registry read never observes a torn writer publication.
- Failed startup persistence does not mutate adopted intent or emit success.

## Named regression guards

- Exact tests selected from the changed test files, always with `-run`.
- Controlled source mutations applied only to a temporary detached worktree or
  temporary source copy, never the operator's live worktree or state.
