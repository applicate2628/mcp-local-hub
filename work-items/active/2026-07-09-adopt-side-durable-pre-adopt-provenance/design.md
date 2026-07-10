# Design — adopt-side durable pre-adopt provenance

Role: $architect. Source of truth: `research.md` (PASS) + the CONSUMER contract
at `../2026-07-09-deadopt-hub-to-native/design.md:69-77` and
`../2026-07-09-deadopt-hub-to-native/review.md:77-82,86-91`. No implementation
code here. All `file:line` anchors verified on-disk at repo HEAD `ceb01c18`.

Store-shape decision (required prerequisite): filed as
`work-items/decisions/2026-07-10-adopt-provenance-store-shape.md` (status
`proposed`; the `$architecture-reviewer` gate on this design promotes it). This
design assumes that decision: **a new `<state-dir>/adopted-entries.json`**.

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
- **Alt-2 — store the extracted entry (not the whole file) as a serialized
  blob inside the JSON record.** Rejected for v1: the restore helper operates on
  a whole config-format file, so a blob would need a new per-adapter
  "single-entry config writer" + a new restore-from-blob path — more new code,
  more per-adapter surface, and it forfeits verbatim reuse of the proven restore
  extraction. Revisit only if the whole-file snapshot's sibling-entry
  over-collection (see Security) proves unacceptable.
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
      "adopt_manifest_hash":    "<sha256 of manifest.yaml bytes at adopt time>",
      "expected_manifest_hash": "<sha256; == adopt_manifest_hash at adopt; de-adopt updates on a subset binding edit>",
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
          "snapshot_sha256":   "<sha256 of the pinned snapshot bytes>",           // integrity; present-only
          "expected_hub_shape": {                    // what the LIVE entry should be now (post-adopt)
            "kind":     "http",                      // http|relay-server|relay-url
            "url":      "http://127.0.0.1:9137/mcp",
            "relay_url": "",
            "daemon":   "default",
            "url_path": "/mcp"
          }
        },
        {
          "client":            "codex-cli",
          "original_state":    "absent",             // entryless-fanout client
          "restore_mode":      "n/a",
          "snapshot_ref":      "",
          "snapshot_sha256":   "",
          "expected_hub_shape": { "kind": "http", "url": "http://127.0.0.1:9137/mcp", "daemon": "default", "url_path": "/mcp" }
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
| adopt-generated manifest hash | `adopt_manifest_hash` | Immutable. `ManifestHashContent` (SHA-256 of manifest bytes, `manifest_hash.go:17`) of the freshly-created manifest. |
| current expected hash (hash-gated edit/delete) | `expected_manifest_hash` | Starts == `adopt_manifest_hash`; de-adopt updates it after a subset binding edit so the next de-adopt hash-gate (review F1) matches. Both feed `ManifestEditInWithHash`'s `ErrManifestHashMismatch` gate (`manifest.go:708-721`) and close the "ManifestDelete has no hash gate" gap (`manifest.go:774-801`). |
| per-client original state present/absent | `clients[].original_state` | `present` = a same-name pre-adopt entry existed; `absent` = entryless-fanout client (`adopt.go:183` `alsoPresent` / the fanout case pinned by `adopt_test.go:1510-1539`). |
| present → pinned backup ref or serialized adapter snapshot | `clients[].snapshot_ref` (+ `snapshot_sha256`) | Pinned whole-config-file snapshot; see "Pinned artifact". Non-prunable. |
| per-client original config-shape hash | `clients[].snapshot_sha256` | SHA-256 of the pinned pre-adopt config bytes (the shape de-adopt restores FROM); tamper-detection on restore. |
| expected hub-managed live shape | `clients[].expected_hub_shape` | The hub entry adopt installed; de-adopt's fail-closed pre-mutation check (`deadopt/design.md:103-104`, mirrors `demigrate.go:417-429` live-shape refusal). Derived from the manifest binding (`adopt.go:482-492`) + `install.go:2679-2687`. |
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
committed, and the flip is idempotent on retry. A crash BEFORE `Install`
(pending `adopting`, no live hub shape) is abortable cleanup — the adopt failed,
so the row + snapshots are orphan debris a re-run or a bounded reconcile
removes.

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
- **Restore (consumed by de-adopt):**
  `RestoreEntryFromBackupForRollbackWithConfigWriter(client, <abs snapshot
  path>, entryName, writer)` (`clients.go:353-362`). The snapshot is a valid
  whole config-format file, so the adapter parses it and extracts `entryName`
  exactly as it would from a timestamped backup.

## Fail-closed capture seam — exact placement

Inside `ExecuteAdoptWithOpts` (`adopt.go:211-253`), the ordered mutations today
are `persistAdoptRoutedSecrets` (`:218`) → `ManifestCreate` (`:221`) → `Install`
(`:230`). New ordering:

```
ExecuteAdoptWithOpts(plan, w, opts):
  0. [NEW capture — BEFORE :218]
     rec, err := a.captureAdoptProvenance(plan)   // read each selected client
                                                  // config, classify present/absent,
                                                  // pin hardened snapshots for present,
                                                  // compute hashes + expected_hub_shape,
                                                  // write record with state="adopting".
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
     if err := promoteAdoptProvenanceToAdopted(rec.ManifestName, adoptManifestHash); err != nil {
         // Install COMMITTED. Do NOT roll back a successful adopt for a flip-write
         // failure. Emit a loud warn event; leave state="adopting" (recoverable).
         // Adopt still returns success.
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
same-name entry (`client.GetEntry(entryName) != nil`, `clients.go:208-209`); an
`absent` client is a selected fanout target with no same-name entry.

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

type AdoptExpectedHubShape struct {
    Kind     string `json:"kind"`               // "http" | "relay-server" | "relay-url"
    URL      string `json:"url,omitempty"`
    RelayURL string `json:"relay_url,omitempty"`
    Daemon   string `json:"daemon,omitempty"`
    URLPath  string `json:"url_path,omitempty"`
}

type AdoptClientProvenance struct {
    Client           string                `json:"client"`
    OriginalState    AdoptOriginalState    `json:"original_state"`
    RestoreMode      AdoptRestoreMode      `json:"restore_mode"`
    SnapshotRef      string                `json:"snapshot_ref"`      // state-dir-relative; present-only
    SnapshotSHA256   string                `json:"snapshot_sha256"`   // integrity; present-only
    ExpectedHubShape AdoptExpectedHubShape `json:"expected_hub_shape"`
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
func writeAdoptClientSnapshot(manifestName, client string, configBytes []byte) (ref, sha256Hex string, err error) // WriteStateFileBytesAtomic
func removeAdoptSnapshots(manifestName string) error                       // RemoveAll the manifest snapshot dir

// ---- adopt-side lifecycle (THIS item) ----
func (a *API) captureAdoptProvenance(plan *AdoptPlan) (*AdoptProvenanceRecord, error) // -> writes state="adopting"
func promoteAdoptProvenanceToAdopted(manifestName, adoptManifestHash string) error    // adopting -> adopted (idempotent)
func abortAdoptProvenance(rec *AdoptProvenanceRecord) error                            // delete row + snapshots (idempotent)

// ---- read/mutation surface CONSUMED by de-adopt (declared here, IMPLEMENTED by the de-adopt item) ----
func ReadAdoptProvenance(manifestName string) (*AdoptProvenanceRecord, bool, error)
func MarkAdoptProvenanceDeAdopting(manifestName string) error                           // adopted -> de_adopting
func UpdateAdoptExpectedManifestHash(manifestName, newHash string) error                // subset binding edit
func CloseAdoptProvenance(manifestName string) error                                    // de_adopting -> closed + delete snapshots
```

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
- **Secret material lifecycle bound to the row.** `closed` / abort MUST
  `RemoveAll` the snapshot dir so pre-adopt secret literals do not linger past
  the operation. The record's `routed_secret_keys` are NAMES only; secret VALUES
  live only in the vault and (transiently) in the owner-only snapshot.
- **Event bodies carry no secret values or config contents.** Provenance events
  log manifest names, client names, present/absent counts, snapshot PATHS, and
  key NAMES only — matching the existing `adopt-executed` body which logs
  `secret_routed_keys` names, never values (`adopt.go:537-551`). This is a
  redaction requirement, not an option.
- **Residual (flagged for $security-reviewer):** the whole-file snapshot
  over-collects — it copies sibling entries' secrets too, not just the adopted
  entry's. Exposure is bounded (owner-only DACL; deleted on close; the material
  already exists at the same trust level in the live config), but a v2 tightening
  (minimal-entry snapshot via a per-adapter single-entry serializer, Alt-2) would
  reduce it. Recorded as an adjacent finding.
- **Co-resident parent-swap residual:** identical to every other state file — a
  `FILE_DELETE_CHILD` co-resident can delete/replace the directory entry but
  cannot read the owner-only content; `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` is the
  documented mitigation. No new posture introduced.

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
   Assert the row stays `adopting`, `ExecuteAdopt` still returns success, and a
   subsequent read sees a recoverable `adopting` + live-hub-shape state.

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
2. **Consumer's `expected_hub_shape` is partly derivable, so storing it is
   belt-and-suspenders.** De-adopt could recompute the expected hub shape from
   the manifest binding at plan time (as `liveEntryMatchesManifestBinding`
   /`demigrate.go:417-429` already do) instead of reading a stored descriptor.
   This design stores it to pin the shape against manifest edits between adopt and
   de-adopt; if you prefer a leaner record, `expected_hub_shape` could be dropped
   and recomputed — a minor schema simplification. Not a blocker; noted so the
   redundancy is a conscious choice.
3. **De-adopt's manifest hash-gate (review F1) needs BOTH hashes; confirm the
   `ManifestDelete` gap is de-adopt's to close.** `ManifestEditInWithHash` has an
   expected-hash gate (`manifest.go:717-720`) but `ManifestDeleteIn` does NOT
   (`manifest.go:788-801`). This design provides `adopt_manifest_hash` +
   `expected_manifest_hash` so de-adopt CAN hash-gate a last-binding delete, but
   adding the gate to the delete path is de-adopt's implementation (review
   prerequisite 3, `deadopt/review.md:88`), not adopt-side capture. Flagged so it
   is not lost between the two items.

## Gate decision

**PASS.** The design is traceable to accepted research facts and the pinned
consumer contract; the store-shape decision is filed; alternatives, seams,
dependency direction, blast radius, failure modes, security, observability, and
test strategy are explicit; the three known limits are handled without promising
byte-equivalence; no implementation code is included. Next stage: architecture +
security review (which promotes the store-shape decision `proposed → accepted`),
then plan.
