# status - phase-2 de-adopt hub to native

Template: design (full-delivery). Orchestrator: `$lead`.
State: **DESIGN REWORKED to v1 all-clients-only — awaiting an independent FABLE audit,
then planning.** The blocking prerequisite is DELIVERED; the design was reworked to the
multi-model synthesis (v1 = all-clients-only, gate-OFF-only atomic de-adopt). Implementation
not started.
Depends-on: 2026-07-09-adopt-side-durable-pre-adopt-provenance, bug:2026-07-11-gc-phase2-stale-candidate-reaps-committed-row, bug:2026-07-11-classifier-committed-signal-blind-to-entry-drift

Dependency note: `2026-07-09-adopt-side-durable-pre-adopt-provenance` is DELIVERED +
closed (2026-07-10, PR #528 squash `16dba601`), archived at
`work-items/archive/2026-07/2026-07-09-adopt-side-durable-pre-adopt-provenance/`. The
durable provenance store (`<state-dir>/adopted-entries.json` + pinned snapshots) this
item consumes exists on master, so the `Depends-on:` edge is met.

Adopt-GC dependency (round 5, P1-A) — SATISFIED at HEAD (master `c7e2534b`): de-adopt reads the
provenance rows + secret-bearing snapshots the adopt GC could DESTROY from a STALE Phase-1 candidate.
Before #531/#532, `reapAdoptProvenanceRow` dropped every row matching the manifest NAME with no
`(state, UpdatedAt)` reap-time filter and Phase-2 classified the stale Phase-1 copy under a
later-acquired lease, so de-adopt's per-manifest lease did NOT fence it (the GC's decision inputs
pre-date the lease); three interleaves could destroy de-adopt's provenance (pre-de-adopt destruction;
de-adopt→re-adopt destroyed; crash-after-E3 `de_adopting` row reaped → permanent manifest/secret leak).
BOTH `Depends-on` adopt-GC bugs are now FIXED at HEAD (both work-items `Status: fixed`):
`2026-07-11-gc-phase2-stale-candidate-reaps-committed-row` (FIXED by #531 — Phase-2 re-reads the row
under the held lease at `internal/api/adopted_entries.go:1144-1177` and `reapAdoptProvenanceRow`
(`:946-978`) no-ops unless `(ManifestName, state, UpdatedAt)` still match, matched at `:954` — closes all
three interleaves) and `2026-07-11-classifier-committed-signal-blind-to-entry-drift` (FIXED by #532 —
committed-KEEP hardened against live-entry drift at Signal 2b `:521-530`, protects the claim-10
recoverability contract). See design.md "Adopt-GC dependency". The `Depends-on:` edges are MET; the
declaration above is retained as a traceability record.

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
- **Design revised (2026-07-11, round 4 — Sol xhigh P1-a atomic-seam delta-check).** Added the
  read-only `ClassifyEntryUnderLock` capability method as the ONE under-lock classification owner
  for BOTH the `--accept-conflict` acceptance decision and the resume done-ness derivation
  (removed the round-3 parallel unlocked read). Claims → 18; T15 added.
- **Design revised (2026-07-11, round 5 — fable-5 adversarial P1-B/P1-A + P2/P3 fold-in).**
  The round-4 seam had pinned `ClassifyEntryUnderLock`'s live derivations to "the same code
  GetEntry wraps" — WRONG for mimocode (`GetEntry` is a MERGED multi-layer view, `mimocode.go:3868-3951`),
  which misclassifies a merged-lower's re-emerged lower layer after a successful remove as
  GenuineConflict when the truth is RestoreDone (a CLOSE-READY wedge) and voids the atomicity claim
  (reads files the ConfigPath lock does not cover). **FIX (P1-B, a COLLAPSE not an edge):** pin
  BOTH the live `*MCPEntry` and raw-subtree derivations to the WRITE-TARGET-PHYSICAL bytes read once
  under the lock (the `EntryPresentInBytes` single-file section owner, `entry_bytes.go:95-103`) for
  EVERY adapter — correct-by-construction (adopt wrote the write target; merged-lower success ≡
  write-target absence). **P1-A:** added `Depends-on` for the two filed adopt-GC bugs + the "Adopt-GC
  dependency" section; corrected design:564/:795 (they asserted a reap-time state filter
  `reapAdoptProvenanceRow` did not have AT ROUND-5 TIME — SINCE ADDED by #531, both edges now MET).
  **P2:** a cleanly-absent config → empty-config (live==nil),
  never `ClassifyUnreadable`. **P3-a:** the classify forwarder holds `withConfigReadLock`
  (missing-dir short-circuit, `config_lock.go:150-158`) so plan-time classify of an absent config has
  no FS side effect. **P3-b:** one-sentence G8 extension (accepted-conflict verdict not re-checked at
  E6). Claims → 19; tests T16 (merged-lower write-target-physical) + T17 (empty-config ≠ Unreadable)
  added. No architecture / scope / protected-surface change — only WHICH BYTES classify reads.

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
**Phase 0 DONE (2026-07-14): design.md C6 doc-freshness refresh.** The "Adopt-GC dependency" section,
every present-tense "no filter" assertion, and every `internal/api/adopted_entries.go` line citation in
`design.md` were re-pointed to HEAD (master `c7e2534b`) and now assert the RESOLVED state (both
`Depends-on` adopt-GC edges FIXED by #531/#532; `reapAdoptProvenanceRow` HAS the `(state, UpdatedAt)`
identity gate at `:946-978`/`:954`; the transition-window reap is CLOSED). Docs-only — no design
decision, claim count (still 19), scope, or architecture changed. The round-4 `withConfigLock`→
`withConfigReadLock` supersession marker and the shared-owner-#3 scope note (the `.snapshot`
secret-bearing half already landed via #532; only the `state_read_caps.go` read-cap line remains) were
also folded in.

**Phase 1 IN PROGRESS on branch `feat/deadopt-phase1-manifest-delete-hash`** (the `ManifestDeleteInWithHash`
delete-mutation-point hash gate, per the Change-Surface Contract). The two adopt-GC `Depends-on` edges
are now SATISFIED at HEAD, so that blocker is CLEARED. The remaining v1 delivery gates ($lead-owned)
are unchanged: a codex (Sol) DELTA-recheck of the round-5 amended sections (the write-target-physical
collapse in `ClassifyEntryUnderLock` + the resume/accept wiring, the `withConfigReadLock` P3-a forwarder,
the "Adopt-GC dependency" section, the P2 empty-config pin, claims 19 + T16/T17) — NOT a full re-audit —
then `$planner` breaks it into delivery phases respecting the v1 Change-Surface Contract (THREE additive
shared-owner changes). No provenance-CODE gap blocks de-adopt; the one under-specified read-cap detail
is flagged in the design's "Provenance-gap flag" + filed as an adjacent bug. Do NOT reopen the tracked
provenance residuals (`work-items/backlog/2026-07-10-adopt-provenance-lease-hygiene.md`) or patch the
protected provenance surfaces.
