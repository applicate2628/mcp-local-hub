# status - phase-2 de-adopt hub to native

Template: design (full-delivery). Orchestrator: `$lead`.
State: **DESIGN REWORKED to v1 all-clients-only — awaiting an independent FABLE audit,
then planning.** The blocking prerequisite is DELIVERED; the design was reworked to the
multi-model synthesis (v1 = all-clients-only, gate-OFF-only atomic de-adopt). Implementation
not started.
Depends-on: 2026-07-09-adopt-side-durable-pre-adopt-provenance

Dependency note: `2026-07-09-adopt-side-durable-pre-adopt-provenance` is DELIVERED +
closed (2026-07-10, PR #528 squash `16dba601`), archived at
`work-items/archive/2026-07/2026-07-09-adopt-side-durable-pre-adopt-provenance/`. The
durable provenance store (`<state-dir>/adopted-entries.json` + pinned snapshots) this
item consumes exists on master, so the `Depends-on:` edge is met.

## Active agents / lanes
- None. Design rework complete; the next gate is an independent FABLE audit, then `$planner`.

## Completed agents / lanes
- Design memo accepted and copied into this work-item as `design.md`.
- Adversarial architecture review recorded in `review.md` (verdict REVISE, blocked on
  adopt-side provenance).
- **Design revised (2026-07-10, round 1)** against the delivered provenance contract:
  `design.md` rewritten to consume the AS-SHIPPED `AdoptProvenanceRecord`/
  `AdoptClientProvenance` store (`internal/api/adopted_entries.go`), resolving F1
  (hash-gated delete), F2 (secret cleanup ordering), F3 (`/g/` policy), the new
  `present-merged-lower` state, P2-1 (sha256 fail-closed gate),
  backup-retention/lock-order/schema gaps, and T1–T6. `review.md` carries a
  "## Revision resolves" mapping and the original REVISE is cleared.
- **Design revised (2026-07-10, round 2 — arch + security gate fold-in).** Both design
  gates returned REVISE (design-level, none a redesign); all folded into `design.md` +
  the decision: SECURITY P1 (single-read restore via a new bytes-input helper), P2-a
  (fail-closed-on-empty-hash polarity + retained path-escape guard), P2-b (remove-path
  exact-hub-entry gate + documented residual + relax-lane warning), P2-c (redaction
  contract + test), P3-a (shared-key operator warning), P3-b (bounded residual); ARCH
  F-A (OperationState-branched resume contract, reconciled with test 14), F-B (the
  SECOND shared-owner change — gate-ON zero-binding prune extends `BuildHubReconcilePlan`;
  blast radius + scope corrected), F-C (full lock total order + no-reverse-edge, T6),
  F-D (routed-secret pre-filter). Blast radius named THREE additive shared-owner changes.
- **Design REWORKED (2026-07-11, round 3 — multi-model synthesis + v1 scope cut).** 5
  independent adversarial lanes (codex xhigh + 4 fable-5) reviewed the round-2 design;
  LEAD synthesis (`review-multimodel-2026-07-11.md`) drove a REWORK to **v1 =
  all-clients-only, gate-OFF-only atomic de-adopt** (subset + gate-ON + `--reconstruct-legacy`
  CUT). 7 blocking fixes folded: close=DELETE-row-snapshots-first (no `closed` tombstone);
  gate-ON refused; equality via the shipped recognizer (byte-exact cut, codex P0-1 REFUTED);
  anchored snapshot read + recomputed path + `.snapshot` cap/secret-bearing; CAS on a
  capability interface; client-config CAS mutation-point gate. Threat model corrected (owner
  anchor = authenticity root). `review.md` carries the "## Third-round design gate" mapping.
  design.md is a full rewrite; claims are 13; blast radius is 3 additive shared-owner changes
  (`ManifestDeleteInWithHash`, the CAS capability interface, the `.snapshot` read-cap/secret
  additions). `BuildHubReconcilePlan` is NOT touched in v1.

## Decisions
- `2026-07-11-deadopt-v1-all-clients-only-scope` (**`status: accepted`**) — v1 scope: atomic
  all-clients-only, gate-OFF-only; subset + gate-ON + `--reconstruct-legacy` deferred.
- `2026-07-10-deadopt-manifest-delete-hash-gate` (**`status: accepted`**) — `ManifestDeleteInWithHash`
  (fail-closed-on-empty polarity, retained path guard). Its Consequence (c) F-B (reconcile
  prune) is now DEFERRED with gate-ON.

## Dependents / follow-ups
- `work-items/backlog/2026-07-11-deadopt-subset-and-gate-on-followup.md` — subset + gate-ON de-adopt.
- Adjacent bugs filed: `work-items/bugs/2026-07-11-classify-dead-adopting-row-gate-on-blind.md`
  (adopt-side gate-ON blindness), `.../2026-07-11-hub-reconcile-gate-on-zero-binding-stale-aggregate.md`
  (pre-existing reconcile gap), `.../2026-07-11-adopt-snapshot-read-cap-too-small.md`
  (snapshot read-cap; fix lands with de-adopt).

## Next action
An independent FABLE audit of the reworked `design.md`, then `$planner` breaks it into
delivery phases respecting the v1 Change-Surface Contract (THREE additive shared-owner
changes). No provenance-CODE gap blocks de-adopt; the one under-specified read-cap detail is
flagged in the design's "Provenance-gap flag" + filed as an adjacent bug. Do NOT reopen the
tracked provenance residuals (`work-items/backlog/2026-07-10-adopt-provenance-lease-hygiene.md`)
or patch the protected provenance surfaces.
