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

### Round 4 (2026-07-11) — P1-a atomic classification seam (design-text only; no architecture change)

Source of truth: a mandatory second-reviewer (codex gpt-5.6-sol xhigh) DELTA-recheck of round-3
(`7a208643`). It confirmed **P1-b CLOSED** but found **P1-a only PROSE-closed**: round-3 mandates
that `--accept-conflict` be honored only after a mutation-point re-read PROVES a genuine conflict,
but the declared capability surface exposed NO callable seam that atomically classifies
{still-hub, restore-done, genuine-conflict, unreadable} READ-ONLY UNDER the held config lock — the
two CAS methods MUTATE, `EntryRawSubtree` is a pure lock-free bytes function, and merged
`lockingClient` leaves `GetEntry` UNLOCKED (`config_lock.go:160-173`). An unlocked live read reopens
the plan→execute TOCTOU the CAS gate exists to close; a mutating CAS method cannot "test" a still-hub
client without changing it. This round adds that ONE seam and wires BOTH consumers onto it — no scope,
security-core, or protected-surface change:

- **NEW read-only capability method `ClassifyEntryUnderLock` on `CASEntryMutator`** (change (2),
  mirroring the shipped `EntryBytesChecker` capability pattern): its `lockingClient` forwarder HOLDS
  `withConfigLock` across the live read + the raw-subtree deep-compare (SUPERSEDED in round 5 — the
  forwarder holds the read-selection `withConfigReadLock` and reads the WRITE-TARGET-PHYSICAL bytes,
  see Round 5); the concrete body is read-only + lock-free (same non-reentrant-mutex constraint as
  the CAS bodies, `config_lock.go:24-30`).
- **The E3 `--accept-conflict` acceptance decision AND the resume/plan done-ness derivation BOTH call
  this ONE capability** — a single atomic classification owner. The round-3 resume derivation's
  PARALLEL UNLOCKED `os.ReadFile(adapter.ConfigPath())` live read is REMOVED (it was the very TOCTOU
  the seam closes); the snapshot- and manifest-availability dimensions (not config-lock operations)
  stay api-layer and compose with the classifier's live verdict.
- **New falsification test T15** — read-only assertion + a concurrent live-config state change vs the
  classifier + a still-hub client passed to `--accept-conflict` REFUSED with its snapshot INTACT.

### Round 5 (2026-07-11) — write-target-physical collapse + adopt-GC dependency (design-text only; no architecture change)

Source of truth: a mandatory fable-5 adversarial review of round-4 (`d9dbec56`). It confirmed the
round-4 seam but found the seam's live-read WHICH-BYTES decision introduced a NEW P1 and left the
adopt-GC dependency unfenced. This round changes ONLY which bytes `ClassifyEntryUnderLock` reads and
adds the dependency edge — no scope, security-core, or protected-surface change:

- **P1-B (the fix — a COLLAPSE, not another edge).** Round-4 pinned the classify live `*MCPEntry` +
  raw-subtree derivations to "the same code `GetEntry` wraps." For the **mimocode** adapter `GetEntry`
  (`mimocode.go:3868-3951`, via `readJSON`) is a MERGED multi-layer view (write target + lower
  `config.json` + `~/.claude.json` import + higher overlay/env/MDM), NOT one file's bytes — so after
  a successful `CASGuardedRemoveEntry` on a `present-merged-lower` client the lower entry re-emerges
  in the merged view (live≠nil, match==false → `GenuineConflict`) when the TRUTH is `RestoreDone`
  (a permanent CLOSE-READY block, or a coerced `--accept-conflict` whose snapshot-destruction warning
  is meaningless), and the merged projection reads overlay/import files the held `withConfigLock(ConfigPath)`
  does not cover (an atomicity void vs claim 18). **FIX:** pin BOTH derivations to the WRITE-TARGET
  physical bytes read once under the lock — the adapter's single-file section parse, the SAME owner
  `EntryPresentInBytes` documents (`entry_bytes.go:95-103`) — NEVER the multi-layer `GetEntry`. Stated
  uniformly for every adapter: StillHub/RestoreDone/GenuineConflict are judged WRITE-TARGET-PHYSICAL,
  correct-by-construction (adopt wrote the write target ⟹ merged-lower success ≡ write-target absence;
  higher/lower layers are operator-owned, out of classify scope). This is the mimocode multi-layer
  re-resolve class — collapsed to ONE predicate, NOT a mimocode special case.
- **P1-A (adopt-GC dependency — unfenced AT ROUND 5; SINCE CLOSED by #531/#532).** De-adopt's
  per-manifest lease does NOT by itself fence the adopt GC's stale-candidate reap (the GC's decision
  inputs pre-date the lease). Added `Depends-on:` for the two filed adopt-GC bugs to `status.md` + a
  new "Adopt-GC dependency" section, and corrected design:564/:795 which asserted a reap-time
  `adopting`-only filter `reapAdoptProvenanceRow` (`adopted_entries.go:946-978`) did NOT have AT
  ROUND-5 AUTHORING TIME (it was a Phase-1 SELECTION property; the reap primitive then dropped by
  manifest NAME). SUPERSEDED at HEAD: #531 has since ADDED that `(state, UpdatedAt)` identity gate to
  `reapAdoptProvenanceRow` and #532 hardened the classifier, so both `Depends-on` edges are now MET —
  see "Adopt-GC dependency".
- **P2** — a cleanly-absent config file (`os.IsNotExist`) maps to empty-config (live==nil, raw-absent),
  never `ClassifyUnreadable` (which would wedge a `present` client permanently). `Unreadable` reserved
  for genuine read/parse errors on a PRESENT file.
- **P3-a** — the classify forwarder holds `withConfigReadLock` (the missing-dir short-circuit,
  `config_lock.go:150-158`) so a plan-time classify of an absent config has NO FS side effect (no
  `SecureCreateParentDir`, no `.lock` file), delegating to the SAME `withConfigLock` flock when the
  config exists.
- **P3-b** — G8 residual extended: the accepted-`GenuineConflict` verdict is a point-in-time proof at
  E3, not re-checked at E6 (a between-E3-and-close reversion-to-hub is bounded/benign; a reversion
  ALREADY landed AT E3-time IS caught → StillHub → reject).
- **New falsification tests T16** (merged-lower write-target-physical → RestoreDone, not the merged
  GenuineConflict) **+ T17** (cleanly-absent config → empty-config, not Unreadable); **claim 19**
  added (write-target-physical single owner); claim 18 re-anchored on `withConfigReadLock`.

## Change-Surface Contract

De-adopt OWNS this seam decision; the planner and implementers CONSUME it (may
`REVISE`-to-architect on conflict, may NOT redefine it).

- **Intended change surface:**
  - NEW `internal/api/deadopt.go` — `BuildDeAdoptPlan` + `ExecuteDeAdoptWithOpts` (the
    de-adopt owner, sibling to `adopt.go`), plus the de-adopt-owned provenance mutators
    the shipped store declared as comments (`adopted_entries.go:1294-1296`):
    `MarkAdoptProvenanceDeAdopting` (transition `{adopted, committed-adopting} →
    de_adopting`, G6; **B4** — for a `committed-adopting` row it RE-RUNS
    `classifyDeadAdoptingRow == adoptRowCommittedKeep` under the HELD lease before flipping and
    refuses otherwise, so a plan-time admission stale by execute cannot flip a
    now-uncommitted orphan the adopt GC still owns) and `CloseAdoptProvenance` (now
    **deletes** the row snapshots-first — G1/item 1). **C6 landing direction:**
    `UpdateAdoptExpectedManifestHash` is DECLARED-but-UNUSED in v1 (subset cut); the
    implementation change MUST repoint its declaration comment (`adopted_entries.go:1295`)
    and the `ExpectedManifestHash` field doc (`:156-161`, which today reads "de-adopt
    updates ExpectedManifestHash after a subset binding edit") at the subset FOLLOW-UP,
    and update the mutator tombstone (`:1284-1297` — v1 authors 2 of the 3) so the live tree
    asserts only the current state (stale-relation hygiene, arch law C6). Uses the store's
    own same-package unexported helpers (`withAdoptedEntriesLock`,
    `readAdoptedEntries`, `writeAdoptedEntries`, `removeAdoptSnapshots`, `adoptSnapshotDir`,
    `tryAcquireAdoptManifestLease`, and the `reapAdoptProvenanceRow` snapshots-first
    ordering `adopted_entries.go:946-978`).
  - `internal/api/manifest.go` — ADD shared `ManifestDeleteInWithHash` (delete-mutation-
    point hash gate; FAIL-CLOSED on empty/absent hash; RETAINS the path-escape guard
    `manifest.go:793-796`). Decision `2026-07-10-deadopt-manifest-delete-hash-gate.md`
    (`accepted`). This is the ONLY manifest mutation in v1 (always a last-binding delete).
  - `internal/clients/*` — ADD a **CAS + read capability interface** (mirrors the shipped
    `EntryBytesChecker` capability pattern, `entry_bytes.go:24`), NOT a `Client`-interface
    method: `CASRestoreEntryFromBytes` + `CASGuardedRemoveEntry` + the read-only
    `EntryRawSubtree` (the pure raw-subtree extractor) + the read-only `ClassifyEntryUnderLock`
    (the P1-a atomic classification seam — its `lockingClient` forwarder HOLDS the read-selection
    `withConfigReadLock` across the WRITE-TARGET-PHYSICAL live read + compare, delegating to the
    SAME `withConfigLock` flock the CAS mutators hold when the config exists and short-circuiting
    the parent-dir/`.lock`-file creation when it is absent — P1-b/P3-a; UNLIKE the lock-free
    `EntryRawSubtree`/`EntryPresentInBytes` forwards), implemented by exactly the
    adopt-reachable adapters, forwarded by `lockingClient`, fail-closed at the de-adopt
    site if an adapter does not implement it (memo item 5 + 7; P1-a). **B1 — the mutating CAS
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
  - **ENFORCEMENT (Phase-3 constraint — fable-5 Phase-2 audit, 2026-07-14): the
    "implemented by exactly the adopt-reachable adapters" property CANNOT be enforced by
    interface-satisfaction / method-set alone.** `windsurfClient` (`windsurf.go:156/163`,
    NOT adopt-reachable — it overrides `RestoreEntryFromBackup*` with a `serverUrl`-aware
    body) EMBEDS `jsonMCPClient`, and Phase 2 added the base `restoreEntryFromBytes` to
    `jsonMCPClient`; embedding PROMOTES that base method (and would promote any Phase-3
    CAS/classify method added to the base) onto windsurf's method set. So a plain
    `client.(CASEntryMutator)` type-assert at the de-adopt site would SUCCEED for windsurf
    (and every other `jsonMCPClient` embedder) via promotion, with base `url`-field /
    `mcpServers` semantics that CONTRADICT windsurf's own restore — the same latent gap
    `EntryBytesChecker` has (windsurf satisfies it via promotion too, but is kept out by the
    explicit compile-proof set at `entry_bytes.go:31-39`). **Phase-3 MUST gate the
    CAS/classify capability by an EXPLICIT ALLOWLIST / MARKER (mirror the `EntryBytesChecker`
    compile-proof set), NOT the bare type-assert.** Options: add the CAS methods to each
    adopt-reachable adapter as DISTINCT (non-promoted) methods rather than on the shared
    `jsonMCPClient` base, OR keep an explicit adopt-reachable allowlist the de-adopt site
    consults BEFORE the type-assert. Zero callers today (Phase 2 added only the unexported
    restore core), so this is a Phase-3 design constraint, not a current defect.
  - `internal/api/state_read_caps.go` + `internal/api/state_read_inode_anchor.go` — two
    ADDITIVE lines: give the adopt-provenance `.snapshot` kind a client-config-sized read
    cap in `stateFileReadCapBytes` (`:28`; the default is only `maxStateFileBytes` = 1 MiB,
    too small for a real `~/.claude.json` snapshot — see "Provenance-gap flag") and mark it
    secret-bearing in `isSecretBearingStateFilePath` (`:42`, memo item 4 P3-A). **HEAD update
    (#532):** the secret-bearing `.snapshot`-suffix clause has ALREADY landed
    (`state_read_inode_anchor.go:59-61`); only the `stateFileReadCapBytes` read-cap clause
    remains for de-adopt to add.
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
    `adopted_entries.go:1284-1297`); de-adopt lives in `internal/api` and uses the store's
    unexported RMW helpers.
  - D2 — the CAS + read capability interface (item 5+7; P1-a): the destructive-write atomicity
    seam PLUS the read-only `ClassifyEntryUnderLock` classification seam (the accept/resume
    done-ness owner). Its restore body COMPOSES the shipped per-adapter restore core via a
    behavior-preserving `restoreEntryFromBytes` extraction (B1) — no new restore logic, no second
    extraction owner; `ClassifyEntryUnderLock` reuses the SAME `EntryRawSubtree` extractor + the
    SAME injected recognizer, adding NO second equality owner, and reads the WRITE-TARGET-PHYSICAL
    bytes (the `EntryPresentInBytes` single-file section owner) — never the merged multi-layer
    `GetEntry` (P1-b).
  - D3 — the shipped recognizer `liveEntryMatchesManifestBinding` (`managed_entries.go:355`)
    reused read-only as the SINGLE "is the live entry our hub entry" equality owner (memo
    item 3). No second shape-derivation path.
  - D4 — the anchored secure reader `ReadStateFileInodeAnchored` (`state_read_inode_anchor.go:22`)
    for the snapshot read (item 4).
  - D5 — the routed-secret deleter `deleteAdoptRoutedSecrets` (`adopt_secret_route.go:161`),
    pre-filtered to still-present keys (F-D).
  - D6 — the per-manifest adopt LEASE `tryAcquireAdoptManifestLease` (`adopted_entries.go:390`)
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
  interface (the two mutating CAS methods + the read-only `EntryRawSubtree` extractor + the
  read-only `ClassifyEntryUnderLock` classification seam, P1-a) AND a behavior-preserving
  extraction refactor of the per-adapter restore bodies**
  (`restoreEntryFromBackup` → a `restoreEntryFromBytes` core + a thin file-reading wrapper;
  every shipped `RestoreEntryFromBackup*` caller stays byte-unchanged — B1); (3) the two
  additive lines in `state_read_caps.go` + `state_read_inode_anchor.go` for the `.snapshot`
  kind (a SUFFIX/dir-segment clause, not an exact basename — P3; the
  `state_read_inode_anchor.go` secret-bearing half ALREADY landed via #532, so only the
  `state_read_caps.go` read-cap line remains for de-adopt). Changes (1) and (3) are
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
// adopted_entries.go:150-167
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
// adopted_entries.go:133-145
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
`classifyDeadAdoptingRow` (`adopted_entries.go:517`) use. Inputs, exactly as
`classifyDeadAdoptingRow` builds them (`:499-516`):

- `m := &config.ServerManifest{Name: rec.ManifestName, Daemons: [{Name:"default", Port: rec.Port}]}`
- `binding := config.ClientBinding{Client: c, Daemon:"default", URLPath:"/mcp"}`
- `live := adapter.GetEntry(rec.SourceEntryName)` — this is how the SHIPPED `classifyDeadAdoptingRow`
  builds its own live input; de-adopt REUSES the `m`/`binding` construction but NOT this `GetEntry`
  read. De-adopt's `ClassifyEntryUnderLock` derives `live` from the WRITE-TARGET-PHYSICAL bytes (the
  `EntryPresentInBytes` single-file section owner), never the merged multi-layer `GetEntry` (P1-b),
  and passes THAT `*MCPEntry` into the injected recognizer.

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

**`EntryRawSubtree` is NOT a second recognizer (claim 2 intact).** The raw-subtree comparator
(B1) answers a DIFFERENT question — "is the live entry the SNAPSHOT's entry we already restored"
— by deep-comparing verbatim on-disk subtrees, NOT "is the live entry the hub entry" (which stays
single-owned in `liveEntryMatchesManifestBinding`). Two distinct predicates, two distinct owners;
the hub-equality question keeps exactly one owner. **That deep-compare now lives inside the ONE
read-only `ClassifyEntryUnderLock` seam (P1-a)**, which under the held read-selection config lock
reads the WRITE-TARGET-PHYSICAL live entry (the `EntryPresentInBytes` single-file section owner,
`entry_bytes.go:95-103` — never the merged multi-layer `GetEntry`, P1-b), applies the injected hub
recognizer (StillHub), and deep-compares the live raw subtree to the caller's verified snapshot
subtree (RestoreDone vs GenuineConflict) — `EntryRawSubtree` is the single PURE extractor it and the
api-layer snapshot read both call, never a parallel comparator.

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
  `adopted_entries.go:285-303`), NOT from `SnapshotRef` as a raw path. `SnapshotRef` is
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
  `WriteStateFileBytesAtomic` with NO size limit (`adopted_entries.go:285-303`), so a config
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

    // ClassifyEntryUnderLock is the READ-ONLY classification seam (P1-a). UNLIKE
    // EntryRawSubtree (a pure bytes function, LOCK-FREE forward), its lockingClient
    // FORWARDER HOLDS withConfigReadLock(ConfigPath) across the WHOLE call — the
    // READ-SELECTION variant of withConfigLock (config_lock.go:150-158). When the
    // config's parent dir EXISTS it delegates to the SAME withConfigLock flock the CAS
    // mutators hold, so the live read + compare are atomic w.r.t. any concurrent writer
    // (the SAME operator-edit / demigrate race the CAS mutators close; merged lockingClient
    // leaves the read-only GetEntry UNLOCKED — config_lock.go:160-173 — so an unlocked
    // GetEntry here would reopen the plan→execute TOCTOU). When the parent dir is ABSENT
    // it short-circuits to the in-process mutex only, creating NO parent dir and NO
    // `<config>.lock` file — because the read-only advisory plan-time classify (one call
    // per target client) must have NO plan-time filesystem side effect, and withConfigLock
    // unconditionally SecureCreateParentDir + drops a `.lock` file (config_lock.go:117-132)
    // (P3-a; mirrors the shipped backup-READ overrides, config_lock.go:285-316, which use
    // withConfigReadLock for exactly this reason). The concrete body runs UNDER that held
    // lock and is itself LOCK-FREE (same non-reentrant-mutex constraint, config_lock.go:24-30)
    // and NEVER MUTATES.
    //
    // WRITE-TARGET-PHYSICAL (the P1-b collapse — ONE predicate, no per-adapter edge). The
    // body does ONE live read of the adapter's WRITE-TARGET physical file bytes (ConfigPath)
    // and derives BOTH the live *MCPEntry for `match` AND the live raw subtree from the SAME
    // single-file section parse — the PHYSICAL-presence owner EntryPresentInBytes already
    // documents (entry_bytes.go:95-103: mimocode presence is "PHYSICAL presence in the write
    // target"; lower/import layers route to merged-lower BEFORE the byte check) — applied here
    // to derive the entry + its raw subtree via the SAME EntryRawSubtree extraction. It is
    // NEVER the multi-layer GetEntry: for mimocode GetEntry is a MERGED view over write target
    // + lower config.json + ~/.claude.json import + higher overlay/env/MDM (mimocode.go:3868-3951,
    // via readJSON) which (a) reads files the held ConfigPath lock does NOT cover — an atomicity
    // void — and (b) after a SUCCESSFUL merged-lower CASGuardedRemoveEntry re-surfaces the
    // lower-layer entry so live≠nil/match==false → a FALSE ClassifyGenuineConflict when the truth
    // is ClassifyRestoreDone (a permanent CLOSE-READY wedge, or a coerced --accept-conflict whose
    // snapshot-destruction warning is meaningless because merged-lower has no snapshot). For
    // EVERY adapter StillHub/RestoreDone/GenuineConflict are judged WRITE-TARGET-PHYSICAL,
    // CORRECT BY CONSTRUCTION: adopt wrote the write target, so merged-lower success ≡
    // write-target absence, and the higher/lower layers are operator-owned and out of classify
    // scope. For a single-file adapter the write-target parse EQUALS GetEntry, so the rule is a
    // no-op change there and uniform everywhere.
    //
    // A cleanly-ABSENT config file (os.IsNotExist) is NOT unreadable — it maps to live==nil +
    // raw-ABSENT (empty-config), exactly as every adapter's GetEntry treats IsNotExist; only a
    // genuine read/parse error on a PRESENT file is ClassifyUnreadable (P2).
    //
    // `match` is the injected hub recognizer (dependency inversion — like the CAS mutators;
    // the equality owner stays liveEntryMatchesManifestBinding in api). It nil-guards `live`
    // BEFORE calling match (the recognizer derefs live.URL, managed_entries.go:378).
    // `snapshotSubtree` is the caller's ALREADY-verified (api-layer anchored-read + sha-gated)
    // snapshot raw subtree for a `present` client, or nil when the fully-restored state is
    // ABSENCE (absent / merged-lower; also passed nil when a `present` client's snapshot is
    // UNAVAILABLE on resume — see the resume derivation, which then consumes only the
    // StillHub / not-StillHub / Unreadable distinction). Returns:
    //   - ClassifyStillHub       : the WRITE-TARGET-PHYSICAL live entry != nil AND match(live)
    //                              — the hub entry adopt wrote is still in the write target
    //                              (restore/remove PENDING; and the P1-a REJECTION of
    //                              --accept-conflict).
    //   - ClassifyRestoreDone    : NOT still hub AND the post-op success state already holds —
    //                              for a present snapshotSubtree, the WRITE-TARGET-PHYSICAL live
    //                              raw subtree DEEP-EQUALS snapshotSubtree; for nil snapshotSubtree
    //                              (absent / merged-lower), the entry is ABSENT FROM THE WRITE
    //                              TARGET (a merged-lower's re-emerged lower layer is out of scope).
    //   - ClassifyGenuineConflict: NOT still hub AND neither the hub entry NOR the success state —
    //                              a THIRD entry the operator put there (present: WRITE-TARGET-PHYSICAL
    //                              live subtree ≠ snapshotSubtree, or write-target live==nil against a
    //                              present snapshot; absent/merged-lower: a non-hub entry occupies the
    //                              WRITE-TARGET slot). accept-eligible.
    //   - ClassifyUnreadable     : a genuine read/parse ERROR on a PRESENT write-target config file
    //                              under the lock — fail-closed (NEVER proves a conflict; NEVER
    //                              accept-eligible). A cleanly-ABSENT file (os.IsNotExist) is NOT this
    //                              — it is empty-config (live==nil, raw-absent), never Unreadable (P2).
    // Deep compare is reflect.DeepEqual over the verbatim subtrees (the SAME comparator the
    // round-3 resume done-test used, now single-owned here). NEVER MUTATES.
    ClassifyEntryUnderLock(name string, match func(*MCPEntry) bool, snapshotSubtree any) (EntryClassification, error)

    // Read-only PURE bytes function (LOCK-FREE forward like EntryPresentInBytes; NO disk
    // read): the VERBATIM on-disk subtree for `name` in configBytes (the SAME extraction
    // restoreEntryFromBytes uses). The api layer calls it on the SNAPSHOT bytes to build the
    // snapshotSubtree argument (lock-free — the snapshot is immutable + owner-anchored, not
    // subject to the live-config race); ClassifyEntryUnderLock calls it on the WRITE-TARGET
    // PHYSICAL live bytes (ConfigPath) UNDER the held lock — never the merged multi-layer GetEntry
    // (P1-b). It is the SINGLE raw-subtree extraction owner, NOT a second recognizer —
    // deep-comparing raw subtrees answers "is the live entry the snapshot's entry", never "is it
    // the hub entry" (that stays single-owned in liveEntryMatchesManifestBinding). B1: two
    // distinct stdio entries both project to MCPEntry{URL:""} and would falsely read equal under
    // a lean-MCPEntry comparator.
    EntryRawSubtree(configBytes []byte, name string) (subtree any, present bool, err error)
}

// EntryClassification is ClassifyEntryUnderLock's typed verdict (P1-a). Read-only; the
// four cases are exhaustive.
type EntryClassification int

const (
    ClassifyStillHub EntryClassification = iota
    ClassifyRestoreDone
    ClassifyGenuineConflict
    ClassifyUnreadable
)
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
at `managed_entries.go:378`; the shipped caller nil-guards at `adopted_entries.go:513-514`),
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
   wrote, `adopted_entries.go:109-117`) — `CASGuardedRemoveEntry(SourceEntryName, match)`:
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
restore-mode label (`adopted_entries.go:120-128`). No design claim asserts whole-file or
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

1. Delete `RoutedSecretKeys` from the vault BEFORE closing provenance (E5, which runs only under
   CLOSE-READY — so the routed secrets, like the manifest, survive while any client is still
   Failed and the hub entry keeps working). The row stays `de_adopting` (the recoverable state)
   until cleanup is DONE. E6 additionally requires this cleanup complete (see CLOSE-READY).
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
`abortAdoptProvenance` ordering (`adopted_entries.go:892-978`). Rationale:

- A `closed` tombstone permanently WEDGES re-adopt: capture refuses ANY non-`adopting` prior
  row (`adopted_entries.go:599-601`) and adopt v1 pins `manifest == entry name` (no rename
  dodge), so a `closed` row would block ever re-adopting that server.
- Snapshots-first ordering means a crash between snapshot-removal and row-drop leaves a
  row→missing-snapshot, NEVER a snapshot→no-row secret leak that no GC reaps. The adopt-side GC
  never reaps a STEADY-STATE `de_adopting` row: Phase-1 SELECTS candidates by
  `OperationState==adopting ∧ UpdatedAt<cutoff` (`adopted_entries.go:1112-1117`), so a `de_adopting`
  row is never a Phase-2 candidate, and Phase-3 reaps only ROWLESS dirs (this row has a row). NOTE
  — steady-state safety rests on that Phase-1 SELECTION property AND, since #531, a reap-time identity
  gate: `reapAdoptProvenanceRow` (`adopted_entries.go:946-978`) now NO-OPS unless the live row still
  matches the caller's expected `(ManifestName, state, UpdatedAt)` (matched at `:954`), and Phase-2
  RE-READS the row under the later-acquired lease before classify+reap (`:1144-1177`). So a row that
  was `adopting` at Phase-1 and TRANSITIONS to `de_adopting` under de-adopt's own lease no longer
  matches the stale `adopting` candidate → the reap NO-OPS; the transition-window reap is CLOSED (both
  filed adopt-GC bugs de-adopt `Depends-on` are FIXED at HEAD — see "Adopt-GC dependency"). Deleting
  the row keeps the shipped at-most-one-row-per-manifest invariant true and
  collapses P0's `closed` branch to `found=false`.
- **B2b — crash INSIDE close is recoverable, not wedged (soundness rests on the CLOSE-READY
  invariant — P1-b).** E4/E5/E6 run ONLY once CLOSE-READY held (every target client
  RESTORE-DONE-or-genuinely-accepted), and E6 runs snapshots-first AFTER E4. So a crash after
  `removeAdoptSnapshots` and before the row-drop leaves `de_adopting` + manifest ABSENT + snapshot
  dir ABSENT + every client already live≠hub. The resume crash-inside-close branch (snapshot-absent
  AND manifest-absent AND live≠hub → RESTORE-DONE) then classifies all clients done, so the retry
  finishes the row delete instead of routing to "snapshot missing → Failed" and wedging. **This is
  now sound BECAUSE manifest-absence is trustworthy close-ready evidence:** de-adopt's own E4 is the
  only in-scope manifest deleter and it deletes ONLY under CLOSE-READY, so in every crash-only path
  (no external deletion) manifest-absent ⟹ CLOSE-READY held ⟹ this client was
  RESTORE-DONE-or-accepted — the branch CANNOT misclassify a genuinely-unresolved conflict. The
  pre-restore fail-closed rule (live STILL hub + snapshot missing → Failed) is KEPT — only the
  manifest-absent + live≠hub combination signals a mid-close crash.
- **B2b residual — EXTERNAL manifest deletion (bounded, benign, no durable flag added).** The one
  way manifest-absence could coexist with a genuinely-unresolved live≠hub client is an OUT-OF-BAND
  deletion of the manifest file that bypasses the E4 gate, coinciding with an external snapshot-dir
  deletion. This is (a) unreachable by de-adopt's own control flow — no crash-only path produces it,
  since E4 is the only in-scope manifest deleter and it gates on CLOSE-READY; (b) confined to the
  operator or an allowlisted-owner / namespace-rights (`FILE_DELETE_CHILD`) actor — the SAME bounded
  residual the threat model already accepts, never a wrong-owner co-resident (the anchored reader
  refuses those); and (c) BENIGN in impact — de-adopt NEVER writes to a live≠hub client (the CAS gate
  refuses), so the operator's own entry is left untouched, the only "lost" artifact (the snapshot) was
  already externally deleted, and the sole effect is the row is cleaned rather than wedged (no NEW data
  loss). Because no data-loss path remains, v1 adds NO durable close-ready flag: the provenance-row
  schema is a protected surface (a new field bumps the schema version), and "manifest-absent under a
  still-live `de_adopting` row" is the durable close-ready evidence the invariant above relies on.
  Reachable identically via the operator manually deleting BOTH the manifest and the snapshot dir.

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
        state == adopting AND classifyDeadAdoptingRow(rec) == adoptRowCommittedKeep
                                               -> FRESH   (matches Mark's admission; a manifest-present
                                                           committed row whose hub entry merely drifted
                                                           is admitted by Signal 2b)
        state == adopting AND classifier != adoptRowCommittedKeep
                                               -> REFUSE  (true pre-install crash orphan; adopt GC owns it)
        state == de_adopting                 -> RESUME  (per-step / per-client done-ness)
  P2. Manifest hash-gate readiness: on-disk manifest hash == ExpectedManifestHash
      (RESUME: SKIP if the manifest file is already absent — delete step done).
  P3. Per client (FRESH): a StillHub entry remains mutation-pending; a RestoreDone entry is a
      fortuitous no-op already in the de-adopted target state, not a conflict. Only
      GenuineConflict / Unreadable / snapshot-unverifiable clients fail FRESH; for `present`, the
      snapshot exists + sha256 matches before its verdict can prove completion.
      RESUME per client done-ness is DERIVED via the ONE atomic classifier
      ClassifyEntryUnderLock(SourceEntryName, match, snapshotSubtree) — NOT a parallel unlocked
      read. There is exactly ONE live-config read-and-compare code path (this classifier, whose
      lockingClient forwarder HOLDS withConfigReadLock across the WRITE-TARGET-PHYSICAL live read +
      the RAW-subtree deep-compare — never the merged multi-layer GetEntry, P1-b; B1 pins the
      comparator to the RAW on-disk subtree, never lean MCPEntry equality).
      The api layer owns only the NON-config-lock dimensions and feeds them in: for `present` it
      anchored-reads the snapshot (ReadStateFileInodeAnchored) + sha-gates it + extracts
      snapshotSubtree via EntryRawSubtree(snapshotBytes) [pure, lock-free]; for absent/merged-lower
      snapshotSubtree is nil (success == absence). It then maps the classifier verdict, composing
      snapshot- and manifest-availability where the classifier cannot see them:
        (all verdicts judged WRITE-TARGET-PHYSICAL — the EntryPresentInBytes single-file section
         owner, never the merged multi-layer GetEntry; P1-b)
        classifier StillHub (write-target-physical live == hub entry):
          present w/ snapshot readable+sha-OK -> restore pending (E3 restores)
          present w/ snapshot missing/unreadable -> Failed (pre-restore fail-closed; NOT accept-eligible)
          absent / merged-lower                  -> remove pending (E3 removes)
        classifier RestoreDone (present: write-target-physical live raw subtree deep-equals
          snapshotSubtree; absent/merged-lower: entry ABSENT FROM THE WRITE TARGET — a merged-lower's
          re-emerged lower layer is out of scope, so this is RestoreDone NOT a conflict)
                                                 -> RESTORE-DONE (including FRESH: already at the
                                                    de-adopted target, no mutation, idempotent and
                                                    consistent with RESUME)
        classifier GenuineConflict (present: write-target-physical live subtree ≠ snapshot, or
          write-target live==nil vs a present snapshot; absent/merged-lower: a non-hub entry occupies
          the WRITE-TARGET slot)
                                                 -> genuine conflict Failed (accept-eligible)
        classifier Unreadable (a genuine read/parse error on a PRESENT write-target file under the
          lock — a cleanly-absent config is empty-config live==nil, NOT Unreadable; P2)
                                                 -> Failed (fail-closed)
        present with the snapshot UNAVAILABLE (no snapshotSubtree to supply — call the classifier
          with snapshotSubtree=nil and consume ONLY the StillHub / not-StillHub / Unreadable
          distinction; the RestoreDone-vs-GenuineConflict split is not meaningful without the
          snapshot and is NOT consumed here), then compose with manifest-availability:
          snapshot PRESENT-but-unreadable / sha-mismatch
                     -> Failed (fail-closed — cannot prove RESTORE-DONE without a verified snapshot;
                        NOT accept-eligible, the snapshot read must be repaired) [fable P3 resume-cell]
          snapshot ABSENT + manifest ABSENT + classifier not-StillHub -> RESTORE-DONE [B2b crash-inside-close]
          snapshot ABSENT + manifest PRESENT                          -> Failed (anomalous; fail-closed)
          (classifier StillHub here = live still hub + snapshot missing -> Failed, pre-restore)
        (a --accept-conflict client is accepted-done ONLY if the E3 mutation-point ClassifyEntryUnderLock
         re-read PROVES GenuineConflict — StillHub OR snapshot-unreadable/Unreadable → Failed, not
         accepted; see "CLOSE-READY". The plan view here is ADVISORY (it may go stale by execute) but
         uses the SAME atomic classifier — the authoritative validation is the E3 re-call under the
         execute lease's config lock, no separate unlocked check.)
ExecuteDeAdoptWithOpts(server, targets ≡ rec.AdoptClients):
  E1. lease := tryAcquireAdoptManifestLease(manifest); !ok -> "concurrent operation" REFUSE. defer Unlock.
  E2. MarkAdoptProvenanceDeAdopting(manifest)  (idempotent; adopted/committed-adopting -> de_adopting;
      B4: for a committed-adopting row, RE-VERIFY classifyDeadAdoptingRow==adoptRowCommittedKeep under
      the held lease before flipping; refuse otherwise — the adopt GC still owns an uncommitted orphan).
  E3. For each NOT-(RESTORE-DONE|genuinely-accepted) target client: CAS restore/remove
      (present/absent/merged-lower) BEFORE any topology removal. For a client passed via
      --accept-conflict, VALIDATE the acceptance via ClassifyEntryUnderLock at the mutation point
      (its forwarder holds the SAME config lock E3's CAS uses) — honored ONLY on a GenuineConflict
      verdict; StillHub / Unreadable / (present) snapshot-unreadable → REJECT → Failed (P1-a; see
      "CLOSE-READY"). (per-client {Restored, Failed, Accepted} accrues — G4)
  --- CLOSE-READY gate (the SINGLE terminal-state predicate; see below). Compute after E3. If NOT
      close-ready, STOP here: the row stays de_adopting; the manifest, its supervisor intent, the
      routed secrets, AND every snapshot are ALL still intact; E4/E5/E6 do NOT run; E7 returns the
      partial-failure report. ---
  E4. [CLOSE-READY only] ManifestDeleteInWithHash (skip if manifest already absent) + remove
      supervisor-intent descriptors.
  E5. [CLOSE-READY only] Delete still-present RoutedSecretKeys (pre-filtered; skip shared-as-warned).
  E6. [CLOSE-READY AND secrets deleted-or-skipped] CloseAdoptProvenance(manifest) -> DELETE row +
      snapshots (snapshots-first).
  E7. Emit redaction-safe event + GUI operator-action row + return the {Restored, Failed, Accepted} report.
```

**CLOSE-READY — the ONE terminal-state gate (B2a + P1-a + P1-b).** A SINGLE predicate gates the
whole close (E4 + E5 + E6) AND the resume machine's manifest-absence derivation — NOT two
independent guards:

> **CLOSE-READY** ≡ *every* target client ∈ `rec.AdoptClients` is **RESTORE-DONE** OR
> **genuinely-accepted**, where **genuinely-accepted** is proven at the mutation point by the
> read-only `ClassifyEntryUnderLock` seam returning `ClassifyGenuineConflict` (its forwarder holds
> the held per-file config lock, the SAME lock E3's CAS uses) — NOT merely by the operator passing
> `--accept-conflict`.

**The whole close moves behind CLOSE-READY (P1-b — why E4/E5 no longer proceed while a client is
Failed).** Resume stays roll-forward (retry completes forward, never rollback); what changes is
that E4/E5 no longer run past a Failed client. The prior revision let E4/E5 proceed while a client
was still Failed. That is a data-loss path: E4
deletes the manifest while a client remains an unresolved `live≠hub` conflict, and if the
snapshot dir then disappears, the resume crash-inside-close branch (manifest absent + snapshot
absent + live≠hub → RESTORE-DONE) silently accepts a real conflict. Fix: **E4 (manifest delete),
E5 (secret delete), and E6 (snapshot+row delete) ALL run ONLY once CLOSE-READY holds.** If ANY
target client is Failed/unqualified, NONE of E4/E5/E6 run — the manifest, its supervisor intent,
the routed secrets, and every snapshot stay intact, and a still-Failed client keeps working
through its still-live hub entry until the operator fixes the cause and retries (strictly better
than the old design, which left a Failed client pointing at a deleted manifest). This is what
makes manifest-absence trustworthy close-ready evidence for the resume machine (P1-b): de-adopt's
own E4 is the only in-scope manifest deleter, and it deletes ONLY under CLOSE-READY, so
manifest-absent ⟹ CLOSE-READY held when E4 ran.

**`--accept-conflict <client>` must PROVE a genuine conflict (P1-a) — via the atomic
`ClassifyEntryUnderLock` seam.** Un-validated, the flag is a data-loss lever: passing it for a
still-`present` hub client marks it accepted-done, E3 SKIPS its restore, and E6 then DELETES its
snapshot → the pre-adopt native entry (with its original secret-literal spellings) is lost forever,
violating the design's own `live≠hub` safety rationale. The proof is NOT a bare unlocked read (that
would reopen the plan→execute TOCTOU) and is NOT a mutating CAS "probe" (that would CHANGE a
still-hub client). It is the read-only `ClassifyEntryUnderLock(SourceEntryName, match,
snapshotSubtree)`, whose `lockingClient` forwarder holds the SAME per-file config lock E3's CAS
mutators use — ONE atomic live-read-and-classify, no mutation, no separate unlocked check. For
`present` clients the api layer first anchored-reads + sha-gates the snapshot and passes its
verified `snapshotSubtree`; for absent/merged-lower it passes nil (success == absence). **The flag
is HONORED only on a `ClassifyGenuineConflict` verdict**:

- **`ClassifyStillHub` (`match(L)==true` — L is STILL the hub entry adopt wrote) → REJECT the
  flag.** No conflict exists — the client is restorable, so accepting would SKIP restoration and E6
  would then destroy the snapshot. That client → **Failed** ("`--accept-conflict` passed but
  `<client>` is still the hub entry; omit the flag to restore it"); the operation refuses to close
  (CLOSE-READY false). This is the P1-a rejection — the data-loss path the flag must never open.
- **`present` + the pinned snapshot is unreadable / sha-mismatch → REJECT the flag BEFORE the
  classifier is even reached** (the api-layer anchored read + sha-gate fails, so no `snapshotSubtree`
  can be supplied; fail-closed — a genuine conflict cannot be proven without the snapshot to
  establish `L ≠` the restore target). Equally, a `ClassifyUnreadable` verdict (the LIVE config is
  unreadable under the lock) → REJECT. That client → **Failed**. `--accept-conflict` is NOT a bypass
  for a broken snapshot or an unreadable live config.
- **`ClassifyGenuineConflict` — a genuine conflict is proven** — `L` is neither the hub entry NOR
  the state a successful op would produce: for `present`, `L`'s raw subtree ≠ the snapshot's (or
  `L == nil` vs a present snapshot, an operator-emptied slot); for `absent`/`merged-lower` (nil
  snapshotSubtree), `L` is a non-hub entry the operator added. → **genuinely-accepted**: the
  operator ALREADY took that slot, so restoring the snapshot over it would be the very data loss we
  refuse. The client is terminal-done-with-warning; **its pinned snapshot is DESTROYED at close (E6)
  and its pre-adopt original config + original secret-literal spellings are discarded WITHOUT ever
  being restored** — the `--accept-conflict` warning copy MUST say exactly this (fable P3). A client
  that is already RESTORE-DONE (`ClassifyRestoreDone`) needs no flag (there the flag is a harmless
  no-op).

A CLOSE-READY-false operation is NOT wedged-by-silent-close: the row stays recoverable
`de_adopting`, the operator resolves each Failed client (revert the edit and retry, fix the
snapshot-read cause and retry, or `--accept-conflict` a genuinely-conflicted client) and re-runs.
Only when CLOSE-READY holds AND every routed secret is deleted-or-skipped-as-shared does E6 fire
and the row close. A live≠hub client whose snapshot is ALSO permanently unreadable is neither
restorable (CAS refuses) nor accept-eligible (snapshot-read failure rejected) — it stays Failed
until the operator repairs the snapshot read, and an abandoned `de_adopting` row (crash, operator
never retries, never accepts) is the G7 residual — see "Residuals".

**Lock graph (full total order, no reverse edge).** `<manifest>.lease` (E1, outermost, held
E1→E6) → the inners, each transient and mutually NON-nested: `adopted-entries.lock` (each
store mutator), the per-file `config-lock` — `withConfigLock` (`config_lock.go:51`) inside each
CAS method, and the read-selection `withConfigReadLock` (`:150-158`) inside the read-only
`ClassifyEntryUnderLock` seam (which delegates to the SAME `config_lock.go:51` flock when the
config exists and takes only the in-process mutex — creating no `.lock` file — when the config's
parent dir is absent), one file at a time, held by the lockingClient forwarder (the concrete
classify body is lock-free), the supervisor-intent lock (E4). The BuildDeAdoptPlan advisory classify
call takes that per-file config-lock as a transient LEAF (released immediately; no lease held during
plan; and NO `.lock` file created for an absent config — P3-a), adding no reverse edge. Order extends the shipped
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

## Adopt-GC dependency (de-adopt `Depends-on` two filed GC bugs — SATISFIED at HEAD)

De-adopt's per-manifest lease (D6) gives de-adopt↔adopt mutual exclusion for OVERLAPPING
operations, but it does NOT by itself fence the adopt GC's stale-candidate reap, because the GC's
decision INPUTS pre-date the lease. `gcOrphanedAdoptingProvenance` Phase-1 snapshots the aged
`adopting` candidates under the store lock and RELEASES it (`adopted_entries.go:1103-1121`); Phase-2
later `TryLock`s each candidate's lease and (since #531) RE-READS the live row under that lease
before classify+reap (`adopted_entries.go:1144-1177`), and `reapAdoptProvenanceRow`
(`adopted_entries.go:946-978`) NO-OPS unless the live row still matches the caller's expected
`(ManifestName, state, UpdatedAt)` (`:954`). Before #531/#532 landed the reap dropped every row
matching the manifest NAME with NO `(state, UpdatedAt)` filter, so three data-destruction interleaves
could reach the provenance de-adopt reads — all now CLOSED (see the Dependency note):

- **Pre-de-adopt destruction.** The direct filed-bug impact: a committed `adopted` row's provenance
  (+ secret snapshots) is reaped from a stale candidate under a routine adopt's step-0a GC, so
  de-adopt of M later reports "no provenance" and the original entry spelling incl. secret literals
  is gone.
- **de-adopt→re-adopt destroyed.** GC Phase-1 snapshots `R_old(M, adopting, >24h)`; de-adopt of M
  completes (lease released); a re-adopt of M captures a fresh `R_new`; GC Phase-2 reaches M,
  `TryLock` succeeds, classifies the STALE `R_old` → `adoptRowCrashReap` →
  `reapAdoptProvenanceRow(M)` deletes `R_new` + its snapshots.
- **crash-after-E3 `de_adopting` row reaped → permanent manifest/secret leak.** A committed-adopting
  `R_old(M, adopting, >24h)` is admitted (G6); de-adopt flips it `de_adopting` under the lease, E3
  removes the hub binding, then CRASHES before E6 (lease released on death). GC Phase-2 (within the
  same invocation's Phase-1→Phase-2 gap — widenable by many candidates / slow config reads / AV
  stalls) classifies the STALE `R_old` (binding now gone) → `adoptRowCrashReap` → reaps the
  now-`de_adopting` row + its secret-bearing snapshots — orphaning the routed secrets and making the
  row unresumable.

**Dependency (declared `Depends-on:` in `status.md`; adopt-side, a protected surface in v1 — NOT
patched by de-adopt; both edges FIXED at HEAD).** Bug #1
(`work-items/bugs/2026-07-11-gc-phase2-stale-candidate-reaps-committed-row.md`) — FIXED by #531
(master `c7e2534b`): Phase-2 RE-READS the row under the held lease and requires (row exists ∧
`state==adopting` ∧ `UpdatedAt==candidate.UpdatedAt` ∧ still older than cutoff) before classify+reap
(`adopted_entries.go:1144-1177`), and `reapAdoptProvenanceRow` now takes an expected `(state, UpdatedAt)`
identity that no-ops on mismatch (`:946-978`, matched at `:954`) — closing all three interleaves (a
transitioned `de_adopting` row's state/UpdatedAt no longer match the stale `adopting` candidate → no-op;
the fresh `R_new`'s UpdatedAt differs from the reaped-`R_old`'s → no-op). Bug #2
(`...-classifier-committed-signal-blind-to-entry-drift.md`) — FIXED by #532: hardens the classifier's
committed-KEEP side against live-entry drift (Signal 2b, `adopted_entries.go:521-530`), protecting the
committed-but-unflipped `adopting` row de-adopt's claim-10 recoverability contract depends on. Both
`Depends-on` edges are therefore MET; the `Depends-on:` declaration is retained in `status.md` as a
traceability record. De-adopt is no longer blocked behind these edges on this axis.

## Residuals (bounded — accepted or deferred; B3)

Stated in the BODY (not only claimed in the gate decision). Each is bounded, operator-driven,
and either accepted for v1 or explicitly deferred to a follow-up:

- **G7 — abandoned `de_adopting` row (DEFERRED recover).** A crash mid-execute followed by an
  operator who never retries (and never `--accept-conflict`s a permanently-conflicted client),
  OR a client that is neither restorable (live≠hub) NOR accept-eligible (its snapshot is
  permanently unreadable, so the P1-a snapshot-read-failure rejection applies), leaves a
  `de_adopting` row that (a) WEDGES re-adopt of that manifest — capture refuses ANY non-`adopting`
  prior row (`adopted_entries.go:599-601`) — and (b) RETAINS the secret-bearing snapshot dir,
  which no adopt-side GC reaps in steady state (Phase-1 SELECTS only `OperationState==adopting ∧
  UpdatedAt<cutoff` candidates, so a `de_adopting` row is never a Phase-2 candidate, and Phase-3
  reaps only rowless dirs — this row has a row; `reapAdoptProvenanceRow` (since #531) NO-OPS on any
  row whose `(state, UpdatedAt)` does not match the caller's expected identity, and a steady abandoned
  `de_adopting` row is never SELECTED as a candidate anyway, so it is retained, not reaped — the
  transition-window reap is CLOSED (the separate "Adopt-GC dependency", now satisfied)). This is
  structurally identical to the `closed`-tombstone wedge the rework fixed, but on the
  abandoned-retry path the failure table only covers the RETRIED path. v1 does NOT add a
  `de_adopting`-GC or a `mcphub de-adopt --recover`; both are **DEFERRED to the follow-up**
  (`work-items/backlog/2026-07-11-deadopt-subset-and-gate-on-followup.md` § C — de_adopting
  recovery / GC). Bounded (owner-only snapshot DACL — a co-resident cannot read the content;
  operator can delete the snapshot dir manually) and operator-driven.
- **G8 — a concurrent hub-entry writer can clobber a just-restored native entry.** A plain
  `mcphub install <server>` / GUI Apply / hub reconcile running concurrently takes only the
  per-file config locks, NOT de-adopt's `<manifest>.lease` — so in the E3→E4 window it can
  rewrite hub entries OVER the just-restored native entries (the CAS gate protects only
  against writers BEFORE de-adopt's own write, not after it). Under a gate-ON reconcile this is
  a `ClientUpdateRemove{EntryName: server}` against the just-restored entry
  (`install_hub_reconcile.go:256-262`). **P3-b — the same window applies to an ACCEPTED-conflict
  client:** its `ClassifyGenuineConflict` verdict is a point-in-time proof under the lock at E3 and
  is NOT re-checked at E6, so a concurrent no-lease writer can rewrite the accepted slot back to
  hub-shape between E3 and close (bounded and benign — the accepted client's snapshot is discarded
  either way; the E3 classify simply proves the operator held a non-hub slot AT E3-time). A
  reversion-to-hub that has ALREADY landed AT E3-time IS caught: the E3 `ClassifyEntryUnderLock`
  re-read returns `ClassifyStillHub` → the `--accept-conflict` flag is REJECTED → that client Failed,
  no snapshot destroyed. Operator-driven and bounded (the operator is running two conflicting topology
  commands at once); accepted as a residual — de-adopt does not extend the lease over the
  install/reconcile owners in v1.
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
  returns a `{Restored []string, Failed []{Client, Reason}, Accepted []string}` report (precedent
  `DemigrateReport{Restored, Failed}`, `demigrate.go:31-34`; `Accepted` lists the
  genuinely-accepted-with-warning clients whose snapshots were discarded — B2a). A CAS conflict,
  an unreadable snapshot, or a hash mismatch marks that client Failed and the operation is a
  partial success the operator retries (roll-forward). CLI exit: 0 when every client is
  Restored-or-Accepted (CLOSE-READY held, the row closed); non-zero if any client Failed, printing
  the report.
- **CLI:** `mcphub de-adopt <server>` (alias `deadopt`); `--yes` executes, default dry-run
  prints the plan; no provenance → non-zero, no mutation; gate-ON → non-zero "gate OFF
  first". **`--accept-conflict <client>`** (repeatable) requests that a genuinely-CAS-conflicted
  client (operator legitimately took the slot between plan and execute) be treated as
  terminally-done-with-warning so CLOSE-READY is satisfiable and the row can close (B2a). It is
  HONORED only after the E3 `ClassifyEntryUnderLock` re-read (config lock held by the forwarder)
  returns `ClassifyGenuineConflict` (P1-a): a still-hub client, or a `present` client whose snapshot
  is unreadable/sha-mismatched, is REJECTED (that client → Failed, the operation refuses to close) —
  the flag is never a bypass for a restorable entry or a broken snapshot. It never restores or removes that client's entry, only records the
  accepted conflict in the report + event trail. **The warning copy MUST state that the accepted
  client's pinned snapshot is DESTROYED at close and its pre-adopt original config + secret-literal
  spellings are discarded without ever being restored** (fable P3), so the operator is choosing
  irreversible discard with eyes open.

## Round-trip invariants + failure modes

Invariant: `adopt → de-adopt` restores EVERY `AdoptClient` to its pre-adopt state (pinned
snapshot for `present`; absence for `absent`; re-exposed lower layer for
`present-merged-lower`) and releases every hub-owned artifact adopt created for that
manifest. Restore is functional-equivalent (byte-equivalence UNVERIFIED per adapter,
`adopted_entries.go:120-128`). The SOLE exception is a client the operator explicitly
`--accept-conflict`s after genuinely taking that slot between plan and execute: that client is
LEFT at its operator-chosen state (never overwritten) and its pinned snapshot is discarded at
close — a deliberate, warned, operator-driven opt-out of restoration, not a silent divergence.

| Failure mode | Behavior |
|---|---|
| Gate-ON host | REFUSE with "gate OFF first, then de-adopt" (item 2 b). |
| Snapshot tampered / wrong-owner / oversize / missing (`present`) | Anchored read refuses (owner/reparse/cap) or sha256 mismatch → that client Failed, fail-closed before any write. |
| Entry ABSENT in the verified snapshot (`present`) | Impossible-state (capture guaranteed + sha-pinned it) → `CASRestoreEntryFromBytes` REFUSES fail-closed; NEVER silently removes (B5 — removal is `CASGuardedRemoveEntry`'s alone). |
| Snapshot > 16 MiB restore cap (`present`) | Anchored read refuses (cap) → that client Failed; bounded residual until the symmetric capture cap lands (B6, see Residuals). |
| Live client entry no longer the hub entry (operator edit / demigrate between plan+execute) | CAS `match` fails under the lock → REFUSE that client (Failed), never overwrite (items 5+7). A PERMANENT conflict is cleared by operator revert+retry OR a VALIDATED `--accept-conflict <client>` (honored only when the atomic `ClassifyEntryUnderLock` re-read returns `ClassifyGenuineConflict` AND, for `present`, the snapshot is readable — P1-a; the accepted client's snapshot is then DISCARDED at close) so CLOSE-READY holds and the row can close (B2a); otherwise the row stays recoverable `de_adopting`. |
| Live entry VANISHED between plan+execute (nil) | `CASGuardedRemoveEntry` nil-live = already-done success; `CASRestoreEntryFromBytes` nil-live = conflict-refuse fail-closed (never resurrect against intent) — B1 nil-live, no panic. |
| Manifest externally edited | `ManifestDeleteInWithHash` refuses `ErrManifestHashMismatch`; empty/absent hash → fail-closed refusal. |
| No provenance row | REFUSE (no `--reconstruct-legacy` in v1). |
| Row `adopting` classified `adoptRowCommittedKeep` | De-adopt admits it as FRESH, including Signal 2b: manifest present while the live hub binding has drifted away. Any other `adopting` row is a true pre-install crash orphan, so adopt GC owns it and de-adopt refuses. E2 re-verifies `adoptRowCommittedKeep` under the held lease before flipping a committed-adopting row (B4). |
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
    a committed-but-`adopting` row classified `adoptRowCommittedKeep` → admitted as FRESH,
    including manifest-present Signal 2b when its live hub binding has drifted away. **B4:** simulate
    the row becoming UNCOMMITTED between plan-admission and E2 (its hub entries and manifest removed) → E2's
    `MarkAdoptProvenanceDeAdopting` re-runs `classifyDeadAdoptingRow` under the held lease and
    REFUSES the flip (adopt GC owns it), rather than converting a GC-reapable row into a
    GC-immune `de_adopting` wedge.
13. **`--accept-conflict` VALIDATION + close (B2a + P1-a).** (a) A client with a GENUINE CAS
    conflict (operator took the slot: live≠hub AND — for `present` — snapshot readable AND live
    subtree ≠ snapshot subtree) → without `--accept-conflict` the row stays recoverable
    `de_adopting` (CLOSE-READY unsatisfied, NOT wedged-by-silent-close); WITH `--accept-conflict
    <client>` the E3 `ClassifyEntryUnderLock` re-read returns `ClassifyGenuineConflict`, the client is
    accepted-done-with-warning, CLOSE-READY holds, E6 fires, the row is deleted + its snapshot
    destroyed, and re-adopt then succeeds. Assert the accepted client is never restored/removed and
    the acceptance is in the report + event trail. (b) **P1-a misuse falsification:**
    `--accept-conflict` on a STILL-HUB `present` client → `ClassifyEntryUnderLock` returns
    `ClassifyStillHub` (`match(live)==true`) →
    the flag is REJECTED, that client → Failed, the operation REFUSES to close, and the snapshot is
    INTACT (no data loss); `--accept-conflict` on a `present` client whose snapshot is UNREADABLE →
    likewise REJECTED (Failed). A build that marks either accepted-done and lets E6 delete the
    snapshot (restoration skipped, native entry lost) is the falsification.
14. **P1-b — de-adopt never makes the manifest absent while a client is unresolved (CLOSE-READY
    gates E4).** (a) Force one target client into an unresolved live≠hub conflict (operator edit,
    NOT accepted) → assert CLOSE-READY is false → E4 does NOT run → the manifest, its supervisor
    intent, the routed secrets, AND all snapshots survive, and the row stays recoverable
    `de_adopting`. (b) **Snapshot-loss falsification:** with that unresolved-conflict client and the
    manifest STILL PRESENT (E4 gated off), externally delete that client's snapshot → the resume
    classifier hits `snapshot ABSENT + manifest PRESENT → Failed` and the client STAYS Failed — it
    does NOT reach the crash-inside-close branch (which requires manifest ABSENT) and is NEVER
    silently classified RESTORE-DONE. A build where E4 deletes the manifest while a client is Failed
    (the old roll-forward bug), or a classifier that reads snapshot-absent-alone as RESTORE-DONE, is
    the falsification. (The manifest-ALSO-externally-deleted case is the bounded, benign B2b
    residual, not a guaranteed-recovery path.)
15. **T15 — atomic `ClassifyEntryUnderLock` seam (P1-a implementation falsification).** (a)
    **Read-only:** after ANY classify verdict the live config bytes are byte-unchanged (the seam
    never mutates). (b) **Concurrency / atomicity:** run a classify call while a second goroutine
    mutates the live entry to a non-hub value; because the forwarder holds the config lock across
    the live read + compare, the verdict reflects a single CONSISTENT file snapshot (never a torn
    read) and the concurrent write serializes strictly before/after — each of the two interleavings
    yields a well-defined verdict, never a TOCTOU split. (c) **Still-hub-refused with snapshot
    intact:** `--accept-conflict` on a STILL-HUB `present` client → the E3 classify returns
    `ClassifyStillHub` → the flag is REJECTED, that client → Failed, the operation REFUSES to close,
    and the pinned snapshot is INTACT on disk (E6 never ran). A build whose accept decision reads
    the live entry WITHOUT the config lock (a parallel unlocked check, so a between-check-and-act
    write flips the verdict), or that "probes" via a MUTATING CAS method (altering the still-hub
    client), is the falsification. (d) **Single owner:** grep shows the accept decision AND the
    resume done-ness derivation BOTH route through `ClassifyEntryUnderLock`, and there is NO other
    live-config read-and-compare path (no `os.ReadFile(adapter.ConfigPath())` + `EntryRawSubtree`
    comparison outside the seam).
16. **T16 — write-target-physical classify (P1-b merged-lower falsification).** For a mimocode
    `present-merged-lower` client whose original entry resolves from a LOWER layer (config.json /
    `~/.claude.json` import) and whose write target holds the hub entry adopt wrote: after a
    successful `CASGuardedRemoveEntry`, the write target is entry-ABSENT but the MERGED `GetEntry`
    view re-surfaces the lower entry. `ClassifyEntryUnderLock` MUST read the WRITE-TARGET-PHYSICAL
    bytes → classify `ClassifyRestoreDone` (write-target absent). A build that reads the merged
    multi-layer `GetEntry` → live≠nil / match==false → `ClassifyGenuineConflict` (wedging CLOSE-READY,
    or coercing a meaningless `--accept-conflict` whose snapshot-destruction warning is void because
    merged-lower has no snapshot) is the falsification. Also assert the classify read touches ONLY the
    write-target file (no overlay / import / `~/.claude.json` read) so it stays inside the held
    ConfigPath lock's coverage (atomicity). A single-file adapter (write target == GetEntry) passes
    unchanged — the rule is uniform.
17. **T17 — cleanly-absent config is empty-config, not Unreadable (P2).** Delete a target client's
    WHOLE config file (`os.IsNotExist`) between plan and execute → `ClassifyEntryUnderLock` returns
    live==nil / raw-absent (empty-config), NOT `ClassifyUnreadable`: for a `present` client it maps to
    `ClassifyGenuineConflict` (accept-eligible — the operator emptied the slot), a RECOVERABLE state,
    NOT the permanent Failed + not-restorable + not-accept-eligible G7 wedge a wrong `Unreadable`
    verdict would produce. `ClassifyUnreadable` is reserved for a genuine parse error on a PRESENT
    file (corrupt the config to malformed JSON → `ClassifyUnreadable` → Failed fail-closed). A build
    that maps `IsNotExist` to `Unreadable` is the falsification.

GUI/CLI: eligibility-surface test (affordance only for provenance rows, disabled gate-ON);
`{Restored, Failed, Accepted}` report + CLI exit; Playwright round-trip (adopt → gate-OFF
de-adopt → scan native); route tests mirroring `gui/adopt_test.go`.

## Claims (falsifiable — `{ guarantee, single-owner, enforcement-probe }`)

1. `{ guarantee: v1 de-adopt is atomic over ALL AdoptClients of one manifest (targets ≡ rec.AdoptClients), so the resume scope is a fixed set needing no journaled target list; single-owner: BuildDeAdoptPlan reading rec.AdoptClients; enforcement-probe: test 7 (resume) + the absence of any subset code path }`
2. `{ guarantee: "the live entry is our hub entry" has exactly ONE equality owner — the shipped liveEntryMatchesManifestBinding — no byte-exact recompute, no second shape owner; single-owner: managed_entries.go:355; enforcement-probe: grep shows no byte-exact entry reconstruction + no second recognizer in deadopt.go }`
3. `{ guarantee: the snapshot is read through the anchored reader with the path recomputed from (ManifestName, Client), refusing wrong-owner/reparse/oversize before hashing; single-owner: ReadStateFileInodeAnchored (state_read_inode_anchor.go:22) + adoptSnapshotDir; enforcement-probe: test 2 (wrong-owner + oversize + tamper) }`
4. `{ guarantee: every destructive client-config write is COMPARE-AND-SWAP under the lock held by the lockingClient forwarder (concrete bodies lock-free) — refuse unless the live entry is still the hub entry; nil-live is per-branch (remove=success, restore=conflict-refuse); a present-restore whose verified snapshot lacks the entry REFUSES fail-closed (removal is never silent); single-owner: the CAS capability methods (clients) + the injected recognizer predicate; enforcement-probe: test 3 (operator-edit race + nil-live) + test 2 (B5 absent-in-snapshot) }`
5. `{ guarantee: CloseAdoptProvenance DELETES the row + snapshots (snapshots-first), leaving no `closed` tombstone, so re-adopt of the same manifest succeeds; single-owner: CloseAdoptProvenance mirroring reapAdoptProvenanceRow (adopted_entries.go:946-978); enforcement-probe: test 6 (re-adopt after de-adopt) }`
6. `{ guarantee: the last-binding manifest delete is hash-gated at the mutation point, fail-closed on empty hash, path-escape guard retained; single-owner: ManifestDeleteInWithHash (accepted decision); enforcement-probe: test 5 }`
7. `{ guarantee: routed keys are deleted before close; a shared key is skipped-as-warned and the close-done predicate is deleted-OR-skipped so the row never wedges; single-owner: the de-adopt cleanup ordering + pre-filtered deleteAdoptRoutedSecrets; enforcement-probe: test 8 }`
8. `{ guarantee: a crash mid-execute leaves a recoverable de_adopting row that roll-forward resume COMPLETES (skips RESTORE-DONE clients + done steps) — including a crash INSIDE close (snapshot dir + manifest gone + live≠hub → RESTORE-DONE, not wedged), sound BECAUSE E4 deletes the manifest ONLY under CLOSE-READY so manifest-absence implies all-clients-resolved (P1-b) — never a rollback that re-writes hub over native; the resume done-test compares RAW on-disk subtrees, not lean MCPEntry equality; single-owner: BuildDeAdoptPlan RESUME done-ness derivation (routed through the atomic ClassifyEntryUnderLock seam, gated on the CLOSE-READY manifest-absence invariant) + the EntryRawSubtree comparator; enforcement-probe: test 7 (crash-inside-close + two-stdio-husk falsification) + test 14 (manifest-absence never masks an unresolved conflict) }`
9. `{ guarantee: gate-ON de-adopt is refused with an actionable message and zero mutation in v1; single-owner: BuildDeAdoptPlan P0 gate check; enforcement-probe: test 11 }`
10. `{ guarantee: the bytes-restore/guarded-remove live on a CAPABILITY interface implemented only by adopt-reachable adapters, not the Client interface; single-owner: the CASEntryMutator capability (mirrors EntryBytesChecker); enforcement-probe: grep shows no Client-interface restore method + a fail-closed type-assert at the de-adopt site }`
11. `{ guarantee: v1 does NOT modify BuildHubReconcilePlan or add a single-owner entry renderer (both deferred with gate-ON); single-owner: the recognizer-only equality + gate-OFF-only scope; enforcement-probe: git diff shows no change to install_hub_reconcile.go and no new entry-renderer }`
12. `{ guarantee: no secret value / snapshot byte / entry body appears in any de-adopt event/error/log; single-owner: the de-adopt redaction; enforcement-probe: test 10 }`
13. `{ guarantee: the owner anchor (wrong-owner refusal in both modes) is the authenticity root — de-adopt adds no new authenticity mechanism; single-owner: readStateFileInodeAnchoredWithOptions ErrWrongOwner (hub_mcp_state_read_inode_windows.go:194 / posix:135); enforcement-probe: a wrong-owner snapshot/store read fails closed (test 2) }`
14. `{ guarantee: the restore payload is produced by COMPOSING the shipped per-adapter restore core (a behavior-preserving restoreEntryFromBytes refactor), never a lossy GetEntry→AddEntry round-trip and never a parallel per-adapter extraction owner; single-owner: the refactored restoreEntryFromBytes core (one extraction owner across ~15 adapters); enforcement-probe: test 4 (direct-stdio command/args/env survive byte-faithfully — a husk restore fails) + grep shows no GetEntry→AddEntry restore path and no second extraction impl in the CAS bodies }`
15. `{ guarantee: ONE CLOSE-READY predicate (every target client RESTORE-DONE-or-genuinely-accepted) gates the WHOLE close — E4 (manifest delete), E5 (secret delete), AND E6 (snapshot+row delete) all run only under it (E6 additionally requires every routed secret deleted-or-skipped) — so the manifest is never deleted while a client is unresolved (P1-b) and no still-needed snapshot is destroyed (P1-a); --accept-conflict is honored ONLY when the atomic ClassifyEntryUnderLock re-read (config lock held by the forwarder) returns GenuineConflict (StillHub OR unreadable-snapshot/Unreadable → refused → Failed), never a silent close, never a data-losing skip; single-owner: the single ExecuteDeAdopt CLOSE-READY gate (shared by E4/E5/E6 + the resume manifest-absence derivation); enforcement-probe: test 13 (accept validation) + test 14 (E4 close-gated) + test 15 (atomic classification) }`
16. `{ guarantee: E2 re-verifies classifyDeadAdoptingRow==adoptRowCommittedKeep under the held lease before flipping a committed-adopting row, so a plan-time admission stale by execute cannot convert a GC-reapable orphan into a GC-immune de_adopting wedge; single-owner: MarkAdoptProvenanceDeAdopting under the lease; enforcement-probe: test 12 (B4) }`
17. `{ guarantee: the de-adopt restore read cap is a bounded client-config size (16 MiB) and the symmetry invariant (restore cap ≥ capture max) is stated + assigned — the residual > cap config is bounded and directed to the adopt-side capture cap; single-owner: the .snapshot suffix clause in stateFileReadCapBytes + the filed adopt-side capture-cap bug; enforcement-probe: test 2 (oversize cap refusal) + the Residuals section }`
18. `{ guarantee: accept-eligibility AND resume done-ness are decided by ONE read-only under-lock classification seam (ClassifyEntryUnderLock) whose lockingClient forwarder holds withConfigReadLock — the read-selection variant delegating to the SAME withConfigLock flock the CAS mutators use when the config's parent dir exists (atomic vs a concurrent operator edit/demigrate), and short-circuiting to the in-process mutex when the dir is absent so a plan-time classify creates NO parent dir and NO .lock file (P3-a, config_lock.go:150-158 vs :117-132); there is NO parallel unlocked live-config read-and-compare (the round-3 os.ReadFile(ConfigPath) resume read is removed) and NO mutating "probe" of a still-hub client; the hub-equality predicate stays the single injected recognizer and the raw-subtree extraction stays the single EntryRawSubtree owner (no second equality/extraction owner); the concrete classify body is read-only + lock-free (non-reentrant mutex, config_lock.go:24-30); single-owner: ClassifyEntryUnderLock on CASEntryMutator (forwarder-locked via withConfigReadLock, read-only) + the injected liveEntryMatchesManifestBinding recognizer + the EntryRawSubtree extractor; enforcement-probe: test 15 (read-only + concurrent state change + still-hub-refused-snapshot-intact) + grep shows both the accept decision and the resume derivation call ClassifyEntryUnderLock and no os.ReadFile(ConfigPath)+compare outside the seam }`
19. `{ guarantee: ClassifyEntryUnderLock derives BOTH the live *MCPEntry (fed to the hub recognizer) AND the live raw subtree from the WRITE-TARGET-PHYSICAL config bytes read once under the read-selection lock — the EntryPresentInBytes single-file section owner (entry_bytes.go:95-103) — NEVER the merged multi-layer GetEntry (for mimocode a merged view over write target + lower config.json + ~/.claude.json import + higher overlay, mimocode.go:3868-3951); so StillHub/RestoreDone/GenuineConflict are correct-by-construction for EVERY adapter (adopt wrote the write target ⟹ merged-lower success ≡ write-target absence, so a re-emerged lower layer reads RestoreDone not GenuineConflict) and the classify read stays inside the ConfigPath lock's coverage (no overlay/import read the lock cannot cover → claim-18 atomicity holds); a cleanly-absent config maps to empty-config (live==nil, raw-absent), never ClassifyUnreadable (P2); single-owner: the write-target-physical single-file section parse inside ClassifyEntryUnderLock (the EntryPresentInBytes / EntryRawSubtree owner); enforcement-probe: test 16 (merged-lower re-emerged lower layer after CASGuardedRemoveEntry → RestoreDone not GenuineConflict) + test 17 (cleanly-absent config → empty-config not Unreadable) + grep shows no GetEntry/merged-view read in the classify path }`

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
  variable). The same-shape `isSecretBearingStateFilePath` `.snapshot`-suffix clause has ALREADY
  landed at HEAD (`state_read_inode_anchor.go:59-61`, #532); only this `stateFileReadCapBytes`
  read-cap clause remains for de-adopt. **Symmetry invariant:** the restore cap must be ≥ any snapshot capture can pin;
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
3. `work-items/bugs/2026-07-11-adopt-snapshot-read-cap-too-small.md` — the snapshot cap
   asymmetry (capture is uncapped at `writeAdoptClientSnapshot`; the read-back defaults to 1 MiB,
   so a large legitimate client-config snapshot would fail the anchored read).
   `context: adjacent-finding`, `status: open`. **AMENDED this round (B6):** the bug's fix
   direction now requires the capture==restore SYMMETRY (an adopt-side capture-time cap == the
   de-adopt restore cap, both referencing one constant), not merely raising the read cap — so the
   design's B6 claim and the tracked bug now agree.

Follow-up work-item stub (subset + gate-ON de-adopt):
`work-items/backlog/2026-07-11-deadopt-subset-and-gate-on-followup.md`.

## Gate decision

**PASS (rework 2026-07-11 round 5 — fable-5 write-target-physical collapse + adopt-GC dependency fold-in).** v1 is scoped to
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
to "additive interface + behavior-preserving restore-body refactor." **B2a** — the close is
gated on the CLOSE-READY predicate, the permanent-CAS-conflict horn resolved with
`--accept-conflict <client>`. **B2b** — crash-inside-close resume branch added
(snapshot+manifest gone + live≠hub → RESTORE-DONE), fail-closed missing-snapshot KEPT for the
pre-restore case. **B3** — G7 (abandoned `de_adopting` row; recover DEFERRED) + G8 (concurrent
no-lease writer) + the > cap residual now stated in the BODY (new "Residuals" section), so the
gate decision no longer over-claims them. **B4** — E2 re-verifies committed-ness under the held
lease. **B5** — restore fail-closes on entry-absent-in-verified-snapshot (removal stays
`CASGuardedRemoveEntry`'s). **B6** — snapshot cap symmetry invariant stated + assigned. P3
fold-ins: CAS lock ownership pinned (forwarder holds the lock, concrete bodies lock-free),
guarded-refuse restore polarity, `.snapshot`-SUFFIX clauses (not exact basename), C6
landing-comment direction (repoint `UpdateAdoptExpectedManifestHash` + `:156-161` + the mutator
tombstone at the subset follow-up), unknown-field-drop honesty sentence, gate-ON F6 sentence,
doc-anchor cleanup (`adopt.go:355`; dropped the "(T6)" test label).

Round-3 delta-check fixes folded in (all design-text; no architecture change): **P1-a +
P1-b collapsed onto ONE authoritative CLOSE-READY predicate** — `every target client
RESTORE-DONE-or-genuinely-accepted` — that E4 (manifest delete), E5 (secret delete), E6
(snapshot+row delete), AND the resume manifest-absence derivation ALL gate on (not two
parallel guards): **P1-b** — E4/E5 move BEHIND the gate (the manifest is never deleted while a
client is unresolved), which makes manifest-absence trustworthy close-ready evidence and the
crash-inside-close branch sound (the sole residual — external manifest+snapshot deletion — is
now analyzed as bounded/benign, so no durable close-ready flag is added against the protected
row schema); **P1-a** — `--accept-conflict` is HONORED only after a mutation-point re-read
PROVES a genuine non-hub conflict (a still-hub or unreadable-snapshot client is REJECTED →
Failed → refuses to close), closing the data-loss path where the flag skipped restoration and
E6 destroyed the snapshot; tests 13(b) + 14 added as the two falsifications. P2 tracking-doc
fixes: the adjacent snapshot-cap bug amended to require capture==restore SYMMETRY at
`writeAdoptClientSnapshot` (not merely a read-cap raise); a de_adopting-recovery/GC section
added to the subset/gate-ON follow-up so the G7 reference is true. Fable P3s: the
`--accept-conflict` warning copy states the snapshot is DESTROYED at close (original config +
secret-literal spellings discarded, never restored); the resume `live≠hub + snapshot
PRESENT-but-unreadable/sha-mismatch → Failed` cell enumerated; `COMMITTED_KEEP` corrected to
the Go identifier `adoptRowCommittedKeep`; the `EntryRawSubtree` resume comparator's
live-config-bytes read path pinned to `os.ReadFile(adapter.ConfigPath())` (SUPERSEDED in round 4
— that unlocked read is REMOVED; the resume comparator now routes through the atomic
`ClassifyEntryUnderLock` seam, see the round-4 paragraph below).

Round-4 delta-check fix folded in (design-text only; no architecture change): **P1-a's atomic
IMPLEMENTATION contract added** — round-3 mandated a mutation-point re-read to prove a genuine
conflict but declared NO callable seam that could classify {still-hub, restore-done,
genuine-conflict, unreadable} READ-ONLY under the held config lock (the two CAS methods mutate;
`EntryRawSubtree` is a lock-free pure bytes function; merged `lockingClient` leaves `GetEntry`
UNLOCKED, `config_lock.go:160-173`). Fix: a NEW read-only `ClassifyEntryUnderLock` method on the
`CASEntryMutator` capability whose `lockingClient` forwarder HOLDS `withConfigLock` (SUPERSEDED in
round 5 — the forwarder holds the read-selection `withConfigReadLock`; see the round-5 paragraph
below) across the live read + the raw-subtree deep-compare (concrete body read-only + lock-free, same
non-reentrant-mutex constraint as the CAS bodies). BOTH the E3 `--accept-conflict` acceptance decision AND the
resume/plan done-ness derivation now route through this ONE seam (single atomic classification
owner); the round-3 resume derivation's parallel UNLOCKED `os.ReadFile(adapter.ConfigPath())` live
read — the very TOCTOU the seam closes — is REMOVED, with snapshot/manifest-availability composed
api-side. The hub-equality predicate stays the single injected recognizer and the raw-subtree
extraction stays the single `EntryRawSubtree` owner — no second equality/extraction owner. Test T15
added (read-only + concurrent state change + still-hub-refused-with-snapshot-intact); claim 18 added,
claim 15 re-anchored on the seam; blast-radius change (2) + D2 updated to list the read-only
`ClassifyEntryUnderLock` seam alongside the two CAS mutators + `EntryRawSubtree`.

Round-5 fixes folded in (all design-text; no architecture / scope / protected-surface change —
only WHICH BYTES the classify reads + the dependency edge): **P1-B** — the classify live `*MCPEntry`
+ raw-subtree derivations are pinned to the WRITE-TARGET-PHYSICAL single-file section bytes (the
`EntryPresentInBytes` owner, `entry_bytes.go:95-103`), NEVER the merged multi-layer `GetEntry`
(`mimocode.go:3868-3951`), collapsing the mimocode merged-lower misclassification + atomicity void to
ONE correct-by-construction predicate for every adapter; **P1-A** — `Depends-on` the two filed
adopt-GC bugs + a new "Adopt-GC dependency" section, and design:564/:795 corrected (they asserted a
reap-time state filter `reapAdoptProvenanceRow` did not have AT ROUND-5 TIME — SINCE ADDED by #531,
so both `Depends-on` edges are now MET; see "Adopt-GC dependency"); **P2** — cleanly-absent config →
empty-config (live==nil), never `ClassifyUnreadable`; **P3-a** — the classify forwarder holds
`withConfigReadLock` (missing-dir short-circuit) so plan-time classify has no FS side effect; **P3-b**
— G8 extended (accepted-conflict verdict not re-checked at E6). Claims → 19 (claim 19 added, claim 18
re-anchored on `withConfigReadLock`); tests T16 + T17 added.

PASS items kept unchanged (the round-4 `ClassifyEntryUnderLock` seam + its lock discipline, the
CLOSE-READY gate, B1 restore core, E4/E5/E6 gating, roll-forward resume, the `ManifestDeleteInWithHash`
decision, lock/lease/redaction, routed-secret namespacing, demigrate-NOT-reused, composing shipped
owners, the all-clients-only cut) — round 5 touched none of them except to change which bytes the
classify reads. Consistent with the two accepted decisions
(`2026-07-11-deadopt-v1-all-clients-only-scope`, `2026-07-10-deadopt-manifest-delete-hash-gate`)
— `--accept-conflict` is a conflict-resolution escape WITHIN the atomic all-clients operation
(targets ≡ `AdoptClients` unchanged), not a reintroduction of subset de-adopt. Next stage: the
lead re-verifies + a Sol (codex) DELTA-recheck of the round-5 amended sections (the
write-target-physical collapse in `ClassifyEntryUnderLock` + interface block, the resume/accept
wiring, the `withConfigReadLock` P3-a forwarder, the "Adopt-GC dependency" section + the design:564/:795
corrections, the P2 empty-config pin, claims 18-19, T16/T17) — NOT a full re-audit — then, once the
two adopt-GC `Depends-on` edges are satisfied, `$planner`.
