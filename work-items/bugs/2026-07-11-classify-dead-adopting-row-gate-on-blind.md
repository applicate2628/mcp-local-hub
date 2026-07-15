# Adopt-side `classifyDeadAdoptingRow` is blind under gate-ON — can reap a committed adopt's snapshots

- **status:** DATA-LOSS CLOSED by #551 (residual = classifier imprecision, non-data-loss)
- **severity:** high -> low (the data-loss path is closed; the classifier is still imprecise)
- **filed:** 2026-07-11
- **context:** adjacent-finding (surfaced by the de-adopt multi-model design review; memo F6)
- **owner:** unassigned (adopt-side; NOT de-adopt v1 scope)

> **DATA-LOSS CLOSED 2026-07-16 by #551 (verified).** The reap that this bug feared is gated
> TWICE: `classifyDeadAdoptingRow` (Signal 2) → `adoptRowProvablyUnmutatedFn` (the #551
> write-target entry-shape predicate). This bug is about the FIRST gate misfiring; the SECOND
> gate (#551) now catches it. `adoptRowProvablyUnmutated` classifies the live physical
> `SourceEntryName` entry: under gate-ON that per-server entry is ABSENT (replaced by the
> `mcphub-hub` aggregate), and for a PRESENT client (the only client kind that pins a secret
> snapshot) an absent live entry with a non-nil snapshot is `GenuineConflict`, NOT
> `ClassifyRestoreDone` → the predicate returns false → KEEP. So even when the classifier
> misclassifies a gate-ON committed row `CRASH_REAP` (manifest also deleted, so Signal-2b can't
> save it), the snapshots are NOT destroyed. `absent`/`present-merged-lower` clients pin no
> snapshot, so a reap there loses nothing. Regression guard:
> `TestAdoptGcGateOnCommittedManifestAbsentKeeps` (`internal/api/adopt_gate_on_reap_test.go`)
> reproduces the exact worst case (gate-ON aggregate + manifest absent + present client) and
> asserts the row + snapshots survive while confirming the classifier still emits `CRASH_REAP`.
>
> **Residual (LOW, non-data-loss):** `classifyDeadAdoptingRow` is still gate-ON-BLIND — it
> reports `CRASH_REAP` for a committed gate-ON row when it should report `adoptRowCommittedKeep`.
> No data is lost (the predicate keeps the row), but the classifier is imprecise. The
> gate-ON-aware committed recognizer (aggregate present + manifest binding live in the resolver)
> is the SAME building block the gate-ON de-adopt lane needs, so the classifier-correctness fix
> is folded into the de-adopt-v2 gate-ON follow-up
> (`work-items/backlog/2026-07-11-deadopt-subset-and-gate-on-followup.md`) rather than done as a
> standalone now. This bug is DOWNGRADED to that follow-up's precondition; the data-loss
> emergency is over.

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
