# status - phase-2 de-adopt hub to native

Template: design review REVISE / blocked prerequisite. Orchestrator: `$lead`.
State: REVISE / blocked on prerequisite - implementation not started.
Depends-on: 2026-07-09-lsp-relay-per-client-disable-gui, 2026-07-09-intent-collapse-stop-resurrection, 2026-07-09-adopt-side-durable-pre-adopt-provenance

Dependency note: `2026-07-09-adopt-side-durable-pre-adopt-provenance` names
the adopt-side provenance prerequisite established by `review.md`; no active
work-item exists for it yet.

## Active agents / lanes
- None. Parked behind the two in-flight PRs.

## Completed agents / lanes
- Design memo accepted and copied into this work-item as `design.md`.
- Adversarial architecture review recorded in `review.md` with verdict
  REVISE: de-adopt is blocked on adopt-side durable pre-adopt provenance.

## Next action
Design the adopt-side durable pre-adopt provenance prerequisite before any
de-adopt implementation. That prerequisite must cover durable per-client
original state, absent/present state, protected non-prunable restore artifacts,
expected hub shape, generated manifest hash, routed secret keys, and operation
state.

Implementation must NOT start until the de-adopt design is revised and the
adopt-side provenance prerequisite is delivered.
