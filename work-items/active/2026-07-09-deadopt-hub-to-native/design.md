# Phase-2 de-adopt design memo

Role: $architect. Read-only research + design; NO implementation code.

## Revision (2026-07-10) — folded in the DELIVERED adopt-provenance contract (unblock)

This memo was **REVISE / blocked** (`review.md`, 2026-07-09). Its sole blocker was
"adopt-side durable pre-adopt provenance", which **SHIPPED in PR #528** (squash
`16dba601`, merged 2026-07-10) and is archived at
`work-items/archive/2026-07/2026-07-09-adopt-side-durable-pre-adopt-provenance/`.
De-adopt is the **consumer** of that provenance. This revision replaces every
"provenance we need adopt to add" assumption with the **AS-SHIPPED** store contract
(verified against merged code, not the old design's assumptions), and closes the
review findings against the real API. All `file:line` anchors verified on-disk at
this session's `git master` HEAD.

Blocker status: **CLEARED.** The `Depends-on:` prerequisite is delivered; the store
(`<state-dir>/adopted-entries.json` + `<state-dir>/adopt-provenance/<manifest>/`
snapshots) exists on master. What changed since the original memo:

- The provenance **shape** is now fixed by shipped Go types (`AdoptProvenanceRecord`
  / `AdoptClientProvenance` in `internal/api/adopted_entries.go`), not the memo's
  proposed schema. See "Consuming the delivered provenance store".
- A **new per-client state `present-merged-lower`** exists that the original memo
  predated. De-adopt MUST handle it (restore = remove the hub entry, no snapshot).
- `snapshot_sha256` shipped as a **stored fail-closed restore gate** (security P2-1).
- `expected_hub_shape` was **DROPPED** (arch F3); de-adopt recomputes shape via the
  single owner `liveEntryMatchesManifestBinding`.
- The `adopting → adopted → de_adopting → closed` state machine's de-adopt-owned
  mutators are **declared but comment-only stubs today** — de-adopt IMPLEMENTS them.

## Change-Surface Contract

De-adopt OWNS this seam decision; the planner and implementers CONSUME it (may
`REVISE`-to-architect on conflict, may NOT redefine it).

- **Intended change surface:**
  - NEW `internal/api/deadopt.go` — `BuildDeAdoptPlan` + `ExecuteDeAdoptWithOpts`
    (the de-adopt owner, sibling to `adopt.go`), plus the three de-adopt-owned
    provenance mutators the shipped store declared as comments
    (`adopted_entries.go:993-995`): `MarkAdoptProvenanceDeAdopting`,
    `UpdateAdoptExpectedManifestHash`, `CloseAdoptProvenance` — authored against the
    shipped schema, using the store's own (same-package, unexported)
    `withAdoptedEntriesLock` / `readAdoptedEntries` / `writeAdoptedEntries` /
    `removeAdoptSnapshots` helpers.
  - `internal/api/manifest.go` — ADD one shared helper `ManifestDeleteInWithHash`
    (an expected-hash gate at the delete mutation point, matching
    `ManifestEditInWithHash:708-758`; **FAIL-CLOSED when the expected hash is
    empty/absent** — the inverse of the edit path's skip-on-empty, security P2-a;
    **RETAINS** the path-escape guard `manifest.go:793-796`). Decision
    `work-items/decisions/2026-07-10-deadopt-manifest-delete-hash-gate.md`
    (**`status: accepted`** — arch-reviewer promoted it).
  - `internal/clients/*` — ADD a bytes-input restore variant
    `RestoreEntryFromBytesForRollbackWithConfigWriter(configBytes, name, writer)` on
    the `Client` interface + `lockingClient` forward, factoring the tail of
    `restoreEntryFromBackupWithWriter` (`claude_code.go:204-231`) so restore reads NO
    file — de-adopt feeds the already-sha256-verified snapshot bytes (security P1,
    single-read). Additive: the existing path-based helpers stay for install/serena
    rollback; the security-critical de-adopt restore MUST use the bytes variant.
  - `internal/api/install_hub_reconcile.go` — EXTEND `BuildHubReconcilePlan`'s
    gate-ON path so a client dropping to ZERO bindings gets its `mcphub-hub`
    aggregate removed (today only gate-OFF sweeps every client, `:164-180`; the
    gate-ON per-client loop skips zero-binding clients, `:181-185`). Second
    shared-owner change (arch F-B); folded into the decision above.
  - NEW `internal/gui/deadopt.go` — POST `/api/deadopt/plan` + `/api/deadopt`
    (Same-Origin, request/response style identical to `gui/adopt.go:46-123`).
  - NEW `internal/cli/deadopt.go` — `mcphub de-adopt <server>` (alias `deadopt`).
  - Additive de-adopt events on the existing `supervisor-events.log` `adopt`/new
    `deadopt` source; additive GUI `operator-action` audit row.
  - `internal/gui/frontend/` — a `De-adopt to native` affordance for adopt-owned
    `via-hub` rows.
- **Approved extension seam(s):**
  - D1 — the de-adopt-owned provenance mutators against the shipped schema (the store
    already declared them: `adopted_entries.go:983-996`). De-adopt lives in
    `internal/api`, so it uses the store's unexported read-modify-write helpers.
  - D2 — RESTORE from ALREADY-VERIFIED BYTES via a NEW
    `clients.RestoreEntryFromBytesForRollbackWithConfigWriter(configBytes, name, writer)`
    (mirrors the capture side's byte-input `EntryBytesChecker`,
    `adopted_entries.go:704-718`). De-adopt reads the snapshot ONCE, sha256-verifies
    those bytes, and restores from the SAME in-memory bytes — the path-based
    `RestoreEntryFromBackupForRollbackWithConfigWriter` (`clients.go:353-362`) does its
    OWN second `os.ReadFile` (`claude_code.go:200`) and MUST NOT be used for the
    security-critical de-adopt restore (a TOCTOU snapshot-swap between the two reads
    would inject an attacker command/url — security P1). This supersedes the original
    "path swap, no new restore code" framing (S5 reserved a path-based reuse).
  - D3 — the shape recognizer `liveEntryMatchesManifestBinding` (`managed_entries.go:355`)
    reused read-only (a fourth consumer; NOT modified).
  - D4 — the routed-secret deleter `deleteAdoptRoutedSecrets` (`adopt_secret_route.go:161`)
    reused; de-adopt PRE-FILTERS to still-present keys before calling (arch F-D — the
    deleter is all-or-nothing, not a thin idempotent wrapper).
  - D5 — the per-manifest adopt LEASE `tryAcquireAdoptManifestLease`
    (`adopted_entries.go:370`) reused for de-adopt↔adopt mutual exclusion.
  - D6 — the existing uninstall descriptor-cleanup core
    (`install_parsed_manifest.go:1914-2023`) for last-binding supervisor-intent
    teardown; the hub reconcile owner (`install_hub_reconcile.go`) for gate-ON.
- **Protected / must-not-touch surfaces:**
  - `internal/api/adopted_entries.go` capture/promote/abort/GC + `classifyDeadAdoptingRow`
    + lease + snapshot helpers — de-adopt READS the store and IMPLEMENTS the three
    declared de-adopt mutators; it MUST NOT alter the adopt-side capture lifecycle,
    the schema version, or the crash-consistency model shipped in #528. (Do NOT
    reopen the provenance residuals — they are tracked in
    `work-items/backlog/2026-07-10-adopt-provenance-lease-hygiene.md`.)
  - `internal/api/adopt.go` `ExecuteAdoptWithOpts` / `BuildAdoptPlan` — unchanged.
  - `install.go` per-client block + shared rollback contract (`:2632-2710`) — reused
    read-only via the restore-extraction helper; not modified.
  - `managed_entries.go` `ManagedEntry` struct + schema + demigrate readers —
    de-adopt reuses only `liveEntryMatchesManifestBinding` (read-only).
  - The client backup lane (`clients.go:1021-1051,1145-1191`) — de-adopt restores
    from the adopt-owned PINNED snapshot, NOT a timestamped backup.
- **Declared blast radius:** de-adopt Execute/plan path + one new `internal/api` file
  + **THREE additive shared-owner changes**: (1) `ManifestDeleteInWithHash` on
  `manifest.go`; (2) a bytes-input restore variant on the `clients` `Client` interface
  + adapters (security P1); (3) the gate-ON zero-binding `mcphub-hub` prune in
  `BuildHubReconcilePlan` (`install_hub_reconcile.go`, arch F-B). All three are
  ADDITIVE — existing callers unchanged. Plus GUI/CLI routes, a frontend affordance,
  and additive redaction-safe events. The de-adopt mutators WRITE the shipped store's
  `de_adopting`/`closed` states (which the store declared but never writes) and DELETE
  snapshots on close. No change to adopt, install, migrate, demigrate, managed-entries,
  the backup lane, or the provenance store's schema/capture code. No new parallel
  state-sync mechanism: manifest `client_bindings` remain the source of
  `/clients/<client>/mcp` membership; the zero-binding prune EXTENDS the single
  reconcile owner rather than adding a de-adopt-local `mcphub-hub` remove.

## Adopt-flow inventory (unchanged facts; verified)

### Current adopt surface

The adopt UI is the Discovery/Migration screen (`internal/gui/frontend/src/screens/Migration.tsx:560-609`),
which submits `/api/adopt` after a successful plan (`Migration.tsx:263-320`). The
CLI has `mcphub adopt <entry>` (`internal/cli/adopt.go:13-57`). The GUI backend
registers `/api/adopt/plan` + `/api/adopt` (`internal/gui/adopt.go:46-123`). The
backend owner is `internal/api/adopt.go` — `BuildAdoptPlan` (`:126-202`) and
`ExecuteAdoptWithOpts` (`:252`). Adopt hard-limits supported clients to stdio-capable
IDs (`adopt.go:15-32`); it renders a global stdio-bridge manifest binding each
selected client to daemon `default` at `/mcp` (`adopt.go:578-581` constants
`adoptDefaultDaemonName="default"` / `adoptDefaultURLPath="/mcp"`).

The Servers matrix has an inverse-like `via-hub` uncheck-to-`/api/demigrate` flow
(`Servers.tsx:540-576`) — but demigrate restores/removes a client entry only; it does
NOT delete an adopt-created manifest, remove routed adopt secrets, or release
supervisor ownership. De-adopt is a distinct, provenance-backed operation.

### Adopt mutations and their de-adopt inverse (now backed by shipped provenance)

| Mutated surface | What adopt writes | De-adopt inverse (backed by provenance) |
|---|---|---|
| Secret vault | `persistAdoptRoutedSecrets` writes routed `secret:<KEY>` values (`adopt.go` execute; `adopt_secret_route.go:122-159`). | Delete the row's `routed_secret_keys` via `deleteAdoptRoutedSecrets` (`adopt_secret_route.go:161`), only after the last binding for the manifest is released, and BEFORE closing provenance (F2). |
| Disk manifest | `ManifestCreate` (`adopt.go`); create rejects existing names (`manifest.go:414-489`). | Subset: hash-gated `ManifestEditInWithHash` to drop target `client_bindings`. Last binding: hash-gated delete (F1). |
| Client configs | `Install` rewrites each selected client to a hub loopback entry (`install.go:1620-1675,2679-2692`). | Per-client restore keyed on the row's `original_state` (present → snapshot restore; absent → remove; present-merged-lower → remove, re-expose lower layer). |
| Client-config backups | `Install` backs up + rolls back on install failure (`install.go:2642-2708`); backups are prunable (`clients.go:1145-1208`). | NOT used. De-adopt restores from the adopt-owned NON-PRUNABLE pinned snapshot (`<state-dir>/adopt-provenance/<manifest>/<client>.snapshot`). |
| Supervisor intent | Global install writes descriptors + nudges reconcile (`install_parsed_manifest.go:586-783`). | Last binding: remove descriptors via the existing uninstall cleanup (`install_parsed_manifest.go:1914-2023,2064-2093`). |
| Provenance | #528: `captureAdoptProvenance` writes the `adopting` row + snapshots before the first irreversible mutation, promotes to `adopted` after Install (`adopted_entries.go:503-618,762-794`). | De-adopt drives `adopted → de_adopting → closed` and deletes snapshots on close. |

### How adopted state is recognized

Scan has no durable "adopted" marker; it classifies `via-hub` from the live client
config matching the expected hub loopback/relay shape for a manifest daemon
(`scan.go:2012-2143`), and `via-hub-inherited` is intentionally NOT hub-ownable
(`scan.go:2145-2157`; frontend mirror `routing.ts:481-515`). De-adopt therefore keys
ownership on the durable provenance row (`ReadAdoptProvenance`), NOT on hub URL shape
alone; the URL shape is a corroborating fail-closed check, not the ownership source.

## Consuming the delivered provenance store (AS SHIPPED #528)

The store is `internal/api/adopted_entries.go`. De-adopt reads it via the exported
`ReadAdoptProvenance(manifestName) (*AdoptProvenanceRecord, found bool, err error)`
(`adopted_entries.go:323`) and mutates it via the store's unexported helpers (same
package). The as-shipped record — this REPLACES the original memo's proposed schema:

```go
// adopted_entries.go:149-166
type AdoptProvenanceRecord struct {
    ManifestName         string   // record key; == source entry name in adopt v1
    SourceClient         string
    SourceEntryName      string   // the entry name de-adopt restores per client
    Port                 int      // captured; used to recompute the expected hub binding
    AdoptClients         []string
    AdoptManifestHash    string   // immutable; sha256 of plan.ManifestYAML at capture
    ExpectedManifestHash string   // == AdoptManifestHash at capture; de-adopt updates on subset edit
    RoutedSecretKeys     []string // vault key NAMES adopt created
    OperationState       AdoptOperationState // adopting|adopted|de_adopting|closed
    CreatedAt, UpdatedAt time.Time
    Clients              []AdoptClientProvenance
}
// adopted_entries.go:132-144
type AdoptClientProvenance struct {
    Client         string
    OriginalState  AdoptOriginalState // present | absent | present-merged-lower
    RestoreMode    AdoptRestoreMode   // functional-equivalent | byte-equivalent | n/a
    SnapshotRef    string             // state-dir-relative, forward-slashed; PRESENT-ONLY
    SnapshotSHA256 string             // WHOLE-FILE sha256 (hex); fail-closed gate; PRESENT-ONLY
}
```

Assumed-vs-shipped delta (what de-adopt must NOT get wrong):

| Original memo assumed | AS SHIPPED (#528) — de-adopt consumes this |
|---|---|
| Per-client `original_state` is `present` or `absent`. | THREE states: `present`, `absent`, **`present-merged-lower`** (`adopted_entries.go:104-117`). |
| Store the pinned backup ref; may store `expected_hub_shape`. | `SnapshotRef` (state-dir-relative, forward-slashed, present-only) + a stored WHOLE-FILE `SnapshotSHA256`. **No `expected_hub_shape`** — recompute via `liveEntryMatchesManifestBinding`. |
| Per-client config-shape hash. | `SnapshotSHA256` (whole-file). It is a FAIL-CLOSED restore gate, not decorative. |
| Restore from a pinned backup; byte-equivalence maybe. | Restore from the pinned snapshot via a SINGLE-READ bytes-input variant (`RestoreEntryFromBytesForRollbackWithConfigWriter`, de-adopt-added, security P1) fed the sha256-verified bytes — NOT the path-based helper's second read; `RestoreMode` is `functional-equivalent` for every present client (byte-equivalence UNVERIFIED per adapter). |
| Operation states declared by the memo. | The enum + `ReadAdoptProvenance` + snapshot/lease/store helpers SHIP; the three de-adopt mutators are DECLARED as comments (`adopted_entries.go:993-995`) — de-adopt authors them. |
| Hashes captured somewhere. | BOTH `AdoptManifestHash` + `ExpectedManifestHash` are populated AT CAPTURE (arch F1), so even a committed-but-`adopting` row is hash-gate-usable. |

Recompute inputs de-adopt derives (no stored field):

- **Expected hub binding** per client (arch F3, `adopted_entries.go:441-468` shows the
  pattern the shipped classifier uses): synthetic
  `config.ServerManifest{Name: rec.ManifestName, Daemons: [{Name:"default", Port: rec.Port}]}`
  + `config.ClientBinding{Client: c, Daemon:"default", URLPath:"/mcp"}`, fed with the
  live `adapter.GetEntry(rec.SourceEntryName)` into
  `liveEntryMatchesManifestBinding(live, rec.SourceEntryName, binding, expected)`
  (`managed_entries.go:355`). This is the SAME single owner demigrate uses
  (`demigrate.go:426`) — no second shape-derivation path.
- **Snapshot sha256** for the gate: `ManifestHashContent(snapshotBytes)`
  (`manifest_hash.go:17`) — the SAME function `writeAdoptClientSnapshot` used to
  produce the stored value (`adopted_entries.go:288`), so recompute is exact.

## De-adopt reverse-mutation design

### Backend owner

`internal/api/deadopt.go`, sibling to `adopt.go`, in the API layer (`api.go:1-11`
requires CLI/GUI to call `internal/api`, not reach into clients/scheduler directly).
It reuses the sub-owners named in the Change-Surface Contract (D2–D6). No new
state-sync mechanism; `client_bindings` stay the aggregate-membership source.

### Per-client restore — branch on `original_state` FIRST

The `original_state` selects the mechanic; only `present` triggers the snapshot
sha256 gate (absent/merged-lower have no snapshot, so a "missing snapshot" check must
NOT fire for them):

1. **`present`** — read the pinned snapshot bytes ONCE; recompute
   `ManifestHashContent(snapshotBytes)` and compare to the row's `SnapshotSHA256`;
   refuse FAIL-CLOSED on **mismatch OR missing snapshot file** (security P2-1); then
   restore from the SAME in-memory bytes via the NEW
   `RestoreEntryFromBytesForRollbackWithConfigWriter(client, snapshotBytes, SourceEntryName, writer)`.
   **Single-read is load-bearing (security P1).** The path-based
   `RestoreEntryFromBackupForRollbackWithConfigWriter` does its OWN second `os.ReadFile`
   (`claude_code.go:200`), so verify-then-restore-BY-PATH leaves a TOCTOU window where a
   broadened-parent co-resident swaps the snapshot file between the two reads and injects
   an attacker `command`/`url`. De-adopt MUST verify and restore the SAME bytes and MUST
   NOT call the path-based helper here — this mirrors the capture side's own byte-input
   validation (`adopted_entries.go:704-718`). The bytes-input helper extracts the named
   entry and writes it into live config (removing if the bytes lack it). Restores
   original secret-literal spelling (the snapshot was copied BEFORE Install rewrote the
   config).
2. **`absent`** (entryless fanout, `RestoreMode:"n/a"`, empty `SnapshotRef`) — remove
   the hub entry from the client (`client.RemoveEntry(SourceEntryName)`): restore to
   absence. No snapshot, no sha256 gate.
3. **`present-merged-lower`** (NEW state, `RestoreMode:"functional-equivalent"`, empty
   `SnapshotRef`) — remove the hub entry from the client's write target
   (`client.RemoveEntry(SourceEntryName)`); the untouched LOWER read/import layer
   re-emerges via the adapter's merge (its original spelling intact — the hub never
   wrote it). Same mechanic as `absent`, but report it as a successful restore
   (functional-equivalent), NOT as absence — the entry is still resolvable after
   restore. This is the shipped store's own Consumer-contract addition
   (`adopted_entries.go:108-116`; provenance design "Consumer-contract addition").
   No snapshot, no sha256 gate.

**Remove-path integrity gate + threat model (security P2-b).** The `absent` and
`present-merged-lower` branches call `RemoveEntry` driven by the row's `original_state`
— an owner-only field with NO cryptographic binding to the record; the sha256 gate
covers only `present`. On a broadened-parent host a co-resident with `FILE_DELETE_CHILD`
can flip a `present` row to `absent` AND delete its snapshot, making de-adopt REMOVE the
operator's entry instead of restoring it (data loss). `liveEntryMatchesManifestBinding`
does NOT catch this (the live entry is the hub entry in both `present` and `absent`).
Two-layer response:

- **Adopted strengthening — exact-hub-entry match before any `RemoveEntry`.** Recompute
  the EXACT deterministic hub entry adopt wrote (`clients.HubLoopbackURL(rec.Port,
  "/clients/<client>/mcp" | the daemon url_path)` + the hub relay metadata `install.go`
  wrote, `:2679-2692`) and require the LIVE entry to equal it byte-for-byte before
  removing — not merely to match the hub SHAPE. If the live entry is anything else
  (operator already restored native, or an attacker-crafted entry), REFUSE the remove.
  This closes the sub-case where the live entry is not the hub entry.
- **Documented residual + strict-mode mitigation.** The exact-match gate does NOT close
  the `present→absent`+snapshot-delete field-swap (there the live entry IS the hub
  entry). That residual is the same owner-only-DACL namespace-swap every state file
  carries; `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` is the mitigation. De-adopt MUST emit a
  one-line operator-visible warning under the relax lane (mirroring the adopt-side
  `client-write-unhardened-fallback` warn — CLAUDE.md "Hardened client-config writes")
  naming the residual + the env var. No silent downgrade.

### Manifest mutation — hash-gated (review F1)

`ManifestEditInWithHash` already gates on the expected hash (`manifest.go:708-758`,
`ErrManifestHashMismatch` at `:717-721`) but `ManifestDeleteIn` does NOT
(`manifest.go:788-801` — only `checkManifestName` + `os.Stat` + `os.RemoveAll`). So:

- **Subset de-adopt** (bindings remain): re-render the manifest with target clients
  removed from `client_bindings`, write it via
  `ManifestEditInWithHash(dir, name, newYaml, rec.ExpectedManifestHash)`. It returns
  the new content hash; de-adopt then calls
  `UpdateAdoptExpectedManifestHash(name, newHash)` so the NEXT de-adopt's gate matches.
  Keep supervisor intent + routed vault keys (other clients still need them).
- **Last-binding de-adopt** (no bindings remain): delete via a NEW
  `ManifestDeleteInWithHash(dir, name, rec.ExpectedManifestHash)` that re-reads the
  on-disk manifest and refuses with `ErrManifestHashMismatch` if it no longer matches
  — the hash check at the DELETE mutation point (review F1 "check the hash at the
  mutation point"), atomic with the delete, not a de-adopt-local read-then-delete that
  a TOCTOU could slip through. Then remove supervisor-intent descriptors and routed
  keys. Because both hashes are populated at capture (arch F1), even a
  committed-but-`adopting` row supplies a usable gate hash.

**Fail-closed-on-empty polarity (security P2-a).** `ManifestDeleteInWithHash` INVERTS
the edit path's polarity: where `ManifestEditInWithHash` SKIPs the check on an empty
`expectedHash` (`manifest.go:717-721`), the DELETE variant treats an empty/absent
expected hash as a FAIL-CLOSED **refusal to delete** (destructive-default polarity — the
safe path is don't-delete). A tampered row or schema drift that blanks
`ExpectedManifestHash` then cannot trigger an ungated delete of an externally-edited
manifest — otherwise inheriting the edit path's skip would re-open the exact F1
data-loss this closes. It also RETAINS `ManifestDeleteIn`'s path-escape guard
(`manifest.go:793-796`, `Dir(target)==Clean(dir)`).

`ManifestDeleteInWithHash` is a shared-owner addition (decision
`2026-07-10-deadopt-manifest-delete-hash-gate.md`, `status: accepted`).

### Routed-secret cleanup — before forgetting provenance (review F2)

The shipped store keeps `RoutedSecretKeys` in the row through `de_adopting`
(`adopted_entries.go:161`), so de-adopt has the durable key list until close:

1. On last-binding de-adopt only (a subset de-adopt keeps the keys — other clients
   still route through the manifest), delete the row's `RoutedSecretKeys` from the
   vault BEFORE `CloseAdoptProvenance`. The row stays `de_adopting` (the recoverable
   "cleanup pending" state — the shipped enum has no separate `cleanup_pending`, so
   `de_adopting` IS it) until every key is deleted; only then flip to `closed`.
2. **Filter-before-call, not a thin wrapper (arch F-D).** `deleteAdoptRoutedSecrets`
   (`adopt_secret_route.go:161`) is ALL-OR-NOTHING: `deleteAdoptRoutedSecretsLocked`
   errors on ANY `vault.Delete` failure, and `vault.Delete` itself errors on an
   already-absent key (`vault.go:171-177`). So "idempotency-safe" cannot be a
   try/ignore wrapper — de-adopt MUST PRE-FILTER the key set to still-present keys (a
   `vault.Get`/`vault.List` presence pass under the same `vaultMutex` + `WithVaultLock`)
   and pass only those to the deleter (or add a delete-if-present variant). A retry
   after a partial delete then deletes only the remaining keys and never re-errors on
   the ones already gone (T3).
3. **P3-a — shared-key scan is an OPERATOR WARNING, not a silent SHOULD.** Before
   deleting a routed key, scan other live manifests' env for a `secret:<KEY>` reference
   to the same key (a hand-authored manifest could share it). If referenced, SKIP the
   deletion and SURFACE it in the de-adopt plan + response as an operator-visible
   warning (a shared-key break must be visible, not a mystery daemon failure later), OR
   record a documented accepted residual — never a blind delete. De-adopt enumerates
   manifests itself (no shared helper; see "Provenance-gap flag").
4. **P3-b — abandoned-retry orphaned key (bounded, acceptable residual).** If de-adopt
   crashes after `MarkAdoptProvenanceDeAdopting` but the operator never retries, the
   routed keys linger (the `de_adopting` row still names them). Bounded and
   operator-removable; a future `de_adopting`-GC could reclaim it (a sibling to the
   adopt-side `adopting`-orphan GC). Acknowledged, not fixed here.

### Observability + redaction (security P2-c)

The pinned snapshot holds literal secret env values, so de-adopt's events, error
messages, and logs carry ONLY: manifest names, client names, vault key NAMES, snapshot
REFS (state-dir-relative paths), counts, and content hashes — NEVER snapshot bytes,
restored entry bodies, `command`/`args`/`env` values, or secret values. This is the
adopt-side path-free discipline (`adopt_provenance_events.go:49-110`; the `adopt-executed`
body logs `secret_routed_keys` NAMES only, `adopt.go:537-551`), carried forward. A
redaction test asserts no secret value appears in any emitted event body, error string,
or operator-action narration (mirroring the sanitized-narration path
`gui/adopt.go:112-116`).

### Operation-state machine + lock graph

De-adopt implements the three declared mutators (`adopted_entries.go:993-995`) driving
`adopted → de_adopting → closed`:

- `MarkAdoptProvenanceDeAdopting(name)` — `adopted → de_adopting`; idempotent; the
  durable journal marker for a resumable de-adopt.
- `UpdateAdoptExpectedManifestHash(name, newHash)` — after a subset binding edit.
- `CloseAdoptProvenance(name)` — `de_adopting → closed` + `removeAdoptSnapshots(name)`
  (deletes the pinned snapshots + secret-bearing dir).

Fail-closed ordering. The plan gates BRANCH on `OperationState` (a fresh `adopted` row
vs a resuming `de_adopting` row); every execute step is skip-if-already-done so a partial
failure leaves a recoverable `de_adopting` row a retry COMPLETES rather than refuses
(arch F-A — the original single gate-set, written for a fresh row, would have BLOCKED
resume):

```
BuildDeAdoptPlan:
  P0. ReadAdoptProvenance(manifest) -> classify by OperationState:
        found=false                         -> FAIL CLOSED (no provenance)
        closed                              -> FAIL CLOSED (already de-adopted)
        adopting WITH a live hub binding    -> ACCEPT as committed-but-unflipped
              (classifyDeadAdoptingRow COMMITTED_KEEP, adopted_entries.go:441-468) -> treat as FRESH
        adopting WITHOUT a live hub binding  -> FAIL CLOSED (pre-install crash orphan;
              the adopt-side GC reclaims it, not de-adopt)
        adopted                             -> FRESH de-adopt (full gates P1f-P3f)
        de_adopting                         -> RESUME de-adopt (per-step done-ness P1r-P3r)
  --- FRESH (adopted / committed-adopting) ---
  P1f. Hash-gate: recompute the on-disk manifest hash; MUST equal ExpectedManifestHash
       (else conflict/merge prompt — do not mutate).
  P2f. Per client, recompute the expected hub shape (liveEntryMatchesManifestBinding);
       the live entry MUST still be the hub entry. A shape MISMATCH here = operator
       edited the config after adopt -> REFUSE that client (test 10).
  P3f. For `present`, snapshot exists + sha256 matches (else fail closed).
  --- RESUME (de_adopting) ---   [the shipped schema has NO per-client "restored" flag,
                                   so done-ness is DERIVED, not read]
  P1r. Per-CLIENT done-ness. A client is RESTORE-DONE when its live entry no longer
       matches the expected hub binding AND matches its restore target (present: live
       entry == the snapshot's entry; absent: entry gone; present-merged-lower: hub
       write-target entry gone). A RESTORE-DONE client is SKIPPED, not refused. This is
       the disambiguator P2f lacks: fresh + mismatch = operator-edit -> REFUSE; resume +
       mismatch-TOWARD-the-restore-target = already-restored -> SKIP. Only a client that
       is NEITHER still-the-hub-entry NOR matching-its-restore-target is a genuine
       conflict -> refuse.
  P2r. Per-STEP done-ness for manifest/secret/close. Manifest-delete is DONE when the
       manifest file is already absent (a last-binding resume past E4) -> do NOT run the
       P1f hash-gate against a missing manifest. A subset edit is DONE when the on-disk
       hash already equals the post-edit ExpectedManifestHash.
  P3r. Secret cleanup is DONE when none of RoutedSecretKeys is still present in the vault.
ExecuteDeAdoptWithOpts (each step is skip-if-done: a fresh row runs all, a resume runs
only the remainder):
  E1. Acquire the per-manifest LEASE (tryAcquireAdoptManifestLease, adopted_entries.go:370)
      — mutual exclusion with a concurrent adopt/GC/second de-adopt; TryLock fails ->
      "concurrent operation in progress", fail closed. defer Unlock.
  E2. MarkAdoptProvenanceDeAdopting(manifest)  (idempotent; adopted->de_adopting, no-op if already).
  E3. Restore each NOT-yet-RESTORE-DONE target client (present/absent/present-merged-lower)
      BEFORE removing the hub binding (adds-before-removes: install_hub_reconcile.go:231-260,
      326-329; a short duplicate-routing window beats an outage window).
  E4. Manifest (skip if P2r says done): subset -> ManifestEditInWithHash +
      UpdateAdoptExpectedManifestHash; last binding -> ManifestDeleteInWithHash + remove
      supervisor-intent descriptors (install_parsed_manifest.go:1914-2023).
  E5. Gate-ON only: republish the resolver snapshot + hub-reconcile zero-binding prune
      (idempotent); republish failure -> success-with-restart-required, NOT a rollback.
  E6. Last binding only: delete the STILL-PRESENT RoutedSecretKeys (pre-filtered, F-D) BEFORE close.
  E7. CloseAdoptProvenance(manifest) (de_adopting -> closed + removeAdoptSnapshots) for a
      full de-adopt; for a subset, update adopt_clients + clients[] + expected hash, leave `adopted`.
  E8. Emit backend event + GUI operator-action row.
```

Test 14 (resume-after-restore) is now CONSISTENT with these gates: the already-restored
client is RESTORE-DONE (P1r) so it is SKIPPED, not refused (the old P2f-only gate would
have refused it as a shape-mismatch); the retry completes E4-E7.

**Lock graph (arch F-C — full total order, not just the shipped triple).** De-adopt holds
the per-manifest `<manifest>.lease` (E1) as the OUTERMOST lock across E1–E7; every other
lock is an INNER, transient, NON-nested acquisition. Total order:

```
<manifest>.lease   (outermost, held E1-E7)
  -> { config-lock (per client file, E3) | intent-lock (E4) |
       adopted-entries.lock (each store mutator) | hub-mcp.lock (E5 republish) }
```

The inners are mutually EXCLUSIVE in time — never one held while acquiring another: each
store mutator takes+releases `adopted-entries.lock` around a single read-modify-write;
client restores take one per-file `config-lock` at a time (`config_lock.go:32-50`);
supervisor-intent cleanup takes only the intent lock; gate-ON republish calls
`PublishGroupsSnapshotLocked`, which ACQUIRES `hub-mcp.lock` ITSELF and requires callers
NOT to hold it (`hub_mcp_resolver.go:459-476`) — so de-adopt MUST NOT hold
`adopted-entries.lock` (or any other inner) when it enters E5. **No reverse edge exists:**
nothing acquires the adopt `<manifest>.lease` while holding `hub-mcp.lock` (adopt nests
the SAME direction, `adopted_entries.go:186-188`), and the lease is `TryLock`-based
(non-blocking), so even an inverted acquisition cannot deadlock. **No IPC, kill, or wait
runs while ANY lock is held** — supervisor nudge/kill happens in the descriptor-cleanup
core outside every state lock. T6 asserts this RANKING (the total order + the
no-reverse-edge property), not merely "no IPC/kill/wait under a state lock". This resolves
the review's lock-order gap; the journaled `de_adopting` marker resolves the
cleanup-journaling gap.

### Gate-ON aggregate + `/g/` groups (review F3)

- **Gate OFF:** restoring/removing the per-server client entry is sufficient; no
  aggregate rewrite.
- **Gate ON (`/clients/<client>/mcp`):** after the binding removal, republish the
  resolver so `/clients/<target>/mcp` drops the server (existing sessions revalidate
  tools/list against the live snapshot and drop removed bindings,
  `hub_mcp_aggregator.go:345-387`; tools/call may `-32601` a moved route). If the target
  client has ZERO remaining bindings, its `mcphub-hub` aggregate entry must be pruned —
  today `BuildHubReconcilePlan` sweeps every client for `mcphub-hub` removal ONLY on
  gate-OFF (`install_hub_reconcile.go:164-180`); its gate-ON per-client loop `continue`s
  a zero-binding client (`:181-185`), leaving a stale `mcphub-hub` URL. **This is a
  SECOND shared-owner change (arch F-B):** EXTEND the single owner `BuildHubReconcilePlan`
  to emit the zero-binding `mcphub-hub` removal under gate-ON — do NOT hand-roll a
  de-adopt-local `mcphub-hub` remove (that would duplicate the reserved-entry-name
  `hubReconcileAggregateEntryName` the reconcile owner holds). Folded into the decision
  `2026-07-10-deadopt-manifest-delete-hash-gate.md` as the second shared-owner change.
- **`/g/` groups (F3) — server-scoped, not client-scoped.** Group bindings come from
  `Group.Servers` (`hub_mcp_resolver.go:204-214`), so a group binds a SERVER, not a
  (client, server). Policy:
  - **Subset de-adopt** (manifest still exists for other clients): the server is STILL
    a live manifest, so `/g/<group>/mcp` continues to route it — CORRECT and unchanged.
    De-adopt does NOT touch `groups.yaml` on a subset.
  - **Last-binding de-adopt** (manifest deleted): the server leaves the resolver, so
    every `/g/<group>` that named it now routes nothing for it. De-adopt does NOT edit
    `groups.yaml` (the operator owns it via the Groups screen), but it MUST surface an
    **orphaned-group warning** in the plan + response naming each group that referenced
    the deleted server, and emit a `deadopt-orphaned-group` event. A declared-but-empty
    group already returns an empty `tools/list` success, not a 404
    (`hub_mcp_resolver.go:98-106`), so the residue is benign but must be operator-visible.
  This keeps de-adopt's `/clients/` behavior and its `/g/` behavior defined separately,
  per the review's required revision.

### Trigger surfaces

GUI: an explicit `De-adopt to native` action for adopt-provenance-owned `via-hub` rows
only (NOT `via-hub-inherited`, NOT direct/unknown/external). Do not overload the
Servers uncheck-to-demigrate flow (`Servers.tsx:540-576`) — demigrate has narrower
semantics. Return `restart_required`/`hub_live` when a durable mutation succeeded but
gate-ON republish failed, matching manifest routes (`gui/manifest.go:169-224`).

CLI: `mcphub de-adopt <server>` (alias `deadopt`), flags `--client`/`--clients`/`--all`/
`--yes`/`--dry-run`; default dry-run unless `--yes` (matches adopt, `cli/adopt.go:13-57`).
No provenance row → fail closed (a `--reconstruct-legacy` escape hatch may build a
functional native entry from the manifest but cannot recover original secret spelling —
that is the honest legacy limit). Under gate-ON, if the CLI cannot republish the live
GUI resolver, it reports restart-required rather than claiming full live convergence
(current CLI manifest delete only removes the file, `cli/manifest.go:187-209`).

### In-flight client sessions

De-adopt is a config/topology mutation, not a live session migration. Existing sessions
continue until the client reloads; the GUI/CLI must tell the operator to restart/reconnect
the client to complete the switch. Gate-ON aggregate sessions revalidate against the live
snapshot (`hub_mcp_aggregator.go:345-387`); tools/call can `-32601` a removed route.

## Round-trip invariants + failure modes

Primary invariant: `adopt → de-adopt` restores every selected client's MCP config to its
pre-adopt state (from the pinned snapshot for `present`; absence for `absent`; the
re-exposed lower layer for `present-merged-lower`) and releases every hub-owned artifact
adopt created for that client. Restore is functional-equivalent (byte-equivalence
UNVERIFIED per adapter; declared in the plan).

| Failure mode | Behavior |
|---|---|
| Snapshot tampered / missing (`present` client) | Recomputed `ManifestHashContent(snapshotBytes) != SnapshotSHA256`, or the snapshot file is gone → refuse the restore FAIL-CLOSED before any mutation (security P2-1). |
| Manifest externally edited since adopt | On-disk hash ≠ `ExpectedManifestHash` → fail closed (merge prompt); the last-binding delete's own `ManifestDeleteInWithHash` gate is the atomic backstop at the mutation point (F1). |
| No provenance row | Fail closed. `--reconstruct-legacy` may build a functional entry but cannot recover secret spelling. |
| Row `adopting` with NO live hub binding | Treated as a pre-install crash orphan (adopt's `classifyDeadAdoptingRow` CRASH_REAP class) — NOT a de-adopt candidate; the adopt-side GC reclaims it. De-adopt fails closed. |
| Routed-secret delete fails mid-cleanup | Row stays `de_adopting` (keys retained); retry deletes idempotently (skip already-absent keys) then closes (F2/T3). |
| Partial failure after client restore, before manifest update | Recoverable `de_adopting` row; retry sees native restored + hub binding present and resumes at manifest/aggregate release. |
| Partial failure after manifest delete, before intent removal | Row stays `de_adopting`; retry removes descriptors by server name via the existing cleanup core (`install_parsed_manifest.go:1914-2023`) even if the manifest is gone. |
| Gate-ON republish fails after durable mutation | Success-with-restart-required (`gui/manifest.go:192-224`); do NOT roll back the durable restore. |
| Quarantined/backing-off daemon | Allow de-adopt; descriptor removal is independent of daemon health. |
| Gate-ON, other clients still routing | Remove only target bindings; republish so `/clients/<target>/mcp` drops the server; prune `mcphub-hub` only for zero-binding targets. |
| Last-binding delete orphans a `/g/` group | Surface an orphaned-group warning + `deadopt-orphaned-group` event; do NOT auto-edit `groups.yaml` (F3). |

## Test strategy

API/unit (falsification):

1. **T1 — round-trip via PERSISTED provenance only (review T1, `review.md:103-104`).**
   Adopt a seeded stdio entry, then read provenance/de-adopt from a FRESH `API`
   instance (no shared in-memory snapshot), assert the restored client entry equals the
   pre-adopt state. FAIL if the test hands de-adopt an in-memory snapshot — proves de-adopt
   uses only `adopted-entries.json` + the pinned snapshot. (The provenance item's
   T-capture-persisted proves the artifact is durable; T1 proves de-adopt consumes it.)
2. **Tamper gate + single-read (security P2-1 + P1).** Swap the pinned snapshot bytes
   after adopt; assert de-adopt refuses the restore fail-closed on `SnapshotSHA256`
   mismatch. Also assert a DELETED snapshot file → same fail-closed refusal. Single-read
   sub-test: inject a snapshot-file swap in the window BETWEEN the sha256 verify and the
   restore (a test seam that mutates the file after verification); assert the restored
   entry is the VERIFIED bytes, never the swapped-in attacker bytes — proving de-adopt
   restores from the in-memory verified bytes via the bytes-input helper, not a second
   path read.
3. **Present-merged-lower restore.** Adopt a MiMoCode-style client whose entry resolves
   from a lower layer (`SourceBelowWriteTarget=true` → `present-merged-lower`, no
   snapshot); de-adopt; assert de-adopt REMOVES the hub write-target entry and the lower
   layer re-emerges (no snapshot restore attempted; no false "missing snapshot" refusal).
4. **Absent fanout.** Adopt into an entryless fanout client; de-adopt; assert the entry
   is absent (restore-to-absence), no snapshot touched.
5. **T2 — plan/execute manifest-delete race (`review.md:105`).** Build the de-adopt plan,
   edit the manifest, execute a last-binding de-adopt; assert `ManifestDeleteInWithHash`
   conflicts and does NOT delete (F1).
6. **T3 — routed-secret cleanup retry (`review.md:106-107`).** Inject a vault delete
   failure after manifest/supervisor cleanup; assert the row stays `de_adopting` (keys
   retained) and a rerun deletes idempotently (already-absent keys skipped) then closes.
7. **Subset keeps manifest/supervisor/secrets.** Adopt two clients; de-adopt one; assert
   the manifest survives with only remaining `client_bindings`, `ExpectedManifestHash`
   updated, routed keys + supervisor intent intact, provenance still `adopted`.
8. **Last client deletes manifest/intent/secrets/provenance.** De-adopt the last client;
   assert manifest dir gone, supervisor-intent rows gone, routed keys gone, snapshots
   removed, row `closed`.
9. **No-provenance fail-closed.** A hub-looking manifest/client with no provenance row →
   no mutation.
10. **Live-config-edited fail-closed.** Change the live client entry to a non-matching
    shape after adopt; assert de-adopt refuses (via `liveEntryMatchesManifestBinding`)
    before restoring/removing anything.
11. **T4 — `/g/` policy (`review.md:108-109`).** Seed a group containing the adopted
    server. Subset de-adopt: assert `/g/<group>` still routes the server (manifest lives).
    Last de-adopt: assert the orphaned-group warning + event fire and the resolver drops
    the server; assert `groups.yaml` is NOT auto-edited.
12. **T5 — snapshot survives backup churn (`review.md:110-111`).** Set `backups.keep_n`
    low, adopt, churn client-config backups past retention, then de-adopt; assert restore
    still succeeds from the NON-PRUNABLE pinned snapshot.
13. **T6 — lock order / no re-entrancy (`review.md:112-113`).** Assert the FULL total
    order (`<manifest>.lease` outermost; `config-lock` / `intent-lock` /
    `adopted-entries.lock` / `hub-mcp.lock` inner + mutually non-nested): de-adopt holds
    NO inner lock when it enters E5 republish (which self-acquires `hub-mcp.lock`), and
    no reverse edge exists (nothing takes the lease while holding `hub-mcp.lock`); no
    IPC/kill/wait runs while any lock is held; de-adopt↔adopt mutual exclusion via the
    per-manifest lease.
14. **Resume after restore before manifest (reconciles with the F-A resume gates).**
    Inject a failure after client restore; retry sees the restored client as RESTORE-DONE
    (P1r → SKIP, not refuse) and completes manifest/provenance/supervisor cleanup without
    corrupting the restored entry.
15. **Quarantined daemon releases intent.** Descriptor present, runtime unhealthy;
    de-adopt still removes supervisor intent, no health precondition.
16. **P2-c redaction.** Drive a de-adopt that emits events + an operator-action row +
    (via an injected failure) an error; assert NO snapshot bytes, entry
    `command`/`args`/`env` values, or secret values appear in any emitted body / error /
    narration — only names, key NAMES, refs, counts, hashes.
17. **P2-b remove-integrity.** (a) Flip a `present` row's live entry to a NON-hub entry
    (operator-restored-native) then de-adopt an `absent`/`present-merged-lower` client;
    assert the exact-hub-entry gate REFUSES the `RemoveEntry`. (b) Assert the relax lane
    emits the operator-visible residual warning naming `MCPHUB_REQUIRE_SINGLE_USER_HOME=1`.

GUI/CLI: route tests mirroring `gui/adopt_test.go` (plan previews without mutation;
execute calls the API; conflicts map to actionable errors; operator-action emitted);
frontend visibility test (`De-adopt to native` only for adopt-provenance `via-hub`
rows); Playwright round-trip (adopt → de-adopt → scan returns to native); restart-required
UI for gate-ON republish failure. CLI: parse `de-adopt`/`deadopt`, dry-run prints plan,
`--yes` executes, missing provenance exits non-zero with no mutation, gate-ON
live-republish-unavailable returns restart-required.

## Claims (falsifiable — `{ guarantee, single-owner, enforcement-probe }`)

1. `{ guarantee: De-adopt restores a `present` client ONLY after the pinned snapshot's whole-file sha256 matches the stored `SnapshotSHA256` (refuse on mismatch OR missing snapshot); single-owner: the de-adopt pre-restore gate recomputing `ManifestHashContent(snapshotBytes)` (manifest_hash.go:17); enforcement-probe: test 2 (tamper + delete) }`
2. `{ guarantee: A `present-merged-lower` client is restored by REMOVING the hub write-target entry (no snapshot), never by a snapshot restore and never with a false missing-snapshot refusal; single-owner: the de-adopt restore branch keyed on `original_state` (adopted_entries.go:104-116); enforcement-probe: test 3 }`
3. `{ guarantee: A last-binding manifest delete is refused fail-closed if the on-disk manifest hash != `ExpectedManifestHash`, checked AT the delete mutation point; single-owner: `ManifestDeleteInWithHash` (new, gating like manifest.go:717-721); enforcement-probe: test 5 (T2) }`
4. `{ guarantee: Routed secret keys are deleted BEFORE provenance is closed, and a partial delete leaves a recoverable `de_adopting` row that retries idempotently; single-owner: the de-adopt cleanup ordering + idempotent `deleteAdoptRoutedSecrets` wrapper (adopt_secret_route.go:161, vault.go:171); enforcement-probe: test 6 (T3) }`
5. `{ guarantee: De-adopt reconstructs the pre-adopt entry from DISK ALONE (adopted-entries.json + pinned snapshot), surviving a process boundary; single-owner: `ReadAdoptProvenance` + the pinned snapshot; enforcement-probe: test 1 (T1) reads via a fresh API instance }`
6. `{ guarantee: There is exactly ONE expected-hub-shape derivation owner — de-adopt reuses `liveEntryMatchesManifestBinding`, no stored/second shape descriptor; single-owner: managed_entries.go:355; enforcement-probe: grep shows no `expected_hub_shape` read and no second shape-derivation path in deadopt.go }`
7. `{ guarantee: The shipped adopt-side provenance capture/promote/abort/GC lifecycle + schema version are UNMODIFIED — de-adopt only reads the store and writes the declared de_adopting/closed states; single-owner: adopt owns capture (adopted_entries.go:503-618), de-adopt owns the three declared mutators (adopted_entries.go:993-995); enforcement-probe: git diff shows no change to captureAdoptProvenance/promote/abort/gc/classify or `adoptedEntriesSchemaVersion` }`
8. `{ guarantee: De-adopt and a concurrent adopt/GC of the same manifest are mutually exclusive; single-owner: the per-manifest `<manifest>.lease` flock (tryAcquireAdoptManifestLease, adopted_entries.go:370); enforcement-probe: test 13 (T6) }`
9. `{ guarantee: Subset de-adopt leaves the manifest, supervisor intent, and routed secrets intact for remaining clients and updates `ExpectedManifestHash`; single-owner: the de-adopt subset branch (ManifestEditInWithHash + UpdateAdoptExpectedManifestHash); enforcement-probe: test 7 }`
10. `{ guarantee: A `/g/` group is untouched on subset de-adopt and, on last-binding delete, produces an operator-visible orphaned-group warning without auto-editing groups.yaml; single-owner: the de-adopt gate reconcile step (server-scoped group policy); enforcement-probe: test 11 (T4) }`
11. `{ guarantee: Restore is functional-equivalent (no byte-equivalence promise) and reports `RestoreMode` honestly per client; single-owner: the shipped `RestoreMode` field (functional-equivalent default, adopted_entries.go:125); enforcement-probe: plan/response asserts restore_mode, no byte-equivalence claim in UI copy }`
12. `{ guarantee: De-adopt introduces no new aggregate-membership state — `client_bindings` stays the source of `/clients/<client>/mcp`; single-owner: the manifest resolver (hub_mcp_resolver.go:279-299); enforcement-probe: grep shows de-adopt mutates bindings + republishes, adds no parallel membership store }`
13. `{ guarantee: The `present`-client restore verifies AND restores the SAME in-memory snapshot bytes (one read), so a between-reads snapshot swap cannot inject an attacker command/url (security P1); single-owner: the new `RestoreEntryFromBytesForRollbackWithConfigWriter` fed the sha256-verified bytes — NOT the path-based helper's second `os.ReadFile` (claude_code.go:200); enforcement-probe: test 2 single-read sub-test }`
14. `{ guarantee: `ManifestDeleteInWithHash` REFUSES to delete on an empty/absent expected hash (fail-closed polarity, inverse of the edit path's skip) and RETAINS the path-escape guard (security P2-a); single-owner: `ManifestDeleteInWithHash` (manifest.go, per the accepted decision); enforcement-probe: a delete-with-empty-hash test asserts refusal + a traversal test asserts `Dir(target)==Clean(dir)` survives }`
15. `{ guarantee: No `RemoveEntry` runs unless the live entry equals the EXACT deterministic adopt-written hub entry; the residual owner-only row-swap is documented + strict-mode-mitigated + relax-lane-warned (security P2-b); single-owner: the de-adopt remove-path exact-match gate; enforcement-probe: test 17 }`
16. `{ guarantee: No secret value, snapshot byte, or entry body appears in any de-adopt event/error/log/narration — names, key NAMES, refs, counts, hashes only (security P2-c); single-owner: the de-adopt event/error redaction (mirrors adopt_provenance_events.go:49-110); enforcement-probe: test 16 }`
17. `{ guarantee: A `de_adopting` resume COMPLETES (skips already-restore-done clients + already-done steps) instead of refusing them; the fresh-vs-resume disambiguation is DERIVED from OperationState + per-client/per-step done-ness (arch F-A); single-owner: BuildDeAdoptPlan's OperationState branch (P0 / P1f-P3f / P1r-P3r); enforcement-probe: test 14 (now consistent with the gates) }`
18. `{ guarantee: De-adopt's lock acquisitions obey ONE acyclic total order (lease outermost; inners mutually non-nested; no inner held across E5's self-acquired hub-mcp.lock; no reverse edge) (arch F-C); single-owner: the lock graph; enforcement-probe: test 13 (T6) asserts the ranking }`
19. `{ guarantee: Gate-ON zero-binding `mcphub-hub` pruning is emitted by the SINGLE reconcile owner `BuildHubReconcilePlan` (extended), not a de-adopt-local remove that duplicates the reserved-entry-name (arch F-B); single-owner: `BuildHubReconcilePlan` (install_hub_reconcile.go); enforcement-probe: grep shows no de-adopt-local `mcphub-hub` remove; the gate-ON T4 test asserts the aggregate is pruned }`

Cross-cutting decisions (registry): the shared-owner `ManifestDeleteInWithHash` gate
(fail-closed-on-empty polarity + retained path-escape guard) AND the second shared-owner
change (gate-ON zero-binding prune in `BuildHubReconcilePlan`, arch F-B) are filed in
`work-items/decisions/2026-07-10-deadopt-manifest-delete-hash-gate.md`
(**`status: accepted`** — arch-reviewer promoted). The bytes-input restore variant (P1),
the de-adopt operation-state ordering + resume contract, and the `/g/` policy are
single-work-item decisions and stay inline here.

## Provenance-gap flag (none blocking)

No genuine provenance gap blocks de-adopt — the full PROVENANCE surface is on master:
`ReadAdoptProvenance` (exported) + the store's same-package unexported helpers, the
non-prunable pinned snapshot + `SnapshotSHA256`, `deleteAdoptRoutedSecrets`,
`liveEntryMatchesManifestBinding`, the restore MECHANIC
`RestoreEntryFromBackupForRollbackWithConfigWriter`, `ManifestHashContent`,
`tryAcquireAdoptManifestLease`, and the declared de-adopt mutators. Three de-adopt-OWNED
implementation shapes (NOT provenance defects, NOT reasons to touch the provenance code):

- **Single-read restore is a de-adopt-authored `clients` addition (security P1).** The
  path-based restore helper is on master, but using it verbatim double-reads the
  snapshot (`claude_code.go:200`); de-adopt ADDS the bytes-input sibling
  `RestoreEntryFromBytesForRollbackWithConfigWriter` (factoring the same extraction
  tail). An additive clients-package change de-adopt owns, not a provenance dependency.
- `deleteAdoptRoutedSecrets` is all-or-nothing and `vault.Delete` errors on an
  already-absent key (`vault.go:171-177`), so de-adopt must PRE-FILTER to still-present
  keys before calling (arch F-D) — an implementation shape, not a provenance defect.
- The P3-a shared-routed-key scan has no shared helper; de-adopt enumerates manifests
  itself and surfaces a shared-key hit as an operator warning (not a blind delete).

De-adopt does NOT reopen the tracked provenance residuals (lease-file DACL, lease-file
accumulation, `present-merged-lower` capture-event count) —
`work-items/backlog/2026-07-10-adopt-provenance-lease-hygiene.md`; none affects de-adopt.

## Recommended delivery sequence (for the planner)

1. Phase 2a: the three de-adopt provenance mutators + the three additive shared-owner
   changes — `ManifestDeleteInWithHash` (fail-closed-on-empty, path-guard retained), the
   bytes-input restore variant `RestoreEntryFromBytesForRollbackWithConfigWriter` (P1),
   and the `BuildHubReconcilePlan` gate-ON zero-binding prune (F-B) — + unit tests (T1,
   tamper/single-read, T2, empty-hash refusal).
2. Phase 2b: `BuildDeAdoptPlan`/`ExecuteDeAdoptWithOpts` — the OperationState-branched
   plan gates + resume derivation (F-A), restore (present single-read/absent/
   present-merged-lower) with the P2-b remove-integrity gate, manifest mutation,
   supervisor-intent teardown, pre-filtered secret cleanup (F2/F-D/T3), redaction (P2-c),
   lease-guarded resume + full lock order (T6/T14).
3. Phase 2c: gate-ON republish + the zero-binding aggregate prune (via the extended
   reconcile owner) + `/g/` orphan policy (T4) with restart-required reporting.
4. Phase 2d: CLI + GUI + frontend affordance.
5. Phase 2e: e2e round-trip (adopt from native → de-adopt → verify client config,
   manifest, supervisor intent, aggregate/`/g/` membership, scan classification).

## Gate decision

**PASS (revised 2026-07-10 — arch + security gate fold-in).** The blocking prerequisite
(adopt-side durable provenance) is DELIVERED (#528); this design consumes the AS-SHIPPED
contract and resolves the original review findings F1/F2/F3/present-merged-lower/P2-1/T1–T6.

This revision additionally folds in the second-round arch + security REVISE (both
design-level, none a redesign):

- **Security:** P1 single-read restore (verify + restore the SAME bytes via a new
  bytes-input helper; the path-based helper's second `os.ReadFile` is banned for the
  security-critical restore); P2-a fail-closed-on-empty-hash polarity on
  `ManifestDeleteInWithHash` (+ retained path-escape guard); P2-b remove-path exact-hub-
  entry gate + documented owner-only row-swap residual + strict-mode/relax-lane warning;
  P2-c redaction contract + test; P3-a shared-key scan elevated to an operator warning,
  P3-b abandoned-retry orphan acknowledged as a bounded residual.
- **Architecture:** F-A resume contract (plan gates branch on `OperationState`; per-step
  and per-client done-ness derivation so a `de_adopting` resume completes rather than
  refuses; reconciled with test 14); F-B the SECOND shared-owner change (gate-ON
  zero-binding `mcphub-hub` prune EXTENDS `BuildHubReconcilePlan`, not a de-adopt-local
  remove) — blast radius + scope note corrected; F-C the full lock total order + no-
  reverse-edge, asserted by T6; F-D routed-secret pre-filter (the all-or-nothing deleter
  needs a still-present-keys filter, not a thin wrapper).

Change-Surface Contract, 19 claims, seams, dependency direction, blast radius (now THREE
additive shared-owner changes), failure modes, the resume-aware ordering, the full lock
graph, and test strategy are explicit; no implementation code is included; both
cross-cutting shared-owner changes are in the now-`accepted` decision. No provenance gap
blocks de-adopt. Next stage: `$planner` breaks this into the delivery phases above.
