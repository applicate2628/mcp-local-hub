# Bug: adopt classifier's committed-signal is blind to entry drift → committed-but-unflipped `adopting` row destroyed by routine ops

Status: open
Filed: 2026-07-11
Severity: P1 (silent, unrecoverable destruction of a "recoverable" provenance row)
Source: fable-5 hidden-bug hunt on delivered adopt-provenance code (#528)
Blocks: de-adopt (promote-failure recoverability contract, design claim 10)

## Root cause

`classifyDeadAdoptingRow` (`internal/api/adopted_entries.go:441-468`) has exactly ONE
committed signal: "some adopt_client currently holds the expected per-server hub entry AT
THE CAPTURED PORT" (:462-464 via `liveEntryMatchesManifestBinding`,
`managed_entries.go:355-408`). The finding-A comment (:433-435) promises an operator "port
change, binding removal" after a committed adopt must NOT let the row's provenance be reaped —
but the earlier fix only stopped reading the manifest FILE; the LIVE ENTRY drifting was left
open.

## Precondition (design-sanctioned)

The row is committed-but-stuck-`adopting` — a promote-flip write failure (`adopt.go:331-338`,
explicitly non-fatal + "recoverable") or a hard crash between Install success and promote.

## Then any of these routine operations erases the committed signal

- `mcphub install --reconcile-hub-mode` gate-ON: removes every per-(server,client) entry,
  replacing with the `mcphub-hub` aggregate (`install_hub_reconcile.go:244-263` — the gate-ON
  blindness class; adopt's own Install never writes the aggregate form).
- A manifest **port edit + reinstall**: entries rewritten to the new port → captured-port
  mismatch → no match (exactly the scenario the finding-A comment claims is protected).
- `mcphub uninstall <server>` / demigrate: entry removed / restored to direct-stdio.

≥24h later, any adopt's step-0a GC reads every adopt_client cleanly, finds no matching entry
→ `adoptRowCrashReap` → row + secret snapshots destroyed. (The capture-UPSERT lane :532-534
fails toward KEEP/refuse; the GC lane fails toward DESTRUCTION.)

## De-adopt impact: DIRECT

Destroys the "recoverable `adopting` row" the promote-failure contract (de-adopt design
claim 10) depends on.

## Fix (needs a design pass — route through security-engineer)

Add non-live-entry committed signals to the GC's KEEP side — e.g. "the adopt-created manifest
file still exists on disk" as a KEEP hint, and/or require positive crash evidence (the
ORIGINAL direct-stdio entry still present in the source client) before REAP; keep UPSERT
semantics separate so operator retries aren't blocked. Also emit a distinct event when GC
reaps a row whose manifest still exists (audit visibility). Related: the de-adopt design's
gate-ON F6 note (`work-items/bugs/2026-07-11-classify-dead-adopting-row-gate-on-blind.md`) is
the same class from the de-adopt side — consolidate.

## Related lower-severity findings from the same hunt (fold in or file separately)

- **P2-2:** adopt `<client>.snapshot` files are NOT matched by `isSecretBearingStateFilePath`
  (`state_read_inode_anchor.go:42-57`) despite holding literal secret env values, so a
  read-broadened snapshot warns-and-proceeds in default-relax instead of refusing like
  `secrets.age`. Add a `.snapshot`-suffix / `adopt-provenance`-dir rule.
- **P3-1 (`.lease` namespace collision):** a manifest named `<x>.lease` makes its snapshot
  DIR path equal manifest `<x>`'s lease FILE path; a failing adopt of `<x>.lease` →
  `removeAdoptSnapshots` → `RemoveAll` unlinks `<x>`'s HELD lease file → split lease ownership.
  One-line fix: refuse `.lease`-suffixed manifest names in `adoptSnapshotDir`/lease-path.
- **P3-3:** GC emits nothing when reap/removeSnapshots fails → a stuck secret-bearing orphan
  has zero operator signal. Emit a warn event.
