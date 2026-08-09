# Internal architecture advisor — PR #588 recovery-state corrections

Date: 2026-07-27  
Role: `$architect`  
Panel disposition: **INPUT_ONLY (non-counted; native context not DP3-sealed)**  
Accepted input: `research-live-findings-2026-07-27.md` (`PASS`)  
Scope: C1, C2, C4, C8, C9, and C10 only

This is a non-canonical advisor package for the active design panel. It does
not claim the design-stage gate, does not authorize implementation, and is not
planner-eligible.

## Evidence posture

The accepted memo's load-bearing citations were spot-checked against current
HEAD `3872ee16` before this design was written.

- The artifact still collapses LSP identity to `(client, language)` at
  `internal/cli/install_reconcile_mcp_front.go:1225-1249`, while snapshot
  capture emits canonical and legacy rows with distinct entry names at
  `internal/api/lsp_client_router_snapshot.go:137-178`.
- The report still carries one global latest port at
  `internal/cli/install_reconcile_mcp_front.go:237-245`; rollback still passes
  it to every LSP row at
  `internal/cli/install_reconcile_mcp_front.go:573-584`.
- Serena's write-ahead hook still runs before `AddEntry` at
  `internal/api/serena_client_reconcile.go:463-487`, and the CLI still
  persists that prepared backup as a Serena `Applied` row before the write at
  `internal/cli/install_reconcile_mcp_front.go:889-925`.
- Serena fingerprint failure still becomes `Recorded=false` at
  `internal/cli/install_reconcile_mcp_front.go:998-1013`, and rollback still
  allows that unjudgeable row through at
  `internal/cli/install_reconcile_mcp_front.go:1100-1137`.
- An unreachable LSP client still becomes Pending only for `Present=true`
  restorable rows at `internal/api/lsp_client_router_snapshot.go:236-253`;
  `restorable` excludes every absent baseline at
  `internal/api/lsp_client_router_snapshot.go:72-84`.
- Snapshot and apply still independently derive clients and independently call
  `Exists()` at `internal/api/lsp_client_router_snapshot.go:127-136` and
  `internal/api/lsp_client_router.go:194-202`; the CLI supplies no shared
  capture object at `internal/cli/install_reconcile_mcp_front.go:439-489`.
- The existing operation lock, read-only command gate, LSP preflight, and route
  session expiry remain present at
  `internal/cli/install_reconcile_mcp_front.go:352-369`,
  `internal/cli/install.go:107-121`,
  `internal/cli/install_reconcile_mcp_front.go:395-412`, and
  `internal/cli/route.go:273-301`.

No test, build, vet, GUI, tray, supervisor, scheduler, or production state path
was run in this architecture stage.

## Defect-class ownership inventory

| Class | Complete participant set | Single correction owner |
| --- | --- | --- |
| C1 — legacy LSP rows collide | `LSPRouterEntrySnapshot`, canonical and legacy capture, `collectLegacyLSPEntriesToMigrate`, CLI report merge/key, snapshot restore | Comparable LSP row identity `(client, language, normalized entry name)` owned by the recovery-row contract in `internal/api/lsp_client_router_snapshot.go`; merge and restore consume it. |
| C2 — global latest port cannot represent partial retry | Report `Port`, journal commit, LSP add/remove results, cross-port retry, rollback ownership check | Mutable `Applied` ownership on each persisted LSP entry row; the report-level port remains diagnostics only. |
| C4 — Serena rollback fails open on unknown fingerprint | `SerenaClientEntryFingerprint`, CLI fingerprint capture, pre-write rollback gate, unconditional restore API | Serena recovery-row state machine plus one command-level whole-Serena ownership preflight; no restore call is reachable without a known matching applied fingerprint. |
| C8 — prepared Serena row is called Applied | `OnBackupCaptured`, CLI backup pin/journal, `AddEntry`, API transient `MigrateReport`, CLI commit | Serena recovery-row state machine; `Applied` promotion is a post-success transition, never the write-ahead state. |
| C9 — absent-created LSP row disappears while unreachable | LSP baseline, applied ownership, unreachable classification, `Pending`, caller retirement gate | `NeedsRollback` on the LSP recovery row, derived from applied/attempt state rather than baseline `Present`. |
| C10 — snapshot/apply population drift | `AllClients`, adapter `Exists`, entry discovery, legacy candidate discovery, CLI snapshot/apply calls | An API-owned immutable `LSPRouterReconcilePlan` whose captured client/entry identity universe is the only input accepted by apply. |
| C3 — `--check` dispatch | Top command gate and its test | **Protected / not affected.** Owner remains `newInstallCmdReal` at `internal/cli/install.go:107-121`. |
| C5 — operation serialization | `runReconcileMCPFront` lock wrapper and contention test | **Protected / not affected.** Owner remains the single wrapper lock at `internal/cli/install_reconcile_mcp_front.go:352-369`. |
| C6 — LSP preflight | `preflightMCPFrontReconcile` and its route test | **Protected / not affected.** Owner remains `preflightMCPFrontReconcile` at `internal/cli/install_reconcile_mcp_front.go:395-412`. |
| C7 — route expiry | route store handoff, cleanup start, cancellation tests | **Protected / not affected.** Owner remains route composition at `internal/cli/route.go:273-301`. |

The existing `RollbackLSPRouterClientEntries` demotion operation remains
separate. Current front rollback calls snapshot restore instead at
`internal/cli/install_reconcile_mcp_front.go:554-584`; the accepted memo
excluded demotion from this defect set.

## Chosen approach

Use one version-3 recovery record with immutable first-baseline rows and mutable
per-row forward-write ownership. Freeze the LSP participant and entry universe
in an API-owned reconcile plan before the first durable journal write. Both
Serena and LSP use the same write lifecycle:

1. persist the immutable recovery input and a `prepared` attempt before a
   client mutation;
2. execute the owning adapter operation;
3. only after success, capture the post-write ownership evidence and promote
   that row to `applied`;
4. on a known write failure, record `failed` without changing the prior
   applied ownership;
5. on a crash, unreadable fingerprint, or post-success journal failure, leave
   `prepared` or mark `ownership_unknown`; rollback fails closed and keeps the
   active report.

This is a local work-item decision. It changes one internal command's private
recovery schema and its direct API seams; it does not outlive this work-item or
constrain another product surface. No `work-items/decisions/` record is
recommended.

### Change-Surface Contract

`{ intended change surface: internal/cli/install_reconcile_mcp_front.go;
internal/api/lsp_client_router_snapshot.go;
internal/api/lsp_client_router.go;
the direct Serena reconcile observer/fingerprint contract in
internal/api/serena_client_reconcile.go; exact regression tests beside those
owners, approved extension seams: the private mcpFrontReconcileReport schema,
the LSP snapshot/restore row contract, an API-owned LSP plan/apply seam, and
post-operation observer callbacks on existing Serena/LSP adapter loops,
protected / must-not-touch surfaces: internal/cli/install.go check dispatch;
mcpFrontReconcileLockLeaf and its wrapper lifetime; preflightMCPFrontReconcile;
internal/cli/route.go session cleanup; RollbackLSPRouterClientEntries demotion;
GUI, tray, supervisor, scheduler, production state paths, declared blast
radius: one private persisted artifact version plus the front-reconcile
callers and focused API/CLI tests; no external CLI flag, route, scheduler, or
client-adapter contract change }`

The operation-level writer remains `runReconcileMCPFront` under the existing
single lock. The downstream-observable committed event is the atomic version-3
report write that clears `Attempt` and installs `Applied` for that row.
There is no second transaction lock and no duplicate ownership check at a
second trusted layer.

## Contracts and data model

### 1. Recovery schema version 3

Version 3 separates immutable baseline from mutable write ownership. The
report-level `Port` may remain for operator output and event diagnostics, but
rollback must never use it as ownership evidence. Current code gives `Port`
that load-bearing role at
`internal/cli/install_reconcile_mcp_front.go:567-584`; version 3 removes that
dependency.

Every row has an immutable baseline and two mutable fields:

```text
Applied = last successfully written state whose ownership evidence was made
          durable
Attempt = latest not-yet-committed write attempt
```

`Attempt` has one of:

- `prepared`: the journal is durable and the adapter call may not have
  happened, may have failed, or may have succeeded before interruption;
- `failed`: the adapter returned failure, so the previous `Applied` value
  remains the latest successful owned state;
- `ownership_unknown`: the adapter write succeeded but its post-write
  fingerprint or durable promotion failed.

`Applied` is updated only after adapter success and ownership capture. A new
attempt never erases the prior `Applied` value. This is required for a retry
from port A to B: if B fails, A remains the last proven write.

### 2. LSP entry identity and ownership

LSP row identity is the comparable triple:

```text
{ Client, Language, EntryNameOrCanonicalFallback }
```

`EntryNameOrCanonicalFallback` uses the recorded `EntryName`; an empty value
from an older in-memory producer normalizes to
`LSPRouterEntryName(Language)`. The identity helper belongs beside
`LSPRouterEntrySnapshot`; the CLI must not reconstruct it with a second
string-key convention.

Each LSP row retains the current baseline fields (`Present`, URL/relay URL,
disabled, Raw) and adds:

```text
Applied.Action = present | absent
Applied.Port   = the port of the successful forward operation
Attempt.Action = present | absent
Attempt.Port
Attempt.Phase
```

- Canonical `AddEntry` success records `Applied{present, actual port}`.
- Legacy `RemoveEntry` success records `Applied{absent, actual port}`.
- A successful retry updates only the row it wrote.
- A failed retry preserves that row's prior `Applied`.
- A legacy row removed in generation A and already absent in generation B
  retains its A ownership; absence is an owned result, not a reason to erase
  history.

Rollback logic is row-local:

- if live state already equals the immutable baseline, the row is satisfied;
- `Applied{present, P}` permits inversion only when the live entry matches the
  router shape at that row's `P`;
- `Applied{absent, ...}` permits recreation of a present baseline only when the
  live entry is still absent;
- a divergent live entry is `Conflict`, never overwritten;
- no `Applied` and no uncertain `Attempt` means this recovery generation never
  mutated the row, so rollback performs no write;
- `prepared` or `ownership_unknown` is an explicit refusal/pending condition,
  not inferred ownership.

This keeps the current exact-baseline restore behavior at
`internal/api/lsp_client_router_snapshot.go:291-338` while replacing the
single global ownership port used at
`internal/api/lsp_client_router_snapshot.go:210-220`.

### 3. Frozen LSP reconcile plan

Introduce one API-owned ephemeral plan. The exact name is implementation
detail; its required contract is:

```text
Capture:
  resolve languages, registry-backed legacy candidates, enablement, adapters,
  client reachability, baseline rows, and allowed entry identities once

Apply:
  accept only that plan; never call AllClients or discover a new entry identity
  while applying
```

The persisted record also carries explicit client capture state
(`captured_reachable` and `captured_eligible`). A client absent at capture is
represented, not omitted. If its config appears after capture, it remains
outside the plan's writable set. A captured client that becomes unreachable
may reduce actual writes to zero and produce a retryable failure, but apply may
never widen to a new client or entry.

On a forward retry with an active report:

- rebuild the ephemeral plan only from the persisted first-generation client
  scope and row identities;
- never append a newly appearing client or entry identity;
- preserve every immutable baseline row;
- re-evaluate live ownership only to decide `no-op`, `safe planned write`, or
  `conflict`; it cannot create a new target.

Existing `EnsureLSPRouterClientEntries` may remain as the public
capture-and-immediately-apply wrapper for callers without a persisted recovery
contract. The front command must use the explicit plan path. Both paths reuse
the same plan builder and apply engine, so enablement, legacy selection, and
adapter operation ordering have one owner. The current operation-order owner
is `applyLSPRouterOps` at
`internal/api/lsp_client_router.go:1023-1055`.

### 4. Serena recovery state

Do not persist write-ahead Serena backups inside a field named `Applied`.
Version 3 stores one row per client:

```text
Baseline = pinned pre-reconcile backup identity and checksum, immutable
Applied  = { port, full projected-entry SHA256 }, optional
Attempt  = { port, phase }, optional
```

The API observer contract needs three lifecycle notifications around the
existing `AddEntry` call:

- `Prepare(client, backupPath, port)` runs before `AddEntry`; failure prevents
  the write;
- `WriteFailed(client, port, cause)` runs only after `AddEntry` returns an
  error; the row is not Applied and prior Applied remains;
- `WriteSucceeded(client, port)` runs only after `AddEntry` succeeds; it reads
  the complete projected Serena entry, requires a reachable and present entry,
  computes its hash, and atomically promotes Applied.

If `WriteSucceeded` cannot read/fingerprint or persist, the adapter mutation is
already real, so the row becomes `ownership_unknown` (or remains durable
`prepared`) and the command returns a distinct failure. It must not claim
Applied.

`SerenaClientEntryFingerprint` currently returns the same empty hash state for
an absent entry and an unreachable config at
`internal/api/serena_client_reconcile.go:609-625`. Replace or wrap it with an
explicit observation:

```text
{ Reachable, Present, SHA256 }
```

Rollback requires `Reachable && Present && SHA256 == Applied.SHA256` for every
Serena row it would restore. Any unknown observation, missing Applied
ownership, prepared/unknown attempt, or mismatch refuses the whole Serena
restore before the first client write. Rows with no Applied ownership are not
restored: this generation did not successfully write them, so replaying their
backup would overwrite unrelated later state.

The existing `RestoreSerenaReconcileApplied` remains the direct compensator for
its short-lived in-process migration caller at
`internal/api/serena_client_reconcile.go:533-584`. The persisted front command
continues to own its whole-set preflight and passes only proven-owned rows to
that restore primitive; it must not add a second defensive CAS inside the
unconditional primitive.

### 5. Journal ordering

For both Serena and LSP:

```text
durable baseline + Attempt(prepared)
  -> adapter operation
  -> post-success ownership observation
  -> atomic Applied promotion + Attempt clear
```

A prepare-write failure prevents the adapter call. A post-success journal
failure stops further operations immediately. The active report stays in
place. A retry may force an idempotent write of the same hub-owned target to
establish a fresh successful write and complete promotion; it may not infer
Applied solely because live bytes happen to equal the intended target.

## Migration and compatibility

The schema must bump from version 2 to version 3 because `Port`, Serena
`Applied`, and LSP row meaning change. Current source explicitly requires a
version bump when field meaning changes at
`internal/cli/install_reconcile_mcp_front.go:142-155`.

No in-place v1/v2 migration is safe:

- v1/v2 do not say which LSP row was last written at which port;
- v1/v2 do not distinguish a prepared Serena backup from a successful
  persisted write;
- a missing or `Recorded=false` Serena fingerprint cannot be reconstructed
  after the command;
- live target-shaped content is not proof that this generation successfully
  wrote it.

Policy:

- parse v1/v2 enough to report the version and recovery-file path;
- refuse forward merge and automatic rollback before any client write;
- keep the artifact untouched and give explicit manual-recovery guidance;
- never relabel it version 3;
- version 3 is the only forward-mergeable and automatically restorable shape.

This intentionally replaces the current conditional v1 rollback admission at
`internal/cli/install_reconcile_mcp_front.go:686-741`. The compatibility window
is read/diagnose-only for v1/v2 and full forward/rollback support for v3.
Rollback of already-migrated state is “retain the old artifact and restore by
operator-guided/manual action”; the implementation must not delete or rewrite
that artifact.

## Failure modes and observable discriminators

| Failure mode | Required behavior | Observable discriminator |
| --- | --- | --- |
| Snapshot/plan read error | No report and no client write. | Error prefix `capture lsp reconcile plan`; exact client/entry read cause. |
| Newly appearing client or entry | Never enters apply operations. | Plan/report contains captured-absent client and zero change rows for it; C10 guard asserts adapter call count zero. |
| Prepare journal write fails | Adapter call is not made. | Failure op/id `recovery-prepare`; report remains at prior durable state. |
| Serena or LSP adapter write fails | Attempt becomes `failed`; prior Applied remains; command returns retryable error. | Per-client failure op `add` or `remove`, plus row `Attempt.Phase=failed`. |
| Write succeeds but fingerprint fails | No Applied promotion; stop further writes; keep report. | Failure op/id `ownership-observation`; row `ownership_unknown` or durable `prepared`. |
| Write succeeds but Applied promotion cannot persist | Stop further writes; keep report; rollback refuses the uncertain row. | Failure op/id `recovery-promote`; report row remains `prepared`. |
| Serena client unreachable at rollback | Refuse the whole Serena restore before any write. | Error id/prefix `serena-ownership-unavailable`; report path named and kept. |
| Serena or LSP live state diverged | Do not overwrite. | Serena error `serena-ownership-conflict`; LSP `Conflicted` row with client/language/entry name. |
| Applied absent-baseline LSP row unreachable | Keep recovery active. | `Pending` contains that exact entry identity; C9 guard asserts nonzero pending. |
| v1/v2 artifact | No automatic client write and no artifact rewrite/delete. | Error prefix `recovery schema version <n> lacks per-row ownership evidence`. |
| Incomplete rollback | Do not retire report. | Any `Pending`, `Failed`, `Conflicted`, `prepared`, or `ownership_unknown` row blocks the existing retirement gate. |
| Complete rollback | Retire once through the existing rename owner. | Existing atomic retirement path at `internal/cli/install_reconcile_mcp_front.go:614-645`; no new retirement mechanism. |

`Skipped` is final only for a row with no successful Applied ownership and no
uncertain attempt. An Applied row that cannot be inverted is Pending,
Conflicted, or Failed; it is never a retirement-neutral Skip.

## Test strategy

Every protected-package run must use `-tags=test_state_path_env`, a fresh
`MCPHUB_STATE_DIR_OVERRIDE`, an anchored `-run` expression, `-count=1`, and the
roadmap's timeout/boundaries
(`work-items/active/2026-07-25-mcp-front-daemon/roadmap.md:14-24`).

| Class | Exact deterministic guard | Falsifying mutation and expected failure |
| --- | --- | --- |
| C1 | `TestMCPFrontV3_LSPArtifactRoundTripPreservesCanonicalAndMultipleLegacyRows` in `internal/cli`: one client/language, one canonical and two legacy names; forward, read the CLI artifact, rollback, assert three identities and exact baseline entries. | Restore `(client, language)` keying. Artifact contains one row and the final assertion reports missing legacy entries. |
| C2 | `TestMCPFrontV3_PartialCrossPortRetryKeepsPerRowAppliedPorts` in `internal/cli`: forward two captured clients at A; retry at B with deterministic Add failure for one client; assert AppliedPort B for the successful row and A for the failed row; rollback restores both. | Replace per-row port lookup with report `Port`. The A row becomes conflict/pending and rollback cannot complete. |
| C4 | `TestMCPFrontV3_RollbackRefusesWhenSerenaFingerprintUnavailable` in `internal/cli`: after forward success, make one captured adapter unreachable or its `GetEntry` fail; assert zero restore calls across all Serena clients and active report retained. | Re-enable the unjudgeable-through branch. Restore call count becomes nonzero or the report retires. |
| C8 | `TestMCPFrontV3_SerenaAddFailureDoesNotPromoteApplied` in `internal/cli`: prepare/pin succeeds, AddEntry fails, then an operator edit is installed; assert Applied is absent, Attempt is failed, rollback does not restore that row, edit remains. | Persist prepared backup as Applied. Rollback overwrites the operator edit or Applied is unexpectedly present. |
| C9 | `TestSnapshotRestore_AppliedAbsentBaselineUnreachableIsPending` in `internal/api`: baseline Present=false, Applied present at a recorded port, adapter unreachable; assert the exact row is Pending. | Restore the current `restorable()` pending predicate. Pending becomes empty. |
| C10 | `TestMCPFrontV3_ClientAppearingBetweenCaptureAndApplyIsNotMutated` in `internal/api` or `internal/cli`: plan captures one existing control client and one captured-absent client; flip the second to reachable before apply; assert control mutation occurs and new client's Add/Remove call counts stay zero. | Re-run `AllClients`/`Exists` during apply. New client's call count becomes nonzero. |
| Schema | `TestMCPFrontV3_V2ArtifactRefusesBeforeAnyWrite` in `internal/cli`. | Admit v2 to automatic rollback. Any adapter write count becomes nonzero. |
| Uncertain write | `TestMCPFrontV3_PreparedOrUnknownAttemptBlocksRollback` in `internal/cli`. | Treat target-shaped live state as Applied. Rollback mutates or retires the record. |

The existing protected guards remain in the anchored regression set:

- C3:
  `TestMCPFrontR2_CheckWithReconcileMutatesNothing`
  (`internal/cli/install_reconcile_mcp_front_pr588_r2_test.go:102-149`);
- C5:
  `TestMCPFrontR2_SecondInvocationRefusesWhileTheTransactionLockIsHeld`
  (`internal/cli/install_reconcile_mcp_front_pr588_r2_test.go:248-281`);
- C6:
  `TestMCPFrontR2_ForwardRefusesWhenOnlyTheSerenaRouteIsLive`
  (`internal/cli/install_reconcile_mcp_front_pr588_r2_test.go:63-100`);
- C7:
  `TestRouteDaemon_SessionStoresAreReachableForExpiry`,
  `TestRouteDaemon_SessionExpiryActuallyReclaimsBoundSessions`, and
  `TestRouteDaemon_SessionExpiryStopsWithContext`
  (`internal/cli/route_session_expiry_test.go:24-117`).

The current tests that encode incomplete rules must be revised, not merely
supplemented:

- the merge test at
  `internal/cli/install_reconcile_mcp_front_pr588_test.go:487-500` must accept
  distinct entry names under one client/language;
- the absent-row test at
  `internal/api/lsp_client_router_snapshot_review_test.go:170-193` must include
  applied ownership and expect Pending when an owned created row is
  unreachable.

Each new guard requires the task's controlled mutation run, captured failure,
restored source, and restored green run. This advisor did not execute those
tests.

## Diff-invisible invariants and named regression guards

| Invariant | Named regression guard and expected result |
| --- | --- |
| Rollback mutates only state proven owned by a successful forward write of this recovery record. | `TestMCPFrontV3_RollbackRefusesWhenSerenaFingerprintUnavailable` plus `TestMCPFrontV3_PreparedOrUnknownAttemptBlocksRollback`: zero restore calls and report kept. |
| Original baseline is immutable; latest ownership is per row. | C1 round-trip and C2 partial cross-port tests: all baseline bytes remain first-generation while Applied ports split A/B. |
| The forward writable identity universe is exactly the captured plan universe. | C10 appearance test: captured control mutates; newly appearing client and unsnapshotted entries have zero adapter calls. |
| Unreachable rows that may still own mutations keep the report active. | C9 API guard plus CLI consumption assertion: Pending is nonempty and active report still exists. |
| Every removed legacy entry has distinct identity and exact inverse. | C1 guard with canonical plus two legacy names: all three rows survive artifact merge and rollback restores both legacy entries. |
| Applied means post-success ownership evidence, never write-ahead intent. | C8 Add failure guard: no Applied row; operator edit remains untouched. |
| Existing C3/C5/C6/C7 owners are unchanged. | Run the seven named existing tests; all pass without modifying their production owners. |
| Existing operation serialization remains the only transaction lock. | Static diff guard: no new `AcquireSupervisorLock`, `flock.New`, lock leaf, or lock field outside the existing wrapper; C5 test remains green. |

## Architecture claims

1. `{ guarantee: Canonical and every legacy LSP recovery row remain distinct
   through capture, persistence, merge, and restore; single-owner:
   LSPRouterEntrySnapshot comparable identity in
   internal/api/lsp_client_router_snapshot.go; enforcement-probe:
   TestMCPFrontV3_LSPArtifactRoundTripPreservesCanonicalAndMultipleLegacyRows }`
2. `{ guarantee: A partial cross-port retry is representable without a global
   ownership guess; single-owner: each LSP recovery row's Applied action and
   port; enforcement-probe:
   TestMCPFrontV3_PartialCrossPortRetryKeepsPerRowAppliedPorts }`
3. `{ guarantee: A Serena backup is not Applied until AddEntry succeeds and a
   post-write fingerprint is durably recorded; single-owner: version-3 Serena
   row state machine; enforcement-probe:
   TestMCPFrontV3_SerenaAddFailureDoesNotPromoteApplied }`
4. `{ guarantee: Unknown or unavailable Serena ownership blocks every Serena
   restore before the first write; single-owner:
   verifyMCPFrontSerenaNotEdited replacement whole-set preflight;
   enforcement-probe:
   TestMCPFrontV3_RollbackRefusesWhenSerenaFingerprintUnavailable }`
5. `{ guarantee: An unreachable LSP row is Pending exactly when Applied or an
   uncertain Attempt says this recovery record may still own a live mutation;
   single-owner: LSP recovery row NeedsRollback predicate;
   enforcement-probe:
   TestSnapshotRestore_AppliedAbsentBaselineUnreachableIsPending }`
6. `{ guarantee: No client or entry identity discovered after capture can be
   mutated by this generation; single-owner: API-owned
   LSPRouterReconcilePlan; enforcement-probe:
   TestMCPFrontV3_ClientAppearingBetweenCaptureAndApplyIsNotMutated }`
7. `{ guarantee: The first baseline is never overwritten while Applied and
   Attempt may advance independently per row; single-owner: version-3 journal
   merge/state transition function; enforcement-probe: C1 and C2 guards plus
   merge unit test }`
8. `{ guarantee: Version-1 and version-2 artifacts never drive automatic writes
   under version-3 ownership semantics; single-owner:
   validateMCPFrontReconcileReport version gate; enforcement-probe:
   TestMCPFrontV3_V2ArtifactRefusesBeforeAnyWrite }`
9. `{ guarantee: The existing operation-level lock remains the only reconcile
   transaction lock; single-owner: runReconcileMCPFront;
   enforcement-probe: static lock-constructor diff plus
   TestMCPFrontR2_SecondInvocationRefusesWhileTheTransactionLockIsHeld }`
10. `{ guarantee: C3, C5, C6, and C7 production owners are not redesigned;
    single-owner: Change-Surface Contract; enforcement-probe: git diff excludes
    internal/cli/install.go command gate and internal/cli/route.go expiry owner,
    while all seven named protected tests pass }`

## Alternatives

### A. Reject every retry whose port differs from the report port

This is smaller but rejected as the primary design. It prevents future
cross-port partial state but cannot represent or repair a mixed state already
created by a partial retry, and it keeps global ownership as a load-bearing
field. The accepted memo proves per-client writes can fail independently at
`internal/api/lsp_client_router.go:1033-1055`.

### B. Infer per-row ownership from the live URL during rollback

Rejected. A live router-shaped URL says what is present, not which successful
forward write produced it. It cannot distinguish an interrupted prepared
write, an operator-created same-shape entry, and a completed command write.
That violates the required successful-write evidence and repeats the current
unjudgeable Serena branch at
`internal/cli/install_reconcile_mcp_front.go:1111-1131`.

### C. Promote all Applied ownership once the whole forward run returns

Rejected. Per-client failures and interruption occur before whole-run return.
The current API already reports add/remove results per entry at
`internal/api/lsp_client_router.go:1033-1055`; delaying persistence discards
the only information needed to represent partial success.

## Security and resource posture

- Pinned backups may contain tokens or environment secrets; retain the existing
  owner-only atomic publication and checksum verification documented at
  `internal/cli/install_reconcile_mcp_front.go:744-787`.
- No new background process, listener, GUI path, scheduler access, or
  process-global mutable state is introduced.
- The API plan is operation-scoped and released when the existing command
  context returns; it owns no resource beyond existing adapter references.
- Journal and client mutations remain inside the existing operation lock and
  timeout. Post-success promotion failure stops the operation rather than
  accumulating additional untracked mutations.

## Adjacent findings

None added. The accepted memo's stale global-Port polarity comment at
`internal/cli/install_reconcile_mcp_front.go:1164-1197` is directly in C2's
owning function and must be corrected as part of the schema/merge edit, not
filed separately.

## Terms and Abbreviations

- **Applied**: durable evidence of the latest successful forward write for one
  recovery row.
- **Attempt**: durable state of a write that is prepared, known failed, or has
  unknown post-write ownership.
- **CAS**: compare-and-swap style check; write only when live state still
  matches the recorded owned state.
- **CLI**: command-line interface.
- **LSP**: Language Server Protocol.
- **Pending**: rollback work that remains active because the client or owned
  state cannot currently be reached.
- **DP3-sealed**: design-panel context-isolation requirement; this native
  advisor run is explicitly not counted toward it.

## Gate

Gate: **RETURN(lead)** — coherent INPUT_ONLY advice is ready for panel
synthesis, but this artifact is non-counted and does not claim design `PASS`.
