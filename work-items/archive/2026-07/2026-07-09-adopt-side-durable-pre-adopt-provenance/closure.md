# Closure — adopt-side durable pre-adopt provenance

Closed: 2026-07-10

## Outcome

Durable, adopt-scoped, per-entry pre-adopt provenance shipped in PR #528, merged to
master as squash commit `16dba601` ("feat(api): durable pre-adopt provenance (prereq
for de-adopt)"), Codex Cloud bot PASS on round 5. The change is additive and
non-destructive: `ExecuteAdoptWithOpts` captures a durable `adopting` provenance row
plus a pinned, hardened, non-prunable whole-config-file snapshot per `present` client
BEFORE the first irreversible adopt mutation (`persistAdoptRoutedSecrets`), flips it to
`adopted` only after `Install` returns success, and aborts (row + snapshots) inside the
existing adopt failure-cleanup. A capture-step failure fails closed with zero side
effects, so a currently-successful adopt is not regressed. This **unblocks the de-adopt
work-item** (`2026-07-09-deadopt-hub-to-native`), which declared this item as its
`Depends-on:` target.

Store: a NEW adopt-owned `<state-dir>/adopted-entries.json` (schema v1, dedicated
`adopted-entries.lock` flock) — NOT a schema-extension of `managed-entries.json` — plus
an owner-only snapshot dir `<state-dir>/adopt-provenance/<manifest>/` written through
the hardened `WriteStateFileBytesAtomic` pipeline. Both governing decisions are
`status: accepted`:
`work-items/decisions/2026-07-10-adopt-provenance-store-shape.md` and
`work-items/decisions/2026-07-10-adopt-provenance-crash-consistency-model.md`.

Delivered surfaces: `internal/api/adopted_entries.go` (store + schema + capture/promote/
abort/GC + snapshot + lease helpers + the single `classifyDeadAdoptingRow` classifier),
`internal/api/adopt_provenance_events.go` (observability events), the additive capture
seam in `internal/api/adopt.go`, `internal/clients/entry_bytes.go` (the
`EntryBytesChecker` snapshot-byte-validation capability), and the CLAUDE.md
"Adopt provenance" narrative. De-adopt-owned mutators are declared in the schema but
NOT stub-landed (arch F7 scope boundary honored).

The converged crash-consistency model (its central architectural payload) is three
orthogonal signals, each single-owned, so the capture path and the GC can never
diverge: (1) a per-manifest `flock` LEASE held capture→promote as the owner-liveness
authority; (2) a hub-binding-live committed signal via the reused
`liveEntryMatchesManifestBinding` owner (replacing manifest-existence); (3) ROW-FIRST
capture ordering plus a snapshot-dir-driven GC backstop, so no crash window leaves a
secret-bearing snapshot dir with no reclaiming row.

As part of this closure, `design.md` was reconciled to the code as MERGED (the r2
addendum captured an intermediate model that r3+r4 refined). The reconciled deltas,
consolidated in the new design.md "## Reconciliation to merged code (as shipped #528)"
section and corrected in-place at each affected spot:
- **r3 (findings A+B):** `classifyDeadAdoptingRow` reads NO manifest file — it derives
  the expected hub binding from the row's IMMUTABLE `manifest_name` + captured `port`
  (so an operator editing/deleting the manifest after a committed adopt can't make the
  committed row reapable); any client-construct/read uncertainty is KEEP. This
  superseded the r2 pseudocode's `manifestExistsIn` / `load manifest` / `bindingFor`
  steps.
- **r4 (finding 1):** `original_state:"present-merged-lower"` is keyed EXCLUSIVELY on
  the adapter field `clients.MCPEntry.SourceBelowWriteTarget`, NOT on a `ConfigPath()`
  ENOENT; a `ConfigPath()` ENOENT for a write-target entry is now a fail-closed capture
  failure. Superseded the r2 MiMoCode `os.ReadFile(ConfigPath())`-`fs.ErrNotExist`
  keying.
- **r4 (finding 3):** a `present` client's snapshot bytes are byte-validated via
  `clients.EntryBytesChecker.EntryPresentInBytes` before pinning (closes the
  delete-then-recreate double-TOCTOU). New mechanism not present in r2.

## Residual risk (non-blocking; tracked)

All three are recorded in
`work-items/backlog/2026-07-10-adopt-provenance-lease-hygiene.md`:

- **(a) `<manifest>.lease` created outside the hardened DACL pipeline (security P3).**
  The per-manifest lease is created with a plain `flock.New` (`adopted_entries.go:375`),
  not the owner-only `WriteStateFileBytesAtomic` path. Bounded to DoS-only: the lease
  file carries NO content (nothing to read/tamper), so it is not a secret-exposure hole;
  a broadened-parent co-resident could only interfere with the flock. Mitigated by
  `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` (the standard strict parent-gate posture).
- **(b) Lease-file accumulation is cosmetic.** One zero-byte `<manifest>.lease` lingers
  per adopted manifest (the snapshot-dir `RemoveAll` deliberately spares the sibling
  lease); harmless, could be swept by a future GC pass.
- **(c) `present-merged-lower` clients are not counted in the capture event body
  (observability P3).** `emitAdoptProvenanceCaptured` counts only `present`/`absent`
  into `present_count`/`absent_count`; a merged-lower client shows in the `clients`
  name array but neither count. Observability-only; the record itself is correct.

The classifier's per-client `GetEntry`-error → KEEP posture (the other security P3
raised) was already implemented as shipped (`adopted_entries.go:457`), so it is NOT an
open residual.

## Archive location

`work-items/archive/2026-07/2026-07-09-adopt-side-durable-pre-adopt-provenance/`
(brief.md, research.md, design.md, plan.md, status.md, closure.md). Both decision
records stay at their canonical `work-items/decisions/` paths, `status: accepted`.

## Retrospective

The load-bearing lesson: **a crash-consistent, concurrent, mutation-robust SECRET store
is deep, and the point-fix → edge-mine loop cannot close it.** Round 1's per-edge
guards (all using `operation_state` + manifest-existence as a proxy for a row's true
condition) kept revealing the next edge across the bot's r2 review — the classic
edge-mine. The loop only broke when the design pivoted to a coherent architect-designed
model (three orthogonal signals + one classifier) instead of a fourth guard. Even then,
r3 and r4 found that the intermediate model's committed-signal still read mutable state
(the manifest file, the write-target read) that an operator or a racing edit could
falsify — resolved by deriving the classifier purely from immutable row fields and the
adapter's authoritative `SourceBelowWriteTarget`, and by byte-validating the exact
snapshot bytes.

Across 5 bot rounds the Codex gate caught 16 real bugs (r1: 3 committed-row bugs; r2: 6
crash/race edge-mine findings; r3: 4 mutation-immune-classifier fixes; r4: 3) that the
architecture review PASS + security review + per-phase gates did NOT surface. For this
class — a durable, concurrent, secret-bearing store with a crash lifecycle — the bot
gate is load-bearing, not redundant with internal review; the internal gates verified
implementation-within-framing while the bot repeatedly falsified the framing's premises.

Hygiene lesson: the merged code const set + the reconciliation greps — not the r2 design
addendum — were the authoritative source for this closure's design reconciliation, and
the doc's own documented surgical-edit-stale-text failure mode required grepping each
changed concept across the whole design.md rather than fixing only the first occurrence.
