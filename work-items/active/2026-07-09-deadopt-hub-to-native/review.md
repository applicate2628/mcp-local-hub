# Adversarial architecture review - 2026-07-09

Verdict: **REVISE** → **RESOLVED (2026-07-10)** — see "## Revision resolves" at the end.

This review rejects the current de-adopt design for implementation. The design
correctly says safe de-adopt requires durable adopt-side provenance, but current
adopt does not persist pre-adopt provenance. Therefore the design's round-trip
invariant, "`adopt -> de-adopt` restores every selected client's MCP config to
the pre-adopt entry state", is **not implementable as written**. De-adopt is
blocked on an adopt-side provenance change before any implementation starts.

> **UPDATE 2026-07-10:** the blocking prerequisite (adopt-side durable pre-adopt
> provenance) SHIPPED in PR #528 (squash `16dba601`), and `design.md` was revised
> to consume the AS-SHIPPED contract and close every finding below. This review's
> REVISE is resolved; see "## Revision resolves (2026-07-10)". The verdict-of-record
> for the current design is the revised `design.md` gate decision **PASS**.

Source: adversarial architecture review memo supplied on 2026-07-09. The
machine-local source path is intentionally not recorded here.

## Confirmed defects

| ID | Field | Detail |
|---|---|---|
| F1 | Severity | Critical |
|  | Defect | Last-binding manifest delete is not protected by an execute-time hash gate. |
|  | Trigger | A plan reads a matching manifest hash; an operator edits the manifest before execute. |
|  |  | De-adopt then deletes the edited manifest when removing the last binding. |
|  | Evidence | The design requires hash-matched deletion at `work-items/active/2026-07-09-deadopt-hub-to-native/design.md:34`. |
|  |  | It requires plan-time hash validation at `work-items/active/2026-07-09-deadopt-hub-to-native/design.md:102`. |
|  |  | Execute later deletes at `work-items/active/2026-07-09-deadopt-hub-to-native/design.md:123`. |
|  |  | `ManifestEdit` has an expected-hash gate at `internal/api/manifest.go:717`-`internal/api/manifest.go:720`. |
|  |  | `ManifestDelete` and `ManifestDeleteIn` only stat and `RemoveAll` at `internal/api/manifest.go:774`-`internal/api/manifest.go:800`. |
|  | Required revision | Add expected-hash protection to both selected-client binding edits and last-binding deletion. |
|  |  | Require the execute path to check the hash at the mutation point. |
| F2 | Severity | Critical |
|  | Defect | Provenance is forgotten before adopt-routed secret keys are deleted. |
|  | Trigger | Last-client de-adopt removes or closes provenance, then routed secret cleanup fails. |
|  |  | Retry no longer has the durable key list. |
|  | Evidence | The design stores routed secret keys in provenance at `work-items/active/2026-07-09-deadopt-hub-to-native/design.md:74`. |
|  |  | It forgets managed/provenance rows before deleting keys at `work-items/active/2026-07-09-deadopt-hub-to-native/design.md:125`-`work-items/active/2026-07-09-deadopt-hub-to-native/design.md:126`. |
|  |  | `deleteAdoptRoutedSecrets` can fail on vault open or key delete at `internal/api/adopt_secret_route.go:161`-`internal/api/adopt_secret_route.go:188`. |
|  | Required revision | Delete routed keys before closing/forgetting provenance. |
|  |  | Or keep a `cleanup_pending` provenance row until all artifact cleanup succeeds. Retry must be idempotent. |
| F3 | Severity | High |
|  | Defect | Gate-ON `/g/` semantics are unresolved. Group bindings are server-scoped, not client-scoped. |
|  | Trigger | A group contains server `s`, and server `s` is adopted by clients A and B. |
|  |  | De-adopt only A: `Bindings[A]` drops `s`, but `/g/<group>` still binds `s` because the manifest remains for B. |
|  |  | De-adopt the last client and delete the manifest: the declared group can remain known while routing nothing. |
|  | Evidence | The design only requires `/clients/<target>/mcp` to drop the server at `work-items/active/2026-07-09-deadopt-hub-to-native/design.md:115`. |
|  |  | It preserves other-client bindings at `work-items/active/2026-07-09-deadopt-hub-to-native/design.md:154`. |
|  |  | It tests only `Bindings[target]` at `work-items/active/2026-07-09-deadopt-hub-to-native/design.md:171`. |
|  |  | Groups are server-name subsets in `groups.yaml` at `CLAUDE.md:1526`-`CLAUDE.md:1545`. |
|  |  | Resolver group bindings are built from `Group.Servers` at `internal/api/hub_mcp_resolver.go:204`-`internal/api/hub_mcp_resolver.go:214`. |
|  |  | Resolver group bindings include every daemon of each named server at `internal/api/hub_mcp_resolver.go:301`-`internal/api/hub_mcp_resolver.go:367`. |
|  |  | Declared groups remain known even with no live bindings at `internal/api/hub_mcp_resolver.go:98`-`internal/api/hub_mcp_resolver.go:106`. |
|  | Required revision | Define `/g/` behavior separately from `/clients/`. |
|  |  | Either preserve server-scoped group routing while any manifest binding remains, or block/report subset de-adopt when group exposure would violate intent. |
|  |  | For final de-adopt, define whether groups are edited or whether an orphaned-group warning is surfaced. |

## Additional blocking gaps

| Gap | Field | Detail |
|---|---|---|
| Backup retention | Trigger | Adopt records an ordinary install backup path; later installs exceed `backups.keep_n`. |
|  |  | Pruning deletes that timestamped backup before de-adopt runs. |
|  | Evidence | The design relies on a pinned backup ref at `work-items/active/2026-07-09-deadopt-hub-to-native/design.md:72`. |
|  |  | It prefers a pinned backup ref at `work-items/active/2026-07-09-deadopt-hub-to-native/design.md:76`. |
|  |  | `BackupKeep` keeps only `keepN` timestamped backups at `internal/clients/clients.go:190`-`internal/clients/clients.go:195`. |
|  |  | Pruning drops older timestamped backups at `internal/clients/clients.go:1141`-`internal/clients/clients.go:1190`. |
|  |  | `PruneBackupsForBackupPath` applies the cap at `internal/clients/clients.go:1193`-`internal/clients/clients.go:1208`. |
|  | Required revision | Store the restore artifact in provenance-owned non-prunable storage, or add a real pin mechanism honored by the backup owner. |
|  |  | An ordinary timestamped backup path is insufficient. |
| Lock order and cleanup journaling | Trigger | Implementation acquires `hub-mcp.lock`, then calls a resolver publish or group read-modify-write owner that acquires the same lock. |
|  |  | Or it holds provenance/aggregate locks while doing supervisor IPC, kill, or wait operations. |
|  | Evidence | The design says execute should acquire provenance and use `hub-mcp.lock` at `work-items/active/2026-07-09-deadopt-hub-to-native/design.md:120`. |
|  |  | It then performs client restore, manifest mutation, supervisor cleanup, republish, and reconcile at `work-items/active/2026-07-09-deadopt-hub-to-native/design.md:121`-`work-items/active/2026-07-09-deadopt-hub-to-native/design.md:124`. |
|  |  | `PublishGroupsSnapshotLocked` says callers do not hold `hub-mcp.lock` and acquires it itself at `internal/api/hub_mcp_resolver.go:459`-`internal/api/hub_mcp_resolver.go:476`. |
|  | Required revision | Add a lock graph and phase boundaries before implementation. |
|  |  | Provenance read-modify-write and journal updates must hold only the provenance lock. |
|  |  | Do not hold outer `hub-mcp.lock` around helpers that acquire it. |
|  |  | Client config locks must remain one-file tight, supervisor-intent locks must cover only intent read-modify-write, and no IPC/kill/wait should run under state locks. |
| Durable schema decision | Trigger | Implementation adds `<state-dir>/adopted-entries.json` or extends `managed-entries.json` from this design alone. |
|  |  | Later owners can diverge on schema and lifecycle. |
|  | Evidence | The design proposes a new adopt provenance file and operation states at `work-items/active/2026-07-09-deadopt-hub-to-native/design.md:68`-`work-items/active/2026-07-09-deadopt-hub-to-native/design.md:75`. |
|  |  | It calls this a new durable surface at `work-items/active/2026-07-09-deadopt-hub-to-native/design.md:194`-`work-items/active/2026-07-09-deadopt-hub-to-native/design.md:196`. |
|  | Required revision | Add an accepted decision registry entry for the durable provenance schema. |
|  |  | Or define a schema-compatible `managed-entries.json` extension before coding. |

## Prerequisites before de-adopt implementation

1. Adopt must write durable pre-adopt provenance before any irreversible adopt mutation: per-client original state, absent/present state, a protected restore artifact, the expected hub shape, the generated manifest hash, routed secret keys, and operation state.
2. The restore artifact must be non-prunable or genuinely pinned by the backup owner. An ordinary timestamped backup path is insufficient.
3. Manifest mutation needs expected-hash protection for both selected-client binding edits and last-binding deletion.
4. De-adopt must define `/g/` semantics separately from `/clients/`.
5. The execute algorithm needs a lock graph and journaled cleanup states before implementation.
6. A decision registry entry is required for the new durable provenance schema or for a schema-compatible `managed-entries.json` extension.

Future prerequisite item to admit: **adopt-side durable pre-adopt provenance**.
It should design and deliver the adopt-side schema and write path that records
the data in prerequisite 1 before any irreversible adopt mutation, plus the
non-prunable restore artifact in prerequisite 2. The item has not been admitted
yet and must not be created by this review-recording task.

## Test-plan gaps

| ID | Gap | Required failing test |
|---|---|---|
| T1 | The round-trip restore test can pass by saving a pre-adopt snapshot inside the test. | Run adopt, simulate a new process or reload, then de-adopt using only persisted provenance. |
|  | It does not prove persisted provenance exists. | Fail if the test passes an in-memory snapshot to de-adopt. |
| T2 | No plan/execute manifest-delete race test. | Build a de-adopt plan, edit the manifest, then execute last-binding de-adopt. Expect a conflict and no delete. |
| T3 | No routed-secret cleanup retry test. | Inject a vault delete failure after manifest or supervisor cleanup. |
|  |  | Assert provenance remains `cleanup_pending` and a rerun deletes keys idempotently. |
| T4 | Gate-ON tests ignore `/g/`. | Seed a group containing the adopted server. For subset de-adopt, assert the documented `/g/` policy. |
|  |  | For final de-adopt, assert either groups.yaml cleanup or an explicit orphan warning plus resolver behavior. |
| T5 | No backup-retention test. | Set `backups.keep_n` low, adopt, churn backups past retention, then de-adopt. |
|  |  | Restore must still work from the pinned or provenance-owned artifact. |
| T6 | No lock-order/deadlock test. | Hold `hub-mcp.lock` and verify de-adopt does not call a helper that re-acquires it while already held. |
|  |  | Separately assert no IPC, kill, or wait happens while provenance or hub locks are held. |

## Status consequence

The work-item is no longer "design accepted". Its state is **REVISE / blocked
on prerequisite**. Implementation must not start until the design is revised,
the adopt-side durable pre-adopt provenance prerequisite is admitted and
delivered, and the revised design accounts for the defects and prerequisites
above.

## Revision resolves (2026-07-10)

Prerequisite DELIVERED: adopt-side durable pre-adopt provenance shipped in PR #528
(squash `16dba601`), archived at
`work-items/archive/2026-07/2026-07-09-adopt-side-durable-pre-adopt-provenance/`.
`design.md` was revised against the AS-SHIPPED store
(`internal/api/adopted_entries.go`). Finding-by-finding resolution (each verified
against merged code):

| Finding / gap | Resolution in revised `design.md` |
|---|---|
| **F1** manifest-delete not hash-gated | New shared `ManifestDeleteInWithHash` gates the last-binding delete at the mutation point using the row's `ExpectedManifestHash` (both hashes populated at capture). Decision `work-items/decisions/2026-07-10-deadopt-manifest-delete-hash-gate.md` (round 2: `status: accepted`, + fail-closed-on-empty polarity per SEC P2-a). "Manifest mutation" section + claim 3 + test T2. |
| **F2** provenance forgotten before secret cleanup | Routed keys deleted via `deleteAdoptRoutedSecrets` BEFORE `CloseAdoptProvenance`; the row stays `de_adopting` (the shipped enum's recoverable "cleanup pending" state, keys retained) until every key is gone; idempotent retry (skip already-absent keys — `vault.Delete` errors on missing); P3-2 shared-key scan folded as a SHOULD. "Routed-secret cleanup" section + claim 4 + test T3. |
| **F3** `/g/` semantics unresolved | Defined server-scoped `/g/` policy separate from `/clients/`: subset de-adopt leaves the group routing (manifest lives); last-binding delete surfaces an orphaned-group warning + event, no auto-edit of `groups.yaml`. "Gate-ON aggregate + `/g/` groups" section + claim 10 + test T4. |
| **present-merged-lower** (new state the memo predated) | Restore by REMOVING the hub write-target entry (no snapshot); the lower layer re-emerges; reported functional-equivalent, distinct from `absent`. "Per-client restore" section + claim 2 + test 3. |
| **P2-1** snapshot integrity | `SnapshotSHA256` recomputed via `ManifestHashContent(snapshotBytes)` and refused fail-closed on mismatch OR missing snapshot before the restore helper (present clients only). Claim 1 + tamper test. |
| Backup retention gap | RESOLVED by the non-prunable pinned snapshot (adopt-owned dir, hardened writer); de-adopt restores from it, not a timestamped backup. Test T5. |
| Lock order / cleanup journaling gap | Lock graph extends the shipped order `<manifest>.lease → adopted-entries.lock → <snapshot>.lock`; per-manifest lease guards de-adopt↔adopt; no IPC/kill/wait under state locks; `de_adopting` is the durable journal marker. Claim 8 + test T6. |
| Durable schema decision gap | RESOLVED — two accepted decisions shipped (`2026-07-10-adopt-provenance-store-shape.md`, `2026-07-10-adopt-provenance-crash-consistency-model.md`); de-adopt consumes the schema, no new store-schema decision. |
| **T1** persisted-only round-trip | De-adopt round-trips from DISK ALONE via `ReadAdoptProvenance` + the pinned snapshot, read via a fresh API instance. Claim 5 + test T1. |

Verdict of record for the current design: the revised `design.md` gate decision
**PASS**. Next stage: `$planner`.

## Second-round design gate (2026-07-10) — arch + security REVISE, folded in

The revised `design.md` then went through both design gates. Both returned **REVISE**
(design-level; none a redesign) and the `ManifestDeleteInWithHash` decision was PROMOTED
to `accepted`. All items are folded into `design.md` + the decision:

| Gate finding | Resolution in `design.md` |
|---|---|
| SEC **P1** — verify-then-restore-by-path double-read TOCTOU (snapshot swap injects attacker command/url) | Single-read: verify AND restore the SAME in-memory bytes via a NEW `clients.RestoreEntryFromBytesForRollbackWithConfigWriter`; the path-based helper's second `os.ReadFile` (`claude_code.go:200`) is BANNED for the security-critical restore. Change-Surface D2, "Per-client restore" §1, claim 13, test 2 single-read sub-test. |
| SEC **P2-a** — delete gate must NOT inherit the edit path's skip-on-empty | `ManifestDeleteInWithHash` treats empty/absent expected hash as a FAIL-CLOSED refusal (destructive-default polarity). "Manifest mutation" §, decision Consequence (a), claim 14. |
| SEC **P2-b** — restore-vs-remove trusts un-integrity-checked `original_state` | Exact-hub-entry match required before any `RemoveEntry`; documented owner-only row-swap residual + `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` + relax-lane operator warning. "Remove-path integrity gate + threat model" §, claim 15, test 17. |
| SEC **P2-c** — secret/snapshot content in events/errors | Redaction contract (names/key-names/refs/counts/hashes only) + test. "Observability + redaction" §, claim 16, test 16. |
| SEC **P3-a / P3-b** — shared-key deletion; abandoned-retry orphan | P3-a elevated to a plan-surfaced operator warning; P3-b acknowledged as a bounded residual. "Routed-secret cleanup" §3-4. |
| ARCH **F-A** — resume contract contradicts the plan gates + test 14 | Plan gates BRANCH on `OperationState` (fresh `adopted` vs resuming `de_adopting`); per-client + per-step done-ness derivation so a resume SKIPS completed work; reconciled with test 14. "Operation-state machine" §, claim 17. |
| ARCH **F-B** — a SECOND shared-owner change the scope note denied | Blast radius + scope corrected to THREE shared-owner changes; gate-ON zero-binding prune EXTENDS `BuildHubReconcilePlan` (not a de-adopt-local remove). "Gate-ON aggregate" §, decision Consequence (c), claim 19. |
| ARCH **F-C** — complete the lock graph | Full total order `lease → {config-lock \| intent-lock \| adopted-entries.lock \| hub-mcp.lock}` + no-reverse-edge; T6 asserts the ranking. "Lock graph" §, claim 18. |
| ARCH **F-D** — `deleteAdoptRoutedSecrets` all-or-nothing | Pre-filter to still-present keys before calling (not a thin wrapper). "Routed-secret cleanup" §2, provenance-gap flag. |
| Decision | Flipped to `status: accepted`; folded Consequences (a) empty-hash polarity, (b) retained path-escape guard, (c) the F-B second shared-owner change. |

Verdict of record after round 2: `design.md` gate decision **PASS (revised)**; the
decision is `accepted`. Next stage: `$planner`.
