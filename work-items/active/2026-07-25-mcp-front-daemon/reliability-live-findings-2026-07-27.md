# PR #588 recovery reliability gate

Date: 2026-07-27  
Role: `$reliability-engineer`  
Returns to: `$lead`  
Inputs:

- `design.md`
- `research-live-findings-2026-07-27.md`
- `research-adapter-cas-seam-2026-07-27.md`
- current source under `internal/cli`, `internal/api`, and `internal/clients`

## R1 gate — superseded by R2 below

**REVISE — not planner-eligible.**

The version-3 row journal is the right recovery direction, and the compact
one-active-plan representation can preserve immutable baselines plus per-row
latest ownership. It is not yet safe to implement because:

1. the selected Serena mutation primitive cannot perform the required restore;
   `CASRestoreEntryFromBytes` deliberately calls the guarded restore core with
   `allowHubEntry=false` and can return `ErrBackupEntryAlreadyMigrated`
   (`internal/clients/cas_mutator.go:250-258`), while Serena rollback explicitly
   needs the bypass polarity to restore its normal legacy hub entry
   (`internal/api/serena_client_reconcile.go:549-560`);
2. neither forward nor rollback defines the LSP dependency barrier that keeps at
   least one route alive: canonical success must precede legacy removals, and
   legacy restoration must be durably verified before a newly-created canonical
   entry is removed. The design currently states only forward ordering
   (`design.md:171-172`) and then treats rollback rows independently
   (`design.md:194-214`);
3. a durable `prepared` attempt has no explicit re-entry settlement transition
   before a new plan replaces the only plan containing that attempt's intended
   post-state. The design says the attempt blocks until safely classified
   (`design.md:143-146`) but defines no classifier invocation in either
   transaction (`design.md:152-215`);
4. plan application does not explicitly require the live entry to still equal
   the captured pre-state immediately before mutation. The current LSP flow
   reads during planning and later mutates through independently locked methods
   (`internal/api/lsp_client_router.go:218-258`,
   `internal/api/lsp_client_router.go:1023-1055`), and the existing advisory
   lock does not make that earlier comparison part of the write critical
   section (`internal/clients/cas_mutator.go:39-52`).

## Method and evidence boundary

CodeGraph was queried first for the recovery flow. It reported that this
worktree has no `.codegraph/` index and that indexing is user-owned, so no index
was initialized. The analysis therefore used targeted, line-numbered reads.

No test, build, vet, GUI, tray, supervisor, scheduler, or process command was
run. The read-only role boundary and the protected test rules are recorded at
`roadmap.md:14-24`. Every failure mode below is therefore
`analysis-only`; the required injection probes are implementation and QA gates,
not evidence already obtained.

## Reliability objectives

These are operation-level service-level objectives (SLOs) for the explicit CLI
transaction, not availability promises for the long-running route daemon.

| SLO | Service-level indicator and measurement point | Window | Threshold | Consequence at burn |
| --- | --- | --- | --- | --- |
| No unjournaled mutation | At the CLI journal boundary, count adapter mutations whose exact row lacks a durable `prepared` state | One forward invocation and its recovery chain | 0 | Error budget exhausted; abort release |
| No unowned inverse | At the adapter mutation boundary, count rollback writes whose live state was not the row's exact applied post-state under the required ownership primitive | One rollback invocation and all re-entries | 0 | Error budget exhausted; abort release |
| No false retirement | At the active-report rename boundary, count retirements with any nondurable, pending, uncertain, or failed row/group | Lifetime of one active journal | 0 | Error budget exhausted; journal logic is unsafe |
| Retry convergence | At the report readback boundary, count completed retry chains that do not converge to one effective receipt or one explicit terminal conflict per row | One active journal lifetime | 100% converge | Error budget exhausted; disable automatic retry/rollback |
| Route continuity | At the LSP client-entry boundary, count rollback failure injections that remove/revert the canonical route before all required legacy restores for that client/language are verified | Every legacy-bearing rollback group | 0 | Error budget exhausted; abort release |

## Compact one-active-plan verdict

**Conditionally accepted after revision.** Full append-only attempt history is
not required for C1/C2/C4/C8/C9/C10. One immutable baseline map, one effective
receipt per row, one latest attempt per row, and one active plan are sufficient
only if all five invariants below are explicit:

| Invariant | Required owner | Current design status |
| --- | --- | --- |
| A plan cannot be replaced while any row references it through `prepared` or `conflict` | New-generation admission gate | Missing; only the blocking intent is stated at `design.md:143-146` |
| Every latest attempt contains or can resolve immutable generation, operation, exact pre-state, and exact intended post-state | Version-3 row attempt schema | Partial; fields are described semantically at `design.md:119-141`, not as a persistence invariant |
| Re-entry settles every `prepared` row before forward planning or rollback classification | Attempt-settlement owner | Missing from both transaction algorithms at `design.md:152-215` |
| `confirmed-no-write` clears only the latest attempt and never deletes an older effective applied receipt | Row transition owner | Stated at `design.md:143-146`; keep |
| A new plan is published atomically as one complete population before its first row preparation | Plan publication owner | Stated at `design.md:156-163`; keep and test |

Without the first three rules, replacing the one active plan erases the only
evidence needed to distinguish "the failed write did nothing" from "the write
landed but receipt persistence failed." With them, old settled plans are
redundant because the immutable baseline and effective receipt retain all
rollback authority.

## Scenario gate

| Scenario | Expected durable transitions | Reliability verdict |
| --- | --- | --- |
| C1: absent canonical plus two legacy rows; forward succeeds | Persist three exact baseline rows; canonical `prepared -> applied`; each legacy removal gets its own `prepared -> applied(absent)` receipt | Representation is sound because identity includes `entry_name` (`design.md:117-133`) |
| C1 rollback; second legacy restore fails | Restore and verify legacy rows before removing the created canonical row; if either legacy row is pending/failed, keep canonical and the group pending | **Missing dependency barrier.** Independent row order can recreate the original "no LSP route" outcome |
| C2: A applied to rows X/Y; B applies to X and provably does not write Y | X receipt becomes B; Y keeps A after B's `confirmed-no-write`; rollback uses B/A independently | Sound after explicit settlement and effective-receipt rules |
| C2: B write to X lands, receipt persistence fails | Durable B `prepared` shadows A; re-entry reads X and promotes B only when exact intended state is observed | Intended fail-closed behavior exists at `design.md:183-185`, but the settlement entry point is missing |
| C4: Serena live entry still equals forward result | Restore the pinned legacy backup only through one lock-scoped expected-live compare and restore; verify backup state | **Selected method is incompatible.** Current CAS restore uses `allowHubEntry=false` (`internal/clients/cas_mutator.go:250-258`), but this rollback requires the bypass (`internal/api/serena_client_reconcile.go:549-560`) |
| C4: operator edits Serena before or during rollback | Expected-live mismatch returns conflict; no write; row becomes terminal `skipped-conflict` with a loud partial outcome | Requires a rollback-capable CAS seam, not the current method |
| C8: Serena `AddEntry` returns error before any mutation | Readback equals prepared pre-state; persist `confirmed-no-write`; no new receipt; preserve any older receipt | Sound after the post-attempt callback becomes mandatory on both success and error |
| C8: Serena `AddEntry` returns error after committing | Readback equals intended state; persist `applied`; the return error does not lie about ownership | Sound after the same callback contract |
| C9: absent LSP baseline was created, then client is unreachable | Effective created receipt makes the row `pending-unreachable`; journal remains active | Sound at `design.md:194-214` |
| C10: client absent during plan build, present during apply | No plan row exists, so the applicator makes zero calls for that client | Sound only if the applicator uses the captured map/plan and never calls `AllClients()` |
| Planned row changes between capture and prepare | Exact mismatch becomes conflict before the mutation; the changed entry is not adopted as a new pre-state | **Missing exact precondition rule.** Capturing the changed value as the attempt pre-state would authorize an overwrite |
| Version 1 or 2 artifact | Identify version, make zero adapter calls, leave bytes unchanged, return `legacy-ownership-unproven` | Sound at `design.md:189-193` |

## Failure-state transition table

| Durable state before action | Observation or event | Required next durable state | May mutate? | May retire? |
| --- | --- | --- | --- | --- |
| no plan | capture/constructor/read fails | no plan | No | No active journal to retire |
| complete active plan, row unattempted | plan persistence or row-prepare persistence fails | unchanged/unattempted | No | No |
| row unattempted | exact live state differs from planned pre-state | `conflict-before-write` | No | Only after explicit terminal-skip policy |
| row unattempted | exact live state equals planned pre-state and prepare persists | `prepared(pre, intended, generation)` | Yes, one adapter attempt | No |
| `prepared` | readback equals intended post-state | `applied` plus durable exact receipt | No further write needed | Not until all rows/groups terminal |
| `prepared` with older receipt | readback equals prepared pre-state | `confirmed-no-write`; older receipt remains effective | No | Not until all rows/groups terminal |
| `prepared` | readback unreadable | `pending-ownership-unknown` | No | No |
| `prepared` | readback equals neither pre nor intended | `conflict` shadowing older receipt | No | No automatic retirement |
| effective receipt, live equals baseline | rollback readback proves already restored | `restored` | No | When durably terminal |
| effective receipt, live equals applied post-state | exact inverse succeeds and baseline readback matches | `restored` | Yes | When durably terminal |
| effective receipt, client unreachable | no trustworthy read | `pending-unreachable` | No | No |
| effective receipt, live is a third state | exact external divergence | `skipped-conflict` | No | Yes only as a loud partial result |
| inverse attempted | write fails or baseline verification fails | `pending-write` or `pending-verify` | No further dependent write | No |
| any transition | journal persistence fails | retain last durable state; stop | No subsequent dependent write | No |
| every row/group durably terminal | active-name rename succeeds | retired | No | Yes |
| every row/group terminal in memory only | terminal-state persistence fails | active journal unchanged | No | No |

## Return-path inventory

### Forward

| Return path | Required state on return |
| --- | --- |
| operation-lock acquisition fails | No report/client change; current lock owner remains authoritative (`internal/cli/install_reconcile_mcp_front.go:352-369`) |
| preflight fails | No report/client change (`internal/cli/install_reconcile_mcp_front.go:395-425`) |
| prior report read, parse, schema, or uncertain-attempt settlement fails | Existing artifact byte state remains active; no new plan or client write |
| client construction, enablement, registry, snapshot, or exact plan build fails | No new plan and no client write; this preserves the current fail-closed capture posture (`internal/api/lsp_client_router_snapshot.go:114-181`) |
| complete-plan persistence fails | No row preparation and no client write |
| row-prepare persistence fails | That row receives no adapter call; stop before dependent operations |
| adapter returns and readback equals pre-state | Persist `confirmed-no-write`; for canonical LSP add failure, do not execute dependent legacy removals |
| adapter returns and readback equals intended state | Persist receipt; only then release dependent operations |
| readback is unavailable or a third state | Keep `prepared`/`conflict`, stop the generation, refuse automatic retry and rollback until settlement |
| receipt persistence fails | Last durable state remains `prepared`; stop; never infer ownership from the adapter return |
| all plan rows settle | Return success only when no pending/conflict row remains; per-row business failures are not converted to success |

### Rollback

| Return path | Required state on return |
| --- | --- |
| read/parse/schema/pin/plan-reference validation fails | Zero adapter writes; artifact untouched |
| version 1/2 encountered | Zero adapter writes; artifact byte-identical; loud refusal |
| `prepared` settlement is unreadable or third-state | Zero inverse for that row/group; pending journal |
| client unreachable with effective or uncertain ownership | Pending journal; no inverse |
| live equals immutable baseline | Persist `restored` without a write |
| live equals effective post-state | Run the exact inverse, read back baseline, then persist `restored` |
| live is exact divergence | No write; persist `skipped-conflict`; report a non-success partial outcome |
| Serena ownership changes inside the mutation lock | CAS conflict; no write; persist terminal conflict only after observing/classifying it |
| legacy LSP restore fails | Keep canonical route unchanged; group pending; independent groups may continue |
| canonical inverse fails after legacy restoration | Both routes may coexist; group pending; retry remains idempotent |
| rollback disposition persistence fails | Stop before retirement and before any dependent destructive inverse |
| any pending/failed/uncertain group remains | Keep active journal; return nonzero |
| all groups durably terminal but active-name rename fails | Keep active journal and return nonzero (`internal/cli/install_reconcile_mcp_front.go:614-623`) |
| retirement succeeds, cleanup fails | Active namespace is clear; warn; cleanup is bounded and non-load-bearing (`internal/cli/install_reconcile_mcp_front.go:624-632`) |

## Retry and idempotency contract

There is no automatic retry loop. Maximum attempts are one adapter mutation per
row per command invocation. Backoff and jitter are therefore not applicable;
subsequent attempts are explicit operator re-entry.

| Mutation | Idempotency mechanism | Settled/committed event |
| --- | --- | --- |
| Forward add/rewrite | Exact planned pre-state guard plus durable `prepared`; exact intended-state readback | Durable per-row `applied` receipt |
| Forward removal | Exact planned legacy-entry guard; blocked until canonical target receipt is effective | Durable absent post-state receipt |
| Rollback restore | Exact effective-post ownership comparison; Serena uses a rollback-capable lock-scoped CAS | Durable `restored` after baseline readback |
| Rollback remove | Exact effective-created state comparison; absence is already-done | Durable `restored` after absence readback |
| Re-entry settlement | No mutation; classify current state against durable pre/intended pair | Durable `applied`, `confirmed-no-write`, or pending/conflict |

The writer/event pair is single-owned by the version-3 row transition owner:
the adapter writes, but only a durable post-observation receipt is the
`settled/committed` event. An adapter's nil return is not that event.

## Critical failure detection and response

| Failure mode | Signal | Detection latency | Page? | Evidence status |
| --- | --- | --- | --- | --- |
| Rollback removes canonical before required legacy restore | Stable `rollback-route-preservation-blocked` row/group diagnostic and nonzero command result | Same invocation, before canonical inverse | No page; operator CLI failure | `analysis-only` — top failure; read-only design stage forbade the required mutation injection |
| Serena rollback primitive rejects the legacy hub backup | Stable `rollback-cas-capability-mismatch` diagnostic with client | Same invocation, before any Serena write | No page; release-blocking test failure | `analysis-only` |
| Crash/error after write before receipt | Durable `prepared` row found at next invocation | First subsequent forward/rollback invocation | No page; command refuses until settled | `analysis-only` |
| New generation tries to replace plan with uncertain rows | Stable `forward-previous-attempt-uncertain` refusal | Same invocation, before plan write | No page | `analysis-only` |
| LSP plan pre-state drifted before apply | Stable `forward-plan-precondition-conflict` row diagnostic | Same invocation, before adapter mutation | No page | `analysis-only` |
| Client becomes unreachable during rollback | Existing `rollback-client-unreachable` class, row remains pending (`design.md:225-225`) | Same invocation | No page | `analysis-only` |
| Journal transition cannot persist | Existing `journal-prepare-failed` / `promotion-not-durable` class (`design.md:220-224`) | Same invocation | No page; command stops | `analysis-only` |
| Retirement rename fails | Nonzero CLI error; active report path still resolves | Same invocation | No page | `analysis-only`; current owner at `internal/cli/install_reconcile_mcp_front.go:614-623` |

Diagnostics must stay content-safe as required by `design.md:232-234`.

## Required design revisions

1. **Replace the Serena CAS claim with a capability that can restore rollback
   backups.** The design must select one owner that performs
   re-read -> expected-live compare -> rollback-bypass restore under one config
   lock. It may be a narrow rollback-specific CAS capability or an explicit
   rollback method added to the existing capability, but it cannot call the
   current `CASRestoreEntryFromBytes` unchanged. The selected method must state
   its adapter allowlist and preserve `ErrCASConflict`. This is an architecture
   surface change because `design.md:16-23` and `design.md:94-94` currently
   assert no CAS-owner change.
2. **Add a single attempt-settlement owner invoked immediately after journal
   validation in both forward and rollback.** It compares current state with
   the durable pre/intended pair and persists exactly `applied`,
   `confirmed-no-write`, or pending/conflict before any new plan or inverse.
3. **Make plan replacement fail closed.** A new generation cannot replace the
   active plan while any latest attempt still references it as
   `prepared`/`conflict`. Attempt records must carry generation and operation,
   and plan validation must prove every reference resolves.
4. **Add exact plan preconditions.** Immediately before `prepared`, the current
   LSP entry must equal the plan's captured pre-state. A mismatch is conflict,
   not a new pre-state to adopt. This is point-in-time protection; the design
   must retain the advisory-lock residual already disclosed at
   `design.md:301-309`.
5. **Model LSP operations as dependency groups keyed by client/language.**
   Forward legacy removals require a durable effective canonical-target receipt.
   Rollback restores and verifies all removed legacy rows before it
   reverts/removes the canonical row. A legacy pending/failure blocks the
   canonical inverse but not unrelated groups.
6. **Persist every rollback disposition before dependent work and retirement.**
   A disposition-write failure keeps the active report and returns nonzero.
   Retirement eligibility is computed from durable state, not only an in-memory
   report.
7. **Define command outcomes.** `skipped-conflict` may be terminal and permit
   retirement, but the requested rollback is partial and must return a stable
   non-success result naming the skipped identities. Pending, uncertain,
   persistence failure, and verification failure always keep the active report.
8. **Make post-attempt observation a total Serena callback contract.** It runs
   after `AddEntry` returns on both success and error, before the API advances
   to another client, and returns an error that stops further writes when the
   readback or journal transition is not durable. The existing callback runs
   only before the write (`internal/api/serena_client_reconcile.go:463-487`).

## Residual risks after revision

- The per-config lock is advisory. Hub participants that use the wrapper are
  serialized, but a non-lock-honoring editor can race any LSP point-in-time
  comparison (`internal/clients/config_lock.go:32-36`;
  `research-adapter-cas-seam-2026-07-27.md:341-345`). The revised design must
  preserve this as an explicit residual, not call LSP rollback atomic CAS.
- Cross-file atomicity between a client config and the journal is unavailable.
  Durable prepare plus settlement converts the write/receipt gap into a visible
  fail-closed recovery state; it does not eliminate the gap
  (`design.md:311-314`).
- A terminal conflict intentionally leaves operator-diverged state in place.
  The command must not describe that as a full restore even if the active
  journal is retired.
- Version-1/version-2 read-only refusal can require manual recovery. That is an
  accepted safe degradation, provided the artifact remains byte-identical and
  the diagnostic names the exact path and version.

## Reliability claims required for PASS

1. `{ guarantee: no client mutation occurs without a durable exact prepared
   row; single-owner: version-3 row transition owner; enforcement-probe:
   TestMCPFrontV3_EveryMutationRequiresDurablePrepared }`
2. `{ guarantee: a prepared row is settled against its durable pre/intended pair
   before a new plan or rollback proceeds; single-owner: attempt settlement
   function; enforcement-probe:
   TestMCPFrontV3_ReentrySettlesWriteReceiptCrashWindows }`
3. `{ guarantee: a plan referenced by an uncertain attempt cannot be replaced;
   single-owner: new-generation admission gate; enforcement-probe:
   TestMCPFrontV3_UncertainAttemptBlocksPlanReplacement }`
4. `{ guarantee: a later confirmed-no-write attempt preserves the older
   effective applied receipt; single-owner: effective-receipt resolver;
   enforcement-probe:
   TestMCPFrontV3_ConfirmedNoWriteKeepsEarlierPortOwnership }`
5. `{ guarantee: Serena rollback compares expected live state and performs the
   rollback-bypass restore under one lock; single-owner: rollback-capable Serena
   CAS owner; enforcement-probe:
   TestMCPFrontV3_SerenaCASRestoresLegacyHubBackupAndRefusesConcurrentEdit }`
6. `{ guarantee: forward never removes a legacy LSP route until the canonical
   target is durably observed; single-owner: LSP dependency-group applicator;
   enforcement-probe:
   TestMCPFrontV3_CanonicalFailurePreservesAllLegacyRoutes }`
7. `{ guarantee: rollback never removes or reverts the canonical LSP route until
   every required legacy restore in its group is durably verified;
   single-owner: LSP rollback dependency-group applicator; enforcement-probe:
   TestMCPFrontV3_LegacyRestoreFailureKeepsCanonicalRoute }`
8. `{ guarantee: a generation mutates only rows whose exact captured pre-state
   still matches and whose identity is in its durable plan; single-owner: LSP
   plan applicator; enforcement-probe:
   TestMCPFrontV3_PlanPopulationAndPrestateAreFrozen }`
9. `{ guarantee: an absent-baseline row with effective or uncertain ownership
   remains pending while unreachable; single-owner: row rollback classifier;
   enforcement-probe:
   TestSnapshotRestore_AppliedAbsentBaselineUnreachableIsPending }`
10. `{ guarantee: active retirement occurs only from durable terminal row/group
    dispositions and an atomic active-name rename; single-owner: rollback
    retirement gate; enforcement-probe:
    TestMCPFrontV3_PersistenceFailureOrPendingGroupPreventsRetirement }`
11. `{ guarantee: version-1 and version-2 artifacts drive zero client writes and
    remain byte-identical; single-owner: recovery schema gate;
    enforcement-probe: TestMCPFrontV3_LegacyArtifactsAreReadOnlyRefusals }`

Claims 1-11 are **requirements**, not accepted implementation facts. The gate
remains `REVISE` until the canonical design incorporates revisions 1-8 and the
planner can assign these probes.

## R1 terminology

- **Applied receipt**: durable exact evidence that one forward mutation's
  intended post-state was observed.
- **CAS**: compare-and-set; compare expected live state and mutate under one
  repository-owned client-config lock.
- **CLI**: command-line interface.
- **LSP**: Language Server Protocol.
- **Pending**: nonterminal recovery state that keeps the active journal.
- **SLO**: service-level objective.

## R2 re-verification — 2026-07-27

### R2 gate

**PASS — RETURN(lead); planner-eligible.**

The lead incorporated all eight R1 design revisions. The compact
one-active-plan journal now has a complete interruption/re-entry contract, and
all eleven R1 reliability claims have one owner plus a scope-complete named
falsifier. This is a **design gate**, not implementation evidence: the current
source still contains the version-2 behavior, and no test, build, vet, GUI,
tray, supervisor, scheduler, or process command was run in this read-only R2
review.

### Eight-revision closure

| R1 revision | Revised design evidence | Current-source grounding | R2 verdict |
| --- | --- | --- | --- |
| 1. Rollback-capable Serena CAS | The decision requires a rollback-specific method with the expected-live comparison and `allowHubEntry=true`, within the current allowlist and forwarder (`design.md:16-31`); the Change-Surface Contract assigns it to `internal/clients/cas_mutator.go` (`design.md:94-103`); rollback names the exact method/polarity and conflict result (`design.md:239-243`) | The generic method currently uses `allowHubEntry=false` (`internal/clients/cas_mutator.go:250-258`); the capability, nine-adapter allowlist, and wrapper resolution live at `internal/clients/cas_mutator.go:70-99`, `internal/clients/cas_mutator.go:112-162`, and `internal/clients/cas_mutator.go:685-702`; Serena's seven clients are the existing set at `internal/api/serena_client_reconcile.go:164-188` | **Closed in design.** It extends the correct owner without widening adapter admission |
| 2. Shared prepared-attempt settlement | One settlement owner is invoked after validation by both forward and rollback and persists `applied`, `confirmed-no-write`, or pending/conflict before later action (`design.md:158-165`, `design.md:173-177`, `design.md:219-225`) | The current source has no equivalent owner: Serena fingerprints are captured only at final commit (`internal/cli/install_reconcile_mcp_front.go:961-1014`), confirming this is a required new version-3 owner rather than duplicated existing logic | **Closed in design.** Both entry paths call the same owner |
| 3. Fail-closed plan replacement and exact references | Every attempt carries generation, identity, operation, pre-state, and intended post-state; unresolved references are corruption; a referenced plan cannot be replaced (`design.md:144-165`) | The current report has one global port and no per-attempt reference fields (`internal/cli/install_reconcile_mcp_front.go:237-277`), so version 3 is the owning schema change | **Closed in design.** Uncertain attempts retain all settlement evidence |
| 4. Exact planned-prestate guard | The applicator re-reads immediately before prepare, requires exact captured pre-state, persists conflict on mismatch, and makes no adapter call (`design.md:187-190`) | Current LSP planning reads at `internal/api/lsp_client_router.go:218-258`, while mutations occur later at `internal/api/lsp_client_router.go:1023-1055`; the design closes that explicit gap while retaining the advisory-lock residual | **Closed in design.** Late state is never adopted as authority |
| 5. LSP dependency groups and legacy-first rollback | The plan groups canonical and legacy rows by client/language (`design.md:127-129`, `design.md:181-202`); rollback restores and durably verifies all legacy rows before canonical inversion and keeps canonical on failure (`design.md:244-250`) | Current restore builds and applies row operations without this dependency barrier (`internal/api/lsp_client_router_snapshot.go:255-342`) | **Closed in design.** Forward and inverse ordering preserve at least one route |
| 6. Durable disposition before dependent work/retirement | Every row/group disposition is persisted before a dependent inverse; persistence failure stops; retirement is computed from a re-read durable journal (`design.md:254-263`) | Current retirement is correctly atomic at the active-name boundary (`internal/cli/install_reconcile_mcp_front.go:614-632`) but has no version-3 row dispositions, so the new durable eligibility owner composes with the existing rename owner | **Closed in design.** In-memory success cannot retire the artifact |
| 7. Loud terminal conflicts | `skipped-conflict` is terminal only after exact divergence, and the command returns stable non-success naming skipped identities even if retirement succeeds (`design.md:229-263`) | Current LSP restore already distinguishes `Skipped` at exact ownership checks (`internal/api/lsp_client_router_snapshot.go:264-309`), while the current CLI can retire after no pending/failed rows (`internal/cli/install_reconcile_mcp_front.go:595-645`); the revised design fixes the caller outcome | **Closed in design.** Safe retention of operator state is not reported as full rollback |
| 8. Total Serena post-attempt callback | The callback runs after `AddEntry` returns on success and error, before the next client; observation or persistence error stops further writes (`design.md:204-209`) | The current API exposes only the pre-write `OnBackupCaptured` hook and continues from `AddEntry` success/error directly into report control flow (`internal/api/serena_client_reconcile.go:463-487`) | **Closed in design.** Every adapter return reaches the settlement writer/event pair |

### Compact representation final verdict

**Accepted.** The revised schema satisfies every condition from the R1 compact
representation gate:

| Required invariant | Single owner in revised design | Falsifier |
| --- | --- | --- |
| Do not replace a referenced plan | New-generation admission gate (`design.md:158-165`) | `TestMCPFrontV3_UncertainAttemptBlocksPlanReplacement` (`design.md:360-362`) |
| Persist exact attempt evidence | Version-3 attempt schema (`design.md:144-151`) | `TestMCPFrontV3_ReentrySettlesWriteReceiptCrashWindows` (`design.md:356-359`) |
| Settle before forward or rollback | Shared attempt-settlement function (`design.md:158-165`, `design.md:173-177`, `design.md:219-225`) | Same crash-window probe |
| Preserve an older receipt after confirmed no-write | Effective-receipt resolver (`design.md:153-156`) | `TestMCPFrontV3_ConfirmedNoWriteKeepsEarlierPortOwnership` (`design.md:363-366`) |
| Publish the complete plan before mutation | Version-3 plan publication owner (`design.md:178-192`) | `TestMCPFrontV3_EveryMutationRequiresDurablePrepared` and `TestMCPFrontV3_PlanPopulationAndPrestateAreFrozen` (`design.md:353-355`, `design.md:375-378`) |

No append-only attempt log is needed: unsettled evidence cannot be overwritten,
and settled history is reduced without loss into the immutable first baseline,
latest exact attempt, and effective receipt.

### Eleven required claims

| R1 claim | Revised design owner | Named falsifier | R2 verdict |
| --- | --- | --- | --- |
| 1. Every mutation has durable prepare | Version-3 row transition owner (`design.md:352-355`) | `TestMCPFrontV3_EveryMutationRequiresDurablePrepared` | PASS |
| 2. Prepared rows settle before plan/rollback | Attempt settlement function (`design.md:355-359`) | `TestMCPFrontV3_ReentrySettlesWriteReceiptCrashWindows` | PASS |
| 3. Referenced plans cannot be replaced | New-generation admission gate (`design.md:360-362`) | `TestMCPFrontV3_UncertainAttemptBlocksPlanReplacement` | PASS |
| 4. Confirmed no-write preserves older ownership | Effective-receipt resolver (`design.md:363-366`) | `TestMCPFrontV3_ConfirmedNoWriteKeepsEarlierPortOwnership` | PASS |
| 5. Serena expected-live CAS restores legacy backup | Rollback-bypass `CASEntryMutator` method through the Serena restore owner (`design.md:331-336`) | `TestMCPFrontV3_SerenaCASRestoresLegacyHubBackupAndRefusesConcurrentEdit` | PASS |
| 6. Canonical success gates forward legacy removal | LSP dependency-group applicator (`design.md:367-370`) | `TestMCPFrontV3_CanonicalFailurePreservesAllLegacyRoutes` | PASS |
| 7. Legacy restore gates canonical rollback | Dependency-group rollback applicator (`design.md:371-374`) | `TestMCPFrontV3_LegacyRestoreFailureKeepsCanonicalRoute` | PASS |
| 8. Frozen population and exact pre-state gate writes | LSP plan applicator (`design.md:346-349`, `design.md:375-378`) | `TestMCPFrontV3_ClientAppearingBetweenCaptureAndApplyIsNotMutated`; `TestMCPFrontV3_PlanPopulationAndPrestateAreFrozen` | PASS |
| 9. Applied or uncertain absent rows stay pending while unreachable | Row rollback classifier (`design.md:341-345`) | Table-driven `TestSnapshotRestore_AppliedOrUncertainAbsentBaselineUnreachableIsPending`, with separate applied and uncertain cases | PASS |
| 10. Retirement uses durable terminal state | Rollback retirement gate (`design.md:379-382`) | `TestMCPFrontV3_PersistenceFailureOrPendingGroupPreventsRetirement` | PASS |
| 11. Version 1 and 2 are read-only refusals | Recovery schema gate (`design.md:383-387`) | Table-driven `TestMCPFrontV3_V1AndV2ArtifactsRefuseBeforeAnyWrite`, with separate v1 and v2 cases | PASS |

### Failure-mode and rollout readiness

The R1 operation-level SLOs, retry/idempotency contract, and critical-failure
table remain binding. The revised design now owns stable same-invocation
signals for uncertain-plan refusal, precondition conflict, promotion failure,
Serena CAS mismatch/conflict, route-preservation blocking, rollback
write/verification failure, and legacy-schema refusal
(`design.md:265-287`). Detection latency remains the current CLI invocation;
these are operator-visible nonzero outcomes, not paging events.

All critical failure modes remain `analysis-only` because this R2 role was
explicitly read-only. The top failure—removing the canonical route before a
required legacy restoration is proven—now has the deterministic mutation
falsifier at `design.md:301-301`, but that drill is
**ASSUMPTION (UNVERIFIED — not yet executed)**.

| Delivery stage | Abort signal | Threshold and observation window | Required drill status |
| --- | --- | --- | --- |
| Version-3 journal and CAS implementation | Any named claim probe fails, or any mutation run does not fail the guard | Zero failures across each scoped command invocation | `ASSUMPTION (UNVERIFIED)` until implementation/QA |
| API/CLI integration | Any C1/C2/C4/C8/C9/C10 or protected-regression guard fails | Zero failures across the required scoped tagged test runs | `ASSUMPTION (UNVERIFIED)` until implementation/QA |
| Commit gate | Mutation proof missing, scoped tagged tests fail, or build/vet exits nonzero | Zero missing proofs/failures in the final verification session | `ASSUMPTION (UNVERIFIED)` until QA and lead reconciliation |

### R2 residual risks

- The LSP lock remains advisory and is not represented as kernel-enforced CAS
  (`design.md:389-397`). The exact pre-state check closes deterministic drift
  inside the plan/apply lifecycle; a non-lock-honoring external editor can
  still race the point-in-time check.
- Journal and client config remain separate files. Durable prepare plus shared
  settlement makes the crash window visible and fail-closed but does not make
  the two writes atomic (`design.md:399-402`).
- The new rollback-bypass CAS method is a planned source change. R2 PASS does
  not claim that the current source already implements it.
- Terminal divergence is an intentional partial rollback. The revised
  non-success result prevents operational tooling from mistaking it for full
  restoration.

### R2 final decision

**PASS — RETURN(lead); planner-eligible.** No reliability blocker remains in
the canonical design. Planning must preserve all eleven claims and the three
unexecuted verification stages above; implementation is not accepted until
their mutation and green-run evidence exists.

## Terms and Abbreviations

- **Applied receipt**: durable exact evidence that one forward mutation's
  intended post-state was observed.
- **CAS**: compare-and-set; compare expected live state and mutate under one
  repository-owned client-config lock.
- **CLI**: command-line interface.
- **LSP**: Language Server Protocol.
- **Pending**: nonterminal recovery state that keeps the active journal.
- **SLO**: service-level objective.
