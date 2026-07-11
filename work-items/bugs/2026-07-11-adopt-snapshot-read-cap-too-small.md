# Adopt-provenance `.snapshot` cap asymmetry — capture is uncapped, read-back defaults to 1 MiB

- **status:** open
- **severity:** medium (a legitimate large client-config snapshot fails the anchored read → de-adopt cannot restore that client)
- **filed:** 2026-07-11
- **updated:** 2026-07-11 (round-3 delta-check: widened from a read-cap-only raise to the capture==restore SYMMETRY the de-adopt design B6 claims)
- **context:** adjacent-finding (surfaced by the de-adopt design rework; memo item 4 under-specified this)
- **owner:** de-adopt work-item authors the READ cap (`state_read_caps.go`); the CAPTURE cap is adopt-side (`writeAdoptClientSnapshot`, a protected surface in de-adopt v1) — see "Fix direction"

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

The fix has TWO halves and must enforce the SYMMETRY invariant, not merely raise the read cap.
Raising only the read cap leaves the asymmetry live in the other direction (a future capture
could still pin a snapshot larger than the read cap), so the de-adopt design (B6, and its claim
17) requires **capture cap == restore cap**.

1. **Read side (de-adopt-owned).** Add a snapshot cap kind to `stateFileReadCapBytes`
   (`internal/api/state_read_caps.go:28-42`): a client-config-sized, bounded ceiling
   (`maxIntentFileBytes` / `maxVaultBlobFileBytes` = 16 MiB, both already defined at
   `state_read_caps.go:15,22`) for `.snapshot` files under `adopt-provenance/`. NOTE: the switch
   keys on `filepath.Base(path)` and a `<client>.snapshot` basename is VARIABLE, so this MUST be a
   `strings.HasSuffix(base, ".snapshot")` / `adopt-provenance` path-segment clause, NOT a new
   `case` label. Land it together with the `isSecretBearingStateFilePath` `.snapshot` addition
   (both are the same `state_read_*` owner touch the de-adopt design declares).

2. **Capture side (adopt-owned — the SYMMETRY half).** `writeAdoptClientSnapshot`
   (`internal/api/adopted_entries.go:272-290`) writes the snapshot via `WriteStateFileBytesAtomic`
   with NO size limit — verified: it passes `configBytes` straight through, so it will pin a config
   of ANY size. Add a capture-time size gate == the de-adopt restore cap (16 MiB): adopt REFUSES to
   pin a snapshot larger than de-adopt can later read back, so a config that adopts SUCCESSFULLY is
   ALWAYS de-adoptable (no "adopts fine, de-adopts as a permanently-Failed client" trap). This is
   the half the de-adopt design cannot land itself — `writeAdoptClientSnapshot` /
   `ExecuteAdoptWithOpts` are protected surfaces in de-adopt v1 — so it must be done here, on the
   adopt side, as this bug's fix.

Both caps MUST reference the same constant (single-owner) so a future bump moves them together and
the symmetry cannot drift.

## Why filed as an adjacent finding

The de-adopt multi-model memo (item 4) embraced the anchored read's size cap as a FEATURE
(OOM protection) but did not note that the DEFAULT read cap is below a real client-config size,
NOR that the capture side is uncapped. The READ half is an additive read-cap OWNER extension
(NOT a provenance-store patch) that de-adopt authors as part of its snapshot-read path; the
CAPTURE half touches the adopt capture lifecycle (`writeAdoptClientSnapshot`), a protected
surface in de-adopt v1, so it is tracked here rather than folded into the de-adopt change. The
de-adopt design carries the residual (a config > 16 MiB adopts but de-adopts as a Failed client)
until BOTH halves land.
