# Adversarial architecture review - 2026-07-09

Verdict: **REVISE**.

This review rejects the current de-adopt design for implementation. The design
correctly says safe de-adopt requires durable adopt-side provenance, but current
adopt does not persist pre-adopt provenance. Therefore the design's round-trip
invariant, "`adopt -> de-adopt` restores every selected client's MCP config to
the pre-adopt entry state", is **not implementable as written**. De-adopt is
blocked on an adopt-side provenance change before any implementation starts.

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
