# Decision: version-3 front-reconcile row journal

- **status:** proposed
- **date:** 2026-07-27
- **owner:** `$architect`
- **work-item:** `2026-07-25-mcp-front-daemon`
- **supersedes:** the unsafe attempt-settlement, Serena pin-authority, and
  point-in-time LSP mutation parts of
  `work-items/active/2026-07-25-mcp-front-daemon/design.md`

## Context

The current version-3 implementation establishes exact row identity, immutable
baselines, frozen LSP population, per-row ports, and durable dispositions
(`internal/cli/install_reconcile_mcp_front.go:162-257`). Independent review
found seven remaining contract gaps:

| Gap | Verified current behavior |
| --- | --- |
| F1 authorization | Serena and LSP compare, back up, prepare, and mutate in separate lock acquisitions; a changed live entry can be overwritten (`.scratch/external-reviews/adversarial.out:57-98`; `internal/api/lsp_client_router.go:292-341`). |
| F2 causation | Re-entry promotes both durable `prepared` and no-write `conflict` attempts solely because live state equals intended state (`internal/cli/install_reconcile_mcp_front.go:327-359`). |
| F3 absent Serena inverse | Serena rollback always restores bytes, while the restore core rejects a backup in which the entry was absent (`internal/api/serena_client_reconcile.go:740-768`; `internal/clients/cas_mutator.go:248-266`). |
| F4 dependency retry | Terminal LSP rows are omitted on retry, so a prior legacy conflict disappears from the canonical-route gate (`internal/cli/install_reconcile_mcp_front.go:818-834`; `internal/api/lsp_client_router_snapshot.go:166-205`). |
| A-01 pin authority | Rollback consumes `Rows`, but pin verification consumes `Serena.Applied` plus `Pins` (`internal/cli/install_reconcile_mcp_front.go:757-780`; `internal/cli/install_reconcile_mcp_front.go:1128-1162`). |
| A-02 mutation probe | The prepared-order test does not invoke a production mutation seam, and the retirement test does not drive the caller's durable re-read (`.scratch/external-reviews/claim.out:65-77`). |
| A-03 decision persistence | No durable decision record owned this cross-package persisted-state contract (`.scratch/external-reviews/claim.out:79-94`). |

The locking decorator is the existing single owner of client-config critical
sections, and every production factory returns that wrapper
(`internal/clients/config_lock.go:160-182`). The existing
`clients.CASEntryMutator` is deliberately a nine-concrete-adapter allowlist
(`internal/clients/cas_mutator.go:119-165`); broadening that restore capability
would incorrectly give unrelated adapters backup-byte semantics they do not
own.

## Decision

Keep version 3 private and read-only-refuse versions 1 and 2. Replace split
authorization and compatibility projections with one exact row journal and one
wrapper-owned conditional mutation primitive.

### Change-Surface Contract

| Field | Contract |
| --- | --- |
| Intended change surface | `internal/clients/config_lock.go`, `internal/clients/cas_mutator.go`, `internal/api/serena_client_reconcile.go`, `internal/api/lsp_client_router.go`, `internal/api/lsp_client_router_snapshot.go`, `internal/cli/install_reconcile_mcp_front.go`, and only their focused tests. |
| Approved extension seams | A new `clients.ConditionalEntryMutator` implemented by `lockingClient`; the existing `clients.CASEntryMutator` for Serena rollback; API callbacks carrying prepared and observed results; version-3 row validation and rollback disposition callbacks. |
| Writer-owner | `lockingClient.ConditionalEntryMutation` is the only writer-owner for Serena forward and every LSP forward/rollback add or remove. Existing `CASEntryMutator` remains the writer-owner for Serena rollback restore/remove. |
| Settled/committed event | The conditional primitive returns `EntryMutationObserved{Invoked, Before, After, MutationErr, ObservationErr}` before releasing the config lock; the CLI's single row-transition owner emits the durable `applied`, `confirmed-no-effect`, `rollback-restored`, or pending/conflict disposition. |
| Protected / must-not-touch | `internal/cli/install.go`, `internal/cli/route.go`, operation-level reconcile lock, total Serena/LSP preflight, exact first baseline, per-row receipt port, frozen LSP population, canonical-before-legacy forward ordering, and version-1/version-2 no-write refusal. |
| Declared blast radius | One private recovery schema and the client-config mutations initiated by `--reconcile-mcp-front`; no public CLI flag, client-config format, dependency, GUI, tray, supervisor, or route-daemon lifecycle change. |

Updating `config_lock.go` is required because its current canonical comment says
each lock scope contains only one adapter write (`internal/clients/config_lock.go:32-50`);
the recovery seam deliberately persists prepared evidence inside that scope.

### 1. Conditional mutation boundary

Add a neutral capability implemented only by `*lockingClient`, not by concrete
adapters:

`ConditionalEntryMutation(request) -> EntryMutationObserved`

The request contains an entry name, an exact expected-live matcher, optional
backup retention, one typed operation (`add` or `remove`), and a
`BeforeMutation` callback. Under one `withConfigLock` call the wrapper:

1. reads the exact live entry through the wrapped concrete adapter;
2. rejects a mismatch as `precondition-conflict` with `Invoked=false`;
3. optionally calls the concrete adapter's `BackupKeep`;
4. calls `BeforeMutation` to persist the exact row, pin, and `prepared` state;
5. if that callback fails, returns with `Invoked=false`;
6. calls the concrete adapter's `AddEntry` or `RemoveEntry`;
7. re-reads the entry and returns the before/after observation before unlock.

The callback may persist recovery state but may not mutate the client or retain
the unwrapped adapter. Lock order is
`reconcile operation lock -> one config lock -> recovery state-file lock`; no
path may acquire these in reverse.

This capability closes F1 for every production adapter because all production
factories return `lockingClient` (`internal/clients/config_lock.go:178-182`).
Capability absence is fail-closed: durable pending/no mutation. It does **not**
add any concrete adapter to `CASEntryMutator`, so the deliberate nine-adapter
backup-restore allowlist remains unchanged
(`internal/clients/cas_mutator.go:127-137`).

Serena forward uses this primitive so its live pre-state, backup, row pin,
durable prepare, adapter invocation, and readback share the same adapter-owned
critical section. LSP forward canonical adds and legacy removes, and LSP
rollback restores/removes, use the same primitive. Existing point-in-time
`GetEntry -> BackupKeep -> AddEntry/RemoveEntry` sequences are removed.

The advisory lock protects concurrent hub participants. A process that ignores
the advisory lock can still race the underlying file replacement; that is a
residual platform limit, not authority to restore point-in-time checks
(`internal/clients/config_lock.go:32-50`).

### 2. Forward attempt state machine

| Durable state | Mutation provenance | Re-entry rule | Automatic inverse authority |
| --- | --- | --- | --- |
| none/planned | none | May authorize only through the conditional primitive | none |
| `precondition-conflict` | `Invoked=false` | Never promote from value equality; terminal external conflict for this attempt | none; an older receipt is shadowed while live state conflicts |
| `prepared` | invocation may or may not have occurred | Always `pending-ownership-unknown` after process re-entry; equality with pre or intended is not causation | none until an atomic write receipt exists |
| `applied` | same-call `Invoked=true`, observed intended state, and durable receipt | Effective per-row receipt | yes |
| `confirmed-no-effect` | same-call `Invoked=true`, observed exact pre-state, and durable transition | Preserve an older effective receipt | no new authority |
| `post-write-unknown` | same-call invocation with unreadable/third state | pending; stop later writes | none |

Only the same-call `EntryMutationObserved` result may promote a prepared row.
If receipt persistence fails after mutation, durable `prepared` remains and the
command stops. Re-entry may report current equality for diagnosis but may not
turn it into `applied` or `confirmed-no-effect`. This deliberately chooses
manual/fail-closed recovery for the cross-file crash window because no current
primitive atomically commits both the client config and recovery journal
(`internal/cli/install_reconcile_mcp_front.go:215-217`).

`precondition-conflict` is not an attempt state consumed by post-write
settlement. It records `Invoked=false` and can never create an applied receipt.
The current equality-based settlement owner and the current
`recordLSPPreconditionConflict -> conflict -> settle as applied` path are
removed (`internal/cli/install_reconcile_mcp_front.go:327-359`;
`internal/cli/install_reconcile_mcp_front.go:1374-1394`).

### 3. Row-owned Serena pins and inverses

Each authoritative Serena row directly contains:

- exact immutable baseline presence and fingerprint;
- pinned backup path, origin, and SHA-256 checksum;
- latest attempt and applied receipt;
- rollback disposition.

The top-level persisted `Serena`, `LSP`, `Pins`, `Applied`, and diagnostic
`Port` projections are removed from the version-3 decision type. Runtime
`MigrateReport` remains a display/result type only. Read logic first decodes a
minimal version envelope: versions 1 and 2 are refused byte-identically; only a
version-3 row type is decoded for mutation.

Before any rollback write, validation iterates authoritative `Rows` and requires
each Serena row to resolve to exactly one nonempty, unique pin path and checksum,
with the path contained under the report's pin directory and its bytes matching
the recorded checksum. Missing, extra, disagreeing, duplicate, escaped, unreadable,
or changed pins reject the whole rollback with zero client writes. LSP rows may
carry no Serena pin.

Serena rollback branches on immutable baseline presence:

| Baseline | Exact inverse | Conflict behavior |
| --- | --- | --- |
| present | `CASRestoreEntryFromBytesForRollback`, matching the effective applied fingerprint | preserve changed/absent live entry; terminal `skipped-conflict` |
| absent | `CASGuardedRemoveEntry`, matching the effective applied fingerprint | preserve changed replacement; terminal `skipped-conflict`; already absent is verified success |

Both methods remain on the existing `CASEntryMutator` allowlist. Successful
return is followed by exact baseline verification before durable `restored`.
Thus every legal Serena add has an ownership-checked inverse; the current
always-restore path (`internal/api/serena_client_reconcile.go:754-768`) is
removed.

### 4. Durable LSP dependency groups

Every rollback invocation reconstructs each `(client, language)` group from
**all** authoritative LSP rows, including terminal conflicts, restored rows,
and baseline-only rows. The CLI never filters terminal rows before calling the
group owner.

For every legacy row, group readiness is recomputed from the live exact entry:

| Legacy row state | Group predicate |
| --- | --- |
| restored | live must still equal immutable baseline and represent an active route |
| baseline-only | live must equal immutable baseline and represent an active route |
| terminal conflict | not ready; preserve canonical |
| pending/failed/unreachable | not ready; preserve canonical |
| exact baseline but disabled/non-routable | not ready; preserve canonical |

The API owns one `legacyRouteReady` predicate using the same exact snapshot and
adapter-format semantics as capture. Omission of a row never means ready.

Only when every legacy row is live-verified ready may the canonical inverse
run. Otherwise:

- retryable unreadable/failed legacy rows leave canonical `pending`;
- a terminal legacy conflict or non-routable baseline gives the canonical row
  terminal `skipped-dependency-conflict` without mutating it.

Terminal dependency conflicts permit atomic retirement only after every other
row is terminal, but rollback returns a stable non-success result naming every
skipped row. Therefore retry cannot erase a legacy barrier, and completion does
not falsely report a full rollback.

### 5. Rollback and retirement

Rollback uses these steps:

1. acquire the operation lock;
2. decode version envelope and validate the complete version-3 row journal;
3. verify every row-owned Serena pin before the first client mutation;
4. classify unresolved forward `prepared` rows pending without equality-based
   promotion;
5. process Serena rows and full LSP groups through their atomic mutation owners;
6. persist each observed row/group disposition before a dependent inverse;
7. re-read the report through `ReadStateFileInodeAnchored`;
8. recompute retirement eligibility from that durable object only;
9. atomically rename the active report; return non-success if any terminal
   conflict was preserved.

An inverse that already equals the immutable baseline is safely satisfied
without claiming who caused it. A rollback prepared-state crash may therefore
converge by baseline verification; unlike forward ownership, inverse completion
does not authorize a later destructive action except through the independently
reconstructed LSP group predicate.

### 6. Superseded helpers and projections

Remove, rather than layer another check over:

- `newMCPFrontReconcileJournal`;
- `mcpFrontReconcileJournal.commit` and fingerprint projections;
- `verifyMCPFrontSerenaNotEdited`;
- `mergeMCPFrontReconcileReport`;
- top-level version-3 `Serena`, `LSP`, `Pins`, `Applied`, and `Port` decision
  fields;
- tests whose only purpose is those superseded pre-v3 helpers.

Go reference inspection found no production caller for
`newMCPFrontReconcileJournal`, `commit`, or
`verifyMCPFrontSerenaNotEdited`; their remaining references are definitions or
tests. `mergeMCPFrontReconcileReport` is reached only through the stale
constructor plus one legacy test. This is the required C6 cleanup: provenance
stays in this decision record and version control, not beside the live row
owner.

## Failure modes and observability

| Failure ID | Discriminator | Required behavior |
| --- | --- | --- |
| `conditional-capability-missing` | adapter is not a locking-wrapper conditional mutator | no config mutation; durable pending/structural failure |
| `forward-precondition-conflict` | lock-scoped live state differs from exact planned state; `Invoked=false` | durable no-write conflict; never equality-promote |
| `journal-prepare-failed` | `BeforeMutation` fails | `Invoked=false`; no config mutation |
| `forward-reentry-causation-unknown` | durable `prepared` survives process boundary | pending/manual; no retry or rollback ownership |
| `forward-postwrite-unknown` | same-call readback fails or is third state | stop later writes; keep active journal |
| `serena-pin-invalid` | row pin missing/extra/disagreeing/outside pin root/unreadable/checksum mismatch | reject before first rollback write |
| `rollback-cas-conflict` | lock-scoped live state differs from effective receipt | no write; terminal skipped conflict |
| `rollback-route-preservation-blocked` | any legacy row is not live-verified route-ready | keep canonical; pending or skipped-dependency-conflict |
| `rollback-disposition-not-durable` | disposition publication fails | stop; active journal remains |
| `rollback-retirement-not-durable` | durable re-read is nonterminal or rename fails | return error; active journal remains |

Diagnostics may include failure ID, surface, client, language, entry name,
generation, and state. They must not include backup contents, headers, tokens,
environment values, raw entries, or pin bytes.

## Deterministic acceptance probes

Each probe must exercise the real API mutation owner, not a transition helper
alone. Each defect mutation must make the named test fail, then the exact source
must be restored and the test rerun green.

| Gap | Named regression guard: expected result | Defect mutation that must fail it |
| --- | --- | --- |
| F1 | `TestMCPFrontV3_ConditionalMutationRejectsInterveningEdit` tables Serena add, LSP canonical add, LSP legacy remove, LSP rollback add/remove; an edit injected after backup/precondition but before the former write point remains byte-identical and hub mutation count is zero. | Replace conditional mutation with `GetEntry` followed by ordinary `AddEntry`/`RemoveEntry`. |
| F2 | `TestMCPFrontV3_NoInvocationStateEqualityNeverCreatesReceipt` tables Serena add, LSP add, and LSP remove with mutation count zero and external state equal to intended; re-entry remains pending/unowned and rollback mutation count is zero. | Promote durable `prepared` or `precondition-conflict` when live equals intended. |
| F3 | `TestMCPFrontV3_SerenaAbsentBaselineUsesOwnedRemove` performs successful absent-to-present forward then rollback; owned entry is removed and verified absent. A changed replacement remains byte-identical and returns conflict. | Send absent baseline through restore-from-bytes or call ordinary remove. |
| F4 | `TestMCPFrontV3_LSPDependencyBarrierSurvivesRetry` runs two rollback calls: terminal legacy conflict plus canonical pending on call 1; call 2 retains canonical. Table a baseline-only legacy row that is missing, unreachable, disabled, and live-baseline-ready. | Filter terminal rows before rebuilding groups or initialize readiness from omission. |
| A-01 | `TestMCPFrontV3_RowsExclusivelyOwnSerenaPins` supplies otherwise-valid artifacts with missing, extra, duplicate, escaped, checksum-disagreeing, and projection-only pins; every case performs zero writes and retains the active artifact. | Verify a top-level compatibility projection instead of each authoritative row. |
| A-02 mutation order | `TestMCPFrontV3_RealMutationSeamsRequireDurablePrepare` forces prepared persistence failure through real Serena and LSP add/remove paths; every adapter mutation counter remains zero. | Move the real adapter call before `BeforeMutation` succeeds. |
| A-02 retirement | `TestMCPFrontV3_RollbackCallerRereadsDurableStateBeforeRetirement` gives in-memory terminal state while durable publication fails/remains pending; retirement counter remains zero and active path remains. | Retire from the in-memory object or helper result. |

The existing C1, C2, C4, C8, C9, and C10 mutation-failing guards remain required.
Protected C3, C5, C6, and C7 guards remain green and their production files
remain diff-free.

## Defect-class completeness audit

| Class / participant | Required disposition |
| --- | --- |
| Authorization: Serena forward | Change: conditional wrapper owns read, backup, durable prepare, add, readback. |
| Authorization: LSP forward canonical add | Change: conditional wrapper; no split check/write. |
| Authorization: LSP forward legacy remove | Change: conditional wrapper; still downstream of durable canonical receipt. |
| Authorization: LSP rollback canonical add/remove | Change: conditional wrapper with effective receipt matcher. |
| Authorization: LSP rollback legacy add | Change: conditional wrapper with effective receipt matcher and exact raw baseline. |
| Authorization: Serena present rollback | Retain existing nine-adapter rollback-bypass CAS; row-owned pin input replaces `MigrateReport`. |
| Authorization: Serena absent rollback | Change: existing nine-adapter `CASGuardedRemoveEntry`. |
| Attempt provenance: precondition conflict | Change: separate `Invoked=false` state; never settlement-promoted. |
| Attempt provenance: prepared crash | Change: permanently pending/manual after re-entry absent stronger atomic receipt. |
| Present/absent inverse: Serena | Change: explicit present restore / absent remove polarity. |
| Present/absent inverse: LSP | Retain polarity, move comparison and mutation into conditional wrapper. |
| Pin authority | Change: path/checksum live on each Serena row; validate rows only. |
| Dependency group/retry | Change: reconstruct all rows every call; terminal and baseline-only rows participate. |
| Retirement | Change tests; retain durable re-read and atomic rename, add durable group/terminal-conflict semantics. |
| Compatibility projection | Remove from version-3 decision paths and persisted v3 type. |
| Stale helpers | Remove four superseded helper families and tests that only cover them. |
| C1 exact identity | Not affected; retain `(surface, client, language, entry_name)`. |
| C2 per-row port | Not affected; retain per-row applied receipt. |
| C4 changed Serena entry | Strengthened; present and absent inverse both CAS-owned. |
| C8 post-success promotion | Strengthened; same-call invoked observation required. |
| C9 absent/unreachable LSP | Not affected; still pending when ownership may exist. |
| C10 frozen population | Not affected; plan application never re-enumerates. |
| C3 `--check` gate | Protected/not affected. |
| C5 operation lock | Protected/not affected. |
| C6 LSP readiness | Protected/not affected. |
| C7 route cleanup | Protected/not affected. |

## Diff-invisible invariants and named guards

| Invariant | Named regression guard | Expected |
| --- | --- | --- |
| No external or concurrent hub edit is overwritten unless comparison and mutation share one adapter-owned critical section. | F1 conditional-mutation table | Injected edit preserved; zero hub mutation. |
| Durable intent or precondition conflict is not mutation causation. | F2 zero-invocation equality table | No receipt and no rollback mutation. |
| Every legal Serena forward transition has an ownership-checked inverse. | F3 present/absent table | Present restores; absent removes; replacement survives. |
| Legacy readiness is reconstructed from every row on every retry. | F4 two-call and baseline-only table | Canonical survives any unready legacy row. |
| Every Serena row resolves to one verified pin before any write. | A-01 valid-v3 malformed-pin table | Zero writes; active artifact retained. |
| Real mutations require durable prepare and retirement requires durable re-read. | A-02 real-seam tests | Mutation/retirement counters remain zero on injected persistence failure. |

## Architecture claims

1. `{ guarantee: every Serena-forward and LSP mutation is authorized and
   executed inside one locking-wrapper critical section; single-owner:
   clients.ConditionalEntryMutator; enforcement-probe:
   TestMCPFrontV3_ConditionalMutationRejectsInterveningEdit }`
2. `{ guarantee: the conditional capability covers every production adapter
   without expanding CASEntryMutator's nine-concrete-adapter allowlist;
   single-owner: lockingClient method set plus AsCASEntryMutator allowlist;
   enforcement-probe: production adapter matrix asserts conditional=yes for all
   and CAS membership remains exactly the current nine }`
3. `{ guarantee: prepared and no-write conflict states never become applied
   from re-entry value equality; single-owner: forward row transition owner;
   enforcement-probe:
   TestMCPFrontV3_NoInvocationStateEqualityNeverCreatesReceipt }`
4. `{ guarantee: Serena absence is inverted only by an applied-fingerprint CAS
   removal; single-owner: Serena row rollback owner;
   enforcement-probe: TestMCPFrontV3_SerenaAbsentBaselineUsesOwnedRemove }`
5. `{ guarantee: each version-3 Serena row owns one verified pin path and
   checksum; single-owner: version-3 row validator;
   enforcement-probe: TestMCPFrontV3_RowsExclusivelyOwnSerenaPins }`
6. `{ guarantee: terminal, baseline-only, pending, and restored legacy rows all
   participate in every reconstructed rollback group; single-owner: LSP
   dependency-group rollback owner; enforcement-probe:
   TestMCPFrontV3_LSPDependencyBarrierSurvivesRetry }`
7. `{ guarantee: no real adapter call occurs before exact prepared persistence;
   single-owner: ConditionalEntryMutator.BeforeMutation boundary;
   enforcement-probe:
   TestMCPFrontV3_RealMutationSeamsRequireDurablePrepare }`
8. `{ guarantee: active retirement is decided only from a durable report
   re-read containing terminal row and group outcomes; single-owner: rollback
   retirement gate; enforcement-probe:
   TestMCPFrontV3_RollbackCallerRereadsDurableStateBeforeRetirement }`
9. `{ guarantee: version-1 and version-2 artifacts remain read-only refused and
   are never rewritten as version 3; single-owner: version envelope gate;
   enforcement-probe: existing separate v1/v2 byte-identity no-write cases }`
10. `{ guarantee: C1/C2/C4/C8/C9/C10 remain closed and C3/C5/C6/C7 owners are
    unchanged; single-owner: Change-Surface Contract; enforcement-probe:
    existing mutation suite plus protected-file diff check }`

## Alternatives rejected

| Alternative | Rejection driver |
| --- | --- |
| Expand `CASEntryMutator` to every LSP adapter | That interface owns adapter-specific backup-byte restore and its concrete method set is a deliberate allowlist (`internal/clients/cas_mutator.go:54-66`, `:119-165`). F1 needs generic add/remove authorization, not new restore semantics. |
| Keep point-in-time LSP comparison | It permits the verified split check/write race (`.scratch/external-reviews/adversarial.out:78-96`). |
| Auto-promote prepared rows from equality | Equality cannot prove whether the adapter was invoked (`.scratch/external-reviews/adversarial.out:100-129`). |
| Add cross-projection validation while retaining two authorities | It layers another check over the A-01 split owner; removing projections gives one maintained decision source (`.scratch/external-reviews/claim.out:156-168`). |
| Append-only event log | It adds replay/repair/compaction owners; no admitted requirement needs full history once ambiguous crash windows remain explicitly pending. |

## Migration and compatibility

Version 3 has not shipped from this branch, so its schema is revised in place.
The report filename remains unchanged. Versions 1 and 2 are decoded only far
enough to return `legacy-ownership-unproven`; their bytes and pins are not
modified. No automatic migration exists. An artifact written by the superseded
version-3 worktree shape is rejected by strict row validation rather than
interpreted through compatibility projections.

Rollback of this decision is a local code rollback before publication. A
version-3 artifact created by the accepted implementation remains owned by its
row schema; older code must not consume it.

## Residual risk

- The client-config file lock is advisory. Non-hub writers that ignore it can
  still race the adapter's final file replacement.
- No cross-file transaction atomically commits the client config and recovery
  journal. The design makes that window explicit and manual instead of guessing
  causation.
- Terminal conflicts can leave the safe front canonical route in place. The
  command reports non-success and names the preserved rows; it does not
  overwrite operator state to force symmetry.

## Terms and Abbreviations

- **CAS:** compare-and-set; compare live state and mutate under one config lock.
- **Conditional mutation:** wrapper-owned live comparison, durable preparation,
  adapter mutation, and readback under one config lock.
- **LSP:** Language Server Protocol.
- **MCP:** Model Context Protocol.
- **Pin:** retention-immune backup path plus its recorded SHA-256 checksum.
- **Receipt:** durable proof from the same invocation that a row mutation was
  invoked and its intended post-state was observed.
- **Terminal conflict:** deliberate no-write preservation of operator state;
  rollback completes partially and returns non-success.

Gate: **PASS** — accepted-ready for backend implementation without a new
architecture choice. The architecture-reviewer owns promotion from `proposed`
to `accepted`.
