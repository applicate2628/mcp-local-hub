# Bug: Present-merged-lower crash orphan remains permanently unreapable

- id: 2026-07-12-adopt-reap-present-merged-lower-permanent-keep
- context: standalone
- status: open
- severity: medium
- area: internal/api/adopted_entries.go
- found-by: qa-engineer

Reproduction by the current control flow:

1. Capture records a MiMoCode entry resolved from a lower layer as `present-merged-lower` with no snapshot (`internal/api/adopted_entries.go:728-744`; fixtures at `internal/api/adopt_provenance_r2_test.go:270-315`).
2. The process crashes before ManifestCreate/Install, leaving the lower-layer native entry unchanged and the manifest absent.
3. `classifyDeadAdoptingRow` therefore returns `adoptRowCrashReap`.
4. `adoptRowProvablyUnmutated` sends every non-`absent`, non-`present` state to the default KEEP branch (`internal/api/adopted_entries.go:1056-1070`).

Expected: the stated snapshotless-client P2 is resolved, including the existing snapshotless `present-merged-lower` state.

Actual: the row can never satisfy the reap predicate, so both garbage collection and same-manifest capture-UPSERT keep/refuse it indefinitely. The canonical entry-shape proposal explicitly says absent and present-merged-lower clients must not block (`work-items/backlog/2026-07-12-adopt-provenance-reap-predicate-native-entry-and-forget.md:47-48`).

Simply skipping this state needs a P1 safety analysis: after a committed upper-layer hub entry is removed, the lower native entry re-emerges, so current shape alone is not a monotonic commit signal.

## Terms and Abbreviations

- GC: garbage collection.
- P1: high-severity data-loss risk.
- P2: medium-severity permanent-block or unreclaimed-state risk.
- UPSERT: insert a new row or replace an existing row with the same key.
