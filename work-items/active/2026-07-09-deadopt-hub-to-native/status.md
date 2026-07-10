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
- **Design revised (2026-07-10)** against the delivered provenance contract: `design.md`
  rewritten to consume the AS-SHIPPED `AdoptProvenanceRecord`/`AdoptClientProvenance`
  store (`internal/api/adopted_entries.go`), resolving F1 (hash-gated delete), F2 (secret
  cleanup ordering), F3 (`/g/` policy), the new `present-merged-lower` state, P2-1 (sha256
  fail-closed gate), backup-retention/lock-order/schema gaps, and T1–T6. `review.md` now
  carries a "## Revision resolves" mapping and the REVISE is cleared.
- One cross-cutting decision filed:
  `work-items/decisions/2026-07-10-deadopt-manifest-delete-hash-gate.md` (`status: proposed`).

## Decisions
- `2026-07-10-deadopt-manifest-delete-hash-gate` (`status: proposed`) — the shared
  `ManifestDeleteInWithHash` gate for the last-binding delete (F1). Awaits the
  `$architecture-reviewer` promotion `proposed → accepted`.

## Next action
`$planner` breaks the revised `design.md` into the delivery phases (2a–2e) it names,
respecting the Change-Surface Contract. No provenance gap blocks de-adopt; the two
de-adopt-owned nuances (idempotent secret delete, P3-2 shared-key scan) are captured in
the design's "Provenance-gap flag" section. Do NOT reopen the tracked provenance residuals
(`work-items/backlog/2026-07-10-adopt-provenance-lease-hygiene.md`).
