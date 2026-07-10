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
  operator moved on) is owned by a bounded GC: `gcOrphanedAdoptingProvenance`.
  The GC MUST distinguish true pre-install orphans from committed-but-unpromoted
  adopts: it may delete an `adopting` row + snapshot dir only when ALL of these
  are true: `updated_at` is older than a threshold (default 24h), there is no
  in-flight adopt for that manifest, the manifest is absent from the manifest
  store, and the live hub-managed client entries do NOT match the manifest
  bindings that adopt would have installed. If the manifest exists and
  `liveEntryMatchesManifestBinding` confirms the live hub shape, the row is a
  committed-but-unpromoted adoption; GC MUST preserve it and SHOULD attempt the
  idempotent `adopting → adopted` promote (or at minimum leave it for de-adopt).
  It runs at the START of every `ExecuteAdoptWithOpts` (the next adopt on the
  host reaps stale orphans — cheap, no new scheduler) and SHOULD also be wired at
  `mcphub supervise` startup (planner decision; a one-line call, not a new
  mechanism).
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
  adopt failure. Orphan GC MUST NOT reap this state when the manifest/live hub
  shape proves the adopt committed; it preserves or idempotently promotes it.

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
    `updated_at` older than the threshold, no in-flight adopt, no manifest, and
    no matching live hub-managed entries; run `gcOrphanedAdoptingProvenance`;
    assert the row + snapshot dir are gone and a fresh (`updated_at` recent)
    `adopting` orphan is PRESERVED.
13. **T-gc-preserves-committed-adopting.** Seed an old `adopting` row whose
    manifest exists and whose live hub-managed client entries match the manifest
    bindings (simulating a crash or write failure after `Install` success but
    before promote). Assert `gcOrphanedAdoptingProvenance` does NOT delete the
    row or snapshot dir, and either promotes it to `adopted` or leaves the fully
    populated `adopting` row available for de-adopt.

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
