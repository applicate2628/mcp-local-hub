# status - phase-2 de-adopt hub to native

Template: design review REVISE. Orchestrator: `$lead`.
State: REVISE — design revision pending; the blocking prerequisite is now DELIVERED
(dependency satisfied), so this item is UNBLOCKED for implementation once the design is
revised. Implementation not started.
Depends-on: 2026-07-09-adopt-side-durable-pre-adopt-provenance

Dependency note: `2026-07-09-adopt-side-durable-pre-adopt-provenance` (the adopt-side
provenance prerequisite established by `review.md`) is DELIVERED + closed (2026-07-10,
PR #528 squash `16dba601`), archived at
`work-items/archive/2026-07/2026-07-09-adopt-side-durable-pre-adopt-provenance/`. The
durable provenance store (`adopted-entries.json` + pinned snapshots) this item consumes
now exists on master, so the `Depends-on:` edge is met.

## Active agents / lanes
- None. The prerequisite is delivered; the remaining gate is this item's own design
  revision (per `review.md`), not the provenance dependency.

## Completed agents / lanes
- Design memo accepted and copied into this work-item as `design.md`.
- Adversarial architecture review recorded in `review.md` with verdict
  REVISE: de-adopt is blocked on adopt-side durable pre-adopt provenance.

## Next action
The adopt-side durable pre-adopt provenance prerequisite is DELIVERED (PR #528). It
covers durable per-client original state, absent/present state (plus the
`present-merged-lower` MiMoCode state), protected non-prunable restore artifacts, the
generated manifest hash + expected hash, routed secret keys, and the
`adopting → adopted → de_adopting → closed` operation state — the store is
`<state-dir>/adopted-entries.json` + `<state-dir>/adopt-provenance/<manifest>/`
snapshots. Note: `expected_hub_shape` was DROPPED by design (arch F3); de-adopt
recomputes it via the existing `liveEntryMatchesManifestBinding` owner. See the
archived
`work-items/archive/2026-07/2026-07-09-adopt-side-durable-pre-adopt-provenance/design.md`
("Consumer-contract handoff" + "Consumer-contract addition") for the MUST/SHOULD
obligations on this consumer.

Remaining before implementation: revise the de-adopt design (per `review.md`) against
the shipped provenance store. Implementation is otherwise unblocked — the prerequisite
dependency is met.
