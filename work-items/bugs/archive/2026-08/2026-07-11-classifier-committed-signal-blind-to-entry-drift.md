# Bug: adopt classifier's committed-signal is blind to entry drift → committed-but-unflipped `adopting` row destroyed by routine ops

Status: fixed
Fixed: 2026-07-13 — the P1 committed-signal-blind class was closed at HEAD by PR #532
(`e545c06e`, "capture-UPSERT reap gains the Part-2 unmutated gate"), architect-adjudicated
(Sol PASS) + security-re-verified + Codex-bot PASS (decision
`2026-07-10-adopt-provenance-crash-consistency-model.md` addendum 2026-07-12). Verified at
HEAD `92552562` by a security-engineer ground-truth pass (2026-07-13):
- **Signal 2b (manifest-exists → KEEP):** `classifyDeadAdoptingRow` now returns
  `adoptRowCommittedKeep` when the adopt-created manifest still exists or cannot be stat'd
  (`adopted_entries.go:507-509`, single owner `adoptManifestExistsFn` :435-441; fail-closed).
- **Positive-crash-evidence gate before REAP, BOTH lanes:** `adoptRowProvablyUnmutated`
  (:1015-1034) wired into the GC crash-reap lane and the capture-UPSERT lane (:585) — REAP
  only if every present-at-capture client's live config still whole-file-sha-matches its
  snapshot; any unprovable client → KEEP.
- Drift table: all three scenarios the root-cause names (gate-ON `mcphub-hub` aggregate,
  manifest port-edit+reinstall, `mcphub uninstall`) now classify to KEEP — each protected by
  TWO independent gates (Signal 2b + the sha gate). Verified `mcphub uninstall` has NO
  ManifestDelete, so Signal 2b fires. Named regression tests present
  (`adopt_classifier_committed_signal_test.go`, 9 tests).
This SATISFIES de-adopt's second `Depends-on` edge (design claim 10 committed-KEEP
recoverability). The residual over-reap (case-3 revert-to-exact-bytes + manifest-delete) and
the merged-lower over-block are ACCEPTED/deferred residuals (decision addendum), NOT this P1,
and do not arise under the named drift scenarios. Split out below: **P3-1** (`.lease`
namespace collision → latent split-lease P1) and **P3-3** (silent GC reap-failure) were
subsequently CLOSED by #537 (master `c7e2534b`, "reserve .lease manifest suffix + GC
reap-failure events") — verified at HEAD: P3-1 = the `.lease` suffix is reserved/refused in
`adopted_entries.go` (`adoptManifestLeaseSuffix` :74, reservation :258, CheckManifestName
parity :368); P3-3 = a `adopt-provenance-reap-failed{phase:…}` audit event now fires on a GC
reap/removeSnapshots failure (:1010/:1016/:1074). Neither remains open.
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

Terminal-at: 2026-08-08T22:58:13Z
Resolution: Pre-V1 terminal status `fixed` is preserved during operator-authorized V1 physical migration.
Evidence: Historical terminal time is unknown; preserved pre-V1 input SHA-256 `518ec3fce60bf578dc6a20858161c63457ce04af9f97b52036398dcb8eba1589`; original terminal status `fixed`; explicit operator-authorized V1 migration.
V1-Migration-Evidence: Historical terminal time is unknown; preserved pre-V1 input SHA-256 `518ec3fce60bf578dc6a20858161c63457ce04af9f97b52036398dcb8eba1589`; original terminal status `fixed`; explicit operator-authorized V1 migration.
