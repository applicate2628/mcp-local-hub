# Adopt-side `classifyDeadAdoptingRow` is blind under gate-ON — can reap a committed adopt's snapshots

- **status:** open
- **severity:** high (data loss: destroys pre-adopt snapshots de-adopt needs)
- **filed:** 2026-07-11
- **context:** adjacent-finding (surfaced by the de-adopt multi-model design review; memo F6)
- **owner:** unassigned (adopt-side; NOT de-adopt v1 scope)

## Symptom

On a gate-ON host, a committed-but-unflipped adopt provenance row (state `adopting`,
`Install` committed but the `adopting → adopted` flip crashed) can be misclassified as a
pre-install crash orphan (`CRASH_REAP`) by the shipped `classifyDeadAdoptingRow`
(`internal/api/adopted_entries.go:441-468`). The adopt GC / capture-UPSERT then destroys the
row + its secret-bearing snapshots — the exact snapshots a later de-adopt needs to restore
the pre-adopt config.

## Root cause

`classifyDeadAdoptingRow` decides "Install committed" by asking whether any `adopt_client`
has a LIVE per-server hub entry that matches the manifest binding (via
`liveEntryMatchesManifestBinding`, `managed_entries.go:355`; `adopted_entries.go:450-467`).
But under gate-ON, `BuildHubReconcilePlan` has REMOVED every per-(server,client) entry
(`internal/api/install_hub_reconcile.go:233-263`) and replaced them with a single
`mcphub-hub` aggregate. So `GetEntry(SourceEntryName)` returns nil for every client, the
committed signal reads false, and the row classifies `CRASH_REAP`.

## Impact

The committed adopt's snapshots are reaped; de-adopt of that server then fails-closed (no
snapshot), and the pre-adopt native config is unrecoverable. Requires: gate-ON host + an
adopt whose promote-flip crashed after Install committed (a narrow but real crash window).

## Fix direction (not scoped here)

The committed signal must be gate-ON-aware: under gate-ON, "Install committed" ⟺ the
`mcphub-hub` aggregate entry is present for the client AND the manifest binding is live in
the resolver, NOT the per-server entry. The gate-ON-aware recognizer should feed BOTH
`classifyDeadAdoptingRow` and gate-ON de-adopt (the de-adopt follow-up
`work-items/backlog/2026-07-11-deadopt-subset-and-gate-on-followup.md`).

## Why not fixed in de-adopt v1

De-adopt v1 refuses gate-ON entirely (decision
`work-items/decisions/2026-07-11-deadopt-v1-all-clients-only-scope.md`), so v1 never depends
on gate-ON snapshots. But this adopt-side bug exists independently and can destroy snapshots
before any de-adopt runs, so it is filed for the adopt owner. Do NOT patch it from the
de-adopt work-item (it is adopt-side crash-consistency, a protected surface).
