# Brief — PR #589 seven-finding correction round

## Scope

- Compare the seven supplied Codex-bot findings with the current local branch,
  including unpushed local commits.
- Classify each finding as `ALREADY FIXED`, `REAL, open`, or `WRONG` with
  commit and `file:line` evidence.
- For every real finding, identify the implied rule, enumerate every
  participant in that defect class, fix every confirmed instance at its owning
  invariant, and add focused regression coverage.
- Produce failing mutation evidence for every real fix, restore exact source,
  and run scoped package tests plus repository build/vet gates.
- Commit the completed correction locally; do not push.

## Supplied findings

| ID | Review location | Claimed defect class |
| --- | --- | --- |
| F1 | `internal/cli/gui_exit_signal.go:77` | cancellation ordering before diagnostics |
| F2 | `internal/daemonrecovery/recovery.go:515` | synchronous release failure lost before audit fallback |
| F3 | `internal/cli/supervise_ensure_alive.go:1376` | stale Unknown confirmation across non-Unknown supervisor-down observations |
| F4 | `internal/cli/supervise_ensure_alive.go:401` | unsupported identity probe misclassified as Alive |
| F5 | `internal/cli/supervise_ensure_alive.go:704` | retained lease not propagated to relaunch decision |
| F6 | `internal/daemonrecovery/recovery.go:799` | committed audit remains only in an in-memory worker at CLI exit |
| F7 | `internal/cli/supervise_ensure_alive.go:880` | unbounded pre-action detection emit blocks recovery |

## Out of scope

- Unrelated packages, refactors, cleanup, and other work-items.
- Any GUI, tray, supervisor, scheduler, daemon, or installed-binary launch.
- Any Task Scheduler mutation, real-state-directory access, or real loopback
  probe.
- Unscoped `go test ./...`, process killing by image name, other worktrees,
  stash/reset/checkout-discard, push, publication, or CI mutation.

## Acceptance criteria

- AC1: all seven findings are classified with current-session code/history
  evidence.
- AC2: every real finding has a class-sweep table covering all sibling
  participants, including correct and unaffected sites.
- AC3: each real defect is corrected at the single owning function, contract,
  invariant, or boundary without catch-and-swallow, fallback masking, or
  duplicated guard layering.
- AC4: every real fix has a deterministic regression test and a captured
  mutation run that fails for the intended assertion before exact restoration.
- AC5: each touched package passes a narrow tagged test with a fresh isolated
  state directory; no authorized command reaches live operator state.
- AC6: `go build -tags=test_state_path_env ./...` and
  `go vet -tags=test_state_path_env ./...` both exit 0 with fresh isolated
  state directories.
- AC7: one focused conventional-commit records every finding disposition and
  closing mechanism; branch remains unpushed.

## Required roles

- `$knowledge-archivist`: recovery/index consistency.
- `$analyst`: factual classification, control-flow map, history, and complete
  defect-class inventory.
- `$architect`: single-owner fix design, seams, blast radius, and regression
  strategy.
- `$reliability-engineer`: cancellation, lock lifetime, durability, recovery,
  and degradation constraints.
- `$planner`: ordered implementation and mutation/verification gates.
- `$backend-engineer`: scoped Go fixes and tests; integration owner for the
  two-package change.
- `$qa-engineer`: independent scoped tests and mutation proof.
- `$architecture-reviewer`: claim verification plus multi-fix anti-layering
  and resource-lifetime review.
- Main conversation as `$lead`: artifact acceptance, final verification,
  commit, and reconciliation.

## Critical risks and owners

- Live-machine side effects: `$qa-engineer`, enforced by the user-supplied tag,
  fresh-state-dir, process, and worktree constraints.
- Cancellation and stale-state races: `$reliability-engineer`.
- Flock release/retained-lease lifetime: `$reliability-engineer` and
  `$architecture-reviewer`.
- Destructive-action audit durability across CLI exit: `$reliability-engineer`.
- Fix layering or instance-only correction: `$architect` and
  `$architecture-reviewer`.
- Cross-package integration and test isolation: `$backend-engineer` as
  integration owner, then `$qa-engineer`.
- Complete seven-row reconciliation and local-only commit: main conversation
  as `$lead`.
