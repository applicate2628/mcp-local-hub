---
status: accepted
date: 2026-07-11
accepted: 2026-07-11 (LEAD arbitration of 5 independent design reviews; $architecture-reviewer gate on the reworked design)
slug: deadopt-v1-all-clients-only-scope
deciders: LEAD (arbitration); $architect (design rework); $architecture-reviewer (promotes)
context: work-items/active/2026-07-09-deadopt-hub-to-native/design.md + review-multimodel-2026-07-11.md
supersedes: none
superseded-by: none
---

# Decision: de-adopt v1 is ALL-CLIENTS-ONLY, ATOMIC, gate-OFF-only (subset + gate-ON + --reconstruct-legacy deferred)

A multi-model review (codex gpt-5.6-sol xhigh + 4 fable-5 angles, synthesized in
`work-items/active/2026-07-09-deadopt-hub-to-native/review-multimodel-2026-07-11.md`)
converged unanimously that the prior design's SUBSET de-adopt was the root of most of the
blocking defects. This decision fixes v1 scope.

## Context

The prior design (`a1c2bcab`) supported subset de-adopt (de-adopt some-but-not-all clients
of an adopt-owned manifest). All 5 reviews flagged that subset drags in structural
complexity the shipped provenance store does not support cleanly:

- an UNDECLARED 4th mutator + a `de_adopting → adopted` reverse transition
  (only 3 mutators are declared at `internal/api/adopted_entries.go:993-995`);
- the `UpdateAdoptExpectedManifestHash` + `ManifestEditInWithHash` edit lane;
- a per-client snapshot-prune gap the whole-dir `removeAdoptSnapshots` and the rowful-dir GC
  cannot express;
- an UNJOURNALED resume target set (a retry could widen/narrow scope);
- a subset `/g/` branch.

## Decision

**v1 = ALL-CLIENTS-ONLY ATOMIC de-adopt of one adopt-owned manifest, under gate-OFF.**
Targets ≡ the record's `AdoptClients` (always all of them). Also CUT from v1:

- **Gate-ON de-adopt** — REFUSED with "gate OFF first, then de-adopt" (memo item 2, option
  b). Under gate-ON the reconcile removed every per-server entry
  (`install_hub_reconcile.go:233-263`), so the gate-OFF recognizer path does not apply; the
  gate-ON model is a distinct surface.
- **`--reconstruct-legacy`** — fail-closed on no provenance is the complete v1 answer.
- **Byte-exact entry equality (old P2-b)** — use the shipped field-equality recognizer
  `liveEntryMatchesManifestBinding` instead (brittle across binary upgrade; `RelayExePath`
  is not stored).

Subset de-adopt + gate-ON de-adopt are DEFERRED to
`work-items/backlog/2026-07-11-deadopt-subset-and-gate-on-followup.md`. Per-client detach
remains available TODAY via the shipped Servers-matrix uncheck → `/api/demigrate` lane.

## Rationale

- **Unambiguous resume scope.** With `targets ≡ AdoptClients`, the roll-forward resume set is
  fixed — no journaled target list, no widen/narrow-on-retry hazard. Per-client and per-step
  done-ness is DERIVED from live state.
- **One manifest mutation.** All clients removed ⇒ the manifest always ends zero-binding ⇒ a
  single hash-gated DELETE. No `ManifestEditInWithHash` lane; no 4th mutator; no
  `de_adopting → adopted` reverse transition. Only 2 declared mutators are used
  (`MarkAdoptProvenanceDeAdopting`, `CloseAdoptProvenance`).
- **Removes two internal contradictions** the reviews found (the subset `/g/` branch and the
  per-client snapshot-prune gap) without weakening the round-trip guarantee.

## Consequences

- The design's manifest mutation is a single hash-gated DELETE via `ManifestDeleteInWithHash`
  (decision `2026-07-10-deadopt-manifest-delete-hash-gate.md`, unchanged/accepted).
- `UpdateAdoptExpectedManifestHash` stays DECLARED but UNUSED in v1.
- `BuildHubReconcilePlan` is NOT modified in v1 (gate-ON deferred). The pre-existing gate-ON
  zero-binding stale-aggregate gap is filed as an adjacent bug and folds into the follow-up.
- Gate-ON de-adopt, subset de-adopt, the reconcile zero-binding prune, and a single-owner
  hub-entry renderer are all deferred to the follow-up item.
- A follow-up work-item stub is created; the subset path needs a journaled target set +
  per-client snapshot pruning + the declared 4th mutator + a `de_adopting → adopted`
  transition before it can ship.
