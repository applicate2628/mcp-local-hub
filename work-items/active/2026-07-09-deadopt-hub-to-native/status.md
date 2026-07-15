# status - de-adopt v1 (hub to native, all-clients-only, gate-OFF-only, atomic)

Template: design → delivery (full-delivery). Orchestrator: `$lead`.
State: **IN PROGRESS — Phases 0-6 DONE + MERGED to master; Phase 7 (BuildDeAdoptPlan) in impl.**
Design round-5 is ACCEPTED (arch delta-recheck PASS 2026-07-13); `$planner` broke it into a
12-phase delivery plan (`plan.md`, 2026-07-14). Phases 0-6 are all delivered + squash-merged:
Phase 0+1 (`ManifestDeleteInWithHash`, #539/82e07b46), Phase 2 (restore-extraction, #540),
Phase 3 (CAS mutators, #542), Phase 4 (`ClassifyEntryUnderLock`+`EntryRawSubtree`, #544),
Phase 5 (`.snapshot` read-cap, #543), Phase 6 (provenance mutators Mark/Close, #545 — with the
codex-bot P2 fix `27a622ac`: Mark/Close assume caller-held lease, not internal re-acquire).
Phase 7 (`BuildDeAdoptPlan` read-only planner, NEW `deadopt.go`) is now UNBLOCKED (deps 4,5,6
all in master) and in impl. Phases 8-11 pending. master HEAD = `a2a109b0`.
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
`2026-07-11-gc-phase2-stale-candidate-reaps-committed-row` (FIXED by #531 `767b6736` — Phase-2 re-reads
the row under the held lease at `internal/api/adopted_entries.go:1144-1177` and `reapAdoptProvenanceRow`
(`:946-978`) no-ops unless `(ManifestName, state, UpdatedAt)` still match, matched at `:954` — closes all
three interleaves) and `2026-07-11-classifier-committed-signal-blind-to-entry-drift` (FIXED by #532
`e545c06e` — committed-KEEP hardened against live-entry drift at Signal 2b `:521-530`, protects the
claim-10 recoverability contract). See design.md "Adopt-GC dependency". The `Depends-on:` edges are MET;
the declaration above is retained as a traceability record.

## Phase status (plan.md = 12 phases, Phase 0-11)

| Phase | Scope | Gate | Status |
|---|---|---|---|
| 0 | Precondition + C6 stale-text/citation refresh (docs) | $knowledge-archivist + $arch confirm | **DONE + MERGED** (design.md re-pointed to HEAD; committed with Phase 1) |
| 1 | `ManifestDeleteInWithHash` (#1) fail-closed hash gate | $security MANDATORY + $qa | **DONE + MERGED** (#539, `82e07b46`) |
| 2 | `restoreEntryFromBackup`→`restoreEntryFromBytes` extraction (#2 pt1, PURE refactor) | $arch + $qa (full regression) + $security light | **DONE + MERGED** (#540) |
| 3 | CAS mutators `CASRestoreEntryFromBytes` + `CASGuardedRemoveEntry` (#2 pt2) | **FULL COMMISSION** ($security MANDATORY + $arch + $qa) | **DONE + MERGED** (#542, `f4623355`) |
| 4 | `ClassifyEntryUnderLock` (read-only) + `EntryRawSubtree` (#2 pt3) | **FULL COMMISSION** (subtlest seam, P1-a/P1-b) | **DONE + MERGED** (#544, `43675926`) |
| 5 | `state_read_caps.go` `.snapshot` read-cap (#3) | $security ($qa) | **DONE + MERGED** (#543, `9c03b3ff`) |
| 6 | Provenance mutators (D1) — author `MarkAdoptProvenanceDeAdopting` + `CloseAdoptProvenance` bodies | **FULL COMMISSION** (protected provenance store) | **DONE + MERGED** (#545, `a2a109b0`; codex-bot P2 lease-fix `27a622ac`) |
| 7 | `BuildDeAdoptPlan` (read-only planner), NEW deadopt.go | $arch + $security + $qa | **IN IMPL** (dep 4,5,6 all merged; codex sol-xhigh) |
| 8 | `ExecuteDeAdoptWithOpts` — ATOMIC all-clients (DO NOT SPLIT, integration) | **FULL COMMISSION + integration owner + Codex-bot PASS + deep-security** | PENDING (dep: 1,3,4,5,6,7) |
| 9 | CLI `mcphub de-adopt` (alias deadopt) | $qa + $arch light + $security light | PENDING (dep: 7,8) |
| 10 | GUI backend routes + eligibility (G3) | $security + $qa | PENDING (dep: 7,8) |
| 11 | Frontend affordance + Playwright | $ux-reviewer + $frontend self-check + $qa | PENDING (dep: 10) |

Mandatory-gate map (from plan.md): full commission on **3, 4, 6, 8**; security-reviewer MANDATORY on
1,3,4,5,6,7,8,10; Phase 8 additionally requires Codex-bot PASS + deep-security.

Change-Surface Contract (v1 — exactly **3 additive shared-owner changes**, delta-review-confirmed):
1. `ManifestDeleteInWithHash` (manifest.go) — Phase 1.
2. CAS+read capability on adopt-reachable clients adapters (`CASRestoreEntryFromBytes` +
   `CASGuardedRemoveEntry` + `EntryRawSubtree` + read-only `ClassifyEntryUnderLock` +
   `restoreEntryFromBackup`→`restoreEntryFromBytes` extraction) — Phases 2-4.
3. `.snapshot` read-cap line in `state_read_caps.go` — Phase 5 (secret-bearing half already at HEAD).
Plus D1 provenance mutator **BODIES** in `adopted_entries.go` (Phase 6, design-authorized, tightly
bounded). Everything else is NEW files. **`BuildHubReconcilePlan` / `install_hub_reconcile.go` is NOT
touched in v1** (gate-ON deferred; claim-11 negative check).

## Active agents / lanes
- None running. Phase 1 is in bot review (PR #539); the immediate gate is Sol review of the
  round-5 amended sections + Codex-bot PASS, then merge Phase 1.

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
- **Arch delta-recheck PASS (2026-07-13).** The round-5 amended sections cleared architecture review;
  design.md round-5 is ACCEPTED as the planning source of truth.
- **Planned (2026-07-14).** `$planner` broke the accepted design into the 12-phase delivery plan
  (`plan.md`) respecting the v1 Change-Surface Contract; plan snapshot saved at
  `.plans/2026-07/plan(main)-2026-07-14_deadopt-v1-12-phase.md`.
- **Phase 0 DONE (2026-07-14).** design.md C6 doc-freshness refresh (see below); committed with Phase 1.
- **Phase 1 DELIVERED to bot review (2026-07-14).** `ManifestDeleteInWithHash` (fail-closed hash gate)
  on branch `feat/deadopt-phase1-manifest-delete-hash` (`98d16cd0`); P3 atomicity-contract doc-fix for
  Codex-bot #539 at `23971e87`. Awaiting Sol review + bot re-trigger, then merge.

## Decisions
- `2026-07-11-deadopt-v1-all-clients-only-scope` (**`status: accepted`**) — v1 scope: atomic
  all-clients-only, gate-OFF-only; subset + gate-ON + `--reconstruct-legacy` DEFERRED.
- `2026-07-10-deadopt-manifest-delete-hash-gate` (**`status: accepted`**) — `ManifestDeleteInWithHash`
  (fail-closed-on-empty polarity, retained path guard). Its Consequence (c) F-B (reconcile
  prune) is now DEFERRED with gate-ON.

## Dependents / follow-ups
- `work-items/backlog/2026-07-11-deadopt-subset-and-gate-on-followup.md` — subset + gate-ON de-adopt.
- Adjacent bugs filed: `work-items/bugs/2026-07-11-classify-dead-adopting-row-gate-on-blind.md`
  (adopt-side gate-ON blindness), `.../2026-07-11-hub-reconcile-gate-on-zero-binding-stale-aggregate.md`
  (pre-existing reconcile gap), `.../2026-07-11-adopt-snapshot-read-cap-too-small.md`
  (snapshot read-cap; fix lands with de-adopt Phase 5).

## Next action
**Merge Phase 1 (PR #539):** land the round-5-amended Sol delta-recheck + Codex-bot PASS on the
`ManifestDeleteInWithHash` change, then merge to master.

**Then Phase 2 (REDO fresh off master).** Phase 2 (`restoreEntryFromBackup`→`restoreEntryFromBytes`
PURE extraction) was started once, then abandoned mid-refactor at a session limit; that partial branch
was DELETED. It must be REDONE fresh off master AFTER Phase 1 merges (a pure refactor with the existing
restore/demigrate/rollback suites as its regression gate). The capability track (Phases 2→3→4) plus
Phase 5 (state-read cap) and Phase 6 (provenance mutators D1) can then proceed; full-commission phases
(3, 4, 6, 8) each get the multi-model commission (Sol+Terra+fable) + security-reviewer before the bot,
and Phase 8 is D-scale (Codex-bot + deep-security).

Do NOT reopen the tracked provenance residuals (`work-items/backlog/2026-07-10-adopt-provenance-lease-hygiene.md`)
or patch the protected provenance surfaces. `BuildHubReconcilePlan` stays untouched in v1 (gate-ON deferred).

**Phase 0 record (2026-07-14):** the design.md "Adopt-GC dependency" section, every present-tense
"no filter" assertion, and every `internal/api/adopted_entries.go` line citation in `design.md` were
re-pointed to HEAD (master `c7e2534b`) and now assert the RESOLVED state (both `Depends-on` adopt-GC
edges FIXED by #531/#532; `reapAdoptProvenanceRow` HAS the `(state, UpdatedAt)` identity gate at
`:946-978`/`:954`; the transition-window reap is CLOSED). Docs-only — no design decision, claim count
(still 19), scope, or architecture changed. The round-4 `withConfigLock`→`withConfigReadLock`
supersession marker and the shared-owner-#3 scope note (the `.snapshot` secret-bearing half already
landed via #532; only the `state_read_caps.go` read-cap line remains) were also folded in.
