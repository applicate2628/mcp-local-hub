# Bug: adopt GC Phase 2 reaps from a STALE Phase-1 row copy → can destroy a committed `adopted` row + secret snapshots

Status: open
Filed: 2026-07-11
Severity: P1 (silent, unrecoverable data destruction of provenance)
Source: fable-5 hidden-bug hunt on delivered adopt-provenance code (#528)
Blocks: de-adopt (2026-07-09-deadopt-hub-to-native) — de-adopt sits on this GC

## Root cause

`gcOrphanedAdoptingProvenance` (`internal/api/adopted_entries.go`):
- **Phase 1** snapshots candidate rows under the store lock, then RELEASES it (:908-926).
- **Phase 2** `TryLock`s the per-manifest lease, then classifies `c.rec` — the **stale
  Phase-1 copy** — and reaps (:929-941). It never re-reads the row under the held lease.
- `reapAdoptProvenanceRow` (:860-882) drops **every** row matching the manifest NAME —
  no `OperationState` filter, no `UpdatedAt` identity check (:869-875) — then `RemoveAll`s
  the snapshot dir.
- Phase 3 DOES re-confirm rowless-ness under the lease (:956-971); Phase 2 lacks the
  equivalent re-validation.

## Failure interleave (inputs → wrong outcome)

1. A stale orphan row `R_old(M, adopting, >24h, port P1)` exists (prior adopt of M crashed
   pre-install).
2. Adopt-A of some manifest starts; its step-0a GC (`adopt.go:264`) Phase 1 snapshots
   `R_old` as a candidate; store lock released.
3. Adopt-B of manifest **M** (the operator's natural retry) runs to COMPLETION inside A's
   Phase-1→Phase-2 gap: UPSERT reaps `R_old`, captures `R_new`, Install writes hub entries
   on a **new** port `P2 ≠ P1` (`pickNextFreeAdoptPort`, `adopt.go:178-179` — easily
   differs 24h later), promote → `adopted`, lease released.
4. Adopt-A's Phase 2 reaches candidate M: `TryLock` succeeds (B finished).
   `classifyDeadAdoptingRow(R_old)` reconstructs the expected binding from the stale **P1**;
   live entries carry **P2** → no match → `adoptRowCrashReap` → `reapAdoptProvenanceRow(M)`
   deletes `R_new` (a committed `adopted` row) + B's fresh snapshot dir.

Window is narrow (B's full lifecycle must fit inside A's Phase-1→Phase-2 gap — widenable by
many candidates / slow config reads / AV stalls) but the outcome is silent, unrecoverable
destruction of exactly the artifact this feature preserves, logged only as a routine
`adopt-provenance-orphan-reaped`.

## De-adopt impact: DIRECT

De-adopt on M later reports "no provenance"; the original entry spelling (incl. secret
literals) is gone.

## Fix

In Phase 2, after acquiring the lease, RE-READ the row under `withAdoptedEntriesLock` and
require (row still exists ∧ `OperationState == adopting` ∧ `UpdatedAt == candidate.UpdatedAt`
∧ still older than cutoff) before classify+reap — mirroring Phase 3's under-lease re-check.
Defense-in-depth: give `reapAdoptProvenanceRow` an expected `(state, UpdatedAt)` identity and
make it a no-op on mismatch.
