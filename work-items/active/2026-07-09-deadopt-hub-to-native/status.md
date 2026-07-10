# status - phase-2 de-adopt hub to native

Template: design (full-delivery). Orchestrator: `$lead`.
State: **DESIGN REVISED — unblocked; ready for planning.** The blocking prerequisite is
DELIVERED and the design has been revised against the AS-SHIPPED provenance contract;
the original REVISE/BLOCKED verdict is cleared. Implementation not started.
Depends-on: 2026-07-09-adopt-side-durable-pre-adopt-provenance

Dependency note: `2026-07-09-adopt-side-durable-pre-adopt-provenance` is DELIVERED +
closed (2026-07-10, PR #528 squash `16dba601`), archived at
`work-items/archive/2026-07/2026-07-09-adopt-side-durable-pre-adopt-provenance/`. The
durable provenance store (`<state-dir>/adopted-entries.json` + pinned snapshots) this
item consumes exists on master, so the `Depends-on:` edge is met.

## Active agents / lanes
- None. Design revision complete; the next gate is `$planner`.

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
  F-D (routed-secret pre-filter). Blast radius now names THREE additive shared-owner
  changes; claims grew to 19.

## Decisions
- `2026-07-10-deadopt-manifest-delete-hash-gate` (**`status: accepted`** — arch-reviewer
  promoted) — the shared `ManifestDeleteInWithHash` gate for the last-binding delete (F1,
  fail-closed-on-empty polarity, retained path-escape guard) PLUS the F-B second
  shared-owner change (gate-ON zero-binding `mcphub-hub` prune extends
  `BuildHubReconcilePlan`).

## Next action
`$planner` breaks the revised `design.md` into the delivery phases (2a–2e) it names,
respecting the Change-Surface Contract (now THREE additive shared-owner changes:
`ManifestDeleteInWithHash`, the bytes-input restore variant, the hub-reconcile
zero-binding prune). No provenance gap blocks de-adopt; the de-adopt-owned nuances
(routed-secret pre-filter F-D, P3-a shared-key scan) are in the design's "Provenance-gap
flag" section. Do NOT reopen the tracked provenance residuals
(`work-items/backlog/2026-07-10-adopt-provenance-lease-hygiene.md`).
