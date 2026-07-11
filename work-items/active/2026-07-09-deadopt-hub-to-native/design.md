# Phase-2 de-adopt design memo — v1 = ALL-CLIENTS-ONLY ATOMIC de-adopt

Role: $architect. Read-only research + design; NO implementation code. All `file:line`
anchors verified on-disk against merged master this session.

## Rework (2026-07-11) — multi-model review synthesis fold-in + v1 scope cut

Source of truth: `review-multimodel-2026-07-11.md` (LEAD arbitration of 5 independent
adversarial design reviews — codex gpt-5.6-sol xhigh + 4 fable-5 angles — run against
`design.md` rev `a1c2bcab`). This rework implements that memo. What changed vs the prior
revision:

- **v1 is ALL-CLIENTS-ONLY, ATOMIC de-adopt of one adopt-owned manifest. Subset de-adopt
  is CUT** (deferred to a follow-up). Targets ≡ the record's `AdoptClients`, always — so
  the resume scope is unambiguous and only 2 of the 3 declared mutators are used
  (`UpdateAdoptExpectedManifestHash` and the `ManifestEditInWithHash` edit lane are gone).
- **v1 requires gate-OFF.** Gate-ON de-adopt is REFUSED with a "gate OFF first" message
  and deferred to the follow-up (memo item 2, option b).
- **Close DELETES the row (snapshots-first)** — no `closed` tombstone (memo item 1).
- **The equality gate is the SHIPPED recognizer `liveEntryMatchesManifestBinding`** — no
  byte-exact recompute, no second shape owner (memo item 3).
- **The destructive client-config write is a COMPARE-AND-SWAP under one config lock** via
  a new per-adapter CAS capability interface (memo items 5 + 7).
- **The snapshot is read through the anchored secure reader** with the path recomputed
  from `(ManifestName, Client)` (memo item 4).
- **Also CUT:** `--reconstruct-legacy` and byte-exact P2-b (memo LEAD decision).

### Round 2 (2026-07-11) — 3-fable audit fold-in (design-text only; no architecture change)

Source of truth: `review-fable-audit-2026-07-11.md` (LEAD arbitration of 3 independent
fable-5 audits of the round-1 rework `f0798f9d`). The structural rework PASSED; the audit
found the restore primitive and the close/resume state machine under-specified at their
hardest points. This round folds in B1-B6 + the P3 list — all design-text, the scope and
security core unchanged:

- **B1 — restore composes the SHIPPED per-adapter restore core, never lossy `GetEntry`.**
  `CASRestoreEntryFromBytes` = the CAS gate + a bytes-parameterized refactor of the shipped
  `restoreEntryFromBackup` body; `GetEntry`→`AddEntry` is BANNED (lossy for direct-stdio —
  `MCPEntry` has no Command/Args), a parallel per-adapter re-implementation is BANNED, and
  the resume done-test compares RAW on-disk subtrees, not lean-`MCPEntry` equality.
- **B2 — close/resume state machine's two hard points** pinned: E6 gated on all-clients-
  RESTORE-DONE-or-accepted + secrets-done, the permanent-CAS-conflict horn resolved with a
  `--accept-conflict <client>` escape, and a crash-INSIDE-close resume branch added.
- **B3 — G7/G8 (+ cap) residuals** now stated in the BODY (a new Residuals section), not
  only claimed in the gate decision.
- **B4/B5/B6** — E2 re-verifies committed-ness under the held lease; restore fail-closes
  on entry-absent-in-verified-snapshot (never destroys); snapshot cap symmetry stated.
- **P3 fold-ins** — CAS lock ownership pinned (forwarder holds the lock, concrete bodies
  lock-free), restore guard polarity (guarded-refuse default), cap/secret-bearing SUFFIX
  clause (not exact-basename), C6 landing-comment direction, unknown-field-drop honesty,
  gate-ON F6 sentence, doc-anchor cleanup.

Governing decisions: `work-items/decisions/2026-07-11-deadopt-v1-all-clients-only-scope.md`
(`status: accepted`, cites the synthesis memo) and
`work-items/decisions/2026-07-10-deadopt-manifest-delete-hash-gate.md` (`status: accepted`;
Option A holds — the memo PASS'd it). Subset + gate-ON de-adopt are deferred to
`work-items/backlog/2026-07-11-deadopt-subset-and-gate-on-followup.md`.

## Change-Surface Contract

De-adopt OWNS this seam decision; the planner and implementers CONSUME it (may
`REVISE`-to-architect on conflict, may NOT redefine it).

- **Intended change surface:**
  - NEW `internal/api/deadopt.go` — `BuildDeAdoptPlan` + `ExecuteDeAdoptWithOpts` (the
    de-adopt owner, sibling to `adopt.go`), plus the de-adopt-owned provenance mutators
    the shipped store declared as comments (`adopted_entries.go:993-995`):
    `MarkAdoptProvenanceDeAdopting` (transition `{adopted, committed-adopting} →
    de_adopting`, G6; **B4** — for a `committed-adopting` row it RE-RUNS
    `classifyDeadAdoptingRow == COMMITTED_KEEP` under the HELD lease before flipping and
    refuses otherwise, so a plan-time admission stale by execute cannot flip a
    now-uncommitted orphan the adopt GC still owns) and `CloseAdoptProvenance` (now
    **deletes** the row snapshots-first — G1/item 1). **C6 landing direction:**
    `UpdateAdoptExpectedManifestHash` is DECLARED-but-UNUSED in v1 (subset cut); the
    implementation change MUST repoint its declaration comment (`adopted_entries.go:994`)
    and the `ExpectedManifestHash` field doc (`:155-158`, which today reads "de-adopt
    updates ExpectedManifestHash after a subset binding edit") at the subset FOLLOW-UP,
    and update the mutator tombstone (`:983-996` — v1 authors 2 of the 3) so the live tree
    asserts only the current state (stale-relation hygiene, arch law C6). Uses the store's
    own same-package unexported helpers (`withAdoptedEntriesLock`,
    `readAdoptedEntries`, `writeAdoptedEntries`, `removeAdoptSnapshots`, `adoptSnapshotDir`,
    `tryAcquireAdoptManifestLease`, and the `reapAdoptProvenanceRow` snapshots-first
    ordering `adopted_entries.go:860-882`).
  - `internal/api/manifest.go` — ADD shared `ManifestDeleteInWithHash` (delete-mutation-
    point hash gate; FAIL-CLOSED on empty/absent hash; RETAINS the path-escape guard
    `manifest.go:793-796`). Decision `2026-07-10-deadopt-manifest-delete-hash-gate.md`
    (`accepted`). This is the ONLY manifest mutation in v1 (always a last-binding delete).
  - `internal/clients/*` — ADD a **CAS capability interface** (mirrors the shipped
    `EntryBytesChecker` capability pattern, `entry_bytes.go:24`), NOT a `Client`-interface
    method: `CASRestoreEntryFromBytes` + `CASGuardedRemoveEntry` + the read-only
    `EntryRawSubtree` (resume done-test comparator), implemented by exactly the
    adopt-reachable adapters, forwarded by `lockingClient`, fail-closed at the de-adopt
    site if an adapter does not implement it (memo item 5 + 7). **B1 — the mutating CAS
    methods COMPOSE the shipped per-adapter restore core, they do not re-implement it:**
    each adapter's `restoreEntryFromBackup` body (`claude_code.go:191-232`, `codex_cli.go`,
    `amazon_q.go`, `aider.go`, `continue.go`, `json_mcp.go`, `mimocode.go`, `vscode.go`,
    `zed.go`, … — ~15 adapters, each with stdio-verbatim restore + remove-on-absent tests)
    is refactored into a `restoreEntryFromBytes(configBytes, name, allowHubEntry, writer)`
    core; the existing backup-path variant becomes a thin `os.ReadFile`+call-core wrapper
    (every `RestoreEntryFromBackup*` caller stays byte-unchanged). `CASRestoreEntryFromBytes`
    feeds the verified snapshot bytes into that SAME core (removing the `GetEntry`→`AddEntry`
    lossiness install rollback already avoids, `install.go:2642-2647`). **Lock ownership
    (P3):** `withConfigLock` is held by the `lockingClient` forwarder (type-assert-inside-
    lock, mirroring `AddEntryWithConfigWriter` `config_lock.go:229-239` /
    `RestoreEntryFromBackupForRollbackWithConfigWriter` `:259-269`); the concrete CAS bodies
    run UNDER that held lock and are themselves LOCK-FREE (the per-path mutex is
    non-reentrant `config_lock.go:24-30` → a concrete body calling `withConfigLock` would
    self-deadlock). The read-only `EntryRawSubtree` forwards LOCK-FREE like
    `EntryPresentInBytes` (`entry_bytes.go:109-114`).
  - `internal/api/state_read_caps.go` + `internal/api/state_read_inode_anchor.go` — two
    ADDITIVE lines: give the adopt-provenance `.snapshot` kind a client-config-sized read
    cap in `stateFileReadCapBytes` (`:28`; the default is only `maxStateFileBytes` = 1 MiB,
    too small for a real `~/.claude.json` snapshot — see "Provenance-gap flag") and mark it
    secret-bearing in `isSecretBearingStateFilePath` (`:42`, memo item 4 P3-A).
  - NEW `internal/gui/deadopt.go` — POST `/api/deadopt/plan` + `/api/deadopt`, plus a
    de-adopt eligibility read-surface (G3, below); Same-Origin, response style like
    `gui/adopt.go:46-123`.
  - NEW `internal/cli/deadopt.go` — `mcphub de-adopt <server>` (alias `deadopt`).
  - Additive redaction-safe de-adopt events (`supervisor-events.log`) + a GUI
    `operator-action` audit row.
  - `internal/gui/frontend/` — a `De-adopt to native` affordance driven by the backend
    eligibility surface (no shape heuristic, G3).
- **Approved extension seam(s):**
  - D1 — the two de-adopt provenance mutators against the shipped schema (declared at
    `adopted_entries.go:983-996`); de-adopt lives in `internal/api` and uses the store's
    unexported RMW helpers.
  - D2 — the CAS capability interface (item 5+7): the destructive-write atomicity seam.
    Its restore body COMPOSES the shipped per-adapter restore core via a behavior-preserving
    `restoreEntryFromBytes` extraction (B1) — no new restore logic, no second extraction owner.
  - D3 — the shipped recognizer `liveEntryMatchesManifestBinding` (`managed_entries.go:355`)
    reused read-only as the SINGLE "is the live entry our hub entry" equality owner (memo
    item 3). No second shape-derivation path.
  - D4 — the anchored secure reader `ReadStateFileInodeAnchored` (`state_read_inode_anchor.go:22`)
    for the snapshot read (item 4).
  - D5 — the routed-secret deleter `deleteAdoptRoutedSecrets` (`adopt_secret_route.go:161`),
    pre-filtered to still-present keys (F-D).
  - D6 — the per-manifest adopt LEASE `tryAcquireAdoptManifestLease` (`adopted_entries.go:370`)
    for de-adopt↔adopt/GC mutual exclusion.
  - D7 — the existing uninstall descriptor-cleanup core (`install_parsed_manifest.go:1914-2023`)
    for the last-binding supervisor-intent teardown.
- **Protected / must-not-touch surfaces:**
  - `internal/api/adopted_entries.go` capture/promote/abort/GC + `classifyDeadAdoptingRow`
    + lease/snapshot helpers — de-adopt READS the store and IMPLEMENTS the declared
    de-adopt mutators; it MUST NOT alter the adopt-side capture lifecycle, the schema
    version, or the shipped crash-consistency model. (Do NOT reopen the provenance
    residuals — `work-items/backlog/2026-07-10-adopt-provenance-lease-hygiene.md`.)
  - `internal/api/adopt.go` `ExecuteAdoptWithOpts` / `BuildAdoptPlan` — unchanged.
  - `install.go` per-client block + rollback contract (`:2632-2710`) — untouched; de-adopt
    does NOT thread through Install.
  - `managed_entries.go` `ManagedEntry` struct + schema + demigrate readers — de-adopt
    reuses only `liveEntryMatchesManifestBinding` (read-only).
  - `BuildHubReconcilePlan` (`install_hub_reconcile.go`) — UNCHANGED in v1 (gate-ON
    de-adopt deferred, so the gate-ON zero-binding prune is a follow-up change, NOT a v1
    shared-owner edit).
  - The client backup lane — de-adopt restores from the adopt-owned PINNED snapshot.
- **Declared blast radius:** de-adopt Execute/plan path + one new `internal/api` file +
  **THREE shared-owner changes**: (1) `ManifestDeleteInWithHash` on `manifest.go` (additive);
  (2) on the adopt-reachable `clients` adapters — an **additive CAS + read capability
  interface AND a behavior-preserving extraction refactor of the per-adapter restore bodies**
  (`restoreEntryFromBackup` → a `restoreEntryFromBytes` core + a thin file-reading wrapper;
  every shipped `RestoreEntryFromBackup*` caller stays byte-unchanged — B1); (3) the two
  additive lines in `state_read_caps.go` + `state_read_inode_anchor.go` for the `.snapshot`
  kind (a SUFFIX/dir-segment clause, not an exact basename — P3). Changes (1) and (3) are
  purely additive; (2) is additive-plus-refactor with a preserved-behavior contract on the
  existing callers. Plus GUI/CLI routes (incl. the `--accept-conflict <client>` escape) +
  eligibility surface, a frontend affordance, and additive redaction-safe events. The
  de-adopt mutators
  WRITE the `de_adopting` state (declared, never written by adopt) and DELETE the row +
  snapshots on close. **NOT changed in v1:** `BuildHubReconcilePlan` (gate-ON deferred),
  the single-owner entry renderer (recognizer suffices — see item 3), install, migrate,
  demigrate, managed-entries, the provenance store schema/capture code. No new
  aggregate-membership state.

> **Reconciliation with the coordinator's shared-owner note.** The coordinator listed the
> reconcile sweep (F-B/#6) and a single-owner entry renderer (#3) as additive shared-owner
> changes. Choosing memo item-2 **option (b) — refuse gate-ON in v1** removes BOTH from v1
> scope: the reconcile sweep is only needed when de-adopt runs under gate-ON (deferred), and
> the renderer is unnecessary because the equality gate reuses the EXISTING single recognizer
> `liveEntryMatchesManifestBinding` rather than reconstructing an entry to compare (adding a
> renderer de-adopt never calls would be dead weight and a second shape owner). Both move to
> the gate-ON follow-up. This is a deliberate rescoping flagged in the report, not an omission.

## v1 scope decision — all-clients-only, gate-OFF-only (memo LEAD decision)

**What v1 IS:** atomic de-adopt of ONE adopt-owned manifest across ALL its
`AdoptClients` at once, under gate-OFF. On success the manifest, its supervisor intent,
its routed secrets, its provenance row, and its snapshots are gone, and every adopted
client is restored to its pre-adopt state (native entry / absence / re-exposed lower
layer). It is the exact inverse of one adopt.

**What v1 CUTS (deferred to
`work-items/backlog/2026-07-11-deadopt-subset-and-gate-on-followup.md`):**

- **Subset de-adopt** (de-adopt some-but-not-all clients of a manifest). It drags in an
  undeclared 4th mutator + a `de_adopting→adopted` reverse transition, the
  `UpdateAdoptExpectedManifestHash` + `ManifestEditInWithHash` edit lane, a per-client
  snapshot-prune gap the whole-dir `removeAdoptSnapshots` cannot express, and an
  unjournaled resume target set. Per-client detach remains available TODAY via the shipped
  Servers-matrix uncheck → `/api/demigrate` lane (weaker prunable-backup guarantee).
- **Gate-ON de-adopt.** Under gate-ON the reconcile has removed every per-server entry
  (`install_hub_reconcile.go:233-263`), so the gate-OFF recognizer path does not apply and
  a separate expected-state model + the reconcile zero-binding prune are required. v1
  REFUSES gate-ON with "gate OFF first, then de-adopt" and defers the full gate-ON path.
- **`--reconstruct-legacy`** (no-provenance rows). Fail-closed on no provenance is the
  complete v1 answer.

Cutting subset collapses the manifest mutation to the single hash-gated DELETE and makes
`targets ≡ AdoptClients` — the resume scope is a fixed set, so roll-forward resume needs no
journaled target list and the 2 used mutators suffice.

## Consuming the shipped provenance store (AS SHIPPED #528)

Store: `internal/api/adopted_entries.go`. Read via exported
`ReadAdoptProvenance(manifestName) (*AdoptProvenanceRecord, found bool, err error)`
(`:323`); mutate via the same-package unexported helpers. As-shipped record fields:

```go
// adopted_entries.go:149-166
type AdoptProvenanceRecord struct {
    ManifestName, SourceClient, SourceEntryName string
    Port                 int                     // recompute the expected hub binding
    AdoptClients         []string                // v1 target set ≡ this
    AdoptManifestHash    string                  // immutable; sha256(plan.ManifestYAML)
    ExpectedManifestHash string                  // == AdoptManifestHash (v1 never edits)
    RoutedSecretKeys     []string
    OperationState       AdoptOperationState     // adopting|adopted|de_adopting|closed
    CreatedAt, UpdatedAt time.Time
    Clients              []AdoptClientProvenance
}
// adopted_entries.go:132-144
type AdoptClientProvenance struct {
    Client         string
    OriginalState  AdoptOriginalState  // present | absent | present-merged-lower
    RestoreMode    AdoptRestoreMode    // functional-equivalent (v1)
    SnapshotRef    string              // present-only; NOT trusted as a raw path (item 4)
    SnapshotSHA256 string              // whole-file sha256; present-only; fail-closed gate
}
```

`de_adopting` and `closed` are declared enum values the store never writes; de-adopt drives
them. Note: `closed` is used ONLY as a transient conceptual state — `CloseAdoptProvenance`
DELETES the row rather than persisting `closed` (item 1).

## The single equality owner — `liveEntryMatchesManifestBinding` (memo item 3)

De-adopt's one notion of "the live entry is the hub entry adopt wrote" is the SHIPPED
recognizer `liveEntryMatchesManifestBinding(live, entryName, binding, m)`
(`managed_entries.go:355-408`), the same owner `demigrate.go:426` and
`classifyDeadAdoptingRow` (`adopted_entries.go:463`) use. Inputs, exactly as
`classifyDeadAdoptingRow` builds them (`:445-448`):

- `m := &config.ServerManifest{Name: rec.ManifestName, Daemons: [{Name:"default", Port: rec.Port}]}`
- `binding := config.ClientBinding{Client: c, Daemon:"default", URLPath:"/mcp"}`
- `live := adapter.GetEntry(rec.SourceEntryName)`

The recognizer does per-shape FIELD equality (NOT byte-exact): HTTP → exact URL across the
3 loopback spellings `localhost`/`127.0.0.1`/`[::1]` (`:373-378`); Antigravity relay →
`RelayServer/RelayDaemon` + `IsMcphubBinary(RelayExePath)` (`:383-388`); relay-URL →
`RelayURL` among the 3 spellings + `IsMcphubBinary` (`:401-406`). This tolerates a binary
move/upgrade (`RelayExePath` is the absolute install path — NOT stored in the record and
NOT byte-comparable) and the loopback-spelling drift the old P2-b byte-recompute would have
false-refused. **URL formula correction:** the per-server entry adopt writes is
`HubLoopbackURL(rec.Port, "/mcp")` (`clients.go:656`); `/clients/<client>/mcp` is the
gate-ON AGGREGATE path, NOT the per-server entry. P2f-shape-match and P2-b are collapsed
onto this ONE recognizer + the ONE `snapshot_sha256` byte-gate — no piled gate stack, no
second shape owner (preserves claim 2).

**`EntryRawSubtree` is NOT a second recognizer (claim 2 intact).** The resume done-test's
raw-subtree comparator (B1) answers a DIFFERENT question — "is the live entry the SNAPSHOT's
entry we already restored" — by deep-comparing verbatim on-disk subtrees, NOT "is the live
entry the hub entry" (which stays single-owned in `liveEntryMatchesManifestBinding`). Two
distinct predicates, two distinct owners; the hub-equality question keeps exactly one owner.

## Snapshot read — anchored, path-recomputed, secret-bearing (memo item 4)

De-adopt reads a `present` client's pinned snapshot through
`ReadStateFileInodeAnchored(path)` (`state_read_inode_anchor.go:22`), which enforces BEFORE
any byte is trusted: an OOM size cap, reparse/symlink refusal, and — unconditionally in
every mode — wrong-owner refusal (`ErrWrongOwner`, `hub_mcp_state_read_inode_windows.go:194`
/ posix `:135`). A plain `os.ReadFile` is banned here (an attacker planting a multi-GB file
at the namespace-writable path is an OOM lever slurped before the hash gate, and it discards
the owner signal).

- **Path is recomputed, not trusted.** De-adopt derives the snapshot path from the
  IMMUTABLE `(rec.ManifestName, client)` via the store's own `adoptSnapshotDir(rec.ManifestName)`
  + `client + ".snapshot"` (same construction `writeAdoptClientSnapshot` used,
  `adopted_entries.go:272-289`), NOT from `SnapshotRef` as a raw path. `SnapshotRef` is
  cross-checked for a mismatch warning only.
- **Two additive shared-owner clauses (item 4 P3-A) — SUFFIX/dir-segment, NOT exact
  basename (P3).** Both owners today branch on the EXACT `filepath.Base(path)`
  (`state_read_caps.go:30` `switch base`; `state_read_inode_anchor.go:44` `switch base` +
  substring `Contains`), and a `<client>.snapshot` basename is VARIABLE
  (`claude-code.snapshot`, `cursor.snapshot`, …) matching none of the fixed cases — so the
  additions MUST be suffix/dir-segment clauses, not new `case` labels:
  (i) `isSecretBearingStateFilePath` (`state_read_inode_anchor.go:42-57`) gains a
  `strings.HasSuffix(base, ".snapshot")` OR `adopt-provenance` path-segment clause, so a
  read-broadened snapshot FAILS CLOSED like the vault files instead of relaxing;
  (ii) `stateFileReadCapBytes` (`state_read_caps.go:28-42`) gains a matching
  `.snapshot`-suffix clause returning a client-config-sized bounded cap
  (`maxIntentFileBytes`/`maxVaultBlobFileBytes` = 16 MiB, already defined) — the default
  `maxStateFileBytes` (1 MiB) is too small for a real `~/.claude.json`. See "Provenance-gap
  flag".
- **B6 — snapshot cap SYMMETRY invariant.** Adopt CAPTURE writes the snapshot via
  `WriteStateFileBytesAtomic` with NO size limit (`adopted_entries.go:272-289`), so a config
  that adopts fine today could exceed the de-adopt read cap and become PERMANENTLY
  unrestorable (a Failed client — feeds the B2a horn). The invariant: **the restore cap MUST
  be ≥ any snapshot adopt capture can pin.** v1 sets the de-adopt-owned restore cap generously
  (16 MiB, covering every realistic client config); true symmetry additionally requires a
  matching CAPTURE-time cap (== the restore cap) so adopt REFUSES to pin what de-adopt cannot
  restore — that is adopt-side (a protected surface in v1), so it is directed to the filed
  adjacent bug (widened to require capture==restore symmetry, not merely raising the read
  cap). The residual (a pathological config > 16 MiB) is bounded + assigned — see "Residuals".
- **After the anchored read**, recompute `ManifestHashContent(snapshotBytes)`
  (`manifest_hash.go:17`) and compare to `SnapshotSHA256`; refuse FAIL-CLOSED on mismatch
  OR missing snapshot (present clients only). The sha256 gate extends the owner-anchored
  trust to the exact bytes.

## Per-client restore — via the CAS capability (memo items 5 + 7; B1 + B5)

The destructive client-config write is the F1 mutation-point-atomicity principle applied to
the CLIENT CONFIG. `withConfigLock` (`config_lock.go:51`) wraps EACH adapter method
individually, so a plan-time recognizer check followed by a later `AddEntry`/`RemoveEntry`
is NOT atomic — an operator hand-edit (or a `demigrate`) between plan and execute would let
de-adopt restore a stale snapshot OVER the operator's fresh edit (silent data loss). Fix: a
new **CAS + read capability interface** in `internal/clients` (mirroring `EntryBytesChecker`),
whose mutating methods do the whole re-read → check → mutate atomically:

```go
// internal/clients — capability, NOT a Client method (item 5: never-adoptable adapters
// must not be compile-forced to implement a restore they can never run).
type CASEntryMutator interface {
    // The lockingClient FORWARDER holds withConfigLock(ConfigPath) across the whole
    // call (P3: type-assert-inside-lock, mirroring AddEntryWithConfigWriter). The
    // concrete body below runs UNDER that held lock and is itself LOCK-FREE — the
    // per-path mutex is non-reentrant (config_lock.go:24-30), so a concrete body
    // calling withConfigLock would self-deadlock.
    //
    // Under the held lock: re-read the named live entry.
    //   - live == nil  -> CONFLICT-refuse (ErrCASConflict): restoring into an
    //     operator-emptied slot resurrects an entry against intent (B1 nil-live).
    //   - match(live) false -> REFUSE (ErrCASConflict).
    //   - else run EntryPresentInBytes(snapshotBytes, entryName): if ABSENT it is an
    //     impossible state for a `present` caller (capture GUARANTEED + sha-pinned the
    //     entry) -> REFUSE fail-closed, NEVER silently remove (B5 — removal is
    //     CASGuardedRemoveEntry's alone); if PRESENT, restore via the SHIPPED restore
    //     core: restoreEntryFromBytes(snapshotBytes, entryName, allowHubEntry=false, ...)
    //     — the guarded polarity (P3), refusing a hub-shaped snapshot entry. One read,
    //     one write, one lock.
    CASRestoreEntryFromBytes(entryName string, match func(*MCPEntry) bool, snapshotBytes []byte) error
    // Under the held lock: re-read the named live entry.
    //   - live == nil  -> already-done idempotent SUCCESS (nothing to remove; B1 nil-live).
    //   - match(live) false -> REFUSE (ErrCASConflict).
    //   - else remove it.
    CASGuardedRemoveEntry(entryName string, match func(*MCPEntry) bool) error
    // Read-only (LOCK-FREE forward like EntryPresentInBytes): the VERBATIM on-disk
    // subtree for `name` in configBytes (the SAME extraction restoreEntryFromBytes
    // uses). The resume done-test deep-compares this across live-config vs snapshot
    // bytes — NOT lean MCPEntry equality (B1: two distinct stdio entries both project to
    // MCPEntry{URL:""} and would falsely read equal).
    EntryRawSubtree(configBytes []byte, name string) (subtree any, present bool, err error)
}
```

**B1 — the restore body COMPOSES the shipped per-adapter restore core; two owners are
BANNED.** The concrete `CASRestoreEntryFromBytes` feeds the verified snapshot bytes into a
bytes-parameterized refactor of each adapter's existing `restoreEntryFromBackup` core
(`claude_code.go:191-232` extracts `backupServers[name]` and comment-preserving-sets it into
the live config; ~15 adapters share this shape). Explicitly BANNED: (a) the
`GetEntry`→`AddEntry` round-trip — LOSSY for the canonical direct-stdio adopt source
(`MCPEntry` has no `Command`/`Args`, `clients.go:24-48`), which is exactly why install
rollback restores from the backup FILE not `GetEntry`→`AddEntry` (`install.go:2642-2647`); a
round-trip would restore an empty-URL husk, destroying the "original secret-literal spelling
intact" guarantee the snapshot exists to deliver; (b) a parallel per-adapter extraction
re-implementation (a second extraction owner). The extraction stays single-owned in the
refactored core; the CAS method is a thin composition of {nil/match/present gate} + {that
core}.

**Restore guard polarity (P3): the guarded variant, not `ForRollback`.** The core is composed
with `allowHubEntry=false` — the DEFAULT guarded polarity that REFUSES a hub-HTTP-shaped
snapshot entry (`ErrBackupEntryAlreadyMigrated`, `claude_code.go:219-224`). A pinned snapshot
whose entry is itself hub-shaped signals adopt absorbed an already-hub-managed entry;
restoring it would re-apply hub data, so de-adopt reports that client Failed rather than using
the verbatim `RestoreEntryFromBackupForRollback` polarity.

`match` is injected by the api-layer de-adopt as a closure over the single recognizer —
`func(live *clients.MCPEntry) bool { ok, _ := liveEntryMatchesManifestBinding(live, entryName, binding, m); return ok }`
— so the recognizer stays single-owned in `internal/api` (dependency inversion: `clients`
defines the callback signature; `api` injects the implementation; no upward import). The
concrete CAS method nil-guards `live` BEFORE calling `match` (the recognizer derefs `live.URL`
at `managed_entries.go:378`; the shipped caller nil-guards at `adopted_entries.go:459-461`),
applying the per-branch nil-live policy above. Branch on `original_state`:

1. **`present`** — anchored-read the snapshot, sha256-gate it, then
   `CASRestoreEntryFromBytes(SourceEntryName, match, snapshotBytes)`. The verified bytes are
   the ONLY thing read and written (single read — closes the between-reads swap window). The
   native pre-adopt entry (original secret-literal spelling intact — the snapshot predates
   Install's rewrite) is restored only if the live entry is STILL the hub entry AND the
   snapshot physically contains the entry (B5).
2. **`absent`** (entryless fanout, no snapshot) — `CASGuardedRemoveEntry(SourceEntryName, match)`:
   remove the hub entry (restore to absence) only if it is still the hub entry (nil-live =
   already-done success).
3. **`present-merged-lower`** (no snapshot; entry resolves from a lower layer the hub never
   wrote, `adopted_entries.go:108-116`) — `CASGuardedRemoveEntry(SourceEntryName, match)`:
   remove the hub write-target entry only if it is still the hub entry; the untouched lower
   layer re-emerges via the adapter's merge. Reported functional-equivalent, distinct from
   `absent`.

Every path is CAS-gated on "live is still the hub entry", so no un-gated destructive write
exists (the `match` check is the integrity gate for `absent`/`present-merged-lower`, which
have no sha256). A CAS conflict (`ErrCASConflict`) is a per-client FAILURE surfaced in the
report (G4), never a silent overwrite.

**Restore honesty (P3 — unknown per-entry fields).** The restore core writes the snapshot's
entry subtree so the env VALUES (the original secret literals) are preserved — the guarantee
the snapshot mechanism exists to deliver. Whether EVERY per-entry field the adapter's on-disk
shape does not model survives is adapter-dependent (an adapter whose set-member re-serializes
through a typed struct can drop an unmodeled sibling field); the round-trip is therefore
honestly labeled `functional-equivalent`, NOT `byte-equivalent`, matching the shipped
restore-mode label (`adopted_entries.go:119-128`). No design claim asserts whole-file or
unmodeled-field byte-identity.

## Manifest delete — the single hash-gated DELETE (F1)

v1 removes ALL clients, so the manifest always ends with zero bindings → a single delete via
`ManifestDeleteInWithHash(dir, rec.ManifestName, rec.ExpectedManifestHash)` (the accepted
decision): re-reads the on-disk manifest and refuses `ErrManifestHashMismatch` if it moved,
FAIL-CLOSED on empty/absent expected hash, path-escape guard retained. Then the supervisor
intent descriptors are removed via the existing uninstall cleanup core
(`install_parsed_manifest.go:1914-2023`). No `ManifestEditInWithHash` lane exists in v1
(subset cut) — confirming P3-B's "no remaining edit-path".

**Atomicity residual (memo item P3-D, downgraded to P3).** The in-call
read→compare→`RemoveAll` window inside `ManifestDeleteInWithHash` is the SAME narrow window
the shipped `ManifestEditInWithHash` already accepts (`manifest.go:713-721`), far narrower
than a plan-time check. Recorded as a bounded residual; an optional manifest-dir flock across
check+delete is a future hardening, not a v1 blocker.

## Routed-secret cleanup (F2 / F-D / P1-4)

The row keeps `RoutedSecretKeys` through `de_adopting`, so de-adopt has the durable list
until close:

1. Delete `RoutedSecretKeys` from the vault BEFORE closing provenance. The row stays
   `de_adopting` (the recoverable state) until cleanup is DONE.
2. **Filter-before-call (F-D).** `deleteAdoptRoutedSecrets` (`adopt_secret_route.go:161`) is
   all-or-nothing (`deleteAdoptRoutedSecretsLocked` errors on ANY `vault.Delete` failure, and
   `vault.Delete` errors on an already-absent key, `vault.go:171-177`). De-adopt PRE-FILTERS
   to still-present keys (a `vault.Get`/`List` pass under `vaultMutex`+`WithVaultLock`) and
   passes only those — so a resume after a partial delete never re-errors on already-gone keys.
3. **Shared-key scan → operator warning + close-predicate fix (P1-4).** Before deleting a
   key, scan other live manifests' env for a `secret:<KEY>` reference; if referenced, SKIP the
   deletion and SURFACE an operator warning (record the skip in the event). **The cleanup-done
   predicate is "every routed key is DELETED **or** deliberately SKIPPED-as-shared"** — a
   skipped key is never absent, so a naive "done when NONE present" predicate would wedge the
   row `de_adopting` forever (P1-4). The skip is durable in the event trail.

## Close = DELETE the row, snapshots-first (memo item 1)

`CloseAdoptProvenance` does NOT flip to a `closed` tombstone. It DELETES the row and the
snapshot dir, snapshots-FIRST, mirroring the shipped `reapAdoptProvenanceRow` /
`abortAdoptProvenance` ordering (`adopted_entries.go:815-882`). Rationale:

- A `closed` tombstone permanently WEDGES re-adopt: capture refuses ANY non-`adopting` prior
  row (`adopted_entries.go:529-531`) and adopt v1 pins `manifest == entry name` (no rename
  dodge), so a `closed` row would block ever re-adopting that server.
- Snapshots-first ordering means a crash between snapshot-removal and row-drop leaves a
  row→missing-snapshot, NEVER a snapshot→no-row secret leak that no GC reaps (the adopt-side
  GC Phase-2 reaps only `adopting` rows; Phase-3 only rowless dirs). Deleting the row keeps
  the shipped at-most-one-row-per-manifest invariant true and collapses P0's `closed` branch
  to `found=false`.
- **B2b — crash INSIDE close is recoverable, not wedged.** Because E6 runs snapshots-first
  and ONLY after E3 (all restored) + E4 (manifest deleted), a crash after `removeAdoptSnapshots`
  and before the row-drop leaves `de_adopting` + manifest ABSENT + snapshot dir ABSENT + every
  client already live≠hub. The resume done-test's crash-inside-close branch (P3:
  snapshot-absent AND manifest-absent AND live≠hub → RESTORE-DONE) classifies all clients done,
  so the retry finishes the row delete instead of routing to "snapshot missing → Failed" and
  wedging. The pre-restore fail-closed rule (live STILL hub + snapshot missing → Failed) is
  KEPT — only the manifest-absent+live≠hub combination signals a mid-close crash. Reachable
  identically via manual snapshot-dir deletion.

## Operation-state machine + roll-forward resume + lock graph

Two transitions in v1: `{adopted, committed-adopting} → de_adopting` (G6) → **row deleted**.
Roll-forward resume (memo PASS — do NOT switch to atomic/full-rollback; rollback would
re-write hub entries over restored native = worse). Every execute step is skip-if-done so a
crash leaves a recoverable `de_adopting` row a retry COMPLETES:

```
BuildDeAdoptPlan(server):
  P0. gate := detect gate-ON via the reserved mcphub-hub entry (hub_gate_detect.go).
      gate-ON -> REFUSE ("gate OFF first, then de-adopt"; item 2 option b).
  P1. rec, found := ReadAdoptProvenance(manifest):
        found=false                          -> REFUSE (not adopt-owned, or already de-adopted;
                                                no `closed` tombstone exists — item 1)
        state == adopted                     -> FRESH   (full plan gates)
        state == adopting WITH a live binding -> FRESH   (committed-but-unflipped; G6 admits it)
        state == adopting WITHOUT a live binding -> REFUSE (pre-install crash orphan; adopt GC owns it)
        state == de_adopting                 -> RESUME  (per-step / per-client done-ness)
  P2. Manifest hash-gate readiness: on-disk manifest hash == ExpectedManifestHash
      (RESUME: SKIP if the manifest file is already absent — delete step done).
  P3. Per client (FRESH): the live entry MUST still be the hub entry
      (liveEntryMatchesManifestBinding); for `present`, the snapshot exists + sha256 matches.
      RESUME per client (done-ness DERIVED from live state — B1 pins the comparator to the
      RAW on-disk subtree, never lean MCPEntry equality):
        live STILL hub:
          present w/ snapshot readable+sha-OK -> restore pending (E3 restores)
          present w/ snapshot missing/unreadable -> Failed (pre-restore fail-closed)
          absent / merged-lower                  -> remove pending (E3 removes)
        live NO LONGER hub:
          present: EntryRawSubtree(live) deep-equals EntryRawSubtree(snapshot) -> RESTORE-DONE
          present: subtree MISMATCH (operator put a THIRD thing there) -> genuine conflict Failed
          present: snapshot ABSENT + manifest ABSENT -> RESTORE-DONE   [B2b crash-inside-close]
          present: snapshot ABSENT + manifest PRESENT -> Failed (anomalous; fail-closed)
          absent / merged-lower: hub write-target entry gone -> RESTORE-DONE
        (a client marked via --accept-conflict is treated as accepted-done, see below)
ExecuteDeAdoptWithOpts(server, targets ≡ rec.AdoptClients):
  E1. lease := tryAcquireAdoptManifestLease(manifest); !ok -> "concurrent operation" REFUSE. defer Unlock.
  E2. MarkAdoptProvenanceDeAdopting(manifest)  (idempotent; adopted/committed-adopting -> de_adopting;
      B4: for a committed-adopting row, RE-VERIFY classifyDeadAdoptingRow==COMMITTED_KEEP under the
      held lease before flipping; refuse otherwise — the adopt GC still owns an uncommitted orphan).
  E3. For each NOT-(RESTORE-DONE|accepted) target client: CAS restore/remove (present/absent/merged-lower)
      BEFORE any topology removal.  (per-client {Restored, Failed} accrues — G4)
  E4. ManifestDeleteInWithHash (skip if manifest already absent) + remove supervisor-intent descriptors.
  E5. Delete still-present RoutedSecretKeys (pre-filtered; skip shared-as-warned).
  E6. CloseAdoptProvenance(manifest) -> DELETE row + snapshots (snapshots-first),
      ONLY when the close-gate holds (see below).
  E7. Emit redaction-safe event + GUI operator-action row + return the {Restored, Failed} report.
```

**Close-gating + the permanent-conflict horn (B2a).** The E-list is roll-forward: E4/E5 MAY
proceed while some target client is Failed — they touch the manifest, supervisor intent, and
vault, NEVER the per-client snapshots, so a Failed client stays RETRYABLE (its snapshot
survives). **E6 (the snapshot+row DELETE) runs ONLY when every target client is RESTORE-DONE
OR accepted AND every routed secret is deleted-or-skipped-as-shared.** Un-gated E6 would
destroy a still-needed snapshot (permanent data loss, retry impossible); a naively-gated E6
would wedge the row `de_adopting` forever the moment ONE client hits a PERMANENT CAS conflict
— the exact case the CAS gate exists for: an operator LEGITIMATELY edited that client's entry
between plan and execute, so its live entry is neither the hub entry (can't restore) nor the
snapshot's entry (already changed), an unsatisfiable predicate. Resolution: the operator
escape `mcphub de-adopt <server> --accept-conflict <client>` marks that one client
terminally-done-with-warning — defensible because a live entry that is no longer the hub entry
means the operator ALREADY took that slot, so restoring the snapshot over it would be the very
data loss we refuse. The accepted client satisfies the close-gate, E6 fires, and the row
closes. The transient window where a still-hub Failed client (an unreadable-snapshot case,
`present`) points at a now-deleted manifest after E4 is the accepted roll-forward
partial-failure state surfaced in the {Restored, Failed} report; the operator fixes the
snapshot-read cause and retries (E3 restores the native entry, which no longer needs the
manifest). An abandoned `de_adopting` row (crash, operator never retries, never accepts) is
the G7 residual — see "Residuals".

**Lock graph (full total order, no reverse edge).** `<manifest>.lease` (E1, outermost, held
E1→E6) → the inners, each transient and mutually NON-nested: `adopted-entries.lock` (each
store mutator), the per-file `config-lock` inside each CAS method (`config_lock.go:51`, one
file at a time), the supervisor-intent lock (E4). Order extends the shipped
`<manifest>.lease → adopted-entries.lock → <snapshot>.lock` (`adopted_entries.go:186-188`).
No IPC/kill/wait runs while any lock is held (supervisor nudge/kill is in the descriptor
core, outside every state lock). v1 acquires NO `hub-mcp.lock` (gate-ON deferred), so the
prior revision's E5 republish-under-hub-lock ordering hazard is gone. No reverse edge: adopt
nests the same direction and the lease is `TryLock`-based.

## Gate-ON refused in v1 (memo item 2, option b) + adjacent bugs

v1 REFUSES gate-ON de-adopt with a "gate OFF first, then de-adopt" message (detected via the
reserved `mcphub-hub` entry, `hub_gate_detect.go`). Rationale: under gate-ON the reconcile
has removed every per-server entry (`install_hub_reconcile.go:233-263`), so
`GetEntry(SourceEntryName)==nil` and the gate-OFF recognizer path would false-refuse
everything; the correct gate-ON model (expected state = "no per-server entry + `mcphub-hub`
present + manifest binding live in the resolver", plus the zero-binding aggregate prune) is a
distinct surface deferred to the follow-up. Two adjacent findings filed (NOT patched here):

- **Adopt-side bug (memo F6):** the same gate-ON entry-removal blinds the SHIPPED
  `classifyDeadAdoptingRow` — a committed-but-unflipped `adopting` row on a gate-ON host has
  no live per-server entry, so it classifies `CRASH_REAP` and the adopt GC destroys the
  snapshots de-adopt needs. Concretely: until the filed bug lands, a committed-but-unflipped
  `adopting` row >24h old on a gate-ON host can lose its snapshots to a subsequent adopt's GC
  BEFORE the operator gates OFF to de-adopt. Bounded (only the promote-flip-crash window
  produces such a row; a fully-committed `adopted` row is GC-immune) and outside de-adopt
  scope, but the operator should gate-OFF-then-de-adopt promptly after such a crash. Filed
  `work-items/bugs/2026-07-11-classify-dead-adopting-row-gate-on-blind.md`.
- **Pre-existing reconcile bug:** `BuildHubReconcilePlan` gate-ON path leaves a stale
  `mcphub-hub` for a client that drops to zero bindings (it `continue`s zero-binding clients
  at `:181-185`; the gate-OFF sweep at `:164-180` removes it, gate-ON does not). Independent
  of de-adopt. Filed `work-items/bugs/2026-07-11-hub-reconcile-gate-on-zero-binding-stale-aggregate.md`;
  the de-adopt-side prune folds into the gate-ON follow-up.

## Residuals (bounded — accepted or deferred; B3)

Stated in the BODY (not only claimed in the gate decision). Each is bounded, operator-driven,
and either accepted for v1 or explicitly deferred to a follow-up:

- **G7 — abandoned `de_adopting` row (DEFERRED recover).** A crash mid-execute followed by an
  operator who never retries (and never `--accept-conflict`s a permanently-conflicted client)
  leaves a `de_adopting` row that (a) WEDGES re-adopt of that manifest — capture refuses ANY
  non-`adopting` prior row (`adopted_entries.go:529-531`) — and (b) RETAINS the secret-bearing
  snapshot dir, which no adopt-side GC reaps (Phase-2 reaps only `adopting`; the row is
  `de_adopting`). This is structurally identical to the `closed`-tombstone wedge the rework
  fixed, but on the abandoned-retry path the failure table only covers the RETRIED path. v1
  does NOT add a `de_adopting`-GC or a `mcphub de-adopt --recover`; both are **DEFERRED to the
  follow-up** (`work-items/backlog/2026-07-11-deadopt-subset-and-gate-on-followup.md`). Bounded
  (owner-only snapshot DACL — a co-resident cannot read the content; operator can delete the
  snapshot dir manually) and operator-driven.
- **G8 — a concurrent hub-entry writer can clobber a just-restored native entry.** A plain
  `mcphub install <server>` / GUI Apply / hub reconcile running concurrently takes only the
  per-file config locks, NOT de-adopt's `<manifest>.lease` — so in the E3→E4 window it can
  rewrite hub entries OVER the just-restored native entries (the CAS gate protects only
  against writers BEFORE de-adopt's own write, not after it). Under a gate-ON reconcile this is
  a `ClientUpdateRemove{EntryName: server}` against the just-restored entry
  (`install_hub_reconcile.go:256-262`). Operator-driven and bounded (the operator is running
  two conflicting topology commands at once); accepted as a one-line residual — de-adopt does
  not extend the lease over the install/reconcile owners in v1.
- **Snapshot > 16 MiB unrestorable (B6 residual).** Until the symmetric adopt-side capture cap
  lands (filed adjacent bug, widened to require capture==restore), a config exceeding the
  16 MiB restore cap adopts fine but de-adopts as a Failed client. Pathological (no realistic
  client config approaches 16 MiB) and bounded; assigned to the adopt-side capture cap.

## Observability + redaction (P2-c — keep) + threat model (P3-E corrected)

**Redaction (unchanged from the prior revision — keep).** De-adopt events/errors/logs carry
ONLY manifest/client names, vault key NAMES, snapshot REFS (paths), counts, and hashes —
NEVER snapshot bytes, restored entry bodies, `command`/`args`/`env` values, or secret
values (mirrors `adopt_provenance_events.go:49-110`; `adopt.go:355` prints routed vault key
NAMES only). A redaction test asserts no secret value in any body/error/narration.

**Threat model (P3-E — corrected; codex P0-1 REFUTED).** The prior revision's "co-resident
flips `present`→`absent`, deletes the snapshot, de-adopt removes the operator's entry" attack
is REFUTED: the SHIPPED anchored reader refuses a wrong-owner file UNCONDITIONALLY in every
mode (`ErrWrongOwner`, `hub_mcp_state_read_inode_windows.go:194` / posix `:135`; owner
allowlist `hub_mcp_state_dacl_windows.go:181-199`). A co-resident who deletes+recreates
`adopted-entries.json` (or a snapshot) owns the replacement with the ATTACKER's SID → the
read fails closed at `ErrWrongOwner` before any field is trusted. **The owner anchor IS the
authenticity root** — de-adopt CREDITS it; it does NOT add a new authenticity mechanism. The
sha256 gate extends that owner-anchored trust to the exact snapshot bytes. Consequences:

- The CAS gate (items 5+7) is motivated by the OPERATOR-EDIT / demigrate-interleave race
  (a legitimate config change between plan and execute), NOT by a co-resident swap. It
  re-reads under the lock and refuses unless the live entry is still the hub entry.
- **Real residual = an allowlisted-owner attacker** (the operator's own account compromised,
  SYSTEM, or BuiltinAdministrators) — outside the co-resident threat model, bounded and
  accepted. `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` buys namespace/confidentiality strictness,
  NOT authenticity (the anchor already provides authenticity in both modes).

## GUI eligibility surface (G3) + per-client report (G4) + CLI

- **G3 — eligibility read-surface (no shape heuristic).** The frontend MUST NOT infer
  de-adopt-eligibility from hub URL shape. Backend provides `GET /api/deadopt/eligible` →
  `{ manifests: [<provenance manifest names from ReadAdoptProvenance>], gate_on: bool }`
  (or an equivalent `adopt_owned` + `deadopt_blocked_reason` field on the scan response). A
  row is eligible iff it is in the provenance set AND `gate_on == false`; gate-ON disables
  the affordance with "gate OFF first".
- **G4 — per-client partial-failure report + CLI exit semantics.** `ExecuteDeAdoptWithOpts`
  returns a `{Restored []string, Failed []{Client, Reason}}` report (precedent
  `DemigrateReport{Restored, Failed}`, `demigrate.go:31-34`). A CAS conflict, an unreadable
  snapshot, or a hash mismatch marks that client Failed and the operation is a partial
  success the operator retries (roll-forward). CLI exit: 0 all-restored; non-zero if any
  client Failed, printing the report.
- **CLI:** `mcphub de-adopt <server>` (alias `deadopt`); `--yes` executes, default dry-run
  prints the plan; no provenance → non-zero, no mutation; gate-ON → non-zero "gate OFF
  first". **`--accept-conflict <client>`** (repeatable) marks a permanently-CAS-conflicted
  client (operator legitimately took the slot between plan and execute) as
  terminally-done-with-warning so the close-gate is satisfiable and the row can close (B2a);
  it never restores or removes that client's entry, only records the accepted conflict in the
  report + event trail.

## Round-trip invariants + failure modes

Invariant: `adopt → de-adopt` restores EVERY `AdoptClient` to its pre-adopt state (pinned
snapshot for `present`; absence for `absent`; re-exposed lower layer for
`present-merged-lower`) and releases every hub-owned artifact adopt created for that
manifest. Restore is functional-equivalent (byte-equivalence UNVERIFIED per adapter,
`adopted_entries.go:120-127`).

| Failure mode | Behavior |
|---|---|
| Gate-ON host | REFUSE with "gate OFF first, then de-adopt" (item 2 b). |
| Snapshot tampered / wrong-owner / oversize / missing (`present`) | Anchored read refuses (owner/reparse/cap) or sha256 mismatch → that client Failed, fail-closed before any write. |
| Entry ABSENT in the verified snapshot (`present`) | Impossible-state (capture guaranteed + sha-pinned it) → `CASRestoreEntryFromBytes` REFUSES fail-closed; NEVER silently removes (B5 — removal is `CASGuardedRemoveEntry`'s alone). |
| Snapshot > 16 MiB restore cap (`present`) | Anchored read refuses (cap) → that client Failed; bounded residual until the symmetric capture cap lands (B6, see Residuals). |
| Live client entry no longer the hub entry (operator edit / demigrate between plan+execute) | CAS `match` fails under the lock → REFUSE that client (Failed), never overwrite (items 5+7). A PERMANENT conflict is cleared by operator revert+retry OR `--accept-conflict <client>` so the row can close (B2a); otherwise the row stays recoverable `de_adopting`. |
| Live entry VANISHED between plan+execute (nil) | `CASGuardedRemoveEntry` nil-live = already-done success; `CASRestoreEntryFromBytes` nil-live = conflict-refuse fail-closed (never resurrect against intent) — B1 nil-live, no panic. |
| Manifest externally edited | `ManifestDeleteInWithHash` refuses `ErrManifestHashMismatch`; empty/absent hash → fail-closed refusal. |
| No provenance row | REFUSE (no `--reconstruct-legacy` in v1). |
| Row `adopting` with no live binding | Pre-install crash orphan — adopt GC owns it; de-adopt refuses. E2 re-verifies COMMITTED_KEEP under the held lease before flipping a committed-adopting row (B4). |
| Routed-secret delete fails / shared key | Pre-filter + shared-scan; row stays `de_adopting`; retry deletes remaining; close-done = deleted-or-skipped-as-shared (P1-4). |
| Crash mid-execute | Recoverable `de_adopting` row; roll-forward resume skips RESTORE-DONE (raw-subtree comparator, B1) + done steps, completes, DELETES the row. Crash INSIDE close (snapshot dir gone + manifest gone + live≠hub) resumes RESTORE-DONE, not wedged (B2b). |
| Abandoned `de_adopting` row (crash, never retried/accepted) | Wedges re-adopt + retains secret-bearing snapshots no GC reaps; `de_adopting`-GC / `--recover` DEFERRED to the follow-up (G7 residual). |
| Gate-ON republish | N/A in v1 (gate-ON refused). |
| Concurrent install / Apply / reconcile (no lease) | Can clobber a just-restored native entry in the E3→E4 window (G8 residual); operator-driven + bounded. |
| Quarantined daemon | Allowed; descriptor removal is independent of daemon health. |

## Test strategy

API/unit (falsification):

1. **T1 — round-trip via PERSISTED provenance only.** Adopt a seeded stdio entry; de-adopt
   from a FRESH `API` instance (no in-memory snapshot); assert every client restored to
   pre-adopt. FAIL if handed an in-memory snapshot.
2. **Snapshot integrity + single-read + B5.** Swap snapshot bytes → sha256 mismatch → that
   client Failed; delete the snapshot → fail-closed; plant a wrong-owner snapshot → anchored
   read refuses; oversize snapshot (> 16 MiB cap) → cap refusal. Single-read: mutate the file
   between the sha256 verify and the restore → restored entry is the VERIFIED bytes (CAS reads
   once). **B5:** a sha-VALID snapshot whose bytes LACK the entry (synthetically constructed
   inconsistency) → `EntryPresentInBytes` false → `CASRestoreEntryFromBytes` REFUSES
   fail-closed, and the live entry is NOT removed (removal stays `CASGuardedRemoveEntry`'s).
3. **CAS operator-edit race (items 5+7) + nil-live (B1).** Between plan and execute, hand-edit
   the live entry to a non-hub entry → CAS `match` fails → REFUSE, no overwrite. Same for a
   `demigrate` interleave. **nil-live:** delete the live entry between plan and execute →
   `CASGuardedRemoveEntry` = success (already gone), `CASRestoreEntryFromBytes` = conflict-refuse
   (no panic, no resurrection).
4. **present / absent / present-merged-lower + restore-core fidelity (B1).** Each
   original_state restores correctly; merged-lower removes the write-target entry and the lower
   layer re-emerges; absent → entry absent; none attempts a snapshot restore without a snapshot.
   **B1 fidelity:** a `present` DIRECT-STDIO source (`command`/`args`/`env` with a literal
   secret) restores via the composed restore core with its `command`/`args`/env-literal VALUES
   intact (the functional-equivalent guarantee — not a whole-file byte claim); a
   `GetEntry`→`AddEntry` husk restore (empty-URL, no command/args) would FAIL this test (the
   falsification that pins the core-composition over the lossy round-trip). A hub-HTTP-shaped
   snapshot entry → guarded-refuse (Failed), not verbatim re-applied (P3 polarity).
5. **Manifest delete hash gate + empty-hash refusal.** Edit the manifest between plan and
   execute → `ManifestDeleteInWithHash` refuses; blank `ExpectedManifestHash` → fail-closed.
6. **Close DELETES the row + re-adopt works (item 1).** After de-adopt, `ReadAdoptProvenance`
   → `found=false`, snapshot dir gone, AND a fresh adopt of the same manifest name SUCCEEDS
   (no `closed` tombstone wedge).
7. **Roll-forward resume + crash-inside-close (B2b) + raw-subtree done-test (B1).** Inject a
   crash after some clients restored; retry SKIPS the RESTORE-DONE clients (P3 done-ness),
   completes the rest + manifest/secret/close, no double-write, row finally deleted. **B2b
   crash-inside-close:** inject a crash after `removeAdoptSnapshots` but before the row-drop
   (manifest already gone, all clients restored) → retry classifies every client RESTORE-DONE
   via the crash-inside-close branch and finishes the row delete (NOT wedged); the pre-restore
   case (live STILL hub + snapshot missing) still Fails closed. **B1 raw-subtree:** a resume
   where an operator replaced a restored stdio entry with a DIFFERENT stdio entry (both project
   to `MCPEntry{URL:""}`) must read as a genuine conflict (raw-subtree mismatch), NOT falsely
   RESTORE-DONE — a lean-MCPEntry comparator would pass this and is the falsification.
8. **Routed-secret pre-filter + shared-key predicate (F-D / P1-4).** Partial delete → retry
   deletes only remaining; a shared key is SKIPPED-as-warned and the row still closes.
9. **Lock order / no re-entrancy.** Assert the total order (lease outermost; inners
   non-nested), no reverse edge, no IPC/kill/wait under a lock, de-adopt↔adopt mutual
   exclusion via the lease. Assert the concrete CAS bodies are LOCK-FREE (the forwarder holds
   the lock; a concrete body re-entering `withConfigLock` would self-deadlock).
10. **Redaction (P2-c).** No secret value / snapshot byte / entry body in any event / error /
    narration.
11. **Gate-ON refusal.** A gate-ON host → `BuildDeAdoptPlan` refuses with the "gate OFF
    first" message, zero mutation.
12. **No-provenance / committed-adopting admission (G6) + E2 re-verify (B4).** No row → refuse;
    a committed-but-`adopting` row (live binding) → admitted as FRESH. **B4:** simulate the
    row becoming UNCOMMITTED between plan-admission and E2 (its hub entries removed) → E2's
    `MarkAdoptProvenanceDeAdopting` re-runs `classifyDeadAdoptingRow` under the held lease and
    REFUSES the flip (adopt GC owns it), rather than converting a GC-reapable row into a
    GC-immune `de_adopting` wedge.
13. **Permanent-conflict `--accept-conflict` close (B2a).** A client with a permanent CAS
    conflict (operator took the slot) → without `--accept-conflict` the row stays recoverable
    `de_adopting` (close-gate unsatisfied, NOT wedged-by-silent-close); with
    `--accept-conflict <client>` that client is accepted-done-with-warning, the close-gate
    holds, E6 fires, the row is deleted, and re-adopt then succeeds. Assert the accepted client
    is never restored/removed and the acceptance is in the report + event trail.

GUI/CLI: eligibility-surface test (affordance only for provenance rows, disabled gate-ON);
`{Restored, Failed}` report + CLI exit; Playwright round-trip (adopt → gate-OFF de-adopt →
scan native); route tests mirroring `gui/adopt_test.go`.

## Claims (falsifiable — `{ guarantee, single-owner, enforcement-probe }`)

1. `{ guarantee: v1 de-adopt is atomic over ALL AdoptClients of one manifest (targets ≡ rec.AdoptClients), so the resume scope is a fixed set needing no journaled target list; single-owner: BuildDeAdoptPlan reading rec.AdoptClients; enforcement-probe: test 7 (resume) + the absence of any subset code path }`
2. `{ guarantee: "the live entry is our hub entry" has exactly ONE equality owner — the shipped liveEntryMatchesManifestBinding — no byte-exact recompute, no second shape owner; single-owner: managed_entries.go:355; enforcement-probe: grep shows no byte-exact entry reconstruction + no second recognizer in deadopt.go }`
3. `{ guarantee: the snapshot is read through the anchored reader with the path recomputed from (ManifestName, Client), refusing wrong-owner/reparse/oversize before hashing; single-owner: ReadStateFileInodeAnchored (state_read_inode_anchor.go:22) + adoptSnapshotDir; enforcement-probe: test 2 (wrong-owner + oversize + tamper) }`
4. `{ guarantee: every destructive client-config write is COMPARE-AND-SWAP under the lock held by the lockingClient forwarder (concrete bodies lock-free) — refuse unless the live entry is still the hub entry; nil-live is per-branch (remove=success, restore=conflict-refuse); a present-restore whose verified snapshot lacks the entry REFUSES fail-closed (removal is never silent); single-owner: the CAS capability methods (clients) + the injected recognizer predicate; enforcement-probe: test 3 (operator-edit race + nil-live) + test 2 (B5 absent-in-snapshot) }`
5. `{ guarantee: CloseAdoptProvenance DELETES the row + snapshots (snapshots-first), leaving no `closed` tombstone, so re-adopt of the same manifest succeeds; single-owner: CloseAdoptProvenance mirroring reapAdoptProvenanceRow (adopted_entries.go:860-882); enforcement-probe: test 6 (re-adopt after de-adopt) }`
6. `{ guarantee: the last-binding manifest delete is hash-gated at the mutation point, fail-closed on empty hash, path-escape guard retained; single-owner: ManifestDeleteInWithHash (accepted decision); enforcement-probe: test 5 }`
7. `{ guarantee: routed keys are deleted before close; a shared key is skipped-as-warned and the close-done predicate is deleted-OR-skipped so the row never wedges; single-owner: the de-adopt cleanup ordering + pre-filtered deleteAdoptRoutedSecrets; enforcement-probe: test 8 }`
8. `{ guarantee: a crash mid-execute leaves a recoverable de_adopting row that roll-forward resume COMPLETES (skips RESTORE-DONE clients + done steps) — including a crash INSIDE close (snapshot dir + manifest gone + live≠hub → RESTORE-DONE, not wedged) — never a rollback that re-writes hub over native; the resume done-test compares RAW on-disk subtrees, not lean MCPEntry equality; single-owner: BuildDeAdoptPlan RESUME done-ness derivation + EntryRawSubtree comparator; enforcement-probe: test 7 (crash-inside-close + two-stdio-husk falsification) }`
9. `{ guarantee: gate-ON de-adopt is refused with an actionable message and zero mutation in v1; single-owner: BuildDeAdoptPlan P0 gate check; enforcement-probe: test 11 }`
10. `{ guarantee: the bytes-restore/guarded-remove live on a CAPABILITY interface implemented only by adopt-reachable adapters, not the Client interface; single-owner: the CASEntryMutator capability (mirrors EntryBytesChecker); enforcement-probe: grep shows no Client-interface restore method + a fail-closed type-assert at the de-adopt site }`
11. `{ guarantee: v1 does NOT modify BuildHubReconcilePlan or add a single-owner entry renderer (both deferred with gate-ON); single-owner: the recognizer-only equality + gate-OFF-only scope; enforcement-probe: git diff shows no change to install_hub_reconcile.go and no new entry-renderer }`
12. `{ guarantee: no secret value / snapshot byte / entry body appears in any de-adopt event/error/log; single-owner: the de-adopt redaction; enforcement-probe: test 10 }`
13. `{ guarantee: the owner anchor (wrong-owner refusal in both modes) is the authenticity root — de-adopt adds no new authenticity mechanism; single-owner: readStateFileInodeAnchoredWithOptions ErrWrongOwner (hub_mcp_state_read_inode_windows.go:194 / posix:135); enforcement-probe: a wrong-owner snapshot/store read fails closed (test 2) }`
14. `{ guarantee: the restore payload is produced by COMPOSING the shipped per-adapter restore core (a behavior-preserving restoreEntryFromBytes refactor), never a lossy GetEntry→AddEntry round-trip and never a parallel per-adapter extraction owner; single-owner: the refactored restoreEntryFromBytes core (one extraction owner across ~15 adapters); enforcement-probe: test 4 (direct-stdio command/args/env survive byte-faithfully — a husk restore fails) + grep shows no GetEntry→AddEntry restore path and no second extraction impl in the CAS bodies }`
15. `{ guarantee: E6 (snapshot+row DELETE) runs ONLY when every target client is RESTORE-DONE-or-accepted AND every routed secret is deleted-or-skipped, so no still-needed snapshot is destroyed; a PERMANENT CAS conflict is closed via --accept-conflict (never a silent close, never an infinite wedge on the retried path); single-owner: the ExecuteDeAdopt close-gate; enforcement-probe: test 13 }`
16. `{ guarantee: E2 re-verifies classifyDeadAdoptingRow==COMMITTED_KEEP under the held lease before flipping a committed-adopting row, so a plan-time admission stale by execute cannot convert a GC-reapable orphan into a GC-immune de_adopting wedge; single-owner: MarkAdoptProvenanceDeAdopting under the lease; enforcement-probe: test 12 (B4) }`
17. `{ guarantee: the de-adopt restore read cap is a bounded client-config size (16 MiB) and the symmetry invariant (restore cap ≥ capture max) is stated + assigned — the residual > cap config is bounded and directed to the adopt-side capture cap; single-owner: the .snapshot suffix clause in stateFileReadCapBytes + the filed adopt-side capture-cap bug; enforcement-probe: test 2 (oversize cap refusal) + the Residuals section }`

## Provenance-gap flag

No provenance-CODE gap requires patching the shipped store. One under-specified detail the
memo's item 4 implies but did not spell out (I did NOT patch provenance; this is a read-cap
OWNER extension de-adopt authors):

- **Snapshot read cap + symmetry (B6).** `stateFileReadCapBytes` (`state_read_caps.go:28-42`)
  defaults a `<client>.snapshot` to `maxStateFileBytes` (1 MiB). A real client config (e.g. a
  large `~/.claude.json`) can exceed 1 MiB, and adopt's capture side wrote it with NO size
  limit (`WriteStateFileBytesAtomic`), so a legitimate large snapshot would FAIL the anchored
  read back. De-adopt MUST add a snapshot cap kind (client-config-sized, bounded — 16 MiB like
  intent/vault) via a `.snapshot`-SUFFIX clause (NOT an exact-basename `case` — the basename is
  variable), alongside the same-shape `isSecretBearingStateFilePath` `.snapshot`-suffix
  addition. **Symmetry invariant:** the restore cap must be ≥ any snapshot capture can pin;
  because capture is uncapped, true symmetry requires a matching CAPTURE-time cap (adopt-side,
  a protected surface in v1) — the filed adjacent bug is directed to enforce
  capture==restore, not merely raise the read cap. The residual > 16 MiB config is bounded +
  assigned (Residuals). Additive shared-owner change, flagged so the planner scopes it and the
  FABLE audit sees it was caught. (Adjacent finding filed.)

Two de-adopt-OWNED implementation shapes (not provenance defects): the CAS capability
interface (items 5+7) and the routed-secret pre-filter (F-D). De-adopt does NOT reopen the
tracked provenance residuals (`work-items/backlog/2026-07-10-adopt-provenance-lease-hygiene.md`).

## Adjacent findings (filed, NOT in v1 scope)

1. `work-items/bugs/2026-07-11-classify-dead-adopting-row-gate-on-blind.md` — adopt-side:
   gate-ON entry-removal blinds the shipped `classifyDeadAdoptingRow` (committed row →
   CRASH_REAP → snapshots destroyed). `context: adjacent-finding`, `status: open`.
2. `work-items/bugs/2026-07-11-hub-reconcile-gate-on-zero-binding-stale-aggregate.md` —
   pre-existing: `BuildHubReconcilePlan` gate-ON leaves a stale `mcphub-hub` for a
   zero-binding client. `context: adjacent-finding`, `status: open`.
3. `work-items/bugs/2026-07-11-adopt-snapshot-read-cap-too-small.md` — the snapshot read-cap
   gap above (a large legitimate client-config snapshot would fail the anchored read).
   `context: adjacent-finding`, `status: open`. **B6 directs widening it:** the fix must
   enforce the capture==restore SYMMETRY (an adopt-side capture-time cap == the de-adopt
   restore cap), not merely raise the read cap — the planner should update the bug's fix
   direction accordingly.

Follow-up work-item stub (subset + gate-ON de-adopt):
`work-items/backlog/2026-07-11-deadopt-subset-and-gate-on-followup.md`.

## Gate decision

**PASS (rework 2026-07-11 round 2 — 3-fable audit fold-in).** v1 is scoped to
all-clients-only, gate-OFF-only atomic de-adopt (subset + gate-ON + `--reconstruct-legacy`
cut, per the LEAD decision). All 7 round-1 BLOCKING must-fixes remain resolved: (1) close
DELETES the row snapshots-first; (2) gate-ON REFUSED with a message + adjacent bugs filed;
(3) equality via the single shipped recognizer + URL formula corrected, no byte-exact; (4)
anchored snapshot read + recomputed path + secret-bearing/cap additions; (5) CAS on a
capability interface, not Client; (6) reconcile prune deferred with gate-ON + latent bug
filed; (7) client-config CAS mutation-point gate.

Round-2 audit fixes folded in (all design-text; no architecture change): **B1** — the
restore payload composes the SHIPPED per-adapter restore core (behavior-preserving
`restoreEntryFromBytes` refactor), banning the lossy `GetEntry`→`AddEntry` round-trip and a
second extraction owner; the resume done-test compares RAW subtrees (`EntryRawSubtree`), not
lean `MCPEntry`; the nil-live per-branch contract is defined; blast-radius change (2) upgraded
to "additive interface + behavior-preserving restore-body refactor." **B2a** — E6 gated on
all-clients-RESTORE-DONE-or-accepted + secrets-done, the permanent-CAS-conflict horn resolved
with `--accept-conflict <client>`. **B2b** — crash-inside-close resume branch added
(snapshot+manifest gone + live≠hub → RESTORE-DONE), fail-closed missing-snapshot KEPT for the
pre-restore case. **B3** — G7 (abandoned `de_adopting` row; recover DEFERRED) + G8 (concurrent
no-lease writer) + the > cap residual now stated in the BODY (new "Residuals" section), so the
gate decision no longer over-claims them. **B4** — E2 re-verifies committed-ness under the held
lease. **B5** — restore fail-closes on entry-absent-in-verified-snapshot (removal stays
`CASGuardedRemoveEntry`'s). **B6** — snapshot cap symmetry invariant stated + assigned. P3
fold-ins: CAS lock ownership pinned (forwarder holds the lock, concrete bodies lock-free),
guarded-refuse restore polarity, `.snapshot`-SUFFIX clauses (not exact basename), C6
landing-comment direction (repoint `UpdateAdoptExpectedManifestHash` + `:155-158` + the mutator
tombstone at the subset follow-up), unknown-field-drop honesty sentence, gate-ON F6 sentence,
doc-anchor cleanup (`adopt.go:355`; dropped the "(T6)" test label).

PASS items kept unchanged (roll-forward resume, the `ManifestDeleteInWithHash` decision,
lock/lease/redaction, routed-secret namespacing, demigrate-NOT-reused, composing shipped
owners, the all-clients-only cut). Consistent with the two accepted decisions
(`2026-07-11-deadopt-v1-all-clients-only-scope`, `2026-07-10-deadopt-manifest-delete-hash-gate`)
— `--accept-conflict` is a conflict-resolution escape WITHIN the atomic all-clients operation
(targets ≡ `AdoptClients` unchanged), not a reintroduction of subset de-adopt. Next stage per
the audit memo: a DELTA-CHECK of the amended sections (restore primitive, close/resume machine,
gate decision, change-surface) — NOT a full re-audit — then `$planner`.
