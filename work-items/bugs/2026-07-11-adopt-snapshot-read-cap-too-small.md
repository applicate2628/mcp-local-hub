# Adopt-provenance `.snapshot` read cap (1 MiB default) is too small for a real client config

- **status:** open
- **severity:** medium (a legitimate large client-config snapshot fails the anchored read → de-adopt cannot restore that client)
- **filed:** 2026-07-11
- **context:** adjacent-finding (surfaced by the de-adopt design rework; memo item 4 under-specified this)
- **owner:** de-adopt work-item (fix lands with de-adopt; state_read_caps.go owner)

## Symptom

De-adopt reads a `present` client's pinned snapshot through the anchored secure reader
`ReadStateFileInodeAnchored` (`internal/api/state_read_inode_anchor.go:22`), which enforces a
size cap resolved by `stateFileReadCapBytes` (`internal/api/state_read_caps.go:28-42`). A
`<client>.snapshot` basename matches no special case there, so it falls to the `default`
branch → `maxStateFileBytes` = 1 MiB (`:11`). A real client config (e.g. a large
`~/.claude.json` with many servers + unrelated top-level keys) can exceed 1 MiB, so the
anchored read of a legitimately-large snapshot would FAIL with "file size exceeds cap".

## Root cause

The adopt CAPTURE side pins the snapshot via `WriteStateFileBytesAtomic`
(`internal/api/adopted_entries.go:272-289`) with NO size limit — it writes whatever the live
client config is. But the READ-back cap owner (`stateFileReadCapBytes`) has no `.snapshot`
kind, so it applies the small 1 MiB hub-state ceiling. Capture and read-back disagree on the
allowed size.

## Fix direction

Add a snapshot cap kind to `stateFileReadCapBytes`: a client-config-sized, bounded ceiling
(e.g. `maxVaultBlobFileBytes` / `maxIntentFileBytes` = 16 MiB, both already defined at
`state_read_caps.go:15,22`) for `.snapshot` files under `adopt-provenance/`. This keeps OOM
protection (bounded) while allowing legitimate large client configs. Land it together with the
`isSecretBearingStateFilePath` `.snapshot` addition (both are the same two-line
`state_read_*` owner touch the de-adopt design declares).

## Why filed as an adjacent finding

The de-adopt multi-model memo (item 4) embraced the anchored read's size cap as a FEATURE
(OOM protection) but did not note that the DEFAULT cap is below a real client-config size. It
is an additive read-cap OWNER extension (NOT a provenance-store patch); de-adopt authors it as
part of its snapshot-read path.
