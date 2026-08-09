---
status: candidate
type: hygiene
severity: P3
date: 2026-07-10
origin: adopt-provenance delivery (#528) closure — non-blocking security P3 + observability follow-ups
context: work-items/archive/2026-07/2026-07-09-adopt-side-durable-pre-adopt-provenance/
---

# Adopt-provenance follow-ups — lease-file hygiene + present-merged-lower event count

Three non-blocking follow-ups carried out of the shipped adopt-provenance work
(`work-items/archive/2026-07/2026-07-09-adopt-side-durable-pre-adopt-provenance/`,
delivered PR #528 squash `16dba601`). None blocks de-adopt; recorded so they are
not lost.

## (a) `<manifest>.lease` file is created outside the hardened DACL pipeline (security P3)

The per-manifest adopt lease `<state-dir>/adopt-provenance/<manifest>.lease`
(design r2 Signal 1) is created with a plain `flock.New(leasePath)` at
`internal/api/adopted_entries.go:375` (`tryAcquireAdoptManifestLease`), NOT through
the hardened `WriteStateFileBytesAtomic` owner-only-DACL pipeline that the
secret-bearing snapshots use (`adopted_entries.go:282`).

- **Bounded impact — DoS-only, no content exposure.** The lease file carries NO
  content (it is a pure flock handle); there is nothing to read or tamper. On a
  broadened-parent host a co-resident with `FILE_DELETE_CHILD` on the parent could
  delete/replace the directory entry and interfere with the flock (a denial of the
  serialization guarantee), but cannot read a secret from it (there is none).
- **Existing mitigation.** `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` enforces the strict
  parent-dir gate, which removes any co-resident namespace right on the parent — the
  same posture documented for every other state file.
- **Fix (if pursued).** Route the lease-file create through a handle-hardened create
  helper (or accept the DoS-only residual explicitly in the design). Low priority
  given no content exposure + the existing strict-gate mitigation.

## (b) Lease-file accumulation is cosmetic

Each distinct adopted manifest leaves a `<manifest>.lease` file in
`<state-dir>/adopt-provenance/`; nothing prunes them (the `removeAdoptSnapshots`
`RemoveAll` deliberately targets the `<manifest>/` snapshot dir, not the sibling
`.lease` file). Bounded (one per adopted manifest), zero-byte, and harmless — a
future GC pass could sweep leases whose manifest+row are gone. Cosmetic only.

## (c) `present-merged-lower` clients are not counted in the capture event body (observability P3)

`emitAdoptProvenanceCaptured` (`internal/api/adopt_provenance_events.go:72-83`)
switches only on `AdoptOriginalStatePresent` and `AdoptOriginalStateAbsent` when
building `present_count` / `absent_count`. A `present-merged-lower` client (the r4
MiMoCode lower-layer state) falls through both cases — it appears in the `clients`
name array but is counted in neither `present_count` nor `absent_count`, so an
operator reading the `adopt-provenance-captured` event cannot see how many clients
were merged-lower. Observability-only; the record itself is correct. Fix: add a
`present_merged_lower_count` (or fold into a per-state count map) in the emit body.

## Why backlog, not a work-item

All three are non-blocking, bounded, and documented residuals of a shipped feature.
De-adopt (`work-items/active/2026-07-09-deadopt-hub-to-native/`) does not depend on
any of them. Promote to a work-item only if a real incident or the de-adopt lane
surfaces a concrete need.
