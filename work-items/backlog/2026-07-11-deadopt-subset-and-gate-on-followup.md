---
status: candidate
type: feature-followup
severity: P2
date: 2026-07-11
origin: de-adopt v1 scope cut (decision 2026-07-11-deadopt-v1-all-clients-only-scope)
context: work-items/active/2026-07-09-deadopt-hub-to-native/
---

# De-adopt follow-up — subset de-adopt + gate-ON de-adopt + de_adopting recovery/GC

v1 de-adopt is ALL-CLIENTS-ONLY and gate-OFF-only (decision
`work-items/decisions/2026-07-11-deadopt-v1-all-clients-only-scope.md`). This stub tracks
the three deferred pieces: (A) subset de-adopt, (B) gate-ON de-adopt, and (C) `de_adopting`
recovery / GC (the de-adopt design's G7 residual). NOT admitted for delivery —
`$product-manager` admits them if demand appears. Per-client detach and gate-ON detach have
TODAY-workarounds (Servers-matrix uncheck → `/api/demigrate`, and "gate OFF first, then
de-adopt"); the G7 residual has a manual today-workaround too (the operator deletes the
owner-only snapshot dir by hand).

## A. Subset de-adopt (de-adopt some-but-not-all clients of one adopt-owned manifest)

Requires, over the shipped provenance store:

- A JOURNALED target set (the row must record which clients a resumed subset de-adopt is
  operating on — v1 relies on `targets ≡ AdoptClients`, which subset breaks).
- Per-client snapshot pruning (the whole-dir `removeAdoptSnapshots`
  `internal/api/adopted_entries.go` cannot drop one client's `<client>.snapshot`; needs a
  per-file delete that keeps the manifest snapshot dir).
- The declared-but-unused 4th mutator + a `de_adopting → adopted` reverse transition (a
  subset de-adopt leaves the manifest alive with fewer bindings, so the row returns to
  `adopted`, not deleted).
- `UpdateAdoptExpectedManifestHash` + the `ManifestEditInWithHash` edit lane (the manifest is
  EDITED to drop the target clients' bindings, not deleted).
- The subset `/g/` branch (a group naming the server still routes it while other clients
  remain).

## B. Gate-ON de-adopt

Requires:

- A gate-ON expected per-client state model (expected = "no per-server entry + `mcphub-hub`
  aggregate present + manifest binding live in the resolver"), because gate-ON reconcile has
  removed every per-server entry (`internal/api/install_hub_reconcile.go:233-263`), so the
  gate-OFF recognizer path (`GetEntry(SourceEntryName)` → the hub per-server entry) does not
  apply.
- The gate-ON zero-binding aggregate PRUNE — extend `BuildHubReconcilePlan` so a client
  dropping to zero bindings gets its `mcphub-hub` removed under gate-ON (today the gate-OFF
  sweep at `:164-180` does it; the gate-ON per-client loop `continue`s zero-binding clients
  at `:181-185`). This is the shared-owner change the multi-model memo called F-B/#6. Tracked
  independently as a latent bug: `work-items/bugs/2026-07-11-hub-reconcile-gate-on-zero-binding-stale-aggregate.md`.
- A single-owner hub-entry renderer (extract `internal/api/install.go:2679-2687`) IF the
  gate-ON path needs to CONSTRUCT expected aggregate/entries (v1's gate-OFF recognizer-only
  path does not).
- The adopt-side `classifyDeadAdoptingRow` gate-ON fix
  (`work-items/bugs/2026-07-11-classify-dead-adopting-row-gate-on-blind.md`) so a
  committed-but-unflipped `adopting` row on a gate-ON host is not reaped out from under
  de-adopt.

## C. `de_adopting` recovery / GC (de-adopt design G7 residual)

v1 de-adopt roll-forward leaves a recoverable `de_adopting` row on a mid-execute crash. If the
operator NEVER retries (or a client is permanently neither restorable — live≠hub — nor
accept-eligible — its `present` snapshot is permanently unreadable, so the P1-a
snapshot-read-failure rejection applies), that row is ABANDONED and:

- **WEDGES re-adopt** of that manifest — capture refuses ANY non-`adopting` prior row
  (`internal/api/adopted_entries.go:529-531`), so the manifest can never be re-adopted until the
  row is cleared.
- **RETAINS the secret-bearing snapshot dir** — no adopt-side GC reaps it: the cross-manifest GC
  Phase-2 reaps only `adopting` rows, and Phase-3 reaps only ROWLESS snapshot dirs; a
  `de_adopting` row is neither, so its snapshots persist indefinitely.

This is structurally identical to the `closed`-tombstone wedge the v1 rework eliminated, but on
the abandoned-retry path. It is bounded (owner-only snapshot DACL — a co-resident cannot read the
content) and operator-driven (the operator can delete the snapshot dir + row by hand today), so
v1 does NOT ship automated recovery. Requires, over the shipped store:

- **A `mcphub de-adopt --recover <server>` command** (or `de-adopt --abandon`) that, under the
  `<manifest>.lease`, drives an abandoned `de_adopting` row to a clean terminal state: either
  finish the roll-forward (retry E3→E6) or, on operator confirmation, discard the row + snapshots
  (`reapAdoptProvenanceRow`-style, snapshots-first) so re-adopt is unwedged. Must surface which
  clients are still Failed and why (still-hub / unreadable-snapshot / genuine-conflict).
- **A `de_adopting`-aware GC** — extend the adopt-side cross-manifest GC (or add a de-adopt-owned
  pass) to reap a `de_adopting` row whose `updated_at` is older than a threshold, mirroring the
  `gcOrphanedAdoptingProvenance` 24h posture (never a fresh row, never mid-operation under the
  lease). This is the piece that stops abandoned secret-bearing snapshot dirs from accumulating.

Neither is in v1 (the design defers both here); the v1 failure table covers only the RETRIED
resume path.

## Why backlog, not a work-item

All three are bounded, have TODAY-workarounds, and v1 delivers the common inverse-adopt case
(gate-OFF, all clients). Promote to a work-item on demand.
