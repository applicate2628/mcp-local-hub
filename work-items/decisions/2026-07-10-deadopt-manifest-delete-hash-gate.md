---
status: accepted
date: 2026-07-10
accepted: 2026-07-10 ($architecture-reviewer gate on the revised de-adopt design)
slug: deadopt-manifest-delete-hash-gate
deciders: $architect (design); $architecture-reviewer (promoted proposed → accepted)
context: work-items/active/2026-07-09-deadopt-hub-to-native/design.md
supersedes: none
superseded-by: none
---

# Decision: the last-binding manifest delete is hash-gated at the mutation point via a new shared `ManifestDeleteInWithHash` (plus the F-B gate-ON aggregate-prune shared-owner change)

`work-items/active/2026-07-09-deadopt-hub-to-native/review.md:88` (prerequisite 3)
and finding **F1** REQUIRE expected-hash protection on the last-binding manifest
delete, checked "at the mutation point". This entry decides HOW.

## Context

De-adopt deletes the adopt-created manifest when the last client binding is removed.
The shipped adopt provenance record carries `AdoptManifestHash` +
`ExpectedManifestHash`, both populated at capture (`internal/api/adopted_entries.go:159-160`),
precisely so de-adopt can gate the delete. But the current delete path
`ManifestDeleteIn` (`internal/api/manifest.go:788-801`) does only
`checkManifestName` + `os.Stat` + `os.RemoveAll` — NO hash gate. The edit path
`ManifestEditInWithHash` (`manifest.go:708-758`) already gates atomically:
re-reads the on-disk file and refuses with `ErrManifestHashMismatch` (`:717-721`) when
the content moved. F1's failure: a de-adopt plan reads a matching hash, an operator
edits the manifest before execute, and de-adopt then deletes the externally-edited
manifest.

## Options

- **A — new shared `ManifestDeleteInWithHash(dir, name, expectedHash)` on `manifest.go`.**
  Re-reads the on-disk manifest inside the call and refuses on `ErrManifestHashMismatch`
  before `os.RemoveAll`, exactly mirroring `ManifestEditInWithHash`. The check and the
  delete are one call — atomic at the mutation point.
- **B — de-adopt-local read-then-check, then call the existing `ManifestDeleteIn`.**
  De-adopt reads the manifest, computes `ManifestHashContent`, compares to
  `ExpectedManifestHash`, then calls `ManifestDeleteIn`. No shared-owner change.
- **C — no gate; rely on the plan-time check only.** Rejected outright by F1.

## Decision

**Option A.** The gate belongs at the delete mutation point, in the single manifest
owner, symmetric with the existing hash-gated edit — not re-implemented in the
de-adopt caller.

## Rationale

- **Atomicity at the mutation point (F1's explicit requirement).** Option B has a
  TOCTOU window: between de-adopt's read+compare and `ManifestDeleteIn`'s own
  `os.Stat`+`os.RemoveAll` an operator edit can slip in. Option A re-reads and gates
  inside the same call, so the manifest cannot change between the check and the delete.
- **Single owner.** `manifest.go` already owns the hash-gate idiom
  (`ManifestEditInWithHash`, `ErrManifestHashMismatch`). Adding the delete-side gate to
  the same owner keeps ONE hash-gate contract, not a second gate hand-rolled in
  `deadopt.go`. Any future caller wanting a safe hash-gated delete reuses it.
- **Small, additive shared change.** `ManifestDeleteInWithHash` is additive: the
  existing `ManifestDeleteIn` is unchanged (its non-gated callers keep working), and
  the new function is the gated variant. It does not perturb the delete for any current
  caller.

## Consequences

- One additive function in the shared manifest owner (`internal/api/manifest.go`);
  `internal/api/deadopt.go` calls it with the provenance row's `ExpectedManifestHash`.
- Because both provenance hashes are populated at capture, even a committed-but-`adopting`
  row supplies a usable gate hash.
- **(a) Fail-closed-on-empty polarity (security P2-a).** `ManifestDeleteInWithHash`
  INVERTS `ManifestEditInWithHash`'s skip-on-empty (`manifest.go:717-721`): an
  empty/absent `expectedHash` is a FAIL-CLOSED refusal to delete (destructive-default
  polarity — safe path is don't-delete). Inheriting the edit path's skip would re-open
  the exact F1 data-loss (a blanked/tampered `ExpectedManifestHash` → ungated delete of
  an externally-edited manifest).
- **(b) Retains the path-escape guard.** `ManifestDeleteInWithHash` MUST keep
  `ManifestDeleteIn`'s traversal defense (`manifest.go:793-796`, `Dir(target) ==
  Clean(dir)`); a gated variant that drops it is a security regression.
- **(c) F-B — a SECOND shared-owner change is tracked here.** De-adopt's gate-ON
  zero-binding aggregate prune EXTENDS the single reconcile owner `BuildHubReconcilePlan`
  (`internal/api/install_hub_reconcile.go`) so a client dropping to zero bindings gets
  its `mcphub-hub` aggregate removed under gate-ON (today only the gate-OFF sweep does
  it, `:164-180`; the gate-ON per-client loop skips zero-binding clients, `:181-185`).
  This is an EXTENSION of the single owner (it holds the reserved-entry-name
  `hubReconcileAggregateEntryName`), NOT a de-adopt-local `mcphub-hub` remove that would
  duplicate that knowledge. Recorded here rather than as a separate file because it is
  the same de-adopt shared-owner-change class; a future split into its own decision is
  fine if it grows.
- Enforcement: de-adopt test T2 (build plan → edit manifest → execute last-binding
  de-adopt → assert `ErrManifestHashMismatch`, no delete); an empty-hash-refusal test;
  a traversal test for the escape guard; the gate-ON T4 test for the prune.

## Scope note

De-adopt introduces THREE additive shared-owner changes, all additive (existing callers
unchanged): (1) `ManifestDeleteInWithHash` on `manifest.go` (this decision); (2) the
gate-ON zero-binding aggregate prune in `BuildHubReconcilePlan` (consequence (c) above);
and (3) a bytes-input restore variant `RestoreEntryFromBytesForRollbackWithConfigWriter`
on the `clients` adapters (security P1 single-read — recorded inline in `design.md`, not
here, as it is an additive sibling to existing per-adapter restore methods). The de-adopt
operation-state ordering + resume contract and the `/g/` orphaned-group policy are
single-work-item decisions and stay inline in `design.md`.
