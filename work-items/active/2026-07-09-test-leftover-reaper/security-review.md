# Adversarial Security Review — Test-Leftover Reaper

Date: 2026-07-09
Reviewer: fable-headed pre-implementation security gate  
Scope: the pre-security-gate test-leftover reaper design at 64aae2cde929919c7400bd12e71f5e154c09cda3:work-items/active/2026-07-09-test-leftover-reaper/design.md.  
Overall verdict: REVISE. All nine findings survived adversarial refutation. Three are P1 live-kill paths; none is softened or accepted as residual risk.

| Severity | Confirmed findings |
|---|---:|
| P1 | 3 |
| P2 | 3 |
| P3 | 3 |

## 1. P1 (attacker) — e2e branch admits a manually-run hub

| Field | Detail |
|---|---|
| Where | Pre-revision GUI branch: 64aae2cde929919c7400bd12e71f5e154c09cda3:work-items/active/2026-07-09-test-leftover-reaper/design.md:144. The actual fixture places MCPHUB_GUI_TEST_PIDPORT_DIR at internal/gui/e2e/fixtures/hub.ts:76-84 and both e2e markers at :101-107; seeded fixture equivalents are :88-96. |
| Attack | An operator debugging a real GUI at internal/gui/e2e/bin/mcphub.exe exports MCPHUB_STATE_DIR_OVERRIDE. The optional/disjunctive such as, or, and when-present marker wording lets that manually-run target pass every old gate. The old supervise sub-branch required no markers at all. |
| Why confirmed | Fixture path plus an exported override is not test provenance. The absent marker was diagnostic rather than a refusal, so the lane could terminate a live manually-run hub. |
| Required fix | For gui-e2e gui and supervise targets, require both MCPHUB_E2E_SCHEDULER=none and MCPHUB_E2E_SUPERVISOR=none in the target PEB. Absence refuses e2e-markers-absent; no optional marker language remains. |
| Verdict | CONFIRMED — P1 live-kill path; survived adversarial refutation. |

## 2. P1 (safety) — no liveness-of-parent guard

| Field | Detail |
|---|---|
| Where | Pre-revision discriminator demoted parent evidence to diagnostics at 64aae2cde929919c7400bd12e71f5e154c09cda3:work-items/active/2026-07-09-test-leftover-reaper/design.md:93-94 and :147-153. Existing fail-closed helper: internal/api/cleanup.go:1687-1708. |
| Attack | Hold an active test at a breakpoint for more than ten minutes, or use npm run test:debug or test:headed. Its hub can be byte-identical to a leftover in path, argv, environment, and age; the physical difference is that its parent is live. |
| Why confirmed | The old min-age option had no hard floor and parent state was not authorization. A valid active test could therefore be killed. |
| Required fix | Require parentDeathGate for every branch: recorded PPID must pass orphanParentProvenDead. Alive, unknown, missing, and probe-error states refuse parent-alive-or-unproven. Apply clamps --min-age-sec to at least 600. |
| Verdict | CONFIRMED — P1 live-kill path; survived adversarial refutation. |

## 3. P1 (control) — narrative-vs-predicate gap

| Field | Detail |
|---|---|
| Where | Pre-revision positive branches at 64aae2cde929919c7400bd12e71f5e154c09cda3:work-items/active/2026-07-09-test-leftover-reaper/design.md:141-145 omitted go-build-cache mcphub.exe. The committed GUI integration test launches a tagged go run GUI and documents its surviving grandchild at internal/cli/gui_integration_test.go:98-124. |
| Attack | The real 2026-07-09 leftover is a go-build-cache mcphub.exe gui whose supervise grandchild outlives Cmd.Wait. It matches neither mcphub-reliability-* nor the exact internal/gui/e2e/bin/ fixture path, so the old predicate cannot select it. |
| Why confirmed | Corrections 7 and 8 cited the observed class as GO evidence while no positive predicate branch could reach it. The design promised coverage it did not have. |
| Required fix | Add the go-build-cache branch for allowed temporary and LocalAppData cache paths with gui or supervise argv, mandatory buildinfo tag, dead parent, env proof, and production-state guard. Preview descendants and re-scan after GUI reap so supervise is not silently stranded. |
| Verdict | CONFIRMED — P1 control failure with a live destructive gap; survived adversarial refutation. |

## 4. P2 (safety) — apply CLI omits --temp-root

| Field | Detail |
|---|---|
| Where | Pre-revision command synopsis at 64aae2cde929919c7400bd12e71f5e154c09cda3:work-items/active/2026-07-09-test-leftover-reaper/design.md:119-122; pre-revision token material at :155-161. |
| Attack | Preview with --temp-root, then apply without it. Recomputing candidates loses the temp-root branch, so the old candidate-only token cannot bind the original scope. |
| Why confirmed | The only branch intended for operator-scoped temp coverage becomes permanently unrunnable or drifts under an omitted scope. Per-candidate hashes do not bind the temp-root scope itself. |
| Required fix | Apply accepts and conditionally requires the same normalized --temp-root as preview. TestLeftoverConfirmToken includes top-level tempRootHash; omitted or different roots refuse token-mismatch. |
| Verdict | CONFIRMED — P2 safety defect; survived adversarial refutation. |

## 5. P2 (safety) — nothing logged BEFORE terminate

| Field | Detail |
|---|---|
| Where | Pre-revision audit was post-kill and best-effort at 64aae2cde929919c7400bd12e71f5e154c09cda3:work-items/active/2026-07-09-test-leftover-reaper/design.md:163-167. The daemon-squatter owner already emits intent before kill at internal/cli/supervise_squatter.go:395-419; daemon recover audits refusals at internal/cli/daemon_recover.go:192-217. |
| Attack | Crash during the kill loop, or make a best-effort event write fail. An irreversible termination then has no durable proof, and aggregate-only refusal logging cannot identify the affected process. |
| Why confirmed | A post-kill best-effort event cannot prove an operation that already happened. The old proposal retained only hashed per-candidate details and aggregate refusals. |
| Required fix | Before every reapOneOrphan call, synchronously persist a local owner-DACL'd intent event with PID, StartedAt, full path, branch, argv, environment proof and override path, buildinfo result, token, and predicate version. Intent-write failure refuses audit-intent-unavailable. Emit a per-candidate outcome after the loop. |
| Verdict | CONFIRMED — P2 forensic and safety gap; survived adversarial refutation. |

## 6. P2 (safety) — parallel mechanisms

| Field | Detail |
|---|---|
| Where | Pre-revision predicate duplicated production-state logic at 64aae2cde929919c7400bd12e71f5e154c09cda3:work-items/active/2026-07-09-test-leftover-reaper/design.md:139-152 and described a separate revalidation/terminate sequence at :159-161. Existing shared kill owner: internal/api/cleanup.go:1730-1736 and :1756-1771. |
| Attack | Implementers can evolve the copied test-leftover terminate sequence or one of the two production-state checks separately from existing cleanup behavior. |
| Why confirmed | Duplicated policy and termination mechanisms drift, particularly in a destructive lane. The old proposal did not name reapOneOrphan as the sole kill owner. |
| Required fix | Compose env/buildinfo/parent rechecks before reapOneOrphan; do not reimplement termination. Collapse both production-state expressions into one productionStateGuard sourced from daemonStateDirReadOnly at internal/api/state_paths_prod.go:53-69. Keep TestLeftoverConfirmToken as the single token owner in internal/api. |
| Verdict | CONFIRMED — P2 safety architecture defect; survived adversarial refutation. |

## 7. P3 (safety) — vacuous spare tests

| Field | Detail |
|---|---|
| Where | Pre-revision test plan at 64aae2cde929919c7400bd12e71f5e154c09cda3:work-items/active/2026-07-09-test-leftover-reaper/design.md:181-195, especially spare tests 3, 4, 7, and 9. The existing injectable terminate seam is orphanTerminateFn at internal/api/cleanup.go:1674-1685. |
| Attack | Delete the guard nominally under test while an overlapping guard still refuses. The test stays green. The old recycled-PID assertion used an OR condition, so it could not distinguish token drift from lost identity filtering; this is the shape of the shipped aggressive-cleanup-token-omits-started-at bug. |
| Why confirmed | A passing test did not falsify the named invariant and could mask regression of the live-kill controls. |
| Required fix | Every refusal fixture satisfies all other gates, asserts the exact refusal reason, and asserts zero orphanTerminateFn calls. Split token mismatch from identity-filter-excludes-recycled-pid. Add the throwing-guard test with guard-evaluation-error and zero termination. |
| Verdict | CONFIRMED — P3 safety test-quality defect; survived adversarial refutation. |

## 8. P3 (safety) — env presence != consumption

| Field | Detail |
|---|---|
| Where | Pre-revision env authorization at 64aae2cde929919c7400bd12e71f5e154c09cda3:work-items/active/2026-07-09-test-leftover-reaper/design.md:80-84 and :138-139. The tagged-only env fallback is at internal/api/state_paths_envfallback.go:1-16 and :53-75; production state resolution is at internal/api/state_paths_prod.go:53-69. |
| Attack | Place a hand-built untagged binary at the fixture path and export MCPHUB_STATE_DIR_OVERRIDE. The PEB reader sees the variable, while the untagged binary ignores it and resolves production state. |
| Why confirmed | Environment presence reports a caller-controlled string, not whether the target image compiles the consumer. The gate observed that go version -m on the actual fixture binary contains -tags=test_state_path_env, making target-image buildinfo a concrete resolving proof. |
| Required fix | Before any kill, call debug/buildinfo.ReadFile on the target ExecutablePath and require test_state_path_env. An absent, unreadable, malformed, or untagged image refuses not-test-tagged even when the override is present. |
| Verdict | CONFIRMED — P3 safety provenance defect; survived adversarial refutation. |

## 9. P3 (control) — citation drift on the e2e-markers discriminator row

| Field | Detail |
|---|---|
| Where | Pre-revision marker row at 64aae2cde929919c7400bd12e71f5e154c09cda3:work-items/active/2026-07-09-test-leftover-reaper/design.md:85-86 and branch wording at :144. The old cited ranges omitted MCPHUB_GUI_TEST_PIDPORT_DIR at internal/gui/e2e/fixtures/hub.ts:84 and internal/gui/e2e/fixtures/seeded-hub.ts:91. |
| Attack | A reviewer follows the cited ranges and cannot see the full discriminator set, making optional-marker wording harder to challenge and allowing an incorrect predicate to look source-backed. |
| Why confirmed | Citation drift is a control failure in a destructive design: the claimed evidence did not fully contain the named runtime fields. |
| Required fix | Correct the table and branch citations to hub.ts:76-84 and :101-107 plus seeded-hub.ts:88-96. Treat the PID-port directory as corroborating evidence and the two none markers as mandatory authorization gates. |
| Verdict | CONFIRMED — P3 control and traceability defect; survived adversarial refutation. |

## Gate Result

All nine findings are recorded as confirmed. The revised design must be re-run through this adversarial security gate before implementation. Until that gate is clean, no implementation, local product-code commit, or publication is authorized.

## Re-gate 2026-07-09 (round 2)

The round-2 re-gate found that the revision at b5e6f6f344af5ee50873e84fb21d1bc4bcf088ce closed all nine findings above and the accepted A/B/C code-audit contracts. It also found the following two new confirmed findings. Overall verdict: REVISE; implementation remains blocked pending a clean re-run against design revision 3.

### 10. P1 (attacker) — standalone `supervise` is byte-identical to a live in-use supervisor; parentDeathGate is vacuous for it

| Field | Detail |
|---|---|
| Where | The pre-round-3 discriminator and positive branches admitted `supervise` at b5e6f6f344af5ee50873e84fb21d1bc4bcf088ce:work-items/active/2026-07-09-test-leftover-reaper/design.md:38 and :42-45, :115-154, :205-210, :217-255. The GUI spawns `supervise` from its resolved own executable at internal/cli/gui_supervisor_owner.go:128-150 and :888-900, detached at internal/cli/gui_supervisor_owner_windows.go:26-31 and with breakaway added at internal/cli/supervisor_spawn_breakaway_windows.go:12-49. A later GUI can adopt the existing process at internal/cli/gui_supervisor_owner.go:20-32 and :109-110 plus internal/cli/gui.go:674-698. orphanParentProvenDead accepts an error-free dead PID state at internal/api/cleanup.go:1702-1708. |
| Problem | The v0.6 supervisor is designed to outlive and later be adopted by a GUI, so a live in-use supervisor can have the dead recorded PPID of its original GUI. A tagged `go run ... gui` from a go-build-cache image with a scratch MCPHUB_STATE_DIR_OVERRIDE can therefore produce a detached `supervise` process that passes the old go-build-cache image, argv, tag, environment, age, production-state, identity, and parentDeathGate predicates. The e2e marker cannot distinguish it: MCPHUB_E2E_SUPERVISOR=none returns before the GUI spawn block at internal/cli/gui.go:653-668, so a real spawned supervisor can never carry that marker through this path. Preview cannot distinguish the active adopted supervisor from a genuine leftover; confirmation could terminate it and its KILL_ON_JOB_CLOSE Job-Object fleet. |
| Fix | Revision 3 removes `supervise` argv from every positive branch and makes supervise-not-tree-reachable the refusal for every standalone row. A supervisor is reaped only as the identity-bound live descendant of a confirmed go-build-cache test GUI in the same operation. It independently passes buildInfoTagGate, productionStateGuard, identity, and the other applicable non-topology gates; its liveness/provenance comes only from the confirmed live GUI ancestry. The fixed order is descendant-before-GUI because it preserves the observable ancestry and never manufactures the ambiguous orphan state; a refused or unconfirmed child prevents the GUI kill. An already-orphaned standalone supervisor remains outside automated scope and requires manual operator reaping with out-of-band identity verification. |
| Verdict | CONFIRMED — P1 live-kill path. Revision 3 must remove standalone-supervise admission and re-gate. |

### 11. P3 (consistency) — Positive Common Gate 1 names no owner

| Field | Detail |
|---|---|
| Where | Positive Common Gate 1 named snapshot completeness without an owner or citation at b5e6f6f344af5ee50873e84fb21d1bc4bcf088ce:work-items/active/2026-07-09-test-leftover-reaper/design.md:119; the Components table at :72-90 omitted it. parseProcessRows owns row parsing and returns the snapshot error at internal/api/cleanup.go:1251-1275. parseOrphans and parseAggressiveCandidates propagate that `snapErr` at internal/api/cleanup.go:1284, :1420, :1469, and :1557, and AggressiveCleanup fails apply closed at :1623-1630. |
| Problem | Gate 1 was the only positive common gate with no named single owner, leaving the direct row scan free to parse separately or drop the existing truncated-snapshot signal. That would violate Audit B's requirement to reuse runProcessSnapshot plus parseProcessRows and inherit the `snapErr` fail-close. |
| Fix | Revision 3 names parseProcessRows and its existing `snapErr` return as the single owner of snapshot-completeness detection in Positive Common Gate 1 and the Components table. The direct row scan reuses runProcessSnapshot plus parseProcessRows; any `snapErr` makes apply refuse snapshot-degraded before termination. T16 and architectural claim 12 drive this exact return path. |
| Verdict | CONFIRMED — P3 ownership and fail-close consistency defect. Revision 3 must name the owner and re-gate. |

### Round-2 Gate Result

The prior nine findings and A/B/C code contracts are closed, but the two new findings above keep the design at REVISE. No implementation is authorized until design revision 3 passes a clean adversarial security re-gate.

## Re-gate 2026-07-10 (round 3)

Round 3 reviewed design revision 3 at `388fe766`. The live-kill trajectory converged from nine original findings including three P1 findings, through one new P1 in round 2, to zero P1 in round 3: `9 (3×P1) → 1×P1 → 0×P1`. That destructive safety result was achieved only by refusing standalone `supervise`, which is the main observed leftover class from the 2026-07-09 incident. Round 3 also confirmed the following P2 and P3 findings.

### 12. P2 (safety) — descendant-before-GUI order can re-manufacture the out-of-scope orphan through the GUI respawn loop

| Field | Detail |
|---|---|
| Where | Revision 3 reaped and confirmed the `supervise` descendant before the GUI at `388fe766:work-items/active/2026-07-09-test-leftover-reaper/design.md:95` and `388fe766:work-items/active/2026-07-09-test-leftover-reaper/design.md:157-169`. |
|  | Claim 8 and T18/T31 asserted that order at `388fe766:work-items/active/2026-07-09-test-leftover-reaper/design.md:224` and `388fe766:work-items/active/2026-07-09-test-leftover-reaper/design.md:253-266`. |
|  | A GUI-spawned owner arms the loop at `internal/cli/gui_supervisor_owner.go:329-335`; its base delay is one second at `internal/cli/gui_supervisor_owner.go:231`. |
|  | Unexpected exit reaches the respawn path at `internal/cli/gui_supervisor_owner.go:365-460`. Confirm-gone takes another snapshot through `internal/api/processes.go:201-235`. |
| Problem | Reaping the child while its GUI lives is an unexpected exit. The armed manager can respawn after one second while the reaper takes the multi-second confirm-gone snapshot. |
|  | The reaper confirms only the original `{PID, StartedAt}`, then reaps the GUI. The replacement can survive as a new standalone `supervise` that the design refuses to chase. |
|  | The asserted order therefore re-manufactures the exact out-of-scope orphan class it claimed never to create. |
| Why confirmed | Ordering alone did not control the concurrent lifecycle owner. Identity binding protects the original child from PID reuse but does not stop a different child identity. |
|  | The one-second production backoff can beat a fresh WMIC/PowerShell census, so the race is reachable without violating a designed gate. |
| Required fix | Version 1 contains no destructive apply. A future v2 needs a single-owner contract that quiesces or stops respawn before descendant action and holds through GUI exit. |
|  | A deterministic race-window test must enlarge the confirm interval and prove no replacement can install. Timing assumptions and a post-GUI orphan chase are unacceptable. |
| Verdict | CONFIRMED — P2 destructive ordering defect. It does not create a v1 preview risk; it blocks v2 apply. |

### 13. P3 (safety) — snapshot ancestry trusts recyclable PPIDs without identity-binding every edge

| Field | Detail |
|---|---|
| Where | Revision 3 used a direct/discovered descendant as provenance at `388fe766:work-items/active/2026-07-09-test-leftover-reaper/design.md:161-169`. |
|  | It tokenized an unspecified “ancestor chain” at `388fe766:work-items/active/2026-07-09-test-leftover-reaper/design.md:173-177`. |
|  | Census rows hold PID/PPID plus creation time at `internal/api/processes.go:53-62` and parse those fields at `internal/api/processes.go:162-180`. |
|  | The design bound `{PID, StartedAt}` only for GUI and `supervise` endpoints, not every intermediate edge. |
| Problem | PPIDs are recyclable. After a real parent exits, an unrelated process can acquire its PID and make a current-snapshot chain appear to connect `supervise` to an admitted GUI. |
|  | Endpoint identity binding does not prove that each current parent identity existed before and parented its child. A false chain could replace `parentDeathGate` provenance. |
| Why confirmed | The snapshot exposes creation time, but revision 3 required neither parent-before-child ordering nor token/fresh-revalidation binding for every intermediate `{PID, StartedAt}`. |
|  | “Ancestor chain” was a list over recyclable identifiers, not a PID-recycle-safe provenance proof. |
| Required fix | Version 1 may display only an `unverified-ppid-chain` hint and never use it as authorization. |
|  | V2 must bind every edge, require possible temporal ordering, bind the full identity chain into the token, freshly revalidate it, and refuse missing/recycled/changed/contradictory edges. |
|  | If the census cannot prove those invariants, v2 needs a stronger operating-system provenance source. |
| Verdict | CONFIRMED — P3 ancestry-proof defect. It does not create a v1 preview risk; it blocks v2 apply. |

### Round-3 Gate Result

Round 3 found zero P1 live-kill paths in revision 3, so the destructive safety gates converged. The convergence covers only the automatically admitted subset: standalone `supervise` remains refused even though it is the principal real leftover population. Because the safe subset has limited demonstrated value and the remaining tree path has confirmed P2/P3 coordination and ancestry defects, the accepted decision is preview/diagnostics-only v1; destructive apply is deferred to v2. The authoritative v1/v2 separation is in `design.md` and `work-items/decisions/2026-07-10-test-leftover-reaper-preview-only-v1.md`.

## Terms and Abbreviations

- **GUI:** graphical user interface.
- **PID / PPID:** process identifier / parent process identifier.
- **P1 / P2 / P3:** security-review severity levels, from highest to lower priority in this work item.
- **V1 / V2:** preview/diagnostics-only version 1 / deferred destructive version 2.
