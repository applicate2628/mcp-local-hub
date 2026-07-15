# Bug: Native-reverted committed adopt can be reaped as an unmutated crash orphan

- id: 2026-07-12-adopt-reap-native-revert-deletes-committed-provenance
- context: standalone
- status: fixed
- severity: high -> low (data-loss class closed; bookkeeping residual remains)
- area: internal/api/adopted_entries.go
- found-by: qa-engineer
- resolved-by: reap-predicate rewrite (write-target-physical entry-shape via ClassifyEntryUnderLock)

## Original report (against the superseded native-shape probe)

Reproduction by the (then-current) control flow:

1. A committed adopt remains in `operation_state: adopting`.
2. Demigrate restores the pre-hub native stdio entry from backup (`internal/api/demigrate.go:174-180`, `internal/api/demigrate.go:224-244`).
3. The manifest is then deleted, so `classifyDeadAdoptingRow` sees neither an exact hub binding nor a manifest and returns `adoptRowCrashReap` (`internal/api/adopted_entries.go:475-510`).
4. The old `adoptLiveEntryNativeStdioProbeFn` mapped the restored native entry to `adoptStdioProbeNative`, and `adoptRowProvablyUnmutated` accepted that native SHAPE as proof that Install never ran.
5. Both the GC and capture-UPSERT lanes could then delete/replace the committed row and its de-adopt snapshots.

Expected: a committed row is never reaped merely because its current entry has returned to native shape.

Actual (old probe): current native SHAPE was treated as historical proof that Install never committed, although native shape is reversible and demigrate deliberately restores it — and the probe accepted ANY native-shape entry, not one byte-equal to the pinned snapshot, so it could reap when the live entry differed from the snapshot => real secret/config-spelling loss.

## Resolution — the data-loss class is closed by the entry-shape predicate

The reap predicate was rewritten (fix consolidating filed items A/C/present-merged-lower)
to classify each recorded client through `ClassifyEntryUnderLock` against the physical
write target under the config read lock, instead of the shape probe:

- **`present` clients now reap only on `ClassifyRestoreDone`**, which requires
  `reflect.DeepEqual(liveWriteTargetEntrySubtree, pinnedNativeSnapshotEntrySubtree)`
  (`internal/clients/cas_mutator.go:352-354`). The pinned snapshot is therefore deleted
  ONLY when its exact entry content already survives, byte-identical, in the live config
  — **zero restore value at risk**. Sol's committed-then-reverted scenario still reaps,
  but only when the reverted native equals the pinned native (the original spelling is
  already live); a differing native spelling classifies `GenuineConflict` and the present
  branch KEEPS. The old "any native shape => reap" data-loss path no longer exists.
- **`absent` / `present-merged-lower` clients** pin no snapshot by construction, so a reap
  removes only the row + empty snapshot dir — no secret was ever at risk there.

Verified: fable value-at-risk analysis (present reap ⟺ live==snapshot => redundant) +
Sol/Terra concurrency review; adjudicated by `$lead` 2026-07-15. `go build`/`vet`/`-race`
green (adopt provenance + reap suites).

## Residual (severity low, bookkeeping only — accepted)

A committed-then-reverted `adopting` row CAN still be reaped (row + snapshot removed) in
the exact-equality subclass. Because the reap **never deletes the row's routed vault keys**
(`internal/api/adopted_entries.go` GC comment: routed-key cleanup is owned by de-adopt's
hash-gated `--reclaim-crashed`), the worst case is a BOOKKEEPING residual: the orphan row's
`<M>_<ENV>` vault keys linger in the owner-only vault until de-adopt or the operator removes
them. Owner-only, bounded, already the documented reap residual class (same as the
`-preserved` GC-reclaim path). Not a lost secret/config spelling. No further action required
for v1.

## Terms and Abbreviations

- GC: garbage collection.
- P1: high-severity data-loss risk.
- UPSERT: insert a new row or replace an existing row with the same key.
- CAS: compare-and-swap (the de-adopt write-target classify/mutate seam).
