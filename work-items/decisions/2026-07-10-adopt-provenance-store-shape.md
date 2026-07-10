---
status: proposed
date: 2026-07-10
slug: adopt-provenance-store-shape
deciders: $architect (design); $architecture-reviewer (promotes proposed → accepted)
context: work-items/active/2026-07-09-adopt-side-durable-pre-adopt-provenance/design.md
supersedes: none
superseded-by: none
---

# Decision: adopt pre-adopt provenance is a NEW `<state-dir>/adopted-entries.json` store, not a schema-extension of `managed-entries.json`

`work-items/active/2026-07-09-deadopt-hub-to-native/review.md:77-82` and
`:91` REQUIRE an accepted decision on this before any de-adopt code. This
entry decides it. Promotion `proposed → accepted` is the
`$architecture-reviewer` gate's call on the design that references this file.

## Context

De-adopt cannot restore a client's pre-adopt config because adopt persists no
durable, adopt-scoped, per-entry provenance. The only durable trace today is an
unlabelled, prunable, whole-config-file install backup that adopt neither pins
nor records (research.md:80-83; overwrite at `internal/api/install.go:2689`).
The store must carry the full reconstruction contract at
`work-items/active/2026-07-09-deadopt-hub-to-native/design.md:69-77` (manifest
name/hashes, per-client present/absent + pinned restore artifact + shapes,
routed secret keys, and an `adopting → adopted → de_adopting → closed`
operation-state machine).

Two candidate homes were weighed: (A) a new adopt-owned state file
`<state-dir>/adopted-entries.json`; (B) a schema-compatible extension of the
existing `<state-dir>/managed-entries.json`
(`internal/api/managed_entries.go`).

## Decision

**Adopt writes a NEW `<state-dir>/adopted-entries.json`**, single-owned by the
adopt API pipeline, built on the *storage template* of `managed_entries.go`
(hardened state-file write + a dedicated `adopted-entries.lock` flock + an
integer schema version) — the template is a pattern to copy, not a file to
share. Schema and lifecycle are specified in the design (`design.md`).

`managed-entries.json` is NOT extended. Its `ManagedEntry` struct
(`managed_entries.go:120-130`) and its schema version
(`managed_entries.go:73-75`) stay byte-unchanged; its readers/writers
(`RecordManagedEntry :181-204`, `IsManagedEntry :249-266`,
`ForgetManagedEntry :213-232`, `backfillMarkerIfEntryMatchesManifest
:312-335`) are protected surfaces.

Separately (orthogonal, additive): adopt SHOULD also call
`RecordManagedEntry(client, server)` per adopted client after a successful
Install (mirroring `migrate.go:287-305`), so the demigrate ownership marker
stays consistent with the migrate/serena paths. De-adopt's *restoration* relies
solely on `adopted-entries.json`, never on the marker. This tuple-recording is a
single-work-item detail decided inline in `design.md`, not part of THIS
store-shape decision.

## Rejected alternative — extend `managed-entries.json` (loses)

1. **Security-sensitive blast radius for zero reuse gain.** Adding
   reconstruction fields (snapshots, hashes, operation state, routed keys) to
   `ManagedEntry` forces a `managedEntriesSchemaVersion` bump
   (`managed_entries.go:75`), which makes EVERY existing reader re-verify the
   new shape — including demigrate's fail-closed ownership gate
   (`IsManagedEntry`), the removal path (`ForgetManagedEntry`), and the
   v0.4.x-upgrade backfill (`backfillMarkerIfEntryMatchesManifest`). Those are
   the exact paths a prior URL-heuristic data-loss bug was reverted around
   (`managed_entries.go:4-19`). Widening a data-loss-critical marker for a
   feature it does not serve is disproportionate change surface.

2. **Semantic + lifecycle mismatch.** `managed-entries.json` is a lightweight
   *positive-ownership marker* — one `(client, server, installed_at)` tuple,
   documented as carrying "zero reconstruction data" (research Q3;
   `managed_entries.go:1-44`). Its lifecycle is add-on-install /
   forget-on-demigrate. Adopt provenance is a *reconstruction record* whose
   lifecycle is a four-state machine `adopting → adopted → de_adopting →
   closed` keyed by manifest, plus secret-bearing pinned snapshots. Coupling the
   two means a demigrate `ForgetManagedEntry` could evict a row de-adopt still
   needs, or an adopt-provenance close could disturb a marker demigrate owns —
   a split-owner hazard on a shared file.

3. **No existing coupling to preserve — extension would CREATE one.** Adopt does
   NOT currently write `managed-entries.json` at all (research Q2;
   `adopt.go:204-253` ends at Install + event, no marker step). So "reuse the
   same file" reuses nothing; it manufactures a new adopt↔demigrate coupling
   where none exists, against change-surface minimization and
   single-owner-per-invariant.

4. **Independent schema evolution.** A separate file lets the provenance schema
   version independently of the migrate/demigrate marker. Bumping one never
   forces re-verifying the other.

The only thing option B buys is "one fewer state file" — and state files are
cheap and the hardened template is already established (`managed-entries.json`,
`dismissed.json`, `supervisor-*.json`, `hub-mcp.endpoint.json`). That does not
outweigh the coupling, blast-radius, and lifecycle costs above.

## Blast radius

New file `internal/api/adopted_entries.go` (store + schema + capture/promote/
abort + snapshot helpers) and additive edits to `internal/api/adopt.go`
`ExecuteAdoptWithOpts` (`:211-253`). New durable surfaces:
`<state-dir>/adopted-entries.json` (+ `.lock`) and a
`<state-dir>/adopt-provenance/<manifest>/` snapshot dir. Zero change to
`install.go`, `managed_entries.go`, `demigrate.go`, `migrate.go`, the client
backup lane (`clients.go`), or the manifest/secret code. No change to adopt
CLI/API/GUI request or response shapes.

## Gate

`$architecture-reviewer` promotes `proposed → accepted` on the design that
references this decision. Until then it is a proposed decision the planner and
implementers CONSUME (they may flag a conflict as REVISE-to-architect, but may
not redefine the store shape).
