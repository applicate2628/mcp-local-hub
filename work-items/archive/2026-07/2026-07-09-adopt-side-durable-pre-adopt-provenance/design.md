# Design — adopt-side durable pre-adopt provenance

Role: $architect. Source of truth: `research.md` (PASS) + the CONSUMER contract
at `../2026-07-09-deadopt-hub-to-native/design.md:69-77` and
`../2026-07-09-deadopt-hub-to-native/review.md:77-82,86-91`. No implementation
code here. All `file:line` anchors verified on-disk at repo HEAD `ceb01c18`.

Store-shape decision (required prerequisite): filed as
`work-items/decisions/2026-07-10-adopt-provenance-store-shape.md`. The
`$architecture-reviewer` gate on this design PROMOTED it `proposed → accepted`
(no store-shape defect found). This design assumes that decision: **a new
`<state-dir>/adopted-entries.json`**. (The decision file's `status:` frontmatter
is the reviewer's/archivist's to finalize — it is not edited by this revision per
the coordinator's instruction; see the report's flag on the lag.)

## Revision (2026-07-10) — arch + security review fold-in

Reviews: ARCHITECTURE **PASS** (store-shape promoted `proposed → accepted`);
SECURITY **REVISE** (0 P0/P1; core posture verified correct). This revision folds
in the 6 converged MUST-FIX items:

- **F1 (hash timing):** BOTH manifest hashes are now populated in the `adopting`
  row AT CAPTURE (pre-`adopt.go:218`) from `plan.ManifestYAML`; `promote` only
  flips `operation_state`. A committed-but-`adopting` row can no longer carry an
  empty `expected_manifest_hash` (which would make de-adopt's hash-gate SKIP —
  `manifest.go:717` — and risk deleting an externally-edited manifest).
- **F2 == P2-2 (orphan lifecycle):** `captureAdoptProvenance` is an UPSERT keyed
  by `manifest_name` (removes a prior orphan row + snapshot dir first); a named
  bounded GC owns hard-crash orphans. See "Orphan lifecycle + upsert".
- **P2-1 (fail-closed hash gate):** `snapshot_sha256` is now a FAIL-CLOSED restore
  gate de-adopt MUST honor (refuse restore on mismatch OR missing snapshot),
  symmetric with the shape check. See "Consumer-contract handoff".
- **F3 (single-owner):** `expected_hub_shape` is **DROPPED**. De-adopt reuses the
  existing `liveEntryMatchesManifestBinding` owner (`managed_entries.go:355-408`)
  after the hash-gate — no second shape-derivation path.
- **F4 (fail-closed classify):** a `GetEntry`/config-read error at capture is a
  CAPTURE FAILURE (abort, zero side effects), never a guessed `absent`.
- **F5 (honesty):** `snapshot_sha256` granularity is documented as WHOLE-FILE
  (trips on sibling-entry edits too).

SHOULD-TRACK items (P3-1 minimal-entry snapshot follow-up, P3-2 shared-secret
scan handoff, F6 decision reason-1 softening, F7 no-stub planning note) are
recorded in "Follow-ups & planning notes". Code `file:line` anchors remain
verified against code state at `ceb01c18` (unchanged by this docs revision).

## Change-Surface Contract

- **Intended change surface:**
  - NEW file `internal/api/adopted_entries.go` — the provenance store (schema +
    flock + read/write + capture/promote/abort + snapshot helpers), modeled on
    `internal/api/managed_entries.go`.
  - `internal/api/adopt.go` `ExecuteAdoptWithOpts` (`:211-253`) — three additive
    hooks: (1) capture **before** `persistAdoptRoutedSecrets` at `:218`; (2)
    abort folded into the existing failure-cleanup at `:237-248`; (3) promote
    after `Install` returns success, before `emitAdoptExecutedEvent` at `:250`.
  - Additive provenance events on the existing `supervisor-events.log` `adopt`
    source (`adopt.go:527-553` is the emit precedent).
- **Approved extension seam(s):**
  - S1 — the pre-mutation insertion point in `ExecuteAdoptWithOpts` (immediately
    before `adopt.go:218`). Capture is a self-contained read-then-write step
    that depends on nothing the later mutations produce.
  - S2 — the existing adopt failure-cleanup block (`adopt.go:237-248`) gains an
    `abortAdoptProvenance` call, symmetric with the existing manifest/secret
    cleanup.
  - S3 — the storage template: `writeHubMcpStateFile`/`readHubMcpStateFile`
    (`hub_mcp_state.go:79,166`) + a dedicated flock, exactly as
    `managed_entries.go:99-167` composes them.
  - S4 — the hardened snapshot writer `WriteStateFileBytesAtomic`
    (`state_file_helper.go:86`) for the pinned artifact.
  - S5 — the RESTORE extraction `clients.RestoreEntryFromBackupForRollbackWithConfigWriter`
    (`clients.go:353-362`), reused verbatim by the downstream de-adopt consumer
    pointed at our pinned snapshot instead of a timestamped backup. (Read-only
    reuse; not modified by this item.)
- **Protected / must-not-touch surfaces:**
  - `internal/api/install.go` per-client block (`:2632-2710`) and its shared
    rollback contract (`:2702-2708`). Capture stays in the adopt owner (research
    seam option (a)); it is NOT threaded through Install (option (b) rejected),
    so Install's rollback list and multi-caller contract are untouched.
  - `internal/api/managed_entries.go` — `ManagedEntry` struct (`:120-130`),
    schema version (`:73-75`), and demigrate readers (`IsManagedEntry :249-266`,
    `ForgetManagedEntry :213-232`, `backfillMarkerIfEntryMatchesManifest
    :312-335`). Not extended (per the store-shape decision).
  - The client backup lane — `writeBackup`/`pruneOldTimestamped`/`BackupKeep`
    (`clients.go:1021-1051,1145-1191`). Not modified; the pinned artifact lives
    in adopt-owned storage OUTSIDE the prunable set, and we reuse only the
    restore-extraction helper.
  - `BuildAdoptPlan` (`adopt.go:126-202`) — stays side-effect-free; capture
    belongs to Execute, never Build.
  - `adopt_secret_route.go` (`rewriteAdoptSensitiveEnv`, `persistAdoptRoutedSecrets`)
    — unchanged; ordering unchanged.
- **Declared blast radius:** adopt Execute path + one new `internal/api` file +
  one new state file (`adopted-entries.json` + `.lock`) + one new snapshot dir
  (`adopt-provenance/<manifest>/`) + additive events. No behavioral change to a
  currently-successful adopt beyond an additive pre-`Install` capture step. Zero
  change to install / demigrate / migrate / managed-entries / backup-lane /
  manifest / secret code. No change to adopt CLI/API/GUI request or response
  shapes.

## Chosen approach

Capture a durable, adopt-scoped provenance **record per adopt-created manifest**
in `<state-dir>/adopted-entries.json`, plus a **pinned, hardened, non-prunable
whole-config-file snapshot per `present` client** in
`<state-dir>/adopt-provenance/<manifest>/<client>.snapshot`. Write the record in
state `adopting` (with snapshots) BEFORE the first irreversible adopt mutation;
flip it to `adopted` only after `Install` returns success; abort it (row +
snapshots) inside the existing adopt failure-cleanup. De-adopt (a later item)
reads the record and restores by pointing the existing
`RestoreEntryFromBackupForRollback` extraction at the pinned snapshot.

Why this shape:

- **The pinned snapshot is a copy of the LIVE client config taken before
  Install rewrites it** — so it preserves the pre-adopt entry exactly, in the
  client's native on-disk format, including original secret-literal spelling
  (the manifest's `secret:`-routing mutates only the in-memory manifest env at
  `adopt.go:172-176`, never the client config on disk before `AddEntry` at
  `install.go:2689`).
- **Restore is not re-invented.** De-adopt reuses
  `RestoreEntryFromBackupForRollbackWithConfigWriter(client, snapshotPath,
  entryName, writer)` (`clients.go:353-362`). That helper reads the file at the
  given path, extracts the named entry, and writes it into live config
  (removing it if absent) — pointing it at a pinned snapshot instead of a
  timestamped backup is a path swap, no new restore code.
- **Storage is not re-invented.** The record uses the proven
  `managed_entries.go` pattern (hardened state-file + flock + schema version);
  the snapshot uses the hardened `WriteStateFileBytesAtomic` pipeline.

### Alternatives considered

- **Alt-1 — thread a capture hook through `Install`'s per-client block so the
  `BackupKeep` at `install.go:2670` is pinned into provenance (research seam
  (b)).** Rejected: `Install` has multiple non-adopt callers and a shared
  rollback contract (`install.go:2702-2708`); adding an adopt-only capture hook
  perturbs a hot, shared, security-relevant path for every caller. Keeping
  capture in the adopt owner (Alt chosen) is strictly more local and leaves
  Install's contract byte-unchanged. Tradeoff: the adopt owner re-reads each
  client config (a second read after `BuildAdoptPlan`'s extraction) — cheap
  (small JSON/TOML files, one-shot operator action), and it buys a
  whole-file faithful snapshot the `BackupKeep` path never returns to adopt.
- **Alt-2 — minimal-entry snapshot (store only the adopted entry, not the whole
  config file).** Rejected for v1: the restore helper operates on a whole
  config-format file, so a minimal-entry snapshot would need a new per-adapter
  "single-entry config writer" + a new restore-from-blob path — more new code,
  more per-adapter surface, and it forfeits verbatim reuse of the proven restore
  extraction. It is NOT a "revisit if unacceptable" — it is a STANDING tracked
  follow-up (security P3-1) to reduce durable-plaintext-secret over-collection;
  the v1 whole-file mitigations (hardened owner-only DACL; delete on close AND
  abort; documented residual) are MANDATORY in the meantime. See "Follow-ups &
  planning notes".
- **Alt-3 — extend `managed-entries.json` (store-shape decision option B).**
  Rejected in `work-items/decisions/2026-07-10-adopt-provenance-store-shape.md`
  (schema-version bump on a data-loss-critical demigrate marker; lifecycle
  mismatch; manufactures a new adopt↔demigrate coupling).

## Provenance schema — `<state-dir>/adopted-entries.json`

Root file, schema version `1`, one record per adopt-created manifest:

```jsonc
{
  "version": 1,
  "records": [
    {
      "manifest_name":          "context7",          // = adopt manifest name
      "source_client":          "claude-code",
      "source_entry_name":      "context7",          // = manifest_name in adopt v1
      "port":                   9137,
      "adopt_clients":          ["claude-code", "codex-cli"],
      // BOTH hashes populated AT CAPTURE (pre-adopt.go:218) from plan.ManifestYAML
      // (verbatim-written by ManifestCreateIn, manifest.go:489; hashed over raw
      // bytes, manifest_hash.go:17). promote flips state only, never writes these.
      "adopt_manifest_hash":    "<sha256 of plan.ManifestYAML bytes; immutable>",
      "expected_manifest_hash": "<sha256; == adopt_manifest_hash at capture; de-adopt updates on a subset binding edit>",
      "routed_secret_keys":     ["CONTEXT7_CONTEXT7_API_KEY"],
      "operation_state":        "adopted",           // adopting|adopted|de_adopting|closed
      "created_at":             "2026-07-10T12:00:00Z",
      "updated_at":             "2026-07-10T12:00:01Z",
      "clients": [
        {
          "client":            "claude-code",
          "original_state":    "present",            // present|absent
          "restore_mode":      "functional-equivalent", // functional-equivalent|byte-equivalent
          "snapshot_ref":      "adopt-provenance/context7/claude-code.snapshot", // state-dir-relative; present-only
          // WHOLE-FILE sha256 of the pinned snapshot bytes (NOT entry-scoped — it
          // trips on unrelated sibling-entry edits too). De-adopt MUST recompute
          // and FAIL CLOSED on mismatch OR missing snapshot before restoring
          // (see "Consumer-contract handoff"). present-only.
          "snapshot_sha256":   "<whole-file sha256 of the pinned snapshot bytes>"
          // NOTE: expected_hub_shape is DROPPED (arch F3). De-adopt recomputes the
          // expected hub shape via the existing liveEntryMatchesManifestBinding
          // owner (managed_entries.go:355-408) AFTER the manifest hash-gate.
        },
        {
          "client":            "codex-cli",
          "original_state":    "absent",             // entryless-fanout client
          "restore_mode":      "n/a",
          "snapshot_ref":      "",
          "snapshot_sha256":   ""
        }
      ]
    }
  ]
}
```

### Field-by-field mapping to the consumer contract (`deadopt/design.md:69-77`)

| Consumer-required field (`deadopt/design.md`) | Schema field | Notes |
|---|---|---|
| `manifest_name` | `manifest_name` | Record key (unique; adopt v1 name == entry name, `adopt.go:139-141`). |
| source entry name | `source_entry_name` | |
| source client | `source_client` | |
| selected clients | `adopt_clients` | = `plan.AdoptClients` (`adopt.go:194`). |
| selected port | `port` | = `plan.Port`. |
| adopt-generated manifest hash | `adopt_manifest_hash` | Immutable. `ManifestHashContent` (SHA-256 of raw bytes, `manifest_hash.go:17`) of `plan.ManifestYAML`. **Populated at CAPTURE (pre-`adopt.go:218`)**, not at promote — `plan.ManifestYAML` is the exact byte string `ManifestCreateIn` writes verbatim (`manifest.go:489`), so the capture-time hash equals the on-disk manifest hash. |
| current expected hash (hash-gated edit/delete) | `expected_manifest_hash` | == `adopt_manifest_hash` at capture; **both hashes are on the `adopting` row from capture** (arch F1). De-adopt updates `expected_manifest_hash` after a subset binding edit so the next de-adopt hash-gate matches. Both feed `ManifestEditInWithHash`'s `ErrManifestHashMismatch` gate (`manifest.go:708-721`) and the last-binding-delete gate de-adopt adds (`manifest.go:774-801` has none today — review F1). Populating at capture (not promote) prevents a committed-but-`adopting` row from carrying an EMPTY hash that would make the gate SKIP (`manifest.go:717`). |
| per-client original state present/absent | `clients[].original_state` | `present` = a same-name pre-adopt entry existed; `absent` = entryless-fanout client (`adopt.go:183` `alsoPresent` / the fanout case pinned by `adopt_test.go:1510-1539`). A `GetEntry`/read error classifies as NEITHER — it is a capture failure (arch F4; see "Fail-closed capture seam"). |
| present → pinned backup ref or serialized adapter snapshot | `clients[].snapshot_ref` (+ `snapshot_sha256`) | Pinned whole-config-file snapshot; see "Pinned artifact". Non-prunable. |
| per-client original config-shape hash | `clients[].snapshot_sha256` | WHOLE-FILE SHA-256 of the pinned pre-adopt config bytes (granularity per arch F5 — trips on sibling-entry edits too). A FAIL-CLOSED restore gate, NOT decorative: de-adopt MUST recompute it and refuse restore on mismatch OR missing snapshot (security P2-1; see "Consumer-contract handoff"). |
| expected hub-managed live shape | *(no stored field — dropped, arch F3)* | De-adopt recomputes the expected hub shape from the manifest binding via the existing single owner `liveEntryMatchesManifestBinding` (`managed_entries.go:355-408`, the `demigrate.go:417-429` refusal path de-adopt already reuses, `deadopt/design.md:104`) AFTER the manifest hash-gate. Storing a separately-derived descriptor would create a second, driftable shape owner; the hash-gate already implies shape identity (hash-match ⇒ shape equals stored manifest; hash-mismatch ⇒ hash-gate fails first). |
| adopt-created routed secret keys | `routed_secret_keys` | = `plan.SecretRoutedKeys`; consumer deletes keys BEFORE forgetting provenance (review F2), enabled by keeping them in the record until `closed`. |
| operation state adopting→adopted→de_adopting→closed | `operation_state` | See lifecycle below. |
| restore artifact must be non-prunable / pinned | snapshot dir outside the backup-prune scope | See "Pinned artifact". |

### Operation-state machine

`adopting` (pending, written before `adopt.go:218`) → `adopted` (after
`Install` success) → `de_adopting` (de-adopt, later item) → `closed` (de-adopt
complete; row + snapshots deleted). This item OWNS the `adopting`, `adopted`,
and the abort→delete transitions. `de_adopting` and `closed` are DECLARED here
(the schema supports them) but IMPLEMENTED by the de-adopt item — this keeps
scope to adopt-side capture.

Recoverability: a crash between `Install` success and the `adopting → adopted`
flip leaves `adopting` + a live hub shape. That is a benign, recoverable state
(`deadopt/design.md:77`): de-adopt may treat `adopting` + matching live shape as
committed, and the flip is idempotent on retry. Because BOTH manifest hashes are
written at capture (F1), a committed-but-`adopting` row is fully hash-gate-usable
by de-adopt — it never carries an empty `expected_manifest_hash`. A crash BEFORE
`Install` (pending `adopting`, no live hub shape) is abortable cleanup — the
adopt failed, so the row + snapshots are orphan debris. Who removes that debris
is specified concretely below (no hand-wave "a bounded reconcile removes").

### Orphan lifecycle + upsert (arch F2 / security P2-2)

Two concrete owners, because a hard crash (SIGKILL / power-loss) between the
capture-write and abort/promote leaves a secret-bearing owner-only snapshot dir
with no in-process cleanup:

- **(a) Same-manifest re-run — UPSERT.** `captureAdoptProvenance` is an UPSERT
  keyed by `manifest_name`. On entry it FIRST removes any prior row for that
  manifest AND `RemoveAll`s its snapshot dir, THEN writes the fresh `adopting`
  row + snapshots. This cleans a pre-crash orphan on the operator's natural
  retry (re-running the same adopt) and guarantees at most one row per manifest
  — no duplicate/ambiguous rows. The copied `managed_entries.go:191-202`
  template already does in-place update; here it is made explicit AND
  snapshot-aware (delete the stale snapshot dir, not just the row). Required
  test: T-capture-upsert (below). NOTE: adopt-v1 requires `manifest_name` ==
  entry name and `ManifestCreate` rejects a pre-existing disk manifest
  (`adopt.go:139-152`), so a same-manifest re-run only reaches capture when the
  PRIOR attempt did not commit a manifest — i.e. exactly the orphan case the
  upsert must clean.
- **(b) Cross-manifest hard-crash orphan — bounded GC (named owner).** An
  `adopting` row + snapshot dir for manifest X that will never be retried (the
  operator moved on) is owned by a bounded GC: `gcOrphanedAdoptingProvenance`
  sweeps `<state-dir>/adopt-provenance/<m>/` whose row is `operation_state ==
  "adopting"` with `updated_at` older than a threshold (default 24h) AND no
  in-flight adopt for that manifest, deleting the row + snapshot dir. It runs at
  the START of every `ExecuteAdoptWithOpts` (the next adopt on the host reaps
  stale orphans — cheap, no new scheduler) and SHOULD also be wired at `mcphub
  supervise` startup (planner decision; a one-line call, not a new mechanism).
  This is the written contract that closes the residual: **until GC runs, an
  abandoned `adopting` adoption leaves an owner-only, secret-bearing snapshot
  under `<state-dir>/adopt-provenance/<manifest>/`** — bounded (owner-only DACL,
  co-resident cannot read content), and operator-removable by `RemoveAll` of
  that dir. This boundary is symmetric with the consumer's
  missing-manifest-plus-pending-provenance ⇒ abortable-cleanup contract
  (`deadopt/design.md:152`): a `de_adopting`/`adopting` row whose manifest is
  gone is debris, not a de-adopt candidate.

## Pinned artifact — non-prunable, hardened, secret-bearing

- **Location:** `<state-dir>/adopt-provenance/<manifest_name>/<client>.snapshot`
  — a byte copy of each `present` client's LIVE config file, captured before any
  adopt mutation. Directory-per-manifest so `closed`/abort deletes the whole dir
  in one `RemoveAll`.
- **Non-prunable by construction:** `pruneOldTimestamped` only scans siblings of
  the live config path whose names carry the `.bak-mcp-local-hub-` prefix
  (`clients.go:1145-1191`, `1152` `prefix := base + backupSuffixPrefix`). Our
  snapshot lives in a different directory (`<state-dir>/adopt-provenance/…`) and
  carries no backup prefix, so no `BackupKeep`/`PruneBackupsForBackupPath` pass
  can reach it. This directly closes the review's "Backup retention" blocking
  gap (`deadopt/review.md:59-67,87`) and research Q3 gap (a) / known-limit (iii).
- **Hardened write (secret-bearing):** written via `WriteStateFileBytesAtomic`
  (`state_file_helper.go:86`) — owner-only DACL installed handle-bound at
  temp-create, atomic temp+rename, per-file flock, strict/relax parent-gate
  posture + audit. The snapshot copies a config that may hold literal secret
  values in `env`, so it MUST use this pipeline, NOT the generic backup lane's
  plain `copyFile(…, 0600)` (`clients.go:1043,1114-1139`) which lacks the
  handle-bound DACL and parent-gate posture. This is a deliberate deviation:
  reuse the RESTORE mechanic from the backup lane, but OWN the storage under the
  hardened state-file posture.
- **Restore (consumed by de-adopt), fail-closed integrity gate first:** de-adopt
  MUST recompute the WHOLE-FILE `sha256` of the snapshot bytes and compare it to
  the stored `snapshot_sha256`, refusing the restore FAIL-CLOSED on **mismatch OR
  missing snapshot**, BEFORE calling
  `RestoreEntryFromBackupForRollbackWithConfigWriter(client, <abs snapshot path>,
  entryName, writer)` (`clients.go:353-362`). This is not decorative
  tamper-detection: on a default (non-strict) broadened-parent host a co-resident
  with `FILE_DELETE_CHILD` can swap ONLY the snapshot file for an attacker config
  carrying a malicious `command`/`url` — the same fail-closed posture the
  manifest hash-gate and shape recheck already grant. Once the gate passes, the
  snapshot is a valid whole config-format file, so the adapter parses it and
  extracts `entryName` exactly as it would from a timestamped backup. (The gate
  is a de-adopt implementation obligation; this design pins it as a contract in
  "Consumer-contract handoff".)

## Fail-closed capture seam — exact placement

Inside `ExecuteAdoptWithOpts` (`adopt.go:211-253`), the ordered mutations today
are `persistAdoptRoutedSecrets` (`:218`) → `ManifestCreate` (`:221`) → `Install`
(`:230`). New ordering:

```
ExecuteAdoptWithOpts(plan, w, opts):
  0a. [NEW GC — reap stale cross-manifest orphans] gcOrphanedAdoptingProvenance(24h)
  0b. [NEW capture — BEFORE :218]  (UPSERT keyed by manifest_name)
     rec, err := a.captureAdoptProvenance(plan)
        // UPSERT: first RemoveAll any prior row + snapshot dir for plan.ManifestName,
        // then for each selected client: read config, classify present/absent
        //   (a GetEntry/read ERROR is a CAPTURE FAILURE — never guessed "absent", F4),
        // pin hardened whole-file snapshots for present clients,
        // compute snapshot_sha256 (whole-file, F5),
        // compute BOTH manifest hashes = ManifestHashContent(plan.ManifestYAML) (F1),
        // write record with operation_state="adopting".  (no expected_hub_shape, F3)
     if err != nil {
         // FAIL CLOSED. Nothing irreversible has run yet: no vault key, no
         // manifest, no client-config write. A currently-successful adopt is
         // NOT regressed — capture failure aborts with ZERO side effects.
         return fmt.Errorf("adopt: capture pre-adopt provenance: %w", err)
     }
  1. persistAdoptRoutedSecrets(plan.secretValues)   // existing :218
        on error → abortAdoptProvenance(rec)  +  existing return  (:222-228)
  2. a.ManifestCreate(...)                            // existing :221
        on error → abortAdoptProvenance(rec)  +  existing secret cleanup (:222-228)
  3. a.Install(...)                                   // existing :230
        on error → abortAdoptProvenance(rec)  +  existing manifest/secret cleanup (:237-248)
  4. [NEW promote — AFTER :230 success, BEFORE :250]
     if err := promoteAdoptProvenanceToAdopted(rec.ManifestName); err != nil {
         // promote flips operation_state adopting→adopted ONLY; it writes no hashes
         // (both already on the row from capture, F1). It MAY re-verify the row's
         // adopt_manifest_hash against the now-on-disk manifest as a consistency
         // check. Install COMMITTED — do NOT roll back a successful adopt for a
         // flip-write failure. Emit a loud warn; leave state="adopting"
         // (recoverable, fully hash-gate-usable). Adopt still returns success.
         emitAdoptProvenanceCommitFailed(rec.ManifestName, err)
     }
  5. emitAdoptExecutedEvent(plan)                     // existing :250
```

Key placement rules:

- **The `adopting` row + all pinned snapshots are durable before `:218`.** This
  is the fail-closed invariant: the first irreversible mutation
  (`persistAdoptRoutedSecrets`, which writes vault keys) never runs until
  provenance is on disk.
- **Abort is folded into the existing cleanup, not a parallel path.** Each of
  the three existing failure branches (`:222-228`, `:237-248`) additionally
  calls `abortAdoptProvenance(rec)` (delete row + `RemoveAll` the snapshot dir).
  `abort` is idempotent and best-effort-logged; an abort failure appends to the
  existing operator error message (same shape as the existing
  secret/manifest-cleanup notes at `:226,239,244`) — it does not mask the
  original error.
- **Promote is the LAST durable write and never rolls back a committed adopt.**
  A flip-write failure downgrades to a recoverable `adopting` state, not an
  adopt failure.

Capture reads client configs via `clients.AllClients()[clientName]`
(`install.go:2630` uses the same map) → `client.ConfigPath()`
(`clients.go:136-138`) → read bytes → classify: a `present` client has a
same-name entry (`client.GetEntry(entryName)` returns non-nil, `clients.go:208-209`);
an `absent` client is a selected fanout target whose config parses cleanly and
has NO same-name entry.

**Fail-closed classification (arch F4).** `GetEntry` can return `(nil, err)` on a
corrupted lower layer (the same multi-layer read hazard `install.go:2649-2660`
already fails loud on). A `GetEntry` error — or any config-read/parse error — at
capture is a CAPTURE FAILURE: abort with zero side effects (the pre-`:218`
fail-closed return), NEVER a guessed `absent`. Guessing `absent` on an unreadable
config would skip a needed snapshot and later restore the entry to absence —
silent data loss. Only a config that parses cleanly AND lacks the entry is
`absent`.

## Handling the three research-flagged known limits

- **(i) Per-adapter byte-equivalence is UNVERIFIED (`deadopt/design.md:79`).**
  `RestoreEntryFromBackupForRollback` extracts the entry and re-serializes it via
  the adapter's write path — a parse→reserialize round-trip that is not proven
  byte-identical per adapter. So `restore_mode` defaults to
  **`functional-equivalent`** for every client (honest; matches
  `deadopt/design.md:76,140`). A per-adapter probe (a test that adopts, snapshots,
  restores, and asserts byte-equality of the restored entry) is a FOLLOW-UP that
  may upgrade specific adapters to `byte-equivalent`; until then no user-facing
  byte-equivalence promise is made. Filed as an adjacent finding below.
- **(ii) Secret literal spelling lost after `secret:` routing (`adopt.go:173`).**
  The pinned snapshot is a copy of the LIVE client config taken BEFORE `Install`
  rewrites it, and `rewriteAdoptSensitiveEnv` mutates only the in-memory manifest
  env clone (`adopt.go:172-176`), not the on-disk config. So the snapshot
  preserves the ORIGINAL literal `env` values verbatim. De-adopt reconstructs the
  original spelling from the snapshot (NOT from the manifest, which carries
  `secret:` refs). This closes limit (ii) for `present` clients. (A de-adopt
  `--reconstruct-legacy` path for rows with NO provenance still cannot recover
  spelling — but that is de-adopt's out-of-scope legacy escape hatch.)
- **(iii) Generic backup prunable / sentinel not adopt-scoped.** Solved by the
  provenance-owned pinned artifact above (own directory, own hardened writer,
  never touched by `pruneOldTimestamped`). The generic backup lane is left
  entirely unchanged.

## API-contract sketch (signatures only — no bodies)

New file `internal/api/adopted_entries.go` (mirrors `managed_entries.go`):

```go
// ---- schema types ----
type AdoptOperationState string // "adopting" | "adopted" | "de_adopting" | "closed"
type AdoptOriginalState  string // "present" | "absent"
type AdoptRestoreMode    string // "functional-equivalent" | "byte-equivalent" | "n/a"

// (AdoptExpectedHubShape DROPPED per arch F3 — de-adopt recomputes the expected
//  hub shape via the existing liveEntryMatchesManifestBinding owner.)

type AdoptClientProvenance struct {
    Client         string             `json:"client"`
    OriginalState  AdoptOriginalState `json:"original_state"`
    RestoreMode    AdoptRestoreMode   `json:"restore_mode"`
    SnapshotRef    string             `json:"snapshot_ref"`    // state-dir-relative; present-only
    SnapshotSHA256 string             `json:"snapshot_sha256"` // WHOLE-FILE hash; fail-closed restore gate; present-only
}

type AdoptProvenanceRecord struct {
    ManifestName         string                  `json:"manifest_name"`
    SourceClient         string                  `json:"source_client"`
    SourceEntryName      string                  `json:"source_entry_name"`
    Port                 int                     `json:"port"`
    AdoptClients         []string                `json:"adopt_clients"`
    AdoptManifestHash    string                  `json:"adopt_manifest_hash"`
    ExpectedManifestHash string                  `json:"expected_manifest_hash"`
    RoutedSecretKeys     []string                `json:"routed_secret_keys"`
    OperationState       AdoptOperationState     `json:"operation_state"`
    CreatedAt            time.Time               `json:"created_at"`
    UpdatedAt            time.Time               `json:"updated_at"`
    Clients              []AdoptClientProvenance `json:"clients"`
}

type AdoptedEntries struct {
    Version int                     `json:"version"`
    Records []AdoptProvenanceRecord `json:"records"`
}

// ---- storage (mirrors managed_entries.go:99-167) ----
func withAdoptedEntriesLock(fn func() error) error          // flock <state-dir>/adopted-entries.lock + in-proc mutex
func readAdoptedEntries() (*AdoptedEntries, error)          // readHubMcpStateFile; missing => empty{Version:1}
func writeAdoptedEntries(m *AdoptedEntries) error           // writeHubMcpStateFile

// ---- snapshot storage (hardened, non-prunable) ----
func adoptSnapshotDir(manifestName string) (string, error)                 // <state-dir>/adopt-provenance/<manifest>
func writeAdoptClientSnapshot(manifestName, client string, configBytes []byte) (ref, sha256Hex string, err error) // WriteStateFileBytesAtomic; whole-file sha256
func removeAdoptSnapshots(manifestName string) error                       // RemoveAll the manifest snapshot dir

// ---- adopt-side lifecycle (THIS item — FULL bodies, not stubs) ----
func (a *API) captureAdoptProvenance(plan *AdoptPlan) (*AdoptProvenanceRecord, error) // UPSERT by manifest_name; writes state="adopting" with BOTH hashes
func promoteAdoptProvenanceToAdopted(manifestName string) error                       // flip adopting->adopted ONLY (no hash write); idempotent
func abortAdoptProvenance(rec *AdoptProvenanceRecord) error                            // delete row + snapshots (idempotent)
func gcOrphanedAdoptingProvenance(olderThan time.Duration) error                      // reap stale cross-manifest adopting orphans (F2/P2-2)

// ---- read surface CONSUMED by de-adopt — IN SCOPE for THIS item (real body) ----
func ReadAdoptProvenance(manifestName string) (*AdoptProvenanceRecord, bool, error)

// ---- de-adopt-owned MUTATORS — declared for schema/contract shape ONLY.
//      THIS item must NOT land empty/stub bodies for these (anti-layering, arch F7);
//      the de-adopt item implements them against this schema. Listed here so the
//      planner scopes them to de-adopt, not to this item.
//   func MarkAdoptProvenanceDeAdopting(manifestName string) error        // adopted -> de_adopting
//   func UpdateAdoptExpectedManifestHash(manifestName, newHash string)   // subset binding edit
//   func CloseAdoptProvenance(manifestName string) error                 // de_adopting -> closed + delete snapshots
```

**Scope boundary (arch F7).** This item implements the storage layer, the
snapshot helpers, the adopt-side lifecycle (`captureAdoptProvenance`,
`promoteAdoptProvenanceToAdopted`, `abortAdoptProvenance`,
`gcOrphanedAdoptingProvenance`), and the read accessor `ReadAdoptProvenance`. The
three de-adopt-owned mutators are DECLARED (commented above) so the schema
supports them, but MUST NOT ship as empty/stub bodies in this item — the de-adopt
work-item authors them. Landing stubs would be anti-layering (a half-owned state
machine split across two items with no live second consumer).

## Dependency direction + stable contracts

- **Dependency direction (downward-only):** `internal/cli` / `internal/gui` →
  `internal/api` (`api.go:1-11` states CLI/GUI must call `internal/api`, not
  reach into clients/scheduler directly). The new store is inside `internal/api`
  and calls DOWN to `internal/clients` (config read, restore extraction) and to
  the state-file helpers. No upward edge; no new cross-package coupling.
- **Stable internal contract:** `adopted_entries.go`'s exported read/mutation
  surface (`ReadAdoptProvenance`, `MarkAdoptProvenanceDeAdopting`,
  `UpdateAdoptExpectedManifestHash`, `CloseAdoptProvenance`) is the contract
  de-adopt depends on. The JSON schema (version `1`) is the stable on-disk
  contract; a bump requires a read-side migration, isolated from
  `managed-entries.json`.
- **Stable external contract:** none changed. Adopt CLI/API/GUI request and
  response shapes are byte-identical (`api.ts:77-128`, `cli/adopt.go:13-57`,
  `gui/adopt.go:73-123`). Capture is backend-internal.
- **Cross-file consistency owner:** the adopt owner (`ExecuteAdoptWithOpts`)
  sequences the provenance write + snapshots + secret persist + manifest create
  + install, per `state_file_helper.go:68-71` ("multi-step state transitions
  that need to lock across files should serialize at a higher level"). Each
  individual state-file write is flock-atomic; the ORDER is owned by the adopt
  Execute path.

## Security-by-design

- **Snapshot is secret-bearing → hardened DACL, no downgrade.** The pinned
  snapshot copies a config that may hold literal `env` secrets, so it MUST go
  through `WriteStateFileBytesAtomic` (owner-only handle-bound DACL + parent-gate
  posture + audit, `state_file_helper.go:86-116`). It MUST NOT use the backup
  lane's plain 0600 copy. On `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` the strict
  parent-gate applies to the snapshot exactly as to other state files — do NOT
  weaken it.
- **Secret material lifecycle bound to the row.** BOTH `abort`
  (`abortAdoptProvenance`) AND `closed` (`CloseAdoptProvenance`, de-adopt) MUST
  `RemoveAll` the snapshot dir so pre-adopt secret literals do not linger past
  the operation, and the capture UPSERT `RemoveAll`s a stale same-manifest
  snapshot before re-pinning. Hard-crash orphans are reaped by
  `gcOrphanedAdoptingProvenance` (bounded residual — see "Orphan lifecycle +
  upsert"). The record's `routed_secret_keys` are NAMES only; secret VALUES live
  only in the vault and (transiently) in the owner-only snapshot.
- **Event bodies carry no secret values or config contents.** Provenance events
  log manifest names, client names, present/absent counts, snapshot PATHS, and
  key NAMES only — matching the existing `adopt-executed` body which logs
  `secret_routed_keys` names, never values (`adopt.go:537-551`). This is a
  redaction requirement, not an option.
- **Snapshot integrity is a FAIL-CLOSED restore gate (security P2-1), not
  decorative.** `snapshot_sha256` is a whole-file hash the CONSUMER (de-adopt)
  MUST recompute and refuse restore on mismatch OR missing snapshot BEFORE
  restoring — because on a default (non-strict) broadened-parent host a
  co-resident with `FILE_DELETE_CHILD` can swap ONLY the snapshot file for an
  attacker config (malicious `command`/`url`). Pinned as a contract in
  "Consumer-contract handoff".
- **Residual — whole-file over-collection (security P3-1, STANDING follow-up).**
  The whole-file snapshot copies sibling entries' secrets too, not just the
  adopted entry's. The three mitigations are MANDATORY, not best-effort:
  (1) hardened owner-only DACL via `WriteStateFileBytesAtomic`; (2) delete on
  close AND abort AND upsert-replace; (3) documented residual. The reduction to a
  minimal-entry snapshot (Alt-2) is tracked as a real follow-up work-item (see
  "Follow-ups & planning notes"), not a conditional "revisit".
- **Co-resident parent-swap residual:** identical to every other state file — a
  `FILE_DELETE_CHILD` co-resident can delete/replace the directory entry but
  cannot read the owner-only content; `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` is the
  documented mitigation. No new posture introduced.

## Consumer-contract handoff (statements de-adopt MUST/SHOULD honor)

These are contract obligations THIS design imposes on the de-adopt consumer; they
are pinned here so the boundary is written, not implicit. De-adopt implements
them.

- **MUST — fail-closed snapshot integrity gate (security P2-1).** Before
  restoring, de-adopt MUST recompute the whole-file `sha256` of the pinned
  snapshot and compare to the row's `snapshot_sha256`, refusing the restore
  FAIL-CLOSED on **mismatch OR missing snapshot** — symmetric with the manifest
  hash-gate and the shape recheck. (Enabled by the whole-file hash this item
  writes.)
- **MUST — recompute expected hub shape via the single owner (arch F3).** De-adopt
  MUST derive the expected live hub shape from the manifest binding via
  `liveEntryMatchesManifestBinding` (`managed_entries.go:355-408`) AFTER the
  manifest hash-gate, NOT from a stored descriptor (none is stored).
- **MUST — hash-gate the last-binding manifest delete (review F1).**
  `ManifestDeleteIn` has no expected-hash gate today (`manifest.go:788-801`);
  de-adopt MUST add one using `expected_manifest_hash`, matching
  `ManifestEditInWithHash` (`manifest.go:717-720`). This design supplies both
  hashes; the delete-path gate is de-adopt's code (review prerequisite 3,
  `deadopt/review.md:88`).
- **MUST — delete routed keys before closing provenance (review F2).** Keys stay
  in the row until cleanup completes; de-adopt deletes `routed_secret_keys` from
  the vault BEFORE `CloseAdoptProvenance`, or keeps a `cleanup_pending` row —
  enabled by this schema retaining the keys through `de_adopting`.
- **SHOULD — shared-routed-key scan before deletion (security P3-2).** Before
  deleting a routed key, de-adopt SHOULD scan other live manifests for a
  `secret:<KEY>` reference to the same key (a hand-authored manifest could share
  it) and skip deletion if referenced, OR accept + document the risk of removing
  a shared key. Flagged so de-adopt does not blind-delete.

## Observability

New events on the existing `supervisor-events.log` `adopt` source (envelope +
emit pattern per `adopt.go:527-553`; `source: "adopt"`):

- `adopt-provenance-captured` (info) — pending row + N snapshots written. Body:
  `manifest`, `clients`, `present_count`, `absent_count`, `snapshot_refs`
  (paths).
- `adopt-provenance-committed` (info) — `adopting → adopted`. Body: `manifest`,
  `manifest_hash`.
- `adopt-provenance-capture-failed` (error) — capture failed before any mutation
  (fail-closed). Body: `manifest`, `client`, `reason` (path-free class).
- `adopt-provenance-abort` (warn) — row + snapshots removed during failure
  cleanup. Body: `manifest`, `reason`.
- `adopt-provenance-commit-failed` (warn) — Install committed but the flip write
  failed; row left `adopting` (recoverable). Body: `manifest`.
- `adopt-provenance-orphan-reaped` (warn) — the GC or the capture upsert removed a
  stale `adopting` row + snapshot dir. Body: `manifest`, `age_seconds`, `trigger`
  (`gc` | `upsert`). Makes the secret-bearing-orphan cleanup operator-visible.

The existing GUI `operator-action` audit (`gui/adopt.go:106-116`, sanitized
narration on failure `:112-116`) is unchanged — capture is backend-internal and
its failures surface through the existing adopt error path.

## Test strategy

API/unit (new-behavior falsification):

1. **T-capture-persisted (review T1 core, `deadopt/review.md:103-104`).** Seed
   one stdio entry, `ExecuteAdopt`, then open the store from a FRESH `API`
   instance (no shared in-memory state) and assert: (a) `adopted-entries.json`
   has an `adopted` record with the mapped fields; (b) the pinned snapshot exists
   at `snapshot_ref` and `snapshot_sha256` matches its bytes; (c) the snapshot's
   parsed entry equals the pre-adopt entry (so de-adopt CAN reconstruct). Fails
   if any provenance is only in memory — this is T1's "fail if the test passes an
   in-memory snapshot" guard.
2. **T-prune-churn (review T5, `deadopt/review.md:110-111`; research gate 4).**
   Set `backups.keep_n` low, adopt, churn the CLIENT-CONFIG backups past
   retention (repeated `BackupKeep` prunes), then assert the PINNED snapshot at
   `snapshot_ref` still exists and a restore from it still succeeds. Falsifies the
   prunability limit — the pinned artifact is outside the prune scope.
3. **T-capture-fail-closed (research gate 1).** Inject a snapshot-write failure
   (e.g. unwritable state dir). Assert `ExecuteAdopt` returns an error and NO
   vault key was written, NO manifest was created, and NO client config changed
   — a currently-successful adopt is NOT regressed.
4. **T-abort-cleanup.** Inject an `Install` failure after capture+secrets+manifest.
   Assert the pending row AND the snapshot dir are removed (folded into existing
   cleanup); no orphan row/snapshot remains.
5. **T-present-absent.** Adopt with an entryless-fanout client (model on
   `adopt_test.go:1510-1539`). Assert the fanout client's row is
   `original_state:"absent"` with empty `snapshot_ref`, and the source client is
   `present` with a snapshot.
6. **T-secret-spelling (limit ii).** Adopt an entry with a literal secret `env`
   value. Assert the pinned snapshot preserves the ORIGINAL literal (not a
   `secret:` ref), so de-adopt restores the original spelling.
7. **T-snapshot-hardened.** Assert the pinned snapshot file has owner-only
   DACL/mode (reuse the state-file hardening assertions).
8. **T-promote-recoverable.** Inject a flip-write failure after `Install` success.
   Assert the row stays `adopting` and — crucially — its `adopt_manifest_hash` +
   `expected_manifest_hash` are BOTH populated (F1), so de-adopt's hash-gate is
   usable; `ExecuteAdopt` still returns success.
9. **T-capture-upsert (arch F2).** Pre-seed an orphan `adopting` row + snapshot
   dir for the manifest (simulating a pre-crash orphan), run `ExecuteAdopt`.
   Assert exactly ONE row for the manifest afterward and the stale orphan
   snapshot dir was replaced (not duplicated / not two rows).
10. **T-capture-read-error-fail-closed (arch F4).** Make one selected client's
    config unreadable/corrupt so `GetEntry` returns `(nil, err)`. Assert
    `ExecuteAdopt` returns a capture error with ZERO side effects (no vault key,
    no manifest, no client write) and the client is NEVER classified `absent`.
11. **T-hash-at-capture (arch F1).** Assert the `adopting` row on disk carries
    both manifest hashes BEFORE any promote, and that `adopt_manifest_hash` ==
    `ManifestHashContent(plan.ManifestYAML)` == the hash of the manifest
    `ManifestCreate` later writes.
12. **T-gc-orphan (arch F2).** Seed an `adopting` row + snapshot dir with
    `updated_at` older than the threshold and no in-flight adopt; run
    `gcOrphanedAdoptingProvenance`; assert the row + snapshot dir are gone and a
    fresh (`updated_at` recent) `adopting` orphan is PRESERVED.

Consumer-side (de-adopt item, listed so they are not lost): a tamper-gate test —
swap the pinned snapshot bytes after adopt, assert de-adopt refuses restore
fail-closed on `snapshot_sha256` mismatch (security P2-1) — belongs to de-adopt
but is enabled by the whole-file hash this item writes.

(De-adopt's round-trip, hash-race, secret-retry, `/g/`, and lock-order tests
`deadopt/review.md:105-113` belong to the de-adopt item; this item delivers the
persisted provenance those tests consume, and T-capture-persisted + T-prune-churn
are the two that MUST live here to prove the artifact is durable and pinned.)

## Claims (falsifiable guarantees — `{ guarantee, single-owner, enforcement-probe }`)

1. `{ guarantee: The `adopting` provenance row + all pinned snapshots are durable
   on disk BEFORE the first irreversible adopt mutation (`persistAdoptRoutedSecrets`,
   `adopt.go:218`); single-owner: `captureAdoptProvenance` called from
   `ExecuteAdoptWithOpts` before `:218`; enforcement-probe: T-capture-fail-closed
   asserts an injected capture failure leaves zero vault/manifest/client side
   effects }`
2. `{ guarantee: A capture-step failure aborts adopt fail-closed and does NOT
   regress a currently-successful adopt; single-owner: the pre-`:218` capture
   return path; enforcement-probe: T-capture-fail-closed }`
3. `{ guarantee: The restore artifact is non-prunable — no `BackupKeep`/
   `pruneOldTimestamped` pass can delete it; single-owner: `adoptSnapshotDir`
   (separate directory, no backup prefix) vs `pruneOldTimestamped`'s prefix scan
   (`clients.go:1152`); enforcement-probe: T-prune-churn }`
4. `{ guarantee: Pre-adopt provenance survives a process boundary — a fresh
   process reconstructs the pre-adopt entry from disk alone; single-owner:
   `adopted-entries.json` + the pinned snapshot; enforcement-probe:
   T-capture-persisted reads via a fresh API instance }`
5. `{ guarantee: The pinned snapshot preserves original secret-literal spelling
   (limit ii); single-owner: snapshot taken before `Install` rewrites the config;
   enforcement-probe: T-secret-spelling }`
6. `{ guarantee: `install.go`'s per-client block and rollback contract
   (`:2632-2710,:2702-2708`) are unmodified — capture stays in the adopt owner;
   single-owner: seam S1 (adopt Execute); enforcement-probe: `git diff` shows no
   change under `install.go`'s per-client block }`
7. `{ guarantee: `managed-entries.json` schema + demigrate readers are unmodified;
   single-owner: the store-shape decision (new file); enforcement-probe: `git
   diff` shows no change to `ManagedEntry`/schema-version/demigrate readers in
   `managed_entries.go` }`
8. `{ guarantee: The pinned snapshot is written owner-only via the hardened
   state-file pipeline, never the backup lane's plain 0600 copy; single-owner:
   `writeAdoptClientSnapshot` → `WriteStateFileBytesAtomic`; enforcement-probe:
   T-snapshot-hardened asserts owner-only DACL/mode }`
9. `{ guarantee: Adopt CLI/API/GUI request+response shapes are byte-unchanged;
   single-owner: capture is backend-internal to `ExecuteAdoptWithOpts`;
   enforcement-probe: no diff to `api.ts:77-128`, `cli/adopt.go`, `gui/adopt.go`
   response structs }`
10. `{ guarantee: A flip-write failure after Install success never rolls back a
    committed adopt — it downgrades to a recoverable `adopting`; single-owner:
    `promoteAdoptProvenanceToAdopted` (non-fatal on error); enforcement-probe:
    T-promote-recoverable }`
11. `{ guarantee: BOTH manifest hashes are populated on the `adopting` row at
    capture (pre-`:218`), so a committed-but-`adopting` row is never empty-hashed
    and de-adopt's hash-gate never silently SKIPs (`manifest.go:717`) (arch F1);
    single-owner: `captureAdoptProvenance` computing `ManifestHashContent(plan.ManifestYAML)`;
    enforcement-probe: T-hash-at-capture }`
12. `{ guarantee: At most one provenance row + snapshot dir exists per manifest —
    a re-run upserts and reaps the prior orphan; a stale cross-manifest orphan is
    GC'd (arch F2 / P2-2); single-owner: `captureAdoptProvenance` upsert +
    `gcOrphanedAdoptingProvenance`; enforcement-probe: T-capture-upsert,
    T-gc-orphan }`
13. `{ guarantee: There is exactly ONE expected-hub-shape derivation owner —
    `liveEntryMatchesManifestBinding` — no stored/second shape descriptor to drift
    (arch F3); single-owner: `managed_entries.go:355-408` (reused by de-adopt);
    enforcement-probe: `grep` shows no `expected_hub_shape` field in the schema
    or any second shape-derivation path }`
14. `{ guarantee: A `GetEntry`/config-read error at capture aborts fail-closed
    (zero side effects) and never classifies a client `absent` (arch F4);
    single-owner: `captureAdoptProvenance` classification; enforcement-probe:
    T-capture-read-error-fail-closed }`
15. `{ guarantee: `snapshot_sha256` is a whole-file hash sufficient for de-adopt's
    fail-closed restore gate (refuse on mismatch OR missing) (security P2-1/F5);
    single-owner: `writeAdoptClientSnapshot` (whole-file hash) + the de-adopt
    restore gate (Consumer-contract handoff); enforcement-probe: the de-adopt
    tamper-gate test (listed under Test strategy) }`

## Adjacent findings (filed, NOT in this item's scope)

- **Per-adapter byte-equivalence probe (from limit i).** Byte-equivalent restore
  is UNVERIFIED per adapter (`deadopt/design.md:79`); this design ships
  `functional-equivalent` as the honest default. A per-adapter round-trip probe
  is needed before any `byte-equivalent` upgrade. To file in `work-items/bugs/`
  with `context: adjacent-finding`, `status: open` (created by the orchestrator,
  not this design).
- **Whole-file snapshot over-collects sibling secrets (from Security).** v1
  snapshots the whole config; a minimal-entry snapshot (Alt-2) would tighten
  secret scope. Bounded exposure (owner-only + closure-deleted). To file as
  `context: adjacent-finding`.

## Findings for the user (under-specified / consumer-contract conflicts)

1. **`managed-entries.json` tuple-recording is a genuine open sub-decision, not
   settled by the store-shape decision.** The consumer expects to "forget
   managed/provenance rows" on de-adopt (`deadopt/design.md:112,125`), implying
   adopt wrote managed-entries tuples — but adopt does NOT today (research Q2),
   and de-adopt's RESTORATION relies only on `adopted-entries.json`, so
   `ForgetManagedEntry` on an absent row is a harmless no-op (`managed_entries.go:210`).
   This design RECOMMENDS adopt additionally call `RecordManagedEntry` per client
   after Install success (additive, mirrors `migrate.go:287-305`) so the marker
   stays consistent — but flags it as OPTIONAL for de-adopt correctness. **Your
   call:** include the additive tuple-recording in this item's scope, or defer it.
   It does not block de-adopt either way.
2. **[RESOLVED in this revision — arch F3]** `expected_hub_shape` is DROPPED.
   De-adopt recomputes the expected hub shape from the manifest binding via the
   existing single owner `liveEntryMatchesManifestBinding` (`managed_entries.go:355-408`)
   after the hash-gate, so there is no stored second shape-derivation path to
   drift. (Was previously flagged as a conscious redundancy; the reviewer's
   single-owner call settled it in favor of dropping.)
3. **De-adopt's manifest hash-gate (review F1) needs BOTH hashes; confirm the
   `ManifestDelete` gap is de-adopt's to close.** `ManifestEditInWithHash` has an
   expected-hash gate (`manifest.go:717-720`) but `ManifestDeleteIn` does NOT
   (`manifest.go:788-801`). This design provides `adopt_manifest_hash` +
   `expected_manifest_hash` so de-adopt CAN hash-gate a last-binding delete, but
   adding the gate to the delete path is de-adopt's implementation (review
   prerequisite 3, `deadopt/review.md:88`), not adopt-side capture. Flagged so it
   is not lost between the two items.

## Follow-ups & planning notes

- **P3-1 (STANDING follow-up work-item, security).** Minimal-entry snapshot
  (Alt-2) to end whole-config over-collection of sibling secrets. To admit as its
  own work-item (not this item's scope); the v1 mandatory mitigations hold until
  then.
- **P3-2 (consumer handoff, security).** De-adopt SHOULD scan for other live
  `secret:<KEY>` references before deleting a routed key — captured in
  "Consumer-contract handoff"; carried to the de-adopt item.
- **F6 (decision prose, optional).** The accepted store-shape decision's rationale
  rests on reasons 2 (split-owner/lifecycle hazard) + 3 (manufactured
  adopt↔demigrate coupling); reason 1 (forced schema bump) is partially
  overstated — additive JSON fields can be back-compat without a version bump. The
  conclusion stands on 2+3. Softening reason 1 in the decision file is OPTIONAL
  and deferred (the decision is accepted; this design does not edit it).
- **F7 (planner).** This item ships FULL bodies for the adopt-side lifecycle +
  `ReadAdoptProvenance`; it MUST NOT land empty/stub bodies for the three
  de-adopt-owned mutators (anti-layering). See "Scope boundary" under the API
  sketch.
- **`managed-entries.json` tuple-recording** (Findings-for-user #1) remains an
  open scope choice for the orchestrator: additive `RecordManagedEntry` per
  adopted client, or defer. Does not block de-adopt either way.

## Gate decision

**PASS (revised 2026-07-10 — arch+security review fold-in).** All 6 MUST-FIX
items (F1 hash-at-capture, F2/P2-2 upsert+GC orphan lifecycle, P2-1 fail-closed
snapshot gate, F3 drop `expected_hub_shape`, F4 fail-closed classify, F5
whole-file hash granularity) are reconciled across the schema, pseudocode,
field-table, API sketch, claims, and tests — the pseudocode↔field-table hash
timing is now consistent (both hashes at capture; promote flips state only). The
store-shape decision is ACCEPTED. Alternatives, seams, dependency direction,
blast radius, failure modes, security (incl. the orphan-secret residual with a
named GC owner), observability, and test strategy are explicit; the three known
limits are handled without promising byte-equivalence; no implementation code is
included. Next stage: $planner breaks this into delivery phases (respecting the
F7 scope boundary). SHOULD-TRACK items are in "Follow-ups & planning notes".

---

## Crash-consistency + concurrency model (bot r2 resolution)

Role: $architect. Supersedes the round-1 ad-hoc guards at HEAD `45073703`
("protect committed adopt-provenance rows from UPSERT + GC destruction (bot r1)").
Anchors verified against the worktree at that HEAD (NOT the stale `ceb01c18`
anchors above — the code has moved on; this addendum cites the real current
lines). Docs-only; no code. Decision reference:
`work-items/decisions/2026-07-10-adopt-provenance-crash-consistency-model.md`
(`status: accepted` — promoted after the model shipped in PR #528).

> **AS SHIPPED (#528).** This r2 addendum captures the model at its r2 stage. Two
> mechanisms below were further refined by Codex-bot rounds r3 + r4 before merge, and
> the r2 wording at the affected spots is corrected in place: (1) the single classifier
> reads NO manifest file — it derives the expected hub binding from the row's IMMUTABLE
> `manifest_name` + captured `port` (r3 findings A+B); (2) `present-merged-lower` is
> keyed on the adapter field `clients.MCPEntry.SourceBelowWriteTarget`, NOT on a
> `ConfigPath()` ENOENT (r4 finding 1), and a `present` client's snapshot bytes are
> byte-validated via `clients.EntryBytesChecker.EntryPresentInBytes` (r4 finding 3). The
> full delta is consolidated in the "## Reconciliation to merged code (as shipped #528)"
> section at the end of this file. Authoritative anchors are the merged files
> `internal/api/adopted_entries.go`, `internal/api/adopt.go`, and
> `internal/clients/entry_bytes.go`.

### Why the round-1 fixes kept revealing the next edge

Every round-1 guard used `operation_state` + **manifest-existence** as a proxy for
a row's true condition, and that proxy is ambiguous in three independent ways the
bot's r2 review walked one at a time:

- An `adopting` row can be **LIVE in-flight** OR a **crash-orphan** — state alone
  cannot tell them apart (finding 1: the UPSERT reap at `adopted_entries.go:379-388`
  drops a live row).
- A dead-owner `adopting` row with **manifest-exists** can be a **committed-but-
  unflipped** row (KEEP) OR a **pre-promote crash** (REAP) — manifest-existence is
  TRUE for both (finding 2: the GC keep-guard at `adopted_entries.go:678` preserves
  the crash-orphan forever).
- Both abort and GC are **row-driven**, so a snapshot written before its row
  (`captureAdoptClientsProvenance` at `:394`/`:495` runs before the row write at
  `:425`) is invisible to every reaper (findings 3, 4).

The fix is not a fourth guard. It is **three orthogonal signals**, each with ONE
owner, that together classify any row's condition unambiguously.

### The three signals (the coherent core)

**Signal 1 — owner liveness: a per-manifest adopt LEASE (`flock`), held
capture→promote/abort.** A dedicated lock file
`<state-dir>/adopt-provenance/<manifest>.lease` (SIBLING to the `<manifest>/`
snapshot dir, so `removeAdoptSnapshots`' `RemoveAll` of the dir never touches it).
`ExecuteAdoptWithOpts` acquires it with a **non-blocking `TryLock`** at the very
top of the adopt (before capture) and holds it — through `persistAdoptRoutedSecrets`,
`ManifestCreate`, `Install`, and `promote` — releasing on every exit path (`defer`).
The lease **is** the liveness authority; there is no pid math:

- `TryLock` **fails** ⇒ a live same-manifest adopt already holds it ⇒ **FAIL CLOSED**
  (finding 1: never reap, never proceed).
- `TryLock` **succeeds** ⇒ **no live same-manifest adopt exists** (the OS auto-
  releases a dead holder's `flock`), so ANY prior `adopting` row for this manifest
  has a **provably dead owner** and is reap-eligible.
- Every reaper (the capture UPSERT and the cross-manifest GC) `TryLock`s the
  candidate's lease **before** reaping: can't acquire ⇒ live ⇒ **skip**. One
  mechanism, two consumers ("one owner per cross-cutting invariant").

*Why the lease over the alternatives (simplest-correct, RECOMMENDED).* `flock` is
already THE liveness/mutex primitive in this exact file (`adopted-entries.lock`,
`adopted_entries.go:170-186`) and in `state_file_helper.go:97-102`; the lease adds
**zero new primitive**. The lock is the liveness — no `owner_pid`/`owner_start_time`
token, no cross-process process-table probe, no pid-recycling edge, no cross-platform
`CallerStartTime()` comparison. Cost: the lease is held across the (slow) `Install`.
Acceptable — it is PER-MANIFEST (concurrent adopts of *different* manifests never
contend), and concurrent adopts of the *same* manifest MUST be serialized/refused
anyway. **Rejected alt A — fail-closed on ANY existing `adopting` row** (the "simplest"
option): too blunt — a crash-orphan row then blocks all same-manifest retries until
the 24 h GC runs, and `BuildAdoptPlan`'s manifest-exists refusal (`adopt.go:159`)
plus operator manifest-removal still leaves the orphan row un-reapable on retry. It
loses the round-1 UPSERT's reap-on-natural-retry, which the lease preserves.
**Rejected alt B — `owner_pid` + `owner_start_time` liveness token** (reuses
`CallerStartTime()`, `intent_audit_caller_*.go`): equivalent correctness, but needs
a cross-process "is pid P alive with start-time T" probe wired into `internal/api`
(the supervisor's `lookupProcessIdentity`-style probe lives in another layer), plus
pid-recycling reasoning; a hung-but-alive adopt protects its row exactly as the
lease would, so the token buys no advantage for more code.

**Signal 2 — install committed: hub-binding-live via the existing single owner
`liveEntryMatchesManifestBinding`** (`managed_entries.go:355-408`). For a
dead-owner `adopting` row, this is the committed-vs-crash discriminator (finding 2):
Install commits by rewriting each selected client's config to the hub URL, so
"Install committed" ⟺ **at least one of the row's `adopt_clients` has a live entry
that matches the manifest binding**. The computation is the existing per-client
pattern at `demigrate.go:417-426` (`adapter.GetEntry(source_entry_name)` → `live`;
`liveEntryMatchesManifestBinding(live, manifest, binding, m)`), composed over
`adopt_clients` — the GC/capture becomes a THIRD consumer of that one owner (already
reused by `demigrate.go:426` and `lsp_client_router.go:313`), no second
shape-derivation path (consistent with arch F3). This **replaces** the round-1
manifest-exists keep-guard (`:678`): the KEEP decision is hub-binding-live, not
manifest-existence. (AS SHIPPED #528, bot r3: the classifier reads NO manifest file
at all — it derives the expected binding from the row's IMMUTABLE `manifest_name` +
captured `port` + the adopt-v1 binding constants, so an operator editing/deleting the
manifest after a committed adopt can never make the committed row reapable. See the
Reconciliation section.) On any error building the signal → **KEEP** (fail-safe; never
reap on uncertainty — same posture as the round-1 existence-check-error path).

**Signal 3 — durable anchor: ROW-FIRST ordering + a snapshot-dir-driven GC
backstop.** Capture writes a MINIMAL `adopting` row (manifest name, both hashes,
`operation_state:"adopting"`, empty `clients`) **before** any snapshot, then writes
the snapshots, then updates the row with the client provenance. This structurally
eliminates the rowless-snapshot window (findings 3, 4): a snapshot dir is always
subordinate to a row that a row-driven reaper can find. As a **class-level backstop**
(the mandate is to stop the whack-a-mole, not close one instance), the GC ALSO scans
`<state-dir>/adopt-provenance/*` and reaps any `<manifest>/` dir with **no matching
store row** (under the same lease `TryLock`) — so a rowless secret dir from ANY
future ordering bug or partial write is discoverable, not just the ones row-first
prevents. RECOMMENDED: BOTH (row-first is load-bearing; the dir-scan is cheap
insurance that closes the CLASS).

### The single classifier (used by BOTH capture-reap and the GC)

AS SHIPPED (#528, bot r3 findings A+B): the classifier reads NO manifest file. It
derives the EXPECTED hub binding from the row's IMMUTABLE captured fields
(`manifest_name` + captured `port` + the adopt-v1 binding constants daemon `"default"`,
url_path `"/mcp"`) and asks the single recognition owner. An operator editing/deleting
the manifest after a committed adopt must NOT make the committed row reapable (finding
A); any client-construct/read uncertainty is KEEP (finding B). Verbatim at
`adopted_entries.go:441-468`.

```
// Precondition: caller HOLDS the manifest lease, so the row's owner is provably dead.
classifyDeadAdoptingRow(row) -> COMMITTED_KEEP | CRASH_REAP:            // adopted_entries.go:441
    expected := &ServerManifest{Name: row.manifest_name,
                                Daemons: [{Name: "default", Port: row.port}]}  // synthetic; row fields only, no file read
    for each c in row.adopt_clients:
        adapter, ok := AllClients()[c]
        if !ok                     -> COMMITTED_KEEP        // cannot construct client => cannot disprove => KEEP
        live, gErr := adapter.GetEntry(row.source_entry_name)
        if gErr != nil             -> COMMITTED_KEEP        // read error => cannot disprove => KEEP (finding B)
        if live == nil             -> continue              // cleanly no entry here; check other clients
        binding := ClientBinding{Client: c, Daemon: "default", URLPath: "/mcp"}
        if liveEntryMatchesManifestBinding(live, row.source_entry_name, binding, expected):  // managed_entries.go:355
            return COMMITTED_KEEP                           // Install committed a live hub binding (finding 2 KEEP)
    return CRASH_REAP                                       // every client readable, NONE holds the expected hub entry (finding 2 REAP)
```

Capture-reap consumes it: a prior `adopting` row → `COMMITTED_KEEP` ⇒ FAIL CLOSED
(refuse to overwrite a committed adopt — this generalises the round-1
non-`adopting`-row refusal at `:372-376`); `CRASH_REAP` ⇒ reap + proceed. The GC
consumes it identically. **One classification, one committed-signal owner** — the
capture path and the GC can never diverge again (that divergence WAS the bug class).

### Revised lifecycle ordering (supersedes the round-1 pseudocode)

```
ExecuteAdoptWithOpts(plan, w, opts):                       // adopt.go:248
  0a. gcOrphanedAdoptingProvenanceFn(...)   [non-fatal]    // adopt.go:260 — each candidate under ITS OWN lease TryLock
  0b. lease := TryLock(<state-dir>/adopt-provenance/<manifest>.lease)
        !ok -> FAIL CLOSED "concurrent adopt of <manifest> in progress" (finding 1)
        defer lease.Unlock()                                // released on EVERY exit path
  0c. rec := captureAdoptProvenance(plan)     // runs UNDER the held lease   // adopt.go:268
        c1. [store-lock] read store:
              prior non-`adopting` row  -> FAIL CLOSED (committed provenance; :372-376 generalised)
              prior `adopting` row      -> classifyDeadAdoptingRow(row):
                    COMMITTED_KEEP -> FAIL CLOSED (defends a bypass of BuildAdoptPlan's manifest gate)
                    CRASH_REAP     -> removeAdoptSnapshots(m) + drop row   (owner PROVABLY dead — lease held)
              write MINIMAL `adopting` row (manifest + BOTH hashes + empty clients)   <-- ANCHOR (row-first)
              [/store-lock]
        c2. for each present client: writeAdoptClientSnapshot(...)          // adopted_entries.go:495
              GetEntry non-nil + SourceBelowWriteTarget=true  -> present-merged-lower, NO snapshot (finding 5, r4)
              GetEntry non-nil + SourceBelowWriteTarget=false -> snapshot + EntryPresentInBytes byte-validate (r4)
                    (ConfigPath ENOENT here -> FAIL CLOSED, not merged-lower)
              other snapshot/read error -> abort-under-lease + RETURN (surface the cleanup err, finding 4)
        c3. [store-lock] update row with client provenance ; write store  [/store-lock]
        on capture failure -> abortAdoptProvenance(rec) (best-effort; row anchors GC reclaim) + RETURN
  1. persistAdoptRoutedSecrets(...)    on err -> abortAdoptProvenance(rec) + existing return   // adopt.go:273
  2. ManifestCreate(...)               on err -> abortAdoptProvenance(rec) + existing cleanup   // adopt.go:279
  3. Install(...)                      on err -> abortAdoptProvenance(rec) + existing cleanup   // adopt.go:292
  4. promoteAdoptProvenanceToAdopted(rec.ManifestName)  [non-fatal]         // adopt.go:318
  5. emitAdoptExecutedEvent(plan)                                           // adopt.go:321
  (defer releases the lease)
```

Lock order (acyclic, deadlock-free): **`<manifest>.lease` (outermost, held
capture→promote) → `adopted-entries.lock` (store, transient per write) →
`<snapshot>.lock` (innermost, per file)**. This EXTENDS the order already documented
at `adopted_entries.go:167-169`. The GC never holds the store lock while blocking on
a lease: it (i) takes the store lock, enumerates candidate manifests, releases it;
then (ii) per candidate, `TryLock`s the lease OUTSIDE the store lock, and only then
re-takes the store lock to persist the reap. `TryLock` (non-blocking) makes even an
inverted acquisition impossible to deadlock.

### Per-finding resolution

- **Finding 1 (live `adopting` reap, `adopted_entries.go:379-388`).** The lease
  (Signal 1) makes the reap decision safe: capture holds the lease, so a prior
  `adopting` row's owner is provably dead before it is ever reaped; a *live*
  concurrent adopt holds the lease and forces the second one to FAIL CLOSED at
  `0b`. The round-1 `:372-376` committed-row scan stays (generalised into the single
  classifier's `COMMITTED_KEEP` branch), but it is no longer the sole defense — it
  could not distinguish a live `adopting` row from an orphan; the lease can.

- **Finding 2 (pre-promote crash not reapable, `:678`).** Replace the manifest-exists
  keep-guard with Signal 2. A dead-owner `adopting` row is `COMMITTED_KEEP` iff a
  hub binding is live (`liveEntryMatchesManifestBinding` over `adopt_clients`);
  otherwise `CRASH_REAP`. The committed-but-unflipped row (Install committed, flip
  crashed: a live binding exists) is KEPT; the pre-promote crash (NO live binding on
  any `adopt_client`) is now REAPED — closing the "leaked secret snapshots forever"
  hole. The concrete committed signal is the **live hub binding**, not manifest
  existence — AS SHIPPED (#528, r3) the classifier never reads the manifest file at
  all (see the Reconciliation section).

- **Finding 3 (rowless snapshot on capture crash, `:495`).** Row-first ordering
  (Signal 3) writes the minimal `adopting` row at `c1` before any snapshot at `c2`,
  so no snapshot dir ever exists without a row. The snapshot-dir-driven GC backstop
  makes any rowless dir (from a future bug) discoverable regardless. The window is
  structurally closed.

- **Finding 4 (swallowed partial-cleanup error, `:399`).** Two changes: (i) row-first
  makes a failed cleanup **reclaimable** (the row anchors GC/dir-scan reaping) rather
  than a permanent leak; (ii) the cleanup result is **surfaced** — `if err :=
  removeAdoptSnapshots(...); err != nil` wraps into the returned capture error (and
  emits `adopt-provenance-abort`), never `_ = removeAdoptSnapshots(...)`. Consistent
  with the anchor model: cleanup is best-effort but never silent.

- **Finding 5 (MiMoCode merged-layer snapshot source, `:491-493`).** See the rule
  below. A client whose entry resolves from a LOWER read/import layer the hub never
  writes is the first-class state **`present-merged-lower`** (no snapshot), NOT a
  capture failure. AS SHIPPED (#528, r4 finding 1) this is keyed on the adapter's
  authoritative `clients.MCPEntry.SourceBelowWriteTarget` field, NOT on a
  `ConfigPath()` `fs.ErrNotExist` (the r2 keying, superseded — see the Reconciliation
  section). Entry-presence is proven by `GetEntry` (non-nil); a lower-layer source is a
  valid MiMoCode state, not a vanished entry.

- **Finding 6 (`PresentAtBuild` wire leak, `adopt.go:66`).** See the fix below.
  `gui/adopt.go:34-36` anonymously embeds `*api.AdoptPlan` into `adoptPlanResponse`,
  so the round-1-added exported `PresentAtBuild` serializes into `/api/adopt/plan`
  (`gui/adopt.go:67-70`), regressing design claim 9 (byte-unchanged wire shape).

### MiMoCode layer-source rule (finding 5, precise)

Facts (verified `internal/clients/mimocode.go`): `ConfigPath()` returns the FIXED
write target `mimocode.json` (`:678`, `mimoCodeWriteTargetInDir`), the ONLY file the
hub WRITES/DELETES/BACKS-UP; `GetEntry` reads the MERGED view across all read layers
(`readLayerFiles`, `:821-867`: `config.json < mimocode.json < .jsonc < MIMOCODE_CONFIG
< ~/.mimocode < overlay < inline < ~/.claude.json import`), so it can succeed on a
LOWER layer while the write target is absent. Install's `AddEntry` seeds + writes the
hub entry to the write target; de-adopt's restore (`RestoreEntryFromBackupForRollback`,
whose "snapshot lacks the entry ⇒ remove it from live config" semantics are the
interface contract at `clients.go:220-235`) is **write-target-scoped**.

Rule (AS SHIPPED #528, bot r4 finding 1) — for a client whose entry is present
(`GetEntry` non-nil), branch on the adapter-authoritative
`clients.MCPEntry.SourceBelowWriteTarget` field (NOT on an `os.ReadFile(ConfigPath())`
result — that was the r2 keying, superseded by r4; see the Reconciliation section):

1. **`SourceBelowWriteTarget == false`** → the entry lives IN the write target
   (`ConfigPath()`), so `original_state:"present"`: read the write-target bytes,
   VALIDATE the exact captured bytes physically contain the entry via the adapter's
   `clients.EntryBytesChecker.EntryPresentInBytes` (r4 finding 3 — closes the
   delete-then-recreate double-TOCTOU a second `GetEntry` re-verify would miss;
   `entry_bytes.go`, `adopted_entries.go:704-718`), then pin the snapshot +
   whole-file `snapshot_sha256`. (Covers single-file clients AND
   MiMoCode-where-the-entry-is-in-mimocode.json.) A `ConfigPath()` ENOENT here means
   the config vanished mid-capture (no durable bytes) → **FAIL CLOSED**, deliberately
   NOT `present-merged-lower` (`adopted_entries.go:694-702`).
2. **`SourceBelowWriteTarget == true`** → NEW state
   `original_state:"present-merged-lower"`, **no snapshot** (`snapshot_ref:""`,
   `restore_mode:"functional-equivalent"`); the write-target bytes are NOT read. The
   entry lives in a lower layer the hub never touches; de-adopt restores by
   **removing the hub entry from the write target**, which re-exposes the untouched
   lower-layer original (its original secret-literal spelling included, since the hub
   never wrote it). This is NOT the P2 silent-data-loss: `GetEntry` non-nil PROVES the
   entry is still resolvable — a genuinely-vanished present-at-Build entry returns
   `GetEntry` nil, which the fail-closed branches (`adopted_entries.go:730-...`) still
   reject. `present-merged-lower` ⇔ `SourceBelowWriteTarget`, exactly
   (`adopted_entries.go:681`).
3. **Any other read/parse error, or a byte-validation miss** → capture failure (fail
   closed).

**Rejected alt — snapshot the providing lower layer (`config.json`).** Requires a new
"which layer defined the entry" resolver, and de-adopt would restore that layer's
bytes to the WRITE TARGET (wrong file), corrupting the layer structure. It does not
compose with the write-target-scoped restore. So: **tolerate the absent write target;
do not snapshot the lower layer.** `present-merged-lower` is an additive enum value
(no schema-version bump); de-adopt MUST handle it (Consumer-contract addition below).

### Wire-shape fix (finding 6, precise)

`PresentAtBuild` is accessed ONLY within package `api` (set at `adopt.go:211`; read at
`adopted_entries.go:464-465`). **RECOMMENDED: unexport it to `presentAtBuild`** —
strongest fix, because an unexported field is structurally un-serializable and can
never leak through ANY embed, matching its own comment ("Internal-only field; not part
of any adopt CLI/API/GUI wire shape", `adopt.go:65`). Precondition the implementer must
verify: no black-box (`package api_test`) test references the exported name; if one
does, fall back to **`json:"-"` on the field** (also fully closes the leak). It is the
ONLY new leak: every other exported `AdoptPlan` field (`EntryName`, `ManifestName`,
`ManifestYAML`, `AdoptClients`, `AlsoPresent`, `SignatureMismatches`,
`DisabledSameName`, `SecretRoutedKeys`) pre-dates this work-item and is the intended
plan preview; `secretValues` is already unexported. Add a regression guard: a test
asserting the `/api/adopt/plan` JSON body has no `present_at_build`/`PresentAtBuild`
key (closes the class, not just the instance).

### Invariant table (crash matrix — every window classified)

| # | Crash window | Durable residue | Signals (lease / hub-binding / manifest) | Verdict | Reaper / handler | Secret snapshot |
|---|---|---|---|---|---|---|
| C1 | lease held, before minimal row (`c1`) | none (lease auto-released) | dead / — / — | nothing to do | — | none exists |
| C2 | after minimal row, before snapshots | `adopting` row, no dir | dead / no / no | `CRASH_REAP` | same-manifest capture (immediate) or cross-manifest GC | none exists |
| C3 | mid snapshot write (`c2`) | `adopting` row + PARTIAL dir | dead / no / no | `CRASH_REAP` | capture/GC (row-driven) + dir-scan backstop | reclaimed |
| C4 | after full row, before `ManifestCreate` | full row + dir, no manifest | dead / no / no | `CRASH_REAP` | capture/GC | reclaimed |
| C5 | after `ManifestCreate`, Install NOT committed | row + dir + manifest, no live binding | dead / **no** / yes | `CRASH_REAP` | GC (row+dir); operator removes orphan manifest via `BuildAdoptPlan` guidance | reclaimed |
| C6 | Install committed, before `promote` | row + dir + manifest + LIVE binding | dead / **yes** / yes | `COMMITTED_KEEP` | GC keeps (MAY re-drive promote); de-adopt tolerates `adopting`+live-shape | RETAINED (de-adopt needs it) |
| C7 | after `promote` (`adopted`) | committed row + dir + manifest | n/a (not `adopting`) | untouched | de-adopt closes → deletes | retained until close |
| CC | second same-manifest adopt in flight | first's LIVE row | **lease held by first** | FAIL CLOSED (second) | second refuses at `0b` | first's snapshot never reaped |

Optional self-healing (RECOMMENDED, not required): at C6 the GC MAY call the
idempotent `promoteAdoptProvenanceToAdopted` to converge the row to `adopted`;
correctness does not depend on it (de-adopt already tolerates `adopting`+live-shape,
design.md:236-238). Keeping the GC a pure reaper is the equally-valid minimal choice.

### Interaction with the existing accepted design

This addendum changes only the crash/concurrency mechanics; it does NOT touch the
chosen approach, the store shape, the pinned-snapshot hardening, the secret-lifecycle
posture, or the consumer restore contract. The 24 h `adoptOrphanGCThreshold` is
RETAINED for cross-manifest row-bearing orphans (unchanged UX); the lease now makes an
immediate reap SAFE, so the threshold is a secondary comfort guard, not a safety
requirement (same-manifest retry already reaps immediately via capture). The GC's
snapshot-dir backstop is NOT age-gated (a rowless dir has no `updated_at`), but is
gated on the lease + "no store row" — safe.

### Change-Surface Contract (r2 addendum — supersedes the round-1 crash guards)

- **Intended change surface:**
  - `internal/api/adopted_entries.go` — add the per-manifest lease helpers
    (acquire/`TryLock`/release + `<manifest>.lease` path), the single
    `classifyDeadAdoptingRow` (lease-precondition + hub-binding signal), row-first
    capture ordering (minimal-row-then-snapshots-then-full-row), the
    `present-merged-lower` classify branch, the surfaced cleanup error, and the GC
    rewrite (hub-binding keep-signal + snapshot-dir backstop).
  - `internal/api/adopt.go` `ExecuteAdoptWithOpts` (`:248-324`) — hoist the lease
    acquire/`defer`-release to wrap the whole adopt (before `:268` capture, released
    after `:318` promote / in each abort branch).
  - `internal/api/adopt.go` `AdoptPlan.PresentAtBuild` (`:66`) — unexport (or
    `json:"-"`).
- **Approved extension seam(s):** S6 — the per-manifest `flock` lease (new lock leaf,
  same `gofrs/flock` primitive as `adopted-entries.lock`). S7 —
  `liveEntryMatchesManifestBinding` (`managed_entries.go:355`) reused read-only as
  the committed signal (third consumer; not modified). S8 — the GC snapshot-dir scan
  over `<state-dir>/adopt-provenance/*` (new read of an adopt-owned dir).
- **Protected / must-not-touch surfaces:** unchanged from the base design —
  `install.go` per-client block + rollback contract; `managed_entries.go`
  `ManagedEntry`/schema/demigrate readers; `liveEntryMatchesManifestBinding`'s BODY
  (reuse only); the client backup lane; `BuildAdoptPlan`'s side-effect-freedom;
  `adopt_secret_route.go`. **Additionally protected:** the MiMoCode adapter
  (`internal/clients/mimocode.go`) — capture consumes `ConfigPath()`/`GetEntry`
  as-is; the layer semantics are NOT modified.
- **Declared blast radius:** the adopt Execute path + `adopted_entries.go` internals +
  one new lock leaf per manifest (`<manifest>.lease`) + one additive `AdoptOriginalState`
  enum value (`present-merged-lower`) + the `PresentAtBuild` visibility fix. No schema
  version bump (additive enum). No change to install/demigrate/migrate/manifest/secret
  code, to the store file shape, or to any adopt CLI/API request shape; the ONLY wire
  change is the REMOVAL of the accidental `PresentAtBuild` field from the plan response
  (a regression repair toward claim 9, not a new field).

### Claims (r2 addendum — `{ guarantee, single-owner, enforcement-probe }`)

16. `{ guarantee: A live in-flight same-manifest adopt's `adopting` row is NEVER reaped
    by a concurrent capture or the GC; single-owner: the per-manifest `<manifest>.lease`
    flock held capture→promote (a reaper that cannot `TryLock` it skips); enforcement-probe:
    T-r2-concurrent-lease (two overlapping same-manifest adopts; the second FAILs CLOSED,
    the first's row + snapshot survive intact) }`
17. `{ guarantee: A dead-owner `adopting` row is REAPED iff no hub binding is live, and
    KEPT iff one is (committed-but-unflipped); single-owner: `classifyDeadAdoptingRow` over
    `liveEntryMatchesManifestBinding`; enforcement-probe: T-r2-committed-vs-crash (two seeded
    orphans — one with a live client binding, one without; GC keeps the first, reaps the
    second) }`
18. `{ guarantee: No crash window leaves a secret-bearing snapshot dir with no reclaiming
    row; single-owner: row-first capture ordering + the snapshot-dir GC backstop;
    enforcement-probe: T-r2-rowfirst-crash (inject a crash after the minimal row and mid
    snapshot write; assert row + partial dir are both reaped) and T-r2-dirscan (plant a
    rowless `adopt-provenance/<m>/` dir; assert the GC reaps it under lease) }`
19. `{ guarantee: A partial-cleanup failure is surfaced, never swallowed, and is reclaimable;
    single-owner: capture's cleanup path (wrap `removeAdoptSnapshots` err + emit
    `adopt-provenance-abort`); enforcement-probe: T-r2-cleanup-surfaced (force
    `removeAdoptSnapshots` to fail; assert the returned error names it AND a later GC still
    reclaims the row) }`
20. `{ guarantee: A MiMoCode adopt whose write target is absent but whose entry resolves from
    a lower layer captures successfully as `present-merged-lower` (no snapshot), and a
    genuinely-vanished present-at-Build entry still fails closed; single-owner: capture's
    classify branch keyed on `GetEntry`-non-nil + `clients.MCPEntry.SourceBelowWriteTarget`
    (AS SHIPPED #528 r4, not `fs.ErrNotExist`);
    enforcement-probe: T-r2-mimocode-lower (entry only in `config.json`, write target absent →
    `present-merged-lower`, adopt succeeds) and T-r2-vanished (present-at-Build entry gone,
    `GetEntry` nil → capture fails closed, zero side effects) }`
21. `{ guarantee: The `/api/adopt/plan` response carries no `present_at_build` field;
    single-owner: `AdoptPlan.presentAtBuild` unexported (or `json:"-"`); enforcement-probe:
    T-r2-wire-guard asserts the plan JSON has no such key }`
22. `{ guarantee: Capture-reap and GC classify an `adopting` row through ONE function and ONE
    committed-signal owner — they cannot diverge; single-owner: `classifyDeadAdoptingRow`;
    enforcement-probe: `grep` shows both call sites route through it and no second
    hub-binding/manifest-exists classifier exists in `adopted_entries.go` }`

### Test strategy (r2 addendum)

New falsification tests (in addition to T1-T12 above):

- **T-r2-concurrent-lease** (claim 16, finding 1). Drive two same-manifest adopts so the
  second enters capture while the first is between capture and promote (inject a pause via
  the existing `promoteAdoptProvenanceFn`/an install seam). Assert: the second returns a
  "concurrent adopt in progress" error, and the first's row + snapshot dir are byte-intact
  (not reaped). Falsifies the round-1 live-row reap.
- **T-r2-committed-vs-crash** (claim 17, finding 2). Seed two aged `adopting` orphans for
  distinct manifests: (A) manifest exists + a client config carries the live hub binding;
  (B) manifest exists + NO client binding. Run the GC. Assert A is KEPT, B is REAPED
  (row + snapshot dir). Falsifies the manifest-exists keep-guard.
- **T-r2-rowfirst-crash** (claim 18, findings 3/4). Inject a snapshot-write failure at the
  first present client. Assert: the minimal row exists on disk at the failure point, and
  after capture returns error the row + partial dir are removed (or a subsequent GC removes
  them); no rowless dir survives.
- **T-r2-dirscan** (claim 18). Plant a `adopt-provenance/<m>/x.snapshot` with no store row.
  Run the GC. Assert the dir is reaped (under lease) and a live-lease dir is skipped.
- **T-r2-cleanup-surfaced** (claim 19, finding 4). Force `removeAdoptSnapshots` to fail
  during capture cleanup. Assert the returned error names the cleanup failure (not swallowed)
  and the row remains GC-reclaimable.
- **T-r2-mimocode-lower** + **T-r2-vanished** (claim 20, finding 5). (i) Entry only in
  `config.json`, write target `mimocode.json` absent → adopt succeeds, client recorded
  `present-merged-lower` with empty `snapshot_ref`. (ii) A present-at-Build entry removed
  before capture (`GetEntry` nil) → capture fails closed, zero vault/manifest/client side
  effects.
- **T-r2-wire-guard** (claim 21, finding 6). Marshal an `adoptPlanResponse` (or hit
  `/api/adopt/plan`); assert no `present_at_build`/`PresentAtBuild` key.

### Bounded residuals (r2)

- **Abandoned cross-manifest orphan lingers ≤ 24 h.** A hard-crash orphan for a manifest the
  operator NEVER retries is reaped by the next adopt's GC (`adopt.go:260`) or a
  supervisor-startup GC; until then its owner-only, secret-bearing snapshot dir survives.
  UNCHANGED from the base design's documented residual (owner-only DACL; the lease now makes
  an immediate reap safe, so this bound can be TIGHTENED later without a safety argument).
  Same-manifest retries reap immediately via capture. **Recommend actually keeping this as
  the only residual** — every other finding is fully closed, not deferred.
- **`present-merged-lower` restore is a de-adopt obligation.** This item records the state;
  de-adopt must implement "remove the hub entry from the write target" for it. Pinned in the
  Consumer-contract addition below. Not a residual in THIS item (the state is captured
  correctly); flagged so it is not lost across the item boundary.

### Consumer-contract addition (de-adopt MUST honor — extends "Consumer-contract handoff")

- **MUST — handle `original_state:"present-merged-lower"`.** Restore by REMOVING the hub
  entry from the client's write target (`ConfigPath()`); do NOT expect a snapshot. The
  untouched lower layer re-emerges via the adapter's merge. Treat it as a successful restore
  (`restore_mode:"functional-equivalent"`), distinct from `absent` (which had no entry at
  all) for honest reporting.

### Gate decision — r2 addendum

**PASS.** The six r2 findings are resolved by ONE model (three orthogonal signals + one
classifier), not six guards: the per-manifest lease (Signal 1) closes finding 1 and makes
every reap safe; the hub-binding committed signal (Signal 2) closes finding 2 by replacing
manifest-existence; row-first ordering + the dir-scan backstop (Signal 3) close findings 3-4;
the write-target-absent rule closes finding 5 without reopening P2 silent-data-loss; and the
`PresentAtBuild` unexport closes finding 6 and repairs claim 9. Signals, seams, lock ordering,
the crash matrix, the single classifier, tests, and the one bounded residual are explicit; no
implementation code is included; the base design is intact. The cross-cutting decision is filed
`status: proposed` for the `$architecture-reviewer` gate. Next stage: `$planner` folds the r2
model into the delivery plan (it collapses the round-1 guards, so it is a revision of the
existing capture/GC phases, not new phases).

---

## Reconciliation to merged code (as shipped #528)

DELIVERED: PR #528, squash `16dba601` on master (2026-07-10). After the r2 addendum
above, the model was refined by two more Codex-bot rounds (r3, r4) and merged. This
section is the authoritative as-shipped delta; where it and the r2 addendum differ, the
merged code wins. Anchors verified against master `internal/api/adopted_entries.go`,
`internal/api/adopt.go`, `internal/api/adopt_provenance_events.go`, and
`internal/clients/entry_bytes.go`.

### Delta 1 (bot r3, findings A+B) — the classifier reads NO manifest file

`classifyDeadAdoptingRow` (`adopted_entries.go:441-468`) classifies a dead-owner
`adopting` row from the row's IMMUTABLE captured fields ONLY. It builds a synthetic
`config.ServerManifest{Name: rec.ManifestName, Daemons: [{Name: "default", Port:
rec.Port}]}` and asks the single recognition owner
`liveEntryMatchesManifestBinding(live, rec.SourceEntryName, binding, expected)`
(`managed_entries.go:355`) per `adopt_client`. It does NOT call `manifestExistsIn`,
does NOT load the manifest file, and does NOT derive the binding from the file
(`bindingFor(m,c)` is gone). Rationale: an operator editing/deleting the manifest
(port change, binding removal) after a committed adopt must not make the committed
row's provenance reapable (finding A). Uncertainty is ALWAYS KEEP: a client that
cannot be constructed, or whose `GetEntry` ERRORS, → `COMMITTED_KEEP` (finding B).
Only when every `adopt_client` is cleanly readable AND none holds the expected hub
entry is the row `CRASH_REAP`. (Supersedes the r2 classifier pseudocode's
`manifestExistsIn` / `load manifest` / `bindingFor` steps.)

### Delta 2 (bot r4, finding 1) — `present-merged-lower` is keyed on `SourceBelowWriteTarget`, not `ConfigPath()` ENOENT

For a `present` client (`GetEntry` non-nil), capture branches on the adapter field
`clients.MCPEntry.SourceBelowWriteTarget` (`clients.go:70-88`; `adopted_entries.go:681`):

- `SourceBelowWriteTarget == true` → `original_state:"present-merged-lower"`, no
  snapshot (the entry resolves from a lower read/import layer the hub never writes;
  the write-target bytes are not read). `present-merged-lower` ⇔ `SourceBelowWriteTarget`,
  exactly.
- `SourceBelowWriteTarget == false` → `original_state:"present"` with a pinned
  snapshot; a `ConfigPath()` ENOENT in this branch is now a FAIL-CLOSED capture
  failure (config vanished mid-capture, no durable bytes), NOT `present-merged-lower`
  (`adopted_entries.go:694-702`).

(Supersedes the r2 MiMoCode rule's `os.ReadFile(ConfigPath())`-`fs.ErrNotExist`
keying.) The enum value `AdoptOriginalStatePresentMergedLower` ships at
`adopted_entries.go:116` (additive, no schema-version bump). The de-adopt
consumer-contract for it is unchanged from the r2 addendum's "Consumer-contract
addition".

### Delta 3 (bot r4, finding 3) — snapshot bytes are byte-validated before pinning

For a `present` (`SourceBelowWriteTarget == false`) client, capture validates that the
EXACT snapshotted bytes physically contain the adopted entry via
`clients.EntryBytesChecker.EntryPresentInBytes(configBytes, plan.EntryName)`
(`entry_bytes.go`; called at `adopted_entries.go:704-718`) — a pure parse of the
captured bytes, no second disk read. This closes a double-TOCTOU the r2 model left
open (the entry deleted before `os.ReadFile` then re-created before a `GetEntry`
re-verify, pinning a snapshot whose bytes a later de-adopt would restore as a
DELETION). A miss (`present==false` or parse error) is a FAIL-CLOSED capture failure.
New vocabulary: the `clients.EntryBytesChecker` interface, implemented by every
adopt-supported adapter and forwarded through the `lockingClient` wrapper.

### Confirmed unchanged as shipped (r2 mechanisms that survived intact)

- Per-manifest `flock` LEASE `<state-dir>/adopt-provenance/<manifest>.lease`
  (`adopted_entries.go:73,346-382`), `TryLock`-based, held capture→promote/abort; a
  reaper `TryLock`s before reaping. Lock order `<manifest>.lease → adopted-entries.lock
  → <snapshot>.lock`.
- ROW-FIRST capture ordering (minimal anchor row before any snapshot) +
  snapshot-dir-driven GC backstop; `classifyDeadAdoptingRow` is the single classifier
  for BOTH capture-UPSERT reap and the GC (`adopted_entries.go:532`, `:934`).
- Hub-binding-live committed signal via the reused `liveEntryMatchesManifestBinding`
  owner; store shape (`adopted-entries.json`, schema v1) and the hardened owner-only
  snapshot (`WriteStateFileBytesAtomic`) unchanged.

### Known minor gap (tracked, non-blocking)

`emitAdoptProvenanceCaptured` (`adopt_provenance_events.go:72-83`) counts only
`present` and `absent` clients into `present_count` / `absent_count`; a
`present-merged-lower` client appears in the `clients` name list but in neither count.
Observability-only; tracked in `work-items/backlog/2026-07-10-adopt-provenance-lease-hygiene.md`.
