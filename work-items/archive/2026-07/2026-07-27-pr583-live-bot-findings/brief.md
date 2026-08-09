# Brief

## Scope

- PR #583 branch `fix/cursor-not-default-install`.
- The eight supplied Codex-bot findings in `internal/api/default_client_scope_test.go`,
  `internal/api/register.go`, and `internal/cli/register.go`.
- Class-wide sweeps for preflight test isolation, registration binding snapshot
  consistency, relay-only selected clients, and destructive cleanup replacement
  proof.

## Out of scope

- GUI, tray, or supervisor launch.
- Production scheduler or production state access.
- Other worktrees or the main checkout.
- Push, merge, release, unrelated cleanup, or API redesign outside the verified
  defect classes.

## Acceptance criteria

- Classify each supplied finding as `ALREADY FIXED`, `REAL, open`, or `WRONG`
  with current branch evidence.
- Fix every `REAL, open` finding at the owning contract or invariant.
- Add a regression test for every real defect class and capture a failure with
  the fix deliberately removed or mutated.
- Run touched-package tests only with `-tags=test_state_path_env` and a fresh
  `MCPHUB_STATE_DIR_OVERRIDE`.
- Run `go build ./...` and `go vet ./...`.
- Commit the focused fix; do not push.

## Required roles

- `$analyst` / `$bug-hunting`: classification, root-cause evidence, class inventory.
- `$backend-engineer`: implementation and regression tests.
- `$qa-engineer`: independent mutation proofs and verification.
- `$architecture-reviewer`: claim-verify review of snapshot consistency and
  destructive-cleanup replacement proof.
- Main-session `$lead`: integration owner, scope/gate control, commit, and report.

## Critical risks and owners

- Production-state or scheduler contact: main-session `$lead`; the dispatch tag
  and fresh-temp rule is absolute for all API/CLI tests.
- Concurrent settings drift across one registration: `$backend-engineer`,
  independently checked by `$architecture-reviewer`.
- Direct-entry deletion without a live replacement: `$backend-engineer`,
  independently checked by `$qa-engineer` and `$architecture-reviewer`.
- Defect-class incompleteness: `$analyst` inventory, echoed by every downstream
  gate.
- Unrelated dirty-worktree damage: main-session `$lead`.

## Change boundary

Expected owning seam: registration binding resolution and cleanup authorization
inside `internal/api`, plus directly corresponding API tests and CLI promise text
only if the implementation contract requires it.

## Current stage

Research and classification.
