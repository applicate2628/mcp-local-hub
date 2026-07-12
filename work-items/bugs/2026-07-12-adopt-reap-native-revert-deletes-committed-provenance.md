# Bug: Native-reverted committed adopt can be reaped as an unmutated crash orphan

- id: 2026-07-12-adopt-reap-native-revert-deletes-committed-provenance
- context: standalone
- status: open
- severity: high
- area: internal/api/adopted_entries.go
- found-by: qa-engineer

Reproduction by the current control flow:

1. A committed adopt remains in `operation_state: adopting`.
2. Demigrate restores the pre-hub native stdio entry from backup (`internal/api/demigrate.go:174-180`, `internal/api/demigrate.go:224-244`).
3. The manifest is then deleted, so `classifyDeadAdoptingRow` sees neither an exact hub binding nor a manifest and returns `adoptRowCrashReap` (`internal/api/adopted_entries.go:475-510`).
4. `adoptLiveEntryNativeStdioProbeFn` maps the restored native entry to `adoptStdioProbeNative`, and `adoptRowProvablyUnmutated` accepts that as proof that Install never ran (`internal/api/adopted_entries.go:1007-1018`, `internal/api/adopted_entries.go:1056-1072`).
5. Both the garbage-collection and capture-UPSERT lanes may then delete/replace the committed row and its de-adopt snapshots (`internal/api/adopted_entries.go:582-608`, `internal/api/adopted_entries.go:1194-1205`).

Expected: a committed row is never reaped merely because its current entry has returned to native shape.

Actual: current native shape is treated as historical proof that Install never committed, although native shape is reversible and demigrate deliberately restores it.

This violates the case-5 P1 invariant. The accepted crash-consistency decision already states that a committed adopt whose config was reverted is indistinguishable from a pre-Install crash (`work-items/decisions/2026-07-10-adopt-provenance-crash-consistency-model.md:90-99`).

## Terms and Abbreviations

- GC: garbage collection.
- P1: high-severity data-loss risk.
- UPSERT: insert a new row or replace an existing row with the same key.
