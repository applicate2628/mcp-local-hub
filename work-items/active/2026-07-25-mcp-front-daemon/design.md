# PR #588 front-reconcile recovery design

Date: 2026-07-27  
Owner: main conversation holding `$lead`  
Input: `research-live-findings-2026-07-27.md`  
Input SHA-256: `01B263C82328AA4AB6A6874E6EDE12060F2A862DF5294E1E209B50B08F6A734F`

## R2 superseding contract

The accepted-ready R2 architecture contract is
`work-items/decisions/2026-07-27-mcp-front-reconcile-v3-row-journal.md`
(status `proposed`; promotion to `accepted` remains the
`$architecture-reviewer` gate). It supersedes this package's attempt-settlement,
Serena pin-authority, point-in-time LSP mutation, dependency-retry, and
retirement-probe decisions. Downstream planning, implementation, and review
must consume the decision-registry contract where the two artifacts differ.

Its protected surfaces remain binding: `internal/cli/install.go`,
`internal/cli/route.go`, the operation-level reconcile lock, total Serena/LSP
preflight, exact first baseline, per-row receipt port, frozen LSP population,
canonical-before-legacy forward ordering, and version-1/version-2 no-write
refusal.

## Decision

Adopt a version-3, row-owned recovery journal for the six open finding
classes: C1, C2, C4, C8, C9, and C10. The first baseline for each exact client
entry is immutable. Every forward attempt has a frozen mutation population,
and successful ownership is recorded per row only after the resulting state is
observed and durably persisted.

Extend the repository's existing lock-scoped `clients.CASEntryMutator` with a
rollback-specific restore method that preserves the current expected-live
comparison but calls the restore core with the rollback bypass
`allowHubEntry=true`. The current generic CAS restore deliberately uses
`allowHubEntry=false` (`internal/clients/cas_mutator.go:250-258`), while Serena
rollback must restore a normal legacy hub entry through the bypass contract
(`internal/api/serena_client_reconcile.go:549-560`). All seven Serena adapters
already implement the existing capability
(`internal/api/serena_client_reconcile.go:164-188`;
`internal/clients/cas_mutator.go:112-162`), so the new method stays within that
same nine-adapter allowlist and `lockingClient` forwarder. Do not broaden this
change into CAS support for the 38 LSP-eligible adapters outside that
deliberate capability allowlist. The admitted LSP findings are closed by exact
row identity, frozen dependency-group planning, per-row applied state, and the
existing exact live-entry ownership comparison
(`internal/api/lsp_client_router_snapshot.go:255-340`).

The design gate is **PASS**. It is planner-eligible after the required
reliability review.

## Pinned scope and evidence

The accepted research classified the 14 bot rows as ten defect classes:

| Classification | Classes |
| --- | --- |
| ALREADY FIXED | C3 `--check` dispatch; C5 operation lock; C6 LSP readiness; C7 route-owned cleanup |
| REAL, open | C1 legacy row identity; C2 per-row latest port; C4 Serena rollback ownership; C8 Serena success promotion; C9 absent/unreachable pending state; C10 snapshot/apply population |
| WRONG | None |

The accepted class evidence is
`research-live-findings-2026-07-27.md:183-238`. The adapter capability addendum
proves that ordinary rollback calls do not combine the ownership comparison
and mutation under one lock, while `CASEntryMutator` does
(`research-adapter-cas-seam-2026-07-27.md:13-40`,
`research-adapter-cas-seam-2026-07-27.md:284-323`).

## Design-panel record

### Roster

| Lane | Framing | Execution | Counted artifact | Completion |
| --- | --- | --- | --- | --- |
| `journal` | Recovery artifact and retry state machine | external-worker replacing architect; Codex CLI `gpt-5.6-sol` / `xhigh` | `.scratch/design-panel/design-journal-r3.md` | exit 0; `RETURN(lead)` |
| `ownership` | Mutation authority, receipts, and rollback predicates | external-worker replacing architect; Codex CLI `gpt-5.6-sol` / `xhigh` | `.scratch/design-panel/design-ownership-r3.md` | exit 0; `RETURN(lead)` |
| internal advisor | Change surface and current owners | architect; native internal agent | `design-advisor-internal-2026-07-27.md` | non-counted `INPUT_ONLY` |

Both counted lanes received the same pinned research memo and hash, separate
framing files, separate neutral working directories, and no other candidate's
output. The two launch completion oracles independently showed process exit 0,
nonempty output, the requested provenance header, and `RETURN(lead)`. The
earlier uncounted attempts without a durable exit-code record were excluded.

### Set-level comparison

| Claim or surface | Lane source | Agreement or unique contribution | Conflict | Evidence checked | Final disposition |
| --- | --- | --- | --- | --- | --- |
| Version-3 private recovery schema | both counted lanes; internal advisor | Agreement | None | Current version/global-port shape at `internal/cli/install_reconcile_mcp_front.go:220-245` | Adopt |
| Immutable first baseline | both counted lanes; internal advisor | Agreement | None | Existing merge polarity at `internal/cli/install_reconcile_mcp_front.go:1149-1162` | Adopt |
| Exact LSP identity includes entry name | both counted lanes; internal advisor | Agreement | None | Snapshot already carries `EntryName` at `internal/api/lsp_client_router_snapshot.go:61-70`; CLI key omits it at `internal/cli/install_reconcile_mcp_front.go:1245-1249` | Adopt |
| Per-row applied ownership and port | both counted lanes; internal advisor | Agreement | None | One global port is supplied to every row at `internal/cli/install_reconcile_mcp_front.go:573-584` | Adopt |
| Frozen LSP capture/apply population | both counted lanes; internal advisor | Agreement | None | Snapshot and apply independently enumerate at `internal/api/lsp_client_router_snapshot.go:127-136` and `internal/api/lsp_client_router.go:194-202` | Adopt |
| Serena prepare before write, promote after observed result | both counted lanes; internal advisor | Agreement | None | Current write-ahead callback precedes `AddEntry` at `internal/api/serena_client_reconcile.go:463-487` | Adopt |
| Unknown post-write state blocks automatic action | both counted lanes; internal advisor | Agreement | None | Current `Recorded=false` branch proceeds toward restore at `internal/cli/install_reconcile_mcp_front.go:1111-1131` | Adopt |
| Journal representation | `journal` proposes ordered generations; `ownership` proposes baseline, plan, and receipts | Both preserve the same invariants | Full attempt history versus compact current state | Active artifact is temporary; no accepted retention need requires full history | Synthesize a compact row ledger plus one active plan and monotonic generation |
| Serena rollback mutation | `ownership` requires true adapter-owned CAS; `journal` requires exact precondition checking | Same safety goal | Existing generic CAS restore rejects the legacy hub backup Serena rollback must restore | Addendum proves all seven Serena adapters support the capability; reliability review proves the current restore polarity is incompatible at `internal/clients/cas_mutator.go:250-258` | Extend the existing capability with a rollback-bypass CAS restore; preserve the allowlist and conflict result |
| LSP rollback mutation | `ownership` preferred true CAS; `journal` retained exact ownership comparison | Different breadth | CAS exists for only 9/47 adapters; 38 LSP-eligible adapters are outside it | Exhaustive matrix at `research-adapter-cas-seam-2026-07-27.md:102-174` | Do not expand CAS allowlist; retain exact existing LSP compare and state the advisory-lock residual |
| Operator-diverged row | `journal` makes exact divergence a terminal skip; `ownership` leaves conflict active | Different recovery lifecycle | Whether to wedge the active record after a proven external edit | User explicitly permits pending or skipped; exact divergence proves the row is not owned | Use terminal `skipped-conflict`, emit exact diagnostics, never mutate |
| Version-1/version-2 artifacts | both counted lanes; internal advisor | Agreement | None | Those versions lack per-row successful-write evidence | Read for diagnostics; refuse automatic forward merge and rollback; leave artifact untouched |
| Post-completion history | `journal` calls retention unspecified; `ownership` does not require it | No accepted requirement | None | Existing command/report lifecycle has no accepted audit-retention contract | Add no new archive artifact |

No unresolved design conflict remains. The deliberate LSP CAS boundary is not
presented as an atomic conditional-write guarantee: the per-config file lock is
advisory, and non-lock-honoring editors remain outside it
(`internal/clients/config_lock.go:32-36`). That limitation already exists in
the LSP comparison model cited by the finding and is not used to justify
Serena restoration, where the existing CAS capability is available.

## Change-Surface Contract

| Surface | Ownership and permitted change |
| --- | --- |
| `internal/cli/install_reconcile_mcp_front.go` | Own version-3 journal schema, immutable merge, generation/plan persistence, Serena lifecycle callbacks, per-row LSP ownership, compatibility refusal, rollback retirement, and stable diagnostics |
| `internal/api/lsp_client_router.go` | Add an operation-scoped plan builder/applicator or equivalent private seam that uses one captured client population and reports exact per-row mutation outcomes |
| `internal/api/lsp_client_router_snapshot.go` | Preserve exact canonical/legacy identities and classify rollback from row-applied evidence, including absent-baseline/unreachable pending state |
| `internal/api/serena_client_reconcile.go` | Expose post-attempt observation and perform rollback through `clients.CASEntryMutator`, returning conflict/pending outcomes instead of unconditional overwrite |
| `internal/clients/cas_mutator.go` | Add one rollback-bypass CAS restore method to the existing capability, nine admitted concrete implementations, and `lockingClient` forwarder; do not widen the allowlist |
| Tests under `internal/api` and `internal/cli` | Add the six class guards, compatibility guard, and protected-regression guards |

Protected production owners:

- do not change the top-level `--check` gate in `internal/cli/install.go`;
- do not add another reconcile transaction lock or alter the existing lock
  wrapper at `internal/cli/install_reconcile_mcp_front.go:352-369`;
- do not weaken or move the Serena/LSP readiness preflight;
- do not change route session expiry, GUI, tray, supervisor, scheduler,
  launcher, registry, or router-demotion behavior;
- do not make the private recovery schema a general client-adapter contract.

The only persisted-contract change is the private recovery artifact version.
No CLI flag, success-body, GUI, service, or public configuration contract is
changed.

## Version-3 journal model

The journal contains:

- schema version `3`;
- a monotonic generation number;
- one immutable row map keyed by
  `(surface, client, language, entry_name)`;
- one active generation plan containing target port, ordered row identities,
  exact planned operations, and dependency groups keyed by
  `(client, language)`;
- per-row latest forward attempt;
- per-row latest durable applied receipt;
- per-row rollback disposition.

Serena uses `surface=serena`, an empty language, and entry name `serena`. Its
baseline retains the pinned backup path and checksum. Its applied receipt
contains the exact post-write fingerprint.

LSP rows retain the full `LSPRouterEntrySnapshot` baseline. An applied receipt
stores the exact observed post-state—present with the complete adapter
projection, or explicitly absent—and the port written by that row's latest
successful attempt. A legacy removal is a separate row whose baseline is
present and whose applied post-state is absent.

Every attempt persists its generation, row identity, operation, exact planned
pre-state, and exact intended post-state. The latest attempt is one of:

- `prepared`: durable intent exists and the mutation outcome is not yet proven;
- `confirmed-no-write`: readback equals the prepared pre-state;
- `applied`: the observed post-state and durable receipt agree;
- `conflict`: the observed state matches neither the prepared pre-state nor
  the intended post-state.

An older applied receipt remains effective after a later
`confirmed-no-write` attempt. A later `prepared` or `conflict` attempt shadows
older ownership and blocks automatic retry or rollback until its state can be
classified safely. The first baseline is never replaced.

The one active plan cannot be replaced while any row references it through
`prepared` or `conflict`. Immediately after journal validation, both forward
and rollback invoke the single attempt-settlement owner. It compares current
state with the durable pre-state and intended post-state, then durably
transitions to `applied`, `confirmed-no-write`, or an explicit
pending/conflict state before any new plan or inverse. A missing generation,
operation, pre-state, post-state, or plan reference is structural corruption
and authorizes no client mutation.

This compact representation rejects an append-only event log because replay,
tail repair, and compaction would add new correctness owners without serving a
current acceptance criterion.

## Forward transaction

1. Acquire the existing whole-operation reconcile lock.
2. Complete the existing ownership, Serena route, and LSP route preflights.
3. Validate the current journal and settle every durable `prepared` attempt.
   An unreadable or third-state result stops the command; a new generation
   cannot replace a plan still referenced by `prepared` or `conflict`.
4. Construct the eligible client set once, using the fail-closed constructor
   surface. Any constructor, registry, enablement, or snapshot failure aborts
   before a new plan or client mutation.
5. Build one exact plan containing all canonical additions/rewrites and every
   legacy removal. A client absent at capture is outside the plan even if it
   appears before application. Group each canonical row with its legacy rows
   by client/language.
6. Merge only previously unseen immutable baselines, persist the complete plan,
   and then apply only its ordered operations.
7. Immediately before `prepared`, read the exact live entry and require it to
   equal the plan's captured pre-state. A mismatch is a durable
   `conflict-before-write`; it is never adopted as a new pre-state and receives
   no adapter call.
8. Persist `prepared` with the generation, operation, exact captured pre-state,
   and exact intended post-state.
9. Invoke the adapter mutation. After either success or error, inspect the
   resulting entry:
   - intended post-state: persist an applied receipt;
   - original pre-state: persist `confirmed-no-write`;
   - unreadable or any third state: leave `prepared`/`conflict`, stop further
     writes, and return a recovery-evidence error.
10. Release same-group legacy removals only after the canonical target receipt
    is durably effective. A canonical failure or uncertain result preserves
    every legacy route. Unrelated client/language groups may continue only
    after the current row transition is durable.

For Serena, the current backup-captured callback becomes preparation only. A
mandatory post-attempt callback runs after `AddEntry` returns on both success
and error, before the API advances to another client. It performs result
observation and the journal transition, and its error stops further writes.
`MigrateReport.Applied` remains an API write-result and no longer doubles as
durable rollback authority.

For LSP, plan application must not call `AllClients()` or admit a row absent
from the persisted plan. Per-row result reporting must retain client,
language, entry name, operation, observed post-state, and applied port.

If a client write succeeds but receipt persistence fails, the already durable
`prepared` state is the authority. The command stops; automatic retry and
rollback for that row are refused rather than inferring ownership.

## Rollback transaction

1. Acquire the same operation lock and validate schema, completeness, row
   identities, plan references, pins, and checksums before the first mutation.
2. Invoke the same attempt-settlement owner used by forward. Any unreadable or
   third-state attempt remains pending/conflict and blocks its group before an
   inverse.
3. Refuse automatic forward merge or rollback for version-1/version-2
   artifacts. Decode enough to identify the version and artifact path, leave
   the file untouched, and return `legacy-ownership-unproven`.
4. For each version-3 row:
   - no effective applied receipt and no uncertain attempt: terminal
     `baseline-only`, no write;
   - uncertain attempt: `pending-ownership-unknown`, no write;
   - unreachable client with effective ownership: `pending-unreachable`, no
     write;
   - live state equals immutable baseline: terminal `restored`, no write;
   - live state equals the effective applied post-state: apply the exact
     inverse;
   - live state matches neither: terminal `skipped-conflict`, no write.
5. Serena inverses must use `clients.AsCASEntryMutator` and the new
   rollback-bypass CAS restore method, supplying the recorded applied
   fingerprint as the expected-live matcher. `ErrCASConflict` maps to
   `skipped-conflict`; unsupported CAS is a structural error because every
   admitted Serena adapter is in the current CAS allowlist.
6. Process LSP rollback by dependency group. Restore and verify every
   forward-removed legacy row first. Only after all required legacy
   dispositions are durably `restored` may the canonical row be restored or a
   forward-created canonical entry be removed. A legacy pending/failure keeps
   the canonical front route and the group pending. A canonical inverse failure
   after legacy restoration may leave both routes, which is safe and retryable.
   Unrelated groups may continue.
7. LSP inverses use the exact applied post-state/per-row port as ownership
   evidence and the immutable row baseline as the inverse. An absent baseline
   with an applied-created receipt remains pending while unreachable.
8. Read back the immutable baseline after every inverse before marking the row
   restored. A write or verification failure remains pending.
9. Persist each row/group disposition before any dependent inverse and before
   retirement. A disposition persistence failure stops the command and keeps
   the active journal.
10. Compute retirement eligibility from the re-read durable journal, not the
    in-memory result. Retire only when every row/group is terminal:
    `baseline-only`, `restored`, or `skipped-conflict`. A terminal conflict
    makes the requested rollback partial: return a stable non-success result
    naming the skipped identities even if the active journal is retired.

## Failure modes and observability

| Failure code | Meaning | Required behavior |
| --- | --- | --- |
| `capture-incomplete` | Client construction, registry, enablement, or snapshot input failed | No plan and no client write |
| `forward-previous-attempt-uncertain` | A prepared/conflict row still references the active plan after settlement | Refuse plan replacement and every new mutation |
| `forward-plan-precondition-conflict` | Exact live entry changed after plan capture | Persist conflict; no write and do not adopt the new state |
| `journal-prepare-failed` | Plan or row intent was not durable | No write for that row |
| `forward-confirmed-no-write` | Adapter returned error and readback equals pre-state | Keep prior applied receipt, if any |
| `forward-ownership-unknown` | Result cannot be read or matches neither pre nor intended post-state | Stop, keep active journal, no automatic retry/rollback |
| `promotion-not-durable` | Intended post-state was observed but receipt write failed | Durable prepared intent remains; stop |
| `rollback-client-unreachable` | Owned or uncertain row cannot be inspected | Pending; keep journal |
| `rollback-live-diverged` | Exact live state differs from baseline and applied receipt | Skip without write; identify row |
| `rollback-cas-conflict` | Serena CAS observed a different live entry inside the mutation lock | Skip without write; identify row |
| `rollback-cas-capability-mismatch` | Serena adapter lacks the rollback-capable CAS method | Structural failure; no Serena write |
| `rollback-route-preservation-blocked` | A required legacy LSP row is not durably restored | Keep canonical route and group pending |
| `rollback-write-failed` | Exact inverse failed | Pending; keep journal |
| `rollback-verify-failed` | Inverse returned success but baseline readback failed | Pending; keep journal |
| `legacy-ownership-unproven` | Version 1/2 lacks row ownership evidence | Read-only refusal; keep artifact |

Diagnostics may contain stable reason codes, client, language, entry name,
generation, and state. They must not include pinned backup contents, raw
entries, tokens, headers, or environment values.

## Verification strategy

| Class | Mandatory deterministic guard | Defect mutation that must fail it |
| --- | --- | --- |
| C1 | One client/language with absent canonical plus two distinct legacy entries survives CLI persist/reload, forward, and rollback | Remove `entry_name` from row identity |
| C2 | A succeeds for two rows; B succeeds for one and has a proven no-write failure for the other; receipts are B/A and rollback restores both | Resolve ownership from one report-level port |
| C4 | Operator changes a receipted Serena entry before rollback; CAS returns conflict and performs no restore | Replace CAS restore with ordinary restore |
| C8 | Inject Serena `AddEntry` failure before mutation; row is not applied and later operator state is untouched | Promote in the backup-captured callback |
| C9 | Table-driven applied-created and uncertain-created absent baselines become unreachable; `TestSnapshotRestore_AppliedOrUncertainAbsentBaselineUnreachableIsPending` reports pending and keeps the journal for both | Restore the old `restorable()`-only pending predicate or omit the uncertain case |
| C10 | Client is absent at capture and present at application; it receives zero Add/Remove calls until a later generation | Re-enumerate clients during plan application |
| Compatibility | `TestMCPFrontV3_V1AndV2ArtifactsRefuseBeforeAnyWrite` covers version 1 and version 2 as separate table cases; both cause zero adapter writes and remain byte-identical | Fall through to automatic merge/restore or omit either version case |
| Prepared settlement | Simulate write landing before receipt persistence; re-entry promotes only from the durable pre/intended pair and refuses a third state | Replace the active plan before settlement |
| LSP rollback dependency | Make the second legacy restore fail; canonical front route remains and the group stays pending | Invert canonical independently before all legacy rows verify |
| Plan precondition | Change a planned LSP entry after capture; it receives zero adapter mutations | Adopt the late live state as the attempt pre-state |
| Durable retirement | Fail disposition persistence or leave one group pending; active journal remains | Compute retirement from in-memory results |

Each class guard requires a controlled mutation run that fails, restored
source, and a green rerun. Every `internal/api` or `internal/cli` test command
must use `-tags=test_state_path_env` and a fresh
`MCPHUB_STATE_DIR_OVERRIDE`. No unscoped `go test ./...` is permitted.

Protected guards must also remain green:

- `TestMCPFrontR2_CheckWithReconcileMutatesNothing`;
- `TestMCPFrontR2_SecondInvocationRefusesWhileTheTransactionLockIsHeld`;
- `TestMCPFrontR2_ForwardRefusesWhenOnlyTheSerenaRouteIsLive`;
- the three `TestRouteDaemon_SessionExpiry*` tests;
- existing CAS admission/locking tests under `internal/clients`.

## Final architecture claims

1. `{ guarantee: canonical and every legacy LSP entry remain distinct through
   capture, persistence, merge, and restore; single-owner: version-3 row
   identity; enforcement-probe:
   TestMCPFrontV3_LSPArtifactRoundTripPreservesCanonicalAndMultipleLegacyRows }`
2. `{ guarantee: partial cross-port retries retain the latest proven ownership
   independently per row; single-owner: version-3 applied receipt;
   enforcement-probe:
   TestMCPFrontV3_PartialCrossPortRetryKeepsPerRowAppliedPorts }`
3. `{ guarantee: an immutable first baseline is never replaced by a retry;
   single-owner: version-3 row merge; enforcement-probe: the C1 and C2 guards
   compare first-generation baseline bytes after retry }`
4. `{ guarantee: Serena rollback never overwrites an entry that differs from
   the recorded applied fingerprint at mutation time and can restore the normal
   legacy hub backup; single-owner: rollback-bypass method on
   clients.CASEntryMutator through RestoreSerenaReconcileApplied;
   enforcement-probe:
   TestMCPFrontV3_SerenaCASRestoresLegacyHubBackupAndRefusesConcurrentEdit }`
5. `{ guarantee: Serena ownership is promoted only after post-attempt state is
   observed and durable; single-owner: Serena row transition function;
   enforcement-probe:
   TestMCPFrontV3_SerenaAddFailureDoesNotPromoteApplied }`
6. `{ guarantee: unreachable LSP rows remain pending exactly when an applied or
   uncertain attempt may still own a mutation; single-owner: row rollback
   classifier; enforcement-probe:
   TestSnapshotRestore_AppliedOrUncertainAbsentBaselineUnreachableIsPending
   with separate applied and uncertain table cases }`
7. `{ guarantee: a generation mutates only the exact client and entry
   population captured in its durable plan; single-owner: API LSP plan
   applicator; enforcement-probe:
   TestMCPFrontV3_ClientAppearingBetweenCaptureAndApplyIsNotMutated }`
8. `{ guarantee: unknown post-write ownership authorizes neither retry nor
   rollback; single-owner: prepared attempt state; enforcement-probe:
   TestMCPFrontV3_PostWriteEvidenceFailureStaysPending }`
9. `{ guarantee: no client mutation occurs without a durable exact prepared
   row; single-owner: version-3 row transition owner; enforcement-probe:
   TestMCPFrontV3_EveryMutationRequiresDurablePrepared }`
10. `{ guarantee: every prepared row is settled from its durable
   generation/pre/intended tuple before plan replacement or rollback;
   single-owner: attempt settlement function; enforcement-probe:
   TestMCPFrontV3_ReentrySettlesWriteReceiptCrashWindows }`
11. `{ guarantee: a plan referenced by a prepared or conflict attempt cannot be
    replaced; single-owner: new-generation admission gate; enforcement-probe:
    TestMCPFrontV3_UncertainAttemptBlocksPlanReplacement }`
12. `{ guarantee: a later confirmed-no-write attempt preserves the older
    effective applied receipt; single-owner: effective-receipt resolver;
    enforcement-probe:
    TestMCPFrontV3_ConfirmedNoWriteKeepsEarlierPortOwnership }`
13. `{ guarantee: forward never removes a legacy LSP route until the canonical
    target is durably observed; single-owner: LSP dependency-group applicator;
    enforcement-probe:
    TestMCPFrontV3_CanonicalFailurePreservesAllLegacyRoutes }`
14. `{ guarantee: LSP rollback restores and verifies every required legacy row
    before inverting the canonical row; single-owner: dependency-group
    rollback applicator; enforcement-probe:
    TestMCPFrontV3_LegacyRestoreFailureKeepsCanonicalRoute }`
15. `{ guarantee: plan application mutates only a durable row whose exact
    captured pre-state still matches immediately before prepare; single-owner:
    LSP plan applicator; enforcement-probe:
    TestMCPFrontV3_PlanPopulationAndPrestateAreFrozen }`
16. `{ guarantee: active retirement is computed only from durably persisted
    terminal row/group dispositions; single-owner: rollback retirement gate;
    enforcement-probe:
    TestMCPFrontV3_PersistenceFailureOrPendingGroupPreventsRetirement }`
17. `{ guarantee: version-1 and version-2 artifacts never drive automatic writes
    under version-3 semantics; single-owner: recovery schema gate;
    enforcement-probe:
    TestMCPFrontV3_V1AndV2ArtifactsRefuseBeforeAnyWrite with separate version-1
    and version-2 table cases }`
18. `{ guarantee: C3, C5, C6, and C7 retain their current owners and behavior;
    single-owner: Change-Surface Contract; enforcement-probe: protected test
    set plus a diff check excluding install dispatch and route expiry owners }`

## Residual risk

The repository's per-config lock is advisory. Serena rollback is atomic with
respect to hub participants honoring that lock; an unrelated process that
ignores the lock can still race any read-modify-write sequence
(`internal/clients/config_lock.go:32-36`). The LSP path retains its existing
point-in-time ownership comparison because extending the deliberate CAS
allowlist to 38 additional adapters would be a separate cross-cutting client
format project, not a correction required by the six admitted classes.

A crash after a client write and before receipt persistence cannot be made
cross-file atomic with the current storage boundaries. The durable prepared
intent makes that window visible and fail-closed; it may require operator
recovery rather than an unsafe automatic inverse.

## Terms and Abbreviations

- **Applied receipt**: durable exact evidence of the latest successful forward
  mutation owned for one row.
- **CAS**: compare-and-set; compare expected live state and mutate while holding
  the same repository-owned client configuration lock.
- **CLI**: command-line interface.
- **LSP**: Language Server Protocol.
- **Pending**: nonterminal recovery work that keeps the active journal.
- **Row**: one exact Serena or LSP client entry identity and its recovery state.
- **Serena**: the Serena Model Context Protocol client entry reconciled to the
  front route.
